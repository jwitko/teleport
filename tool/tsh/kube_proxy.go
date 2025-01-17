/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"text/template"
	"time"

	"github.com/gravitational/kingpin"
	"github.com/gravitational/trace"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/lib/asciitable"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/kube/kubeconfig"
	"github.com/gravitational/teleport/lib/srv/alpnproxy"
	"github.com/gravitational/teleport/lib/srv/alpnproxy/common"
)

type proxyKubeCommand struct {
	*kingpin.CmdClause
	kubeClusters      []string
	siteName          string
	impersonateUser   string
	impersonateGroups []string
	namespace         string
	port              string
	format            string
}

func newProxyKubeCommand(parent *kingpin.CmdClause) *proxyKubeCommand {
	c := &proxyKubeCommand{
		CmdClause: parent.Command("kube", "Start local proxy for Kubernetes access."),
	}

	c.Flag("cluster", clusterHelp).Short('c').StringVar(&c.siteName)
	c.Arg("kube-cluster", "Name of the Kubernetes cluster to proxy. Check 'tsh kube ls' for a list of available clusters. If not specified, all clusters previously logged in through `tsh kube login` will be used.").StringsVar(&c.kubeClusters)
	c.Flag("as", "Configure custom Kubernetes user impersonation.").StringVar(&c.impersonateUser)
	c.Flag("as-groups", "Configure custom Kubernetes group impersonation.").StringsVar(&c.impersonateGroups)
	// TODO (tigrato): move this back to namespace once teleport drops the namespace flag.
	c.Flag("kube-namespace", "Configure the default Kubernetes namespace.").Short('n').StringVar(&c.namespace)
	c.Flag("port", "Specifies the source port used by the proxy listener").Short('p').StringVar(&c.port)
	c.Flag("format", envVarFormatFlagDescription()).Short('f').Default(envVarDefaultFormat()).EnumVar(&c.format, envVarFormats...)
	return c
}

func (c *proxyKubeCommand) run(cf *CLIConf) error {
	tc, err := makeClient(cf, true)
	if err != nil {
		return trace.Wrap(err)
	}

	defaultConfig, clusters, err := c.prepare(cf, tc)
	if err != nil {
		return trace.Wrap(err)
	}

	localProxy, err := makeKubeLocalProxy(cf, tc, clusters, c.port)
	if err != nil {
		return trace.Wrap(err)
	}
	defer localProxy.Close()

	if err := localProxy.WriteKubeConfig(defaultConfig); err != nil {
		return trace.Wrap(err)
	}

	if err := c.printTemplate(cf, localProxy); err != nil {
		return trace.Wrap(err)
	}

	// cf.cmdRunner is used for test only.
	if cf.cmdRunner != nil {
		go localProxy.Start(cf.Context)
		cmd := &exec.Cmd{
			Path: "test",
			Env:  []string{"KUBECONFIG=" + localProxy.KubeConfigPath()},
		}
		return trace.Wrap(cf.RunCommand(cmd))
	}

	return trace.Wrap(localProxy.Start(cf.Context))
}

func (c *proxyKubeCommand) prepare(cf *CLIConf, tc *client.TeleportClient) (*clientcmdapi.Config, kubeconfig.LocalProxyClusters, error) {
	defaultConfig, err := kubeconfig.Load("")
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// Use kube clusters from arg.
	if len(c.kubeClusters) > 0 {
		if c.siteName == "" {
			c.siteName = tc.SiteName
		}

		var clusters kubeconfig.LocalProxyClusters
		for _, kubeCluster := range c.kubeClusters {
			clusters = append(clusters, kubeconfig.LocalProxyCluster{
				TeleportCluster:   c.siteName,
				KubeCluster:       kubeCluster,
				Impersonate:       c.impersonateUser,
				ImpersonateGroups: c.impersonateGroups,
				Namespace:         c.namespace,
			})
		}
		c.printPrepare(cf, "Preparing the following Teleport Kubernetes clusters:", clusters)
		return defaultConfig, clusters, nil
	}

	// Use logged-in clusters.
	clusters := kubeconfig.LocalProxyClustersFromDefaultConfig(defaultConfig, tc.KubeClusterAddr())
	if len(clusters) == 0 {
		return nil, nil, trace.BadParameter(`No Kubernetes clusters found from the default kubeconfig.

Please provide Kubernetes cluster names to this command:
    tsh proxy kube <kube-cluster-1> <kube-cluster-2>

Or login the Kubernetes cluster first:
	tsh kube login <kube-cluster-1>
	tsh proxy kube`)
	}

	c.printPrepare(cf, "Preparing the following Teleport Kubernetes clusters from the default kubeconfig:", clusters)
	return defaultConfig, clusters, nil
}

