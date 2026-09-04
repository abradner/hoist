package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/rollout"
)

// newWatchFixture writes a minimal GitOps repo (no git needed: gitops.Discover reads plain
// files) with one family "app" in "app-production", a Deployment and a Job, plus a config file
// naming it, and points newArgo/newRollout at fakes for the duration of the test — never a
// real cluster or Argo CD instance, per the hard constraints.
func newWatchFixture(t *testing.T) (cfgPath string, fakeArgo *argo.Fake, fakeRollout *rollout.Fake) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-production-app.yaml", ""+
		"apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-app-production\n  namespace: argocd\n"+
		"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/app-production/app\n"+
		"  destination:\n    server: https://kubernetes.default.svc\n    namespace: app-production\n")
	write("cluster/apps/app-production/app/deployment.yaml", ""+
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64)+"\n")
	write("cluster/apps/app-production/app/migrate-job.yaml", ""+
		"apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\nspec:\n  template:\n    spec:\n      containers:\n        - name: migrate\n          image: ghcr.io/example/migrate:v1@sha256:"+strings.Repeat("2", 64)+"\n")

	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	yaml := "repos:\n  - name: gitops\n    path: " + root + "\n    apps_root: cluster/apps\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeArgo = &argo.Fake{}
	fakeRollout = &rollout.Fake{}
	prevArgo, prevRollout := newArgo, newRollout
	newArgo = func(string) (argo.Argo, string, error) { return fakeArgo, "test-context", nil }
	newRollout = func(string) (rollout.Rollout, string, error) { return fakeRollout, "test-context", nil }
	t.Cleanup(func() { newArgo, newRollout = prevArgo, prevRollout })
	return cfgPath, fakeArgo, fakeRollout
}

func TestWatchPrintsSyncHealthRevisionAndRolloutProgress(t *testing.T) {
	cfgPath, fakeArgo, fakeRollout := newWatchFixture(t)
	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	fakeArgo.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: "abc123", HealthStatus: argo.HealthStatusHealthy})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		Complete: true,
		Detail:   `deployment "app" successfully rolled out`,
	})
	fakeRollout.SetJobLike("app-production", "migrate", "Job", rollout.JobLikeStatus{Detail: "active=0 succeeded=1 failed=0"})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := runWatch([]string{"--app", "app-app-production", "--once"}, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"app-app-production", "namespace app-production",
		"sync=Synced", "health=Healthy", "revision=abc123",
		"Deployment app: ", "successfully rolled out", "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64),
		"Job migrate: active=0 succeeded=1 failed=0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
}

// TestWatchNeverCallsRefresh is invariant 5's own "test or code-inspection-provable guarantee":
// after a full run (including the case where Argo reports an unhealthy/unsynced Application,
// which a writing command might otherwise be tempted to "fix"), Refresh must never appear
// among the Fake's recorded calls.
func TestWatchNeverCallsRefresh(t *testing.T) {
	cfgPath, fakeArgo, fakeRollout := newWatchFixture(t)
	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	fakeArgo.SetStatus(app, argo.Status{SyncStatus: "OutOfSync", HealthStatus: argo.HealthStatusDegraded})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runWatch([]string{"--app", "app-app-production", "--once"}, cfg, selection{given: map[string]bool{}}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, errOut.String())
	}
	for _, call := range fakeArgo.Calls {
		if strings.HasPrefix(call, "Refresh") {
			t.Fatalf("hoist watch must never call Refresh, but Calls = %v", fakeArgo.Calls)
		}
	}
	if len(fakeArgo.Calls) == 0 {
		t.Fatal("expected at least one Get call — the positive control that this test would catch a Refresh if one happened")
	}
}

func TestWatchUnknownAppListsKnownNames(t *testing.T) {
	cfgPath, _, _ := newWatchFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	code := runWatch([]string{"--app", "does-not-exist", "--once"}, cfg, selection{given: map[string]bool{}}, &bytes.Buffer{}, &errOut)
	if code != exitFailure {
		t.Fatalf("exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errOut.String(), "app-app-production") {
		t.Errorf("stderr should list the known Application names: %s", errOut.String())
	}
}

func TestWatchRequiresApp(t *testing.T) {
	cfgPath, _, _ := newWatchFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	code := runWatch(nil, cfg, selection{given: map[string]bool{}}, &bytes.Buffer{}, &errOut)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
}
