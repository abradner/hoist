package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/rollout"
)

// watchTUIFixture discovers newWatchFixture's repo (one family "app" in "app-production":
// a Deployment and a Job) into a real *service.Service — the way runTUI builds one, so
// buildWatchFunc's own svc.Argo/svc.Rollout/svc.Repo calls have something real to read — and
// hands back the fakes the adapter is built over.
func watchTUIFixture(t *testing.T) (*service.Service, *argo.Fake, *rollout.Fake) {
	t.Helper()
	cfgPath, fakeArgo, fakeRollout := newWatchFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(service.Settings{RepoDir: cfg.Repos[0].Path, AppsRoot: cfg.Repos[0].AppsRoot}, serviceDeps())
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}
	return svc, fakeArgo, fakeRollout
}

// TestBuildWatchFuncFlattensTheSameReadsHoistWatchMakes: the adapter resolves the family to
// its Application and workloads and returns the plain Snapshot the screen renders — from
// argo.Fake and rollout.Fake data, so the values the goldens are drawn from are the values
// a real read produces.
func TestBuildWatchFuncFlattensTheSameReadsHoistWatchMakes(t *testing.T) {
	svc, fakeArgo, fakeRollout := watchTUIFixture(t)
	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	reconciled := time.Date(2026, 9, 9, 11, 58, 0, 0, time.UTC)
	fakeArgo.SetStatus(app, argo.Status{SyncStatus: "OutOfSync", SyncRevision: "abc123", HealthStatus: "Progressing", OperationPhase: "Running", ReconciledAt: reconciled})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "init", Init: true, Image: "ghcr.io/example/app:v2"}, {Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		Replicas: 2,
		Detail:   `Waiting for deployment "app" rollout to finish: 1 of 2 updated replicas are available...`,
	})
	fakeRollout.SetJobLike("app-production", "migrate", "Job", rollout.JobLikeStatus{Detail: "active=1 succeeded=0 failed=0"})

	poll := config.PollConfig{Argo: config.Duration(20 * time.Second), Rollout: config.Duration(5 * time.Second)}
	build := buildWatchFunc(svc, "test-context", "argocd", poll)
	if build == nil {
		t.Fatal("buildWatchFunc must never return a nil builder (PR 7: the cluster is retried per call, never frozen at boot)")
	}
	funcs, err := build("app", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if funcs.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want the tighter of poll.argo/poll.rollout (5s), as hoist watch polls", funcs.Interval)
	}
	got, err := funcs.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := watch.Snapshot{
		App: "app-app-production", Namespace: "app-production",
		SyncStatus: "OutOfSync", HealthStatus: "Progressing", Revision: "abc123", OperationPhase: "Running", ReconciledAt: reconciled,
		Workloads: []watch.Workload{
			{Kind: "Deployment", Name: "app", Replicas: 2, Detail: `Waiting for deployment "app" rollout to finish: 1 of 2 updated replicas are available...`,
				Images: []string{"initContainer init=ghcr.io/example/app:v2", "container app=ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
			{Kind: "Job", Name: "migrate", Detail: "active=1 succeeded=0 failed=0"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Snapshot (-want +got):\n%s", diff)
	}
}

// TestBuildWatchFuncNeverCallsRefresh is the adapter half of "watching never refreshes"
// (the screen half is internal/app/watch's TestNeverImportsClusterPackages): after reads
// of an out-of-sync, degraded Application, the only Argo call recorded is Get.
func TestBuildWatchFuncNeverCallsRefresh(t *testing.T) {
	svc, fakeArgo, fakeRollout := watchTUIFixture(t)
	fakeArgo.SetStatus(argo.Application{Namespace: "argocd", Name: "app-app-production"}, argo.Status{SyncStatus: "OutOfSync", HealthStatus: argo.HealthStatusDegraded})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{})
	funcs, err := buildWatchFunc(svc, "test-context", "argocd", config.PollConfig{})("app", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := funcs.Read(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	gets := 0
	for _, call := range fakeArgo.Calls {
		if strings.HasPrefix(call, "Refresh") {
			t.Fatalf("the watch screen's read must never call Refresh, but Calls = %v", fakeArgo.Calls)
		}
		if strings.HasPrefix(call, "Get") {
			gets++
		}
	}
	if gets != 3 {
		t.Fatalf("want 3 Get calls as the positive control, got Calls = %v", fakeArgo.Calls)
	}
}

func TestBuildWatchFuncNamesAnUnknownCell(t *testing.T) {
	svc, _, _ := watchTUIFixture(t)
	build := buildWatchFunc(svc, "test-context", "argocd", config.PollConfig{})
	if _, err := build("ghost", "app-production"); err == nil || !strings.Contains(err.Error(), `no family "ghost" in app-production`) {
		t.Errorf("unknown family: err = %v", err)
	}
	if _, err := build("app", "nowhere"); err == nil || !strings.Contains(err.Error(), `no env "nowhere"`) {
		t.Errorf("unknown env: err = %v", err)
	}
}

// TestBuildWatchFuncErrorsWithoutACluster replaces the old "nil builder" behavior (PR 7): the
// builder is never nil now that svc.Argo/svc.Rollout are consulted per call rather than once at
// boot, so an unreachable cluster is reported as the real error from whichever client failed —
// the operator sees why, not a generic "none is configured" that could mean anything.
func TestBuildWatchFuncErrorsWithoutACluster(t *testing.T) {
	svc, _, _ := watchTUIFixture(t)
	prevArgo := newArgo
	newArgo = func(string) (argo.Argo, string, error) { return nil, "", errors.New("kubeconfig: no such context") }
	t.Cleanup(func() { newArgo = prevArgo })
	build := buildWatchFunc(svc, "test-context", "argocd", config.PollConfig{})
	if build == nil {
		t.Fatal("buildWatchFunc must never return a nil builder")
	}
	if _, err := build("app", "app-production"); err == nil || !strings.Contains(err.Error(), "no such context") {
		t.Errorf("err = %v, want the real Argo client error", err)
	}
}

// TestClusterRetriedAfterBootFailure is PR 7's own regression test for the boot-frozen-closure
// bug (Train 2 design, AGENTS.md §9 candidate): the old buildWatchFunc took the Argo/rollout
// clients (or the boot error) as plain values, so a cluster unreachable when the TUI opened
// stayed unreachable — watchFn was nil — for the rest of the session, with no way for a later,
// working w to retry it. buildWatchFunc now asks svc.Argo/svc.Rollout fresh every call, and
// Service only memoizes a SUCCESS (service.go's own doc comment), so the very next call after a
// transient boot failure goes through again. This test fails without that per-call re-ask: with
// the client frozen at boot, the second build("app", ...) would still see the first failure.
func TestClusterRetriedAfterBootFailure(t *testing.T) {
	svc, fakeArgo, fakeRollout := watchTUIFixture(t)
	fakeArgo.SetStatus(argo.Application{Namespace: "argocd", Name: "app-app-production"}, argo.Status{SyncStatus: "Synced", HealthStatus: "Healthy"})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{})

	prevArgo := newArgo
	calls := 0
	newArgo = func(string) (argo.Argo, string, error) {
		calls++
		if calls == 1 {
			return nil, "", errors.New("cluster unreachable")
		}
		return fakeArgo, "test-context", nil
	}
	t.Cleanup(func() { newArgo = prevArgo })

	build := buildWatchFunc(svc, "test-context", "argocd", config.PollConfig{})
	if _, err := build("app", "app-production"); err == nil || !strings.Contains(err.Error(), "cluster unreachable") {
		t.Fatalf("first call: err = %v, want the boot failure surfaced", err)
	}
	funcs, err := build("app", "app-production")
	if err != nil {
		t.Fatalf("second call after the cluster recovered: err = %v, want it retried and to succeed", err)
	}
	if _, err := funcs.Read(context.Background()); err != nil {
		t.Fatalf("Read after retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("newArgo called %d times, want exactly 2 (boot failure, then the retry)", calls)
	}
}

// TestWatchFuncSeesFamilyAddedByRefresh is PR 7's own regression test for the other half of the
// boot-frozen-closure bug: the old buildWatchFunc closed over the *gitops.Repo read once at TUI
// boot, so a family an F5 refresh (or a landed promotion) added to origin mid-session was
// invisible to w until the process restarted. buildWatchFunc now reads svc.Repo() — the
// service's own CURRENT view — on every call, so a family that appears after a LoadRepo the
// screen didn't cause (mirroring what an F5 refresh or a completion-triggered relist does) is
// visible on the very next w. This test fails if buildWatchFunc is given (or captures) the repo
// only once: the second build call would still 404 on the new family.
func TestWatchFuncSeesFamilyAddedByRefresh(t *testing.T) {
	svc, fakeArgo, fakeRollout := watchTUIFixture(t)
	build := buildWatchFunc(svc, "test-context", "argocd", config.PollConfig{})

	if _, err := build("app2", "app-production"); err == nil || !strings.Contains(err.Error(), `no family "app2"`) {
		t.Fatalf("before the refresh: err = %v, want app2 to not exist yet", err)
	}

	// Simulate what an F5 refresh (svc.RefreshRepo, RepoFromOrigin) delivers: a new family
	// merged into the repo since boot. RepoFromClone against the same, now-mutated directory
	// is the equivalent read for a test that owns no real origin remote.
	settings := svc.Settings()
	root := settings.RepoDir
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-production-app2.yaml", ""+
		"apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-app2-production\n  namespace: argocd\n"+
		"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/app-production/app2\n"+
		"  destination:\n    server: https://kubernetes.default.svc\n    namespace: app-production\n")
	write("cluster/apps/app-production/app2/deployment.yaml", ""+
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app2\nspec:\n  template:\n    spec:\n      containers:\n        - name: app2\n          image: ghcr.io/example/app2:v1@sha256:"+strings.Repeat("3", 64)+"\n")
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}

	fakeArgo.SetStatus(argo.Application{Namespace: "argocd", Name: "app-app2-production"}, argo.Status{SyncStatus: "Synced", HealthStatus: "Healthy"})
	fakeRollout.SetDeployment("app-production", "app2", rollout.DeploymentStatus{})
	funcs, err := build("app2", "app-production")
	if err != nil {
		t.Fatalf("after the refresh: err = %v, want app2 to resolve", err)
	}
	if _, err := funcs.Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}
}