func (c *proxyKubeCommand) printPrepare(cf *CLIConf, title string, clusters kubeconfig.LocalProxyClusters) {
	fmt.Fprintln(cf.Stdout(), title)
	table := asciitable.MakeTable([]string{"Teleport Cluster Name", "Kube Cluster Name"})
	for _, cluster := range clusters {
		table.AddRow([]string{cluster.TeleportCluster, cluster.KubeCluster})
	}
	fmt.Fprintln(cf.Stdout(), table.AsBuffer().String())
}

func (c *proxyKubeCommand) printTemplate(cf *CLIConf, localProxy *kubeLocalProxy) error {
	return trace.Wrap(proxyKubeTemplate.Execute(cf.Stdout(), map[string]interface{}{
		"addr":           localProxy.GetAddr(),
		"format":         c.format,
		"randomPort":     c.port == "",
		"kubeConfigPath": localProxy.KubeConfigPath(),
	}))
}

type kubeLocalProxy struct {
	tc             *client.TeleportClient
	profile        *client.ProfileStatus
	clusters       kubeconfig.LocalProxyClusters
	kubeConfigPath string

	// localProxy is the ALPN local proxy for sending TLS routing requests to
	// Teleport Proxy.
	localProxy *alpnproxy.LocalProxy
	// forwardProxy is a HTTPS forward proxy used as proxy-url for the
	// Kubernetes clients.
	forwardProxy *alpnproxy.ForwardProxy
}

