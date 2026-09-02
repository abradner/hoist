package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abradner/hoist/pkg/redact"
)

// NewCluster builds a Cluster over the user's kubeconfig ($KUBECONFIG or ~/.kube/config)
// using the named context, or the file's current context when kubeconfigContext is "".
// The second result is the context actually in use, for the caller to print — the one
// piece of cluster identity that may reach output (AGENTS.md §4.4). Nothing is contacted
// here; the first request happens in RunningImages or DockerConfigSecret.
func NewCluster(kubeconfigContext string) (Cluster, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeconfigContext}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("k8s: reading kubeconfig: %w", err)
	}
	name := kubeconfigContext
	if name == "" {
		name = raw.CurrentContext
	}
	if name == "" {
		return nil, "", fmt.Errorf("k8s: kubeconfig has no current context; pass --kube-context")
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, "", fmt.Errorf("k8s: kube context %q is not in the kubeconfig", name)
	}
	rest, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("k8s: kube context %q: %w", name, err)
	}
	// Every spelling of the server a client error could echo: the URL as configured, the
	// bare host[:port], and the host alone — an untyped error can name just the host
	// ("x509: … not 192.0.2.10") with no port to strip.
	hide := redact.Host(rest.Host)
	cs, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return nil, "", fmt.Errorf("k8s: kube context %q: %s", name, redact.Error(err, hide...))
	}
	return FromClientset(cs, hide...), name, nil
}
