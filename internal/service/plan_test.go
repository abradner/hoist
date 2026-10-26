package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/resolve"
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

// TestPlanNamesTheBadPrefixesBeforeAskingTheCluster is Codex's finding on the CS-1 PR: with
// digest source "pods" in the order, resolveImages built the Kubernetes client even when the
// promotable prefixes matched nothing, so on a machine with no kubeconfig a mistyped prefix
// reported a kubeconfig error instead of the prefixes. Nothing to resolve means no external
// dependency is touched. Control: with prefixes that do match, the same cluster failure is still
// what the operator sees (an unreachable cluster fails the whole plan, AGENTS.md section 4.10).
func TestPlanNamesTheBadPrefixesBeforeAskingTheCluster(t *testing.T) {
	root := planFixtureRepo(t)
	noCluster := errors.New("no kubeconfig in this environment")
	newSvc := func(prefix string) *Service {
		return New(Settings{
			RepoDir:    root,
			AppsRoot:   "cluster/apps",
			Base:       "main",
			Promotable: []string{prefix},
			Resolve:    ResolveOptions{Order: []resolve.Source{resolve.SourcePods}},
		}, Deps{
			Git:     func() git.Git { return git.Exec{} },
			Cluster: func(string) (k8s.Cluster, string, error) { return nil, "", noCluster },
		})
	}
	r := mustDiscover(t, root)
	req := PlanRequest{Repo: r, Source: "app-staging", Target: "app-production"}

	_, err := newSvc("ghcr.io/nobody/").Plan(context.Background(), req)
	if err == nil || errors.Is(err, noCluster) || !strings.Contains(err.Error(), "ghcr.io/nobody/") {
		t.Fatalf("a prefix matching nothing should be named in the error, got: %v", err)
	}
	if _, err := newSvc("ghcr.io/example/").Plan(context.Background(), req); !errors.Is(err, noCluster) {
		t.Fatalf("control: matching prefixes with an unreachable cluster should still fail on the cluster, got: %v", err)
	}
}

// addDevEnv adds a third env (app-dev) to planFixtureRepo's tree, one family running the same
// image repo as the other two, so a pipeline dev -> staging -> production can be configured and
// then skipped.
func addDevEnv(t *testing.T, root string) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("2", 64)
	wrapper := "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-app-dev\n  namespace: argocd\n" +
		"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/app-dev/app\n" +
		"  destination:\n    server: https://kubernetes.default.svc\n    namespace: app-dev\n"
	dep := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v3@" + digest + "\n"
	for rel, content := range map[string]string{
		"cluster/apps/app-dev-app.yaml":            wrapper,
		"cluster/apps/app-dev/app/deployment.yaml": dep,
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func skipPlanService(root string) *Service {
	return New(Settings{
		RepoDir:    root,
		AppsRoot:   "cluster/apps",
		Base:       "main",
		Promotable: []string{"ghcr.io/example/"},
		Repo: &config.RepoConfig{GitHub: "me/gitops", Envs: config.EnvsConfig{
			Production: []string{"app-production"},
			Pairs:      map[string]string{"app-dev": "app-staging", "app-staging": "app-production"},
		}},
	}, Deps{Git: func() git.Git { return git.Exec{} }})
}

func skippedStagingWarnings(pl gitops.Plan) []gitops.Warning {
	var out []gitops.Warning
	for _, w := range pl.Warnings {
		if w.Code == gitops.WarnSkippedStaging {
			out = append(out, w)
		}
	}
	return out
}

// TestPlanWarnsWhenAPromotionSkipsStagingIntoProduction: app-dev -> app-production while the
// pipeline says app-dev promotes to app-staging. The warning is on the plan itself, so the
// dry run and the PR body (engine.RenderPRBody prints plan.Warnings) carry it, not only the
// plan screen's own sentence (AGENTS.md section 4.5).
func TestPlanWarnsWhenAPromotionSkipsStagingIntoProduction(t *testing.T) {
	root := planFixtureRepo(t)
	addDevEnv(t, root)
	svc := skipPlanService(root)

	pc, err := svc.Plan(context.Background(), PlanRequest{Repo: mustDiscover(t, root), Source: "app-dev", Target: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	ws := skippedStagingWarnings(pc.Plan)
	if len(ws) != 1 {
		t.Fatalf("want one %s warning, got %+v", gitops.WarnSkippedStaging, pc.Plan.Warnings)
	}
	if !strings.Contains(ws[0].Message, "straight to production") || !strings.Contains(ws[0].Message, "skipping app-staging") {
		t.Errorf("message = %q", ws[0].Message)
	}
	body := engine.RenderPRBody("abc123", pc.Plan)
	if !strings.Contains(body, "[skipped-staging]") || !strings.Contains(body, "skipping app-staging") {
		t.Errorf("the PR body does not carry the warning:\n%s", body)
	}
}

// Controls: the configured pair, a non-production target and a flags-only run (no pipeline is
// known, so nothing is claimed) stay silent.
func TestPlanDoesNotWarnSkippedStagingOffTheSkippingPath(t *testing.T) {
	root := planFixtureRepo(t)
	addDevEnv(t, root)
	r := mustDiscover(t, root)
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		svc          *Service
		source, dest string
	}{
		{"configured pair", skipPlanService(root), "app-staging", "app-production"},
		{"non-production target", skipPlanService(root), "app-dev", "app-staging"},
		{"flags only", planTestService(root), "app-dev", "app-production"},
	} {
		pc, err := tc.svc.Plan(ctx, PlanRequest{Repo: r, Source: tc.source, Target: tc.dest})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if ws := skippedStagingWarnings(pc.Plan); len(ws) != 0 {
			t.Errorf("%s: warned: %+v", tc.name, ws)
		}
	}
}