func makeKubeLocalProxy(cf *CLIConf, tc *client.TeleportClient, clusters kubeconfig.LocalProxyClusters, port string) (*kubeLocalProxy, error) {
	certs, err := loadKubeUserCerts(cf.Context, tc, clusters)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// TODO for best performance, avoid loading the entire profile. profile is
	// currently only used for keypaths.
	profile, err := tc.ProfileStatus()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cas, err := loadKubeLocalCAs(profile, clusters.TeleportClusters())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	lpListener, err := alpnproxy.NewKubeListener(cas)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	localProxy, err := alpnproxy.NewLocalProxy(
		makeBasicLocalProxyConfig(cf, tc, lpListener),
		alpnproxy.WithHTTPMiddleware(alpnproxy.NewKubeMiddleware(certs)),
		alpnproxy.WithSNI(client.GetKubeTLSServerName(tc.WebProxyHost())),
		alpnproxy.WithClusterCAs(cf.Context, tc.RootClusterCACertPool),
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	forwardProxy, err := alpnproxy.NewKubeForwardProxy(cf.Context, port, localProxy.GetAddr())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	kubeConfigPath := os.Getenv(proxyKubeConfigEnvVar)
	if kubeConfigPath == "" {
		_, port, _ := net.SplitHostPort(forwardProxy.GetAddr())
		kubeConfigPath = path.Join(profile.KubeConfigPath(fmt.Sprintf("localproxy-%v", port)))
	}

	return &kubeLocalProxy{
		tc:             tc,
		profile:        profile,
		clusters:       clusters,
		kubeConfigPath: kubeConfigPath,
		localProxy:     localProxy,
		forwardProxy:   forwardProxy,
	}, nil
}

// Start starts the local proxy in background goroutines and waits until
// context is done.
func (k *kubeLocalProxy) Start(ctx context.Context) error {
	errChan := make(chan error, 2)
	go func() {
		if err := k.forwardProxy.Start(); err != nil {
			errChan <- err
		}
	}()
	go func() {
		if err := k.localProxy.StartHTTPAccessProxy(ctx); err != nil {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		return trace.Wrap(err)
	case <-ctx.Done():
		return nil
	}
}

// Close removes the temporary kubeconfig and closes the listeners.
func (k *kubeLocalProxy) Close() error {
	removeFileIfExist(k.KubeConfigPath())
	return trace.NewAggregate(k.forwardProxy.Close(), k.localProxy.Close())
}

// GetAddr returns the address of the forward proxy for client to connect.
func (k *kubeLocalProxy) GetAddr() string {
	return k.forwardProxy.GetAddr()
}

// KubeConfigPath returns the temporary kubeconfig path.
func (k *kubeLocalProxy) KubeConfigPath() string {
	return k.kubeConfigPath
}

// WriteKubeConfig saves local proxy settings in the temporary kubeconfig.
func (k *kubeLocalProxy) WriteKubeConfig(defaultConfig *clientcmdapi.Config) error {
	values := &kubeconfig.LocalProxyValues{
		TeleportKubeClusterAddr: k.tc.KubeClusterAddr(),
		LocalProxyURL:           "http://" + k.GetAddr(),
		LocalProxyCAPaths:       make(map[string]string),
		ClientKeyPath:           k.profile.KeyPath(),
		Clusters:                k.clusters,
	}
	for _, teleportCluster := range k.clusters.TeleportClusters() {
		values.LocalProxyCAPaths[teleportCluster] = k.profile.KubeLocalCAPathForCluster(teleportCluster)
	}
	return trace.Wrap(kubeconfig.SaveLocalProxyValues(k.KubeConfigPath(), defaultConfig, values))
}

func loadKubeLocalCAs(profile *client.ProfileStatus, teleportClusters []string) (map[string]tls.Certificate, error) {
	cas := make(map[string]tls.Certificate)
	for _, teleportCluster := range teleportClusters {
		ca, err := loadSelfSignedCA(
			profile.KubeLocalCAPathForCluster(teleportCluster),
			profile.KeyPath(),
			time.Now().Add(defaults.CATTL),
			common.KubeLocalProxyWildcardDomain(teleportCluster),
		)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		cas[teleportCluster] = ca
	}
	return cas, nil
}

func loadKubeUserCerts(ctx context.Context, tc *client.TeleportClient, clusters kubeconfig.LocalProxyClusters) (alpnproxy.KubeClientCerts, error) {
	ctx, span := tc.Tracer.Start(ctx, "loadKubeUserCerts")
	defer span.End()

	// Renew tsh session and reuse the proxy client.
	var proxy *client.ProxyClient
	err := client.RetryWithRelogin(ctx, tc, func() error {
		var err error
		proxy, err = tc.ConnectToProxy(ctx)
		return trace.Wrap(err)
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer proxy.Close()

	// TODO for best performance, load one kube cert at a time.
	keys, err := loadKubeKeys(tc, clusters.TeleportClusters())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	certs := make(alpnproxy.KubeClientCerts)
	for _, cluster := range clusters {
		// Try load from store.
		if key := keys[cluster.TeleportCluster]; key != nil {
			cert, err := kubeCertFromKey(key, cluster.KubeCluster)
			if err == nil {
				log.Debugf("Client cert loaded from keystore for %v.", cluster)
				certs.Add(cluster.TeleportCluster, cluster.KubeCluster, cert)
				continue
			}
			if !trace.IsNotFound(err) {
				return nil, trace.Wrap(err)
			}
		}

		// Try issue.
		cert, err := issueKubeCert(ctx, tc, proxy, cluster.TeleportCluster, cluster.KubeCluster)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		log.Debugf("Client cert issued for %v.", cluster)
		certs.Add(cluster.TeleportCluster, cluster.KubeCluster, cert)
	}
	return certs, nil
}

func loadKubeKeys(tc *client.TeleportClient, teleportClusters []string) (map[string]*client.Key, error) {
	keys := map[string]*client.Key{}
	for _, teleportCluster := range teleportClusters {
		key, err := tc.LocalAgent().GetKey(teleportCluster, client.WithKubeCerts{})
		if err != nil && !trace.IsNotFound(err) {
			return nil, trace.Wrap(err)
		}
		keys[teleportCluster] = key
	}
	return keys, nil
}

func kubeCertFromKey(key *client.Key, kubeCluster string) (tls.Certificate, error) {
	x509cert, err := key.KubeX509Cert(kubeCluster)
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}
	if time.Until(x509cert.NotAfter) <= time.Minute {
		return tls.Certificate{}, trace.NotFound("TLS cert is expiring in a minute")
	}
	cert, err := key.KubeTLSCert(kubeCluster)
	return cert, trace.Wrap(err)
}

func issueKubeCert(ctx context.Context, tc *client.TeleportClient, proxy *client.ProxyClient, teleportCluster, kubeCluster string) (tls.Certificate, error) {
	var mfaRequired bool
	key, err := proxy.IssueUserCertsWithMFA(
		ctx,
		client.ReissueParams{
			RouteToCluster:    teleportCluster,
			KubernetesCluster: kubeCluster,
		},
		func(ctx context.Context, proxyAddr string, c *proto.MFAAuthenticateChallenge) (*proto.MFAAuthenticateResponse, error) {
			return tc.PromptMFAChallenge(ctx, proxyAddr, c, nil /* applyOpts */)
		},
		client.WithMFARequired(&mfaRequired),
	)
	if err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	if err := checkIfCertsAreAllowedToAccessCluster(key, kubeCluster); err != nil {
		return tls.Certificate{}, trace.Wrap(err)
	}

	// Save it if MFA was not required.
	if !mfaRequired {
		if err := tc.LocalAgent().AddKubeKey(key); err != nil {
			return tls.Certificate{}, trace.Wrap(err)
		}
	}

	cert, err := key.KubeTLSCert(kubeCluster)
	return cert, trace.Wrap(err)
}

// proxyKubeTemplate is the message that gets printed to a user when a kube proxy is started.
var proxyKubeTemplate = template.Must(template.New("").
	Funcs(template.FuncMap{
		"envVarCommand": envVarCommand,
	}).
	Parse(`Started local proxy for Kubernetes on {{.addr}}
{{if .randomPort}}To avoid port randomization, you can choose the listening port using the --port flag.
{{end}}
Use the following config for your Kubernetes applications. For example:
{{envVarCommand .format "KUBECONFIG" .kubeConfigPath}}
kubectl version

`))
