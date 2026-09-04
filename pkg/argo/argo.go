// Package argo drives Argo CD entirely through the Kubernetes API: the dynamic client against
// the Application custom resource (argoproj.io/v1alpha1), never Argo's own REST/gRPC server and
// never an Argo API token (AGENTS.md §4.7, docs/repo-map.md's Kubernetes API row). Refresh
// merge-patches exactly one thing — the argocd.argoproj.io/refresh annotation — and Get only
// reads the object's own status subresource; nothing here imports argo-cd/v3 or
// k8s.io/kubectl. Every dynamic-client error goes through the same pkg/redact boundary
// pkg/k8s established (kubeconfig.go): the API server host, in every spelling a client error
// could echo, is scrubbed before the error leaves this package; only the kube context *name*
// may reach output (AGENTS.md §4.4).
//
// Application.Namespace is where the Application custom resource itself lives on the cluster
// (RepoConfig.Kube.ArgoNamespace) — a control-plane namespace, conventionally "argocd" but
// never assumed to be (invariant 1 of the M5 brief). This is a different thing from
// spec.destination.namespace, which is the workload's own target env (see gitops.Env's doc
// comment): this repo's own fixtures already set both, distinctly, on every Application
// wrapper (metadata.namespace: argocd; spec.destination.namespace: <env>) — pkg/gitops reads
// only the latter (it has no reason to read the former, and is out of scope for this
// milestone), so the former comes from config here instead.
package argo

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abradner/hoist/pkg/redact"
)

// gvr is the Application custom resource's GroupVersionResource. Argo CD has shipped exactly
// this one for the whole v1alpha1 lifetime.
var gvr = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// refreshAnnotation and refreshNormal are the exact literal AGENTS.md §4.7 and the M5 brief's
// invariant 1 name: the sole write this package performs.
const (
	refreshAnnotation = "argocd.argoproj.io/refresh"
	refreshNormal     = "normal"
)

// The status literals this package compares against — copied by value from
// github.com/argoproj/argo-cd's pkg/apis/application/v1alpha1/types.go (Apache License 2.0),
// not guessed (invariant 3 of the M5 brief: "confirm the real enum ... do not guess a made-up
// string"). hoist imports none of that module (AGENTS.md §4.7's 83-dependency refusal); these
// are the same strings the Application CR's status fields actually carry, without the import.
const (
	SyncStatusSynced     = "Synced"
	HealthStatusHealthy  = "Healthy"
	HealthStatusDegraded = "Degraded"
	OperationFailed      = "Failed"
	OperationError       = "Error"
)

// ErrNotFound is wrapped by Get and Refresh when the named Application does not exist —
// distinct from a transient plumbing error (a 404 here means "check kube.argo_namespace and
// the repo's Application wrappers", not "retry").
var ErrNotFound = errors.New("argo: application not found")

// Application identifies one Argo CD Application custom resource: its own name and the
// namespace it lives in on the cluster. See the package doc for why Namespace is not
// spec.destination.namespace.
type Application struct {
	Namespace string
	Name      string
}

func (a Application) String() string { return a.Namespace + "/" + a.Name }

// Status is the subset of an Application's .status this package reads, straight off the
// unstructured object with no local caching — every Get is a fresh read (AGENTS.md §4.1).
type Status struct {
	SyncStatus     string    // status.sync.status: "Synced", "OutOfSync", "Unknown", ...
	SyncRevision   string    // status.sync.revision: the commit sha Argo last compared/synced against
	HealthStatus   string    // status.health.status: "Healthy", "Degraded", "Progressing", ...
	OperationPhase string    // status.operationState.phase: "", "Running", "Succeeded", "Failed", "Error", "Terminating"
	ReconciledAt   time.Time // status.reconciledAt; zero when absent or unparsable
}

// Argo is what internal/engine's ArgoRefreshedStep/ArgoSyncedStep need from Argo CD, driven
// entirely through the Kubernetes API (AGENTS.md §4.7). Both methods work on exactly one
// Application at a time; a promotion touching several calls this once per Application.
type Argo interface {
	// Refresh merge-patches app's annotations with argocd.argoproj.io/refresh: normal — the
	// sole write this package performs. Argo's own controller treats a refresh request as
	// idempotent and clears the annotation once it has processed it, which is exactly why
	// ArgoRefreshedStep does not try to detect "already refreshed" from the annotation's own
	// presence (see its doc comment in internal/engine).
	Refresh(ctx context.Context, app Application) error
	// Get reads app's current status. A missing Application wraps ErrNotFound.
	Get(ctx context.Context, app Application) (Status, error)
}

