package argo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const testServer = "https://192.0.2.10:6443" // RFC 5737 TEST-NET-1: never routable, never real

func newApp(status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      "app-production",
			"namespace": "argocd",
		},
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{gvr: "ApplicationList"}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
}

// TestGetReadsAllFourStatusFields is invariant 3's own reading half: Get must surface exactly
// status.sync.status, status.sync.revision, status.health.status and
// status.operationState.phase, plus status.reconciledAt as a parsed time — nothing else, and
// nothing invented.
func TestGetReadsAllFourStatusFields(t *testing.T) {
	reconciled := "2026-01-02T03:04:05Z"
	app := newApp(map[string]any{
		"sync":           map[string]any{"status": "Synced", "revision": "abc123"},
		"health":         map[string]any{"status": "Healthy"},
		"operationState": map[string]any{"phase": "Succeeded"},
		"reconciledAt":   reconciled,
	})
	dyn := newFakeDynamic(app)
	a := FromDynamicClient(dyn)

	st, err := a.Get(context.Background(), Application{Namespace: "argocd", Name: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	want := Status{
		SyncStatus:     "Synced",
		SyncRevision:   "abc123",
		HealthStatus:   "Healthy",
		OperationPhase: "Succeeded",
		ReconciledAt:   mustParse(t, reconciled),
	}
	if st != want {
		t.Errorf("Get = %+v, want %+v", st, want)
	}
}

// TestGetMissingStatusFieldsAreZero: an Application with no status yet (freshly created,
// never reconciled) must not error — it just reports the zero Status, which every caller
// already treats as "not yet satisfied" rather than a plumbing failure.
func TestGetMissingStatusFieldsAreZero(t *testing.T) {
	dyn := newFakeDynamic(newApp(nil))
	a := FromDynamicClient(dyn)
	st, err := a.Get(context.Background(), Application{Namespace: "argocd", Name: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	if st != (Status{}) {
		t.Errorf("Get = %+v, want the zero Status", st)
	}
}

// TestGetUnparsableReconciledAtIsZeroNotError: a malformed timestamp (never expected from the
// real controller, but this package must not panic or fail on it) degrades to the zero time
// rather than an error — the same "treat as not-yet-satisfied" posture as a missing field.
func TestGetUnparsableReconciledAtIsZeroNotError(t *testing.T) {
	app := newApp(map[string]any{"reconciledAt": "not-a-time"})
	dyn := newFakeDynamic(app)
	a := FromDynamicClient(dyn)
	st, err := a.Get(context.Background(), Application{Namespace: "argocd", Name: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.ReconciledAt.IsZero() {
		t.Errorf("ReconciledAt = %v, want zero", st.ReconciledAt)
	}
}

// TestGetReconciledAtWithFractionalSecondsParses pins down a real Go stdlib behavior this
// package's own Parse call relies on: Copilot (PR #51 round 2) claimed status.reconciledAt
// with fractional seconds (which Kubernetes/Argo CD commonly report) fails against
// time.Parse(time.RFC3339, ...), silently zeroing ReconciledAt. Verified false by direct
// testing: time.Parse is documented as lenient about a fractional-seconds suffix regardless of
// whether the layout itself specifies one, so plain RFC3339 already parses this correctly — no
// code change was made. This test exists so a future refactor that swaps in a stricter
// parser (or a non-stdlib one) gets caught if it silently loses this tolerance.
func TestGetReconciledAtWithFractionalSecondsParses(t *testing.T) {
	reconciled := "2026-01-02T03:04:05.123456789Z"
	app := newApp(map[string]any{"reconciledAt": reconciled})
	dyn := newFakeDynamic(app)
	a := FromDynamicClient(dyn)

	st, err := a.Get(context.Background(), Application{Namespace: "argocd", Name: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := time.Parse(time.RFC3339Nano, reconciled)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ReconciledAt.Equal(want) {
		t.Errorf("ReconciledAt = %v, want %v (parsed from a fractional-second timestamp)", st.ReconciledAt, want)
	}
}

// TestGetMissingApplicationWrapsErrNotFound: a real 404 (wrong name, wrong namespace, or the
// Application not created yet) must be distinguishable from every other error — the caller's
// whole point in checking errors.Is(err, ErrNotFound) is to Block rather than retry forever.
func TestGetMissingApplicationWrapsErrNotFound(t *testing.T) {
	dyn := newFakeDynamic(newApp(nil))
	a := FromDynamicClient(dyn)
	_, err := a.Get(context.Background(), Application{Namespace: "argocd", Name: "does-not-exist"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on a missing Application = %v, want it to wrap ErrNotFound", err)
	}
	// Positive control: the same call against the namespace that does not exist at all is
	// still ErrNotFound, not some other shape of error the caller would mis-handle.
	_, err = a.Get(context.Background(), Application{Namespace: "other-ns", Name: "app-production"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get in a namespace with no such Application = %v, want ErrNotFound", err)
	}
}

// TestRefreshPatchesExactlyTheAnnotation: the sole write this package performs (AGENTS.md
// §4.7) — a merge patch setting argocd.argoproj.io/refresh to exactly "normal", nothing else
// on the object touched.
func TestRefreshPatchesExactlyTheAnnotation(t *testing.T) {
	app := newApp(map[string]any{"sync": map[string]any{"status": "Synced"}})
	dyn := newFakeDynamic(app)
	a := FromDynamicClient(dyn)

	if err := a.Refresh(context.Background(), Application{Namespace: "argocd", Name: "app-production"}); err != nil {
		t.Fatal(err)
	}
	got, err := dyn.Resource(gvr).Namespace("argocd").Get(context.Background(), "app-production", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ann, found, err := unstructured.NestedString(got.Object, "metadata", "annotations", refreshAnnotation)
	if err != nil || !found || ann != refreshNormal {
		t.Fatalf("annotation = %q found=%v err=%v, want %q", ann, found, err, refreshNormal)
	}
	// The status this Application already carried must survive the patch untouched — a merge
	// patch on metadata.annotations must not clobber sibling fields.
	sync, _, _ := unstructured.NestedString(got.Object, "status", "sync", "status")
	if sync != "Synced" {
		t.Errorf("status.sync.status = %q after Refresh, want it untouched (Synced)", sync)
	}
}

// TestRefreshMissingApplicationWrapsErrNotFound mirrors Get's own contract for Refresh: Act
// must be able to tell "the Application doesn't exist" (a config problem) from a transient
// failure.
func TestRefreshMissingApplicationWrapsErrNotFound(t *testing.T) {
	dyn := newFakeDynamic()
	a := FromDynamicClient(dyn)
	err := a.Refresh(context.Background(), Application{Namespace: "argocd", Name: "does-not-exist"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Refresh on a missing Application = %v, want ErrNotFound", err)
	}
}

// TestApplicationNeedsNamespaceAndName: an Application with a blank field is a caller bug this
// package must catch by itself, not by handing an empty string to the API server.
func TestApplicationNeedsNamespaceAndName(t *testing.T) {
	a := FromDynamicClient(newFakeDynamic())
	if _, err := a.Get(context.Background(), Application{Name: "x"}); err == nil {
		t.Error("Get with no namespace: want an error")
	}
	if _, err := a.Get(context.Background(), Application{Namespace: "argocd"}); err == nil {
		t.Error("Get with no name: want an error")
	}
	if err := a.Refresh(context.Background(), Application{Name: "x"}); err == nil {
		t.Error("Refresh with no namespace: want an error")
	}
}

// TestNewFromKubeconfigSelectsContext mirrors pkg/k8s's TestNewClusterSelectsContext exactly
// (same fixture shape, same TEST-NET-1 placeholder addresses, AGENTS.md §4.4): naming a
// context, defaulting to current-context, and refusing an unknown one all behave the same way
// pkg/argo and pkg/k8s must, since both follow the same client-go adaptor conventions.
func TestNewFromKubeconfigSelectsContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
current-context: staging
clusters:
- name: staging
  cluster: {server: "` + testServer + `", insecure-skip-tls-verify: true}
- name: closed
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: me
  user: {token: not-a-real-token}
contexts:
- name: staging
  context: {cluster: staging, user: me}
- name: closed-port
  context: {cluster: closed, user: me}
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	if _, got, err := NewFromKubeconfig(""); err != nil || got != "staging" {
		t.Errorf("NewFromKubeconfig(\"\") = %q, %v; want the current context staging", got, err)
	}
	_, _, err := NewFromKubeconfig("nope")
	if err == nil || !strings.Contains(err.Error(), `kube context "nope" is not in the kubeconfig`) {
		t.Errorf("unknown context: %v", err)
	}

	// A real client against a closed local port: the connection is refused, and the error must
	// say so without the address (AGENTS.md §4.4/§4.6, mirroring pkg/k8s's own redaction test).
	a, _, err := NewFromKubeconfig("closed-port")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = a.Get(ctx, Application{Namespace: "argocd", Name: "app-production"})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), ":1/") || strings.Contains(err.Error(), "https://") {
		t.Errorf("error names the server: %v", err)
	}
	if !strings.Contains(err.Error(), "reading argocd/app-production") {
		t.Errorf("error lost its context: %v", err)
	}
}

func TestNewFromKubeconfigWithoutCurrentContextAsksForOne(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
clusters:
- name: staging
  cluster: {server: "` + testServer + `"}
users:
- name: me
  user: {token: not-a-real-token}
contexts:
- name: staging
  context: {cluster: staging, user: me}
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	_, _, err := NewFromKubeconfig("")
	if err == nil || !strings.Contains(err.Error(), "--kube-context") {
		t.Errorf("want an error asking for --kube-context, got %v", err)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
