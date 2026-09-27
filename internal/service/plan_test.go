package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// mustDiscover is gitops.Discover over root/cluster/apps, failing the test on error — used to
// get independent *gitops.Repo pointers over the identical fixture tree, exactly as a boot-time
// discover and a later F5-triggered discover each produce their own pointer despite reading the
// same manifests.
func mustDiscover(t *testing.T, root string) *gitops.Repo {
	t.Helper()
	r, err := gitops.Discover(root, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// planFixtureRepo writes a minimal two-env, one-family GitOps tree (no git repo needed —
// gitops.Discover reads plain files) and returns its root, for tests that only need Service.Plan
// to run against real manifests without the network/forge machinery inflightFixture sets up.
func planFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	digest := "sha256:" + strings.Repeat("1", 64)
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := func(env string) string {
		return "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-" + env + "\n  namespace: argocd\n" +
			"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/" + env + "/app\n" +
			"  destination:\n    server: https://kubernetes.default.svc\n    namespace: " + env + "\n"
	}
	deployment := func(ref string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: " + ref + "\n"
	}
	write("cluster/apps/app-staging-app.yaml", wrapper("app-staging"))
	write("cluster/apps/app-production-app.yaml", wrapper("app-production"))
	write("cluster/apps/app-staging/app/deployment.yaml", deployment("ghcr.io/example/app:v2@"+digest))
	write("cluster/apps/app-production/app/deployment.yaml", deployment("ghcr.io/example/app:v1@sha256:"+strings.Repeat("0", 64)))
	return root
}

// planTestService builds a Service against planFixtureRepo, with no forge/argo/rollout/network
// dependency wired — enough to exercise Plan, which never touches any of them.
func planTestService(root string) *Service {
	return New(Settings{
		RepoDir:    root,
		AppsRoot:   "cluster/apps",
		Base:       "main",
		Promotable: []string{"ghcr.io/example/"},
	}, Deps{
		Git: func() git.Git { return git.Exec{} },
	})
}

// TestPlanFailsClosedWhenOriginViewMovedUnderARequestExplicitRepo is P3-b (t1-review.md,
// verification review): the TUI builds a plan.Func closure over a *gitops.Repo captured at
// screen-load time (internal/app/plan/model.go's loadCmd, internal/app/app.go's own deploy
// confirm path) and hands it back to Plan as PlanRequest.Repo explicitly. If an F5 refresh
// (LoadRepo(RepoFromOrigin)) lands in between and replaces s.view with a NEW *gitops.Repo before
// this plan actually runs, req.Repo no longer equals s.view.Repo — before this fix, Plan quietly
// built PlannedChange.View as a bare RepoView{Repo: req.Repo} (FromOrigin false), which
// StartPromotion's freshness check reads as "never fetched from origin at all" and checks with
// the far looser checkCloneCurrentForBase instead of the tighter origin check CheckRepoViewCurrent
// runs for an origin-mode view — silently downgrading the very check t1-review.md P2 #6 added.
// Fixed: Plan now fails closed with a plain error naming the mismatch instead of ever building
// that downgraded view. Revert the fix (drop the `else if cur.FromOrigin` branch in plan.go) and
// this test fails because Plan succeeds instead of erroring.
func TestPlanFailsClosedWhenOriginViewMovedUnderARequestExplicitRepo(t *testing.T) {
	root := planFixtureRepo(t)
	svc := planTestService(root)

	// r1 and r2 are two independent Discover results over the identical tree — distinct
	// pointers, exactly what a boot-time discover and a later F5 refresh's discover produce,
	// even though nothing about the manifests themselves changed.
	r1 := mustDiscover(t, root)
	r2 := mustDiscover(t, root)

	// Simulate the TUI having already refreshed to r2 (an F5 landing mid-flight) while the
	// caller's plan.Func closure still names r1, the repo its own screen loaded against.
	svc.mu.Lock()
	svc.view = RepoView{Repo: r2, FromOrigin: true, SHA: "origin-tip-after-refresh"}
	svc.mu.Unlock()

	_, err := svc.Plan(context.Background(), PlanRequest{Repo: r1, Source: "app-staging", Target: "app-production"})
	if err == nil {
		t.Fatal("Plan succeeded with a stale explicit Repo while the service's origin view had moved on — want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "repo view changed") {
		t.Errorf("Plan error = %q, want it to name the repo view having changed", err.Error())
	}
}

// TestPlanCLIControlUnaffectedByFailClosedCheck is this fix's control: the CLI never calls
// LoadRepo, so s.view is always the zero RepoView (FromOrigin false, Repo nil) regardless of
// what PlanRequest.Repo names — the fail-closed branch above must never fire for it. Proves the
// P3-b fix is scoped to origin mode, not a blanket "Repo != s.view.Repo" refusal that would break
// every CLI plan/promote/deploy invocation, which always passes its own freshly discovered repo
// explicitly and never touches s.view at all.
func TestPlanCLIControlUnaffectedByFailClosedCheck(t *testing.T) {
	root := planFixtureRepo(t)
	svc := planTestService(root)

	r := mustDiscover(t, root)

	pc, err := svc.Plan(context.Background(), PlanRequest{Repo: r, Source: "app-staging", Target: "app-production"})
	if err != nil {
		t.Fatalf("Plan (CLI-shaped call, s.view never loaded) failed: %v", err)
	}
	if pc.View.Repo != r || pc.View.FromOrigin {
		t.Errorf("PlannedChange.View = %+v, want {Repo: r, FromOrigin: false} — the CLI's own clone-mode shape", pc.View)
	}
}