// client is Argo over a dynamic.Interface, mirroring pkg/k8s's client shape exactly (cluster.go/
// kubeconfig.go): a thin wrapper plus a redaction hide-list for every error boundary.
type client struct {
	dyn  dynamic.Interface
	hide []string
}

// FromDynamicClient wraps an existing dynamic client — the dynamic fake in tests. Every string
// in hide is scrubbed from every error message the returned Argo produces.
func FromDynamicClient(dyn dynamic.Interface, hide ...string) Argo {
	return &client{dyn: dyn, hide: hide}
}

// NewFromKubeconfig builds an Argo over the user's kubeconfig ($KUBECONFIG or ~/.kube/config)
// using the named context, or the file's current context when kubeconfigContext is "" —
// the same loading rules k8s.NewCluster uses (pkg/k8s/kubeconfig.go), duplicated rather than
// shared: pkg/argo is its own self-contained activity-shaped adaptor (AGENTS.md §4.3), and nine
// lines of clientcmd wiring is cheaper than a cross-package dependency between two adaptors
// that otherwise share nothing. The second result is the context actually in use, for the
// caller to print (AGENTS.md §4.4). Nothing is contacted here; the first request happens in
// Refresh or Get.
func NewFromKubeconfig(kubeconfigContext string) (Argo, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeconfigContext}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("argo: reading kubeconfig: %w", err)
	}
	name := kubeconfigContext
	if name == "" {
		name = raw.CurrentContext
	}
	if name == "" {
		return nil, "", fmt.Errorf("argo: kubeconfig has no current context; pass --kube-context")
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, "", fmt.Errorf("argo: kube context %q is not in the kubeconfig", name)
	}
	rest, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("argo: kube context %q: %w", name, err)
	}
	hide := redact.Host(rest.Host)
	dyn, err := dynamic.NewForConfig(rest)
	if err != nil {
		return nil, "", fmt.Errorf("argo: kube context %q: %s", name, redact.Error(err, hide...))
	}
	return FromDynamicClient(dyn, hide...), name, nil
}

// Refresh implements Argo.
func (c *client) Refresh(ctx context.Context, app Application) error {
	if app.Namespace == "" || app.Name == "" {
		return errors.New("argo: application needs a namespace and a name")
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, refreshAnnotation, refreshNormal))
	_, err := c.dyn.Resource(gvr).Namespace(app.Namespace).Patch(ctx, app.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("refreshing %s: %w", app, ErrNotFound)
		}
		return fmt.Errorf("refreshing %s: %s", app, c.describe(err))
	}
	return nil
}

// Get implements Argo.
func (c *client) Get(ctx context.Context, app Application) (Status, error) {
	if app.Namespace == "" || app.Name == "" {
		return Status{}, errors.New("argo: application needs a namespace and a name")
	}
	u, err := c.dyn.Resource(gvr).Namespace(app.Namespace).Get(ctx, app.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Status{}, fmt.Errorf("reading %s: %w", app, ErrNotFound)
		}
		return Status{}, fmt.Errorf("reading %s: %s", app, c.describe(err))
	}
	return statusFromUnstructured(u), nil
}

// statusFromUnstructured reads exactly the four status fields the M5 brief's invariants name,
// off the raw unstructured object — no local caching, no partial-decode into a typed
// Application (which would need argo-cd/v3's own types).
func statusFromUnstructured(u *unstructured.Unstructured) Status {
	str := func(fields ...string) string {
		v, _, _ := unstructured.NestedString(u.Object, fields...)
		return v
	}
	var reconciledAt time.Time
	if v := str("status", "reconciledAt"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			reconciledAt = t
		}
	}
	return Status{
		SyncStatus:     str("status", "sync", "status"),
		SyncRevision:   str("status", "sync", "revision"),
		HealthStatus:   str("status", "health", "status"),
		OperationPhase: str("status", "operationState", "phase"),
		ReconciledAt:   reconciledAt,
	}
}

// describe renders a dynamic-client error without the API server or any response detail,
// mirroring pkg/k8s's client.describe exactly (AGENTS.md §4.4/§4.6 in this package).
func (c *client) describe(err error) string {
	if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
		return redact.Strings(string(reason), c.hide...)
	}
	return redact.Error(err, c.hide...)
}
