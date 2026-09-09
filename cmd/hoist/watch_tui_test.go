package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// watchTUIFixture discovers newWatchFixture's repo (one family "app" in "app-production":
// a Deployment and a Job) and hands back the fakes the adapter is built over.
func watchTUIFixture(t *testing.T) (*gitops.Repo, *argo.Fake, *rollout.Fake) {
	t.Helper()
	cfgPath, fakeArgo, fakeRollout := newWatchFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gitops.Discover(cfg.Repos[0].Path, cfg.Repos[0].AppsRoot)
	if err != nil {
		t.Fatal(err)
	}
	return r, fakeArgo, fakeRollout
}

// TestBuildWatchFuncFlattensTheSameReadsHoistWatchMakes: the adapter resolves the family to
// its Application and workloads and returns the plain Snapshot the screen renders — from
// argo.Fake and rollout.Fake data, so the values the goldens are drawn from are the values
// a real read produces.
func TestBuildWatchFuncFlattensTheSameReadsHoistWatchMakes(t *testing.T) {
	r, fakeArgo, fakeRollout := watchTUIFixture(t)
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
	build := buildWatchFunc(r, fakeArgo, fakeRollout, nil, "argocd", poll)
	if build == nil {
		t.Fatal("a reachable cluster must yield a builder")
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
	r, fakeArgo, fakeRollout := watchTUIFixture(t)
	fakeArgo.SetStatus(argo.Application{Namespace: "argocd", Name: "app-app-production"}, argo.Status{SyncStatus: "OutOfSync", HealthStatus: argo.HealthStatusDegraded})
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{})
	funcs, err := buildWatchFunc(r, fakeArgo, fakeRollout, nil, "argocd", config.PollConfig{})("app", "app-production")
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
	r, fakeArgo, fakeRollout := watchTUIFixture(t)
	build := buildWatchFunc(r, fakeArgo, fakeRollout, nil, "argocd", config.PollConfig{})
	if _, err := build("ghost", "app-production"); err == nil || !strings.Contains(err.Error(), `no family "ghost" in app-production`) {
		t.Errorf("unknown family: err = %v", err)
	}
	if _, err := build("app", "nowhere"); err == nil || !strings.Contains(err.Error(), `no env "nowhere"`) {
		t.Errorf("unknown env: err = %v", err)
	}
}

func TestBuildWatchFuncIsNilWithoutACluster(t *testing.T) {
	r, fakeArgo, fakeRollout := watchTUIFixture(t)
	if build := buildWatchFunc(r, fakeArgo, fakeRollout, errors.New("kubeconfig: no such context"), "argocd", config.PollConfig{}); build != nil {
		t.Error("an unreachable cluster must yield a nil builder, so w says so instead of opening a screen")
	}
	if build := buildWatchFunc(r, nil, nil, nil, "argocd", config.PollConfig{}); build != nil {
		t.Error("nil adaptors must yield a nil builder")
	}
}
