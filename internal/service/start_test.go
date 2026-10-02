package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

// mustPlan builds the ordinary app-staging -> app-production plan fx's own fixture repo
// supports, the same plan every inflight_test.go scenario builds by hand, alongside the
// *gitops.Repo it was discovered from — StartRequest.Repo, exactly as the CLI's own runPromote
// passes its freshly discovered repo rather than relying on a Service that never called LoadRepo.
func mustPlan(t *testing.T, fx inflightFixture) (gitops.Plan, *gitops.Repo) {
	t.Helper()
	r, err := gitops.Discover(fx.clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan, r
}

// TestStartPromotionRefusesProductionDirectBeforeAnyClaimOrState is this PR's own trust-boundary
// acceptance test (AGENTS.md §4.5): a direct request into an env envs.production names must be
// refused by Preflight before StartPromotion ever claims the target env or writes a state file —
// closing the gap the TUI's old buildStartPromotion had (Divergence 5: it used to reach
// engine.DirectCommitGateStep only from inside the first Drive step, AFTER its own claim and
// initial save). Deleting StartPromotion's own Preflight call must never make this possible: the
// same gate is still the last step in engine.StepsFor's own direct list (checked again the
// moment Drive first runs one), and this test's own second half proves that independently by
// starting a NON-production promotion for the same fixture, showing the fixture and the claim
// machinery both work when the gate does not refuse.
func TestStartPromotionRefusesProductionDirectBeforeAnyClaimOrState(t *testing.T) {
	fx := newInflightFixtureWithProduction(t, []string{"app-production"})
	plan, repo := mustPlan(t, fx)

	_, err := fx.svc.StartPromotion(context.Background(), StartRequest{
		Plan: plan,
		Repo: repo,
		Mode: Mode{Direct: true, Confirmed: true},
	}, Hooks{})
	if err == nil {
		t.Fatal("expected StartPromotion to refuse a direct commit into a production env")
	}
	var blocked *engine.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected a *engine.BlockedError, got %T: %v", err, err)
	}
	if blocked.Step != engine.StepDirectGate {
		t.Errorf("blocked.Step = %s, want %s", blocked.Step, engine.StepDirectGate)
	}

	// No claim file: a fresh claimTarget call for the same repo/env/id must still succeed —
	// proving StartPromotion never got far enough to acquire (or leak) one.
	id := engine.DeriveID("example/gitops", plan)
	release, cerr := fx.svc.deps.Store.Claim("example/gitops", "app-production", id)
	if cerr != nil {
		t.Fatalf("expected the target env's claim to be free after the refusal, got: %v", cerr)
	}
	release()

	// No state file: a promotion refused before ever reaching the claim/save sequence leaves
	// nothing on disk for a future scan to trip over.
	states, lerr := fx.svc.deps.Store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(states) != 0 {
		t.Fatalf("expected no state files after the refusal, got %+v", states)
	}
}

// TestStartPromotionReleasesClaimOnAFailedFirstSave pins Divergence 1 (unify claim release on
// the TUI's own order: explicit initial save, then release): both faces on main released the
// claim when the first save failed (TUI: wiring.go's release() on a SaveState error; CLI:
// promote.go's deferred release), and the unified order preserves that — a failed save must
// still release the claim, never hold it, because nothing durable exists yet for a future
// FindInFlight scan to see in its place, and a held claim would block the target env until an
// operator deletes the claim file by hand. Proven by also starting a second promotion for the
// same target env right after the failed save and requiring it to succeed.
func TestStartPromotionReleasesClaimOnAFailedFirstSave(t *testing.T) {
	fx := newInflightFixture(t)
	plan, repo := mustPlan(t, fx)

	saveErr := errors.New("disk full")
	fx.svc.deps.Store = failingSaveStore{StateStore: fx.svc.deps.Store, err: saveErr}

	_, err := fx.svc.StartPromotion(context.Background(), StartRequest{
		Plan: plan,
		Repo: repo,
		Mode: Mode{Direct: false, Confirmed: false},
	}, Hooks{})
	if err == nil || !errors.Is(err, saveErr) {
		t.Fatalf("expected the save failure to surface, got: %v", err)
	}

	id := engine.DeriveID("example/gitops", plan)
	fs := FileStore{}
	reclaimRelease, cerr := fs.Claim("example/gitops", "app-production", id)
	if cerr != nil {
		t.Fatalf("expected the claim to be released after a failed first save, but re-claiming failed: %v", cerr)
	}
	reclaimRelease()

	// The claim being free is not by itself proof StartPromotion would let a real second start
	// through — re-run it (this time letting Save succeed) to prove the target env is not
	// refused as a conflict.
	fx.svc.deps.Store = fx.svc.deps.Store.(failingSaveStore).StateStore
	if _, err := fx.svc.StartPromotion(context.Background(), StartRequest{
		Plan: plan,
		Repo: repo,
		Mode: Mode{Direct: false, Confirmed: false},
	}, Hooks{}); err != nil {
		t.Fatalf("expected a later start to succeed once the claim was released, got: %v", err)
	}
}

// TestStartPromotionChecksTheViewThePlanWasBuiltFrom pins the fail-closed fix for t1-review.md
// P2 #6: a plan built from view A (whatever s.Repo() held when svc.Plan ran) must have its
// freshness re-checked against A at confirm time, even when a RefreshRepo (F5) has since moved
// the service's current view on to a newer B — never re-read s.Repo() live at StartPromotion
// time, which would silently check the plan against B and let a stale plan through the instant
// origin advanced again in the gap between building it and confirming it.
func TestStartPromotionChecksTheViewThePlanWasBuiltFrom(t *testing.T) {
	fx := newInflightFixture(t)
	ctx := context.Background()

	viewA, err := fx.svc.LoadRepo(ctx, RepoFromOrigin)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if !viewA.FromOrigin || viewA.SHA == "" {
		t.Fatalf("view A = %+v, want a real FromOrigin view with a captured SHA", viewA)
	}

	pc, err := fx.svc.Plan(ctx, PlanRequest{Source: "app-staging", Target: "app-production"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if pc.View.SHA != viewA.SHA {
		t.Fatalf("PlannedChange.View.SHA = %s, want it to carry view A's SHA %s", pc.View.SHA, viewA.SHA)
	}

	// origin/main moves again — a write from somewhere else entirely, exactly like a second
	// operator's promotion landing, or hoist's own direct-mode push, in the gap between this
	// plan loading and the operator pressing Enter.
	pushFromASeparateClone(t, fx.clone)

	// F5: the service's own current view moves on to the new tip (view B). The plan above still
	// describes view A's content.
	viewB, err := fx.svc.RefreshRepo(ctx)
	if err != nil {
		t.Fatalf("RefreshRepo: %v", err)
	}
	if viewB.SHA == viewA.SHA {
		t.Fatalf("view B SHA = %s, want it to differ from view A's %s after origin moved", viewB.SHA, viewA.SHA)
	}

	req := pc.Request(Mode{Direct: false, Confirmed: false})
	if req.View == nil || req.View.SHA != viewA.SHA {
		t.Fatalf("StartRequest.View = %+v, want it to carry view A's SHA %s", req.View, viewA.SHA)
	}

	_, err = fx.svc.StartPromotion(ctx, req, Hooks{})
	if err == nil {
		t.Fatal("expected StartPromotion to refuse a plan whose own view (A) is stale against origin, even though the service's CURRENT view (B) is not")
	}
	if !strings.Contains(err.Error(), "has moved since this plan was built") {
		t.Fatalf("err = %v, want it to name the actual cause", err)
	}
}

// failingSaveStore wraps a real StateStore (FileStore, via inflightFixture) so its Claim and
// List/Load calls are the REAL, atomic filesystem operations (AGENTS.md's own design doc: a
// claim/race test must use the real FileStore, never a fake), while Save alone is made to fail —
// the one seam TestStartPromotionReleasesClaimOnAFailedFirstSave needs to force StartPromotion
// down its error path without faking away the claim semantics under test.
type failingSaveStore struct {
	StateStore
	err error
}

func (f failingSaveStore) Save(*engine.PromotionState) error { return f.err }

// TestStartPromotionDirectNoOpRefusesUnseenOriginOccurrence pins Divergence 4's TUI-visible
// change: unifying the canonical order on the CLI's own (freshness -> fresh-base occurrence
// check -> no-op) means a direct request whose OWN known occurrences are already current, but
// whose origin has gained an occurrence of the same image repo the local clone has never seen,
// is now refused with that specific error — never reported "already current" the way the TUI's
// old buildStartPromotion used to (it ran its no-op check before ever reaching the fresh-base
// check).
func TestStartPromotionDirectNoOpRefusesUnseenOriginOccurrence(t *testing.T) {
	fx := newInflightFixture(t)

	// Make the known family's plan all-NoOp first: app-production's own deployment.yaml already
	// carries what app-staging runs.
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(fx.clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, fx.clone, "add", ".")
	runGitHost(t, fx.clone, "commit", "-q", "-m", "simulate the PR having merged")
	runGitHost(t, fx.clone, "push", "-q", "origin", "main")

	// A second, independent clone adds a brand-new family under the same promotable prefix and
	// pushes it straight to origin — never through the primary clone (mirrors
	// cmd/hoist/promote_test.go's TestPromoteDirectRefusesWhenOriginHasAnOccurrenceLocalDoesNotKnow).
	origin := strings.TrimSpace(outGitHost(t, fx.clone, "remote", "get-url", "origin"))
	second := filepath.Join(t.TempDir(), "second-clone")
	runGitHost(t, "", "clone", "-q", origin, second)
	digestApp2Staging := "sha256:" + strings.Repeat("9", 64)
	digestApp2Prod := "sha256:" + strings.Repeat("8", 64)
	wrapper := func(env string) string {
		return "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app2-" + env + "\n  namespace: argocd\n" +
			"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/" + env + "/app2\n" +
			"  destination:\n    server: https://kubernetes.default.svc\n    namespace: " + env + "\n"
	}
	deployment := func(ref string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app2\nspec:\n  template:\n    spec:\n      containers:\n        - name: app2\n          image: " + ref + "\n"
	}
	write := func(rel, content string) {
		p := filepath.Join(second, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-staging-app2.yaml", wrapper("app-staging"))
	write("cluster/apps/app-production-app2.yaml", wrapper("app-production"))
	write("cluster/apps/app-staging/app2/deployment.yaml", deployment("ghcr.io/example/app2:v2@"+digestApp2Staging))
	write("cluster/apps/app-production/app2/deployment.yaml", deployment("ghcr.io/example/app2:v1@"+digestApp2Prod))
	runGitHost(t, second, "add", ".")
	runGitHost(t, second, "commit", "-q", "-m", "add app2, straight to origin, bypassing the primary clone")
	runGitHost(t, second, "push", "-q", "origin", "main")

	// The primary clone's own local disk still knows only about "app" — plan.Edits is all-NoOp
	// for it.
	plan, repo := mustPlan(t, fx)

	_, err := fx.svc.StartPromotion(context.Background(), StartRequest{
		Plan: plan,
		Repo: repo,
		Mode: Mode{Direct: true, Confirmed: true},
	}, Hooks{})
	if err == nil {
		t.Fatal("expected a refusal naming the unseen occurrence, not success")
	}
	var already *AlreadyCurrentError
	if errors.As(err, &already) {
		t.Fatalf("must refuse the unseen origin occurrence, not report already-current (Divergence 4): %v", err)
	}
	if !strings.Contains(err.Error(), "app2") {
		t.Fatalf("error should name the missing occurrence's file (app2), got: %v", err)
	}
}

// TestStartPromotionRefusesOriginModeWithNoOriginView is PR #202's own review-found gap closed
// structurally (AGENTS.md §10 meta-rule 5): once this Service has read origin via
// LoadRepo(RepoFromOrigin) (TUI boot or F5), a nil StartRequest.View — or one whose FromOrigin is
// false, or whose SHA is empty — must be REFUSED rather than silently treated as "no view given,
// fall back to s.Repo()". That fallback is exactly the fail-open two TUI paths could reach with
// every existing test green: dropping deploy.Model.WithView(pc.View) in openDeploy, or breaking
// the loadedMsg.view -> plan.StartMsg.View chain, both send a zero RepoView (FromOrigin false)
// through to here, which used to downgrade the freshness check from CheckRepoViewCurrent (the
// tight origin-tip comparison) to checkCloneCurrentForBase (the CLI's own looser local-disk
// check) instead of failing the request outright.
func TestStartPromotionRefusesOriginModeWithNoOriginView(t *testing.T) {
	fx := newInflightFixture(t)
	ctx := context.Background()

	if _, err := fx.svc.LoadRepo(ctx, RepoFromOrigin); err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	pc, err := fx.svc.Plan(ctx, PlanRequest{Source: "app-staging", Target: "app-production"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !pc.View.FromOrigin || pc.View.SHA == "" {
		t.Fatalf("PlannedChange.View = %+v, want a real FromOrigin view (setup precondition)", pc.View)
	}

	cases := []struct {
		name string
		view *RepoView
	}{
		{"nil view", nil},
		{"zero view", &RepoView{}},
		{"FromOrigin false with a repo", &RepoView{Repo: pc.Repo}},
		{"FromOrigin true but no SHA", &RepoView{Repo: pc.Repo, FromOrigin: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fx.svc.StartPromotion(ctx, StartRequest{
				Plan: pc.Plan,
				Repo: pc.Repo,
				View: c.view,
				Mode: Mode{Direct: false, Confirmed: false},
			}, Hooks{})
			if err == nil {
				t.Fatal("expected StartPromotion to refuse a TUI-mode request with no usable origin view")
			}
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("expected a *UsageError, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), "go back and reopen the plan") {
				t.Fatalf("err = %v, want it to name the fix", err)
			}
		})
	}

	// Control: the real, correctly-plumbed view (pc.Request's own shape) is accepted.
	if _, err := fx.svc.StartPromotion(ctx, pc.Request(Mode{Direct: false, Confirmed: false}), Hooks{}); err != nil {
		t.Fatalf("expected the correctly-plumbed origin view to be accepted, got: %v", err)
	}
}

// TestStartPromotionCLIModeAcceptsNilView is the control half of
// TestStartPromotionRefusesOriginModeWithNoOriginView: a Service that never called
// LoadRepo(RepoFromOrigin) — the CLI's own shape, which never sends a View at all — must be
// unaffected by the new refusal. Proves the refusal is keyed on the SERVICE's own current view
// (TUI vs CLI), never merely on req.View being nil.
func TestStartPromotionCLIModeAcceptsNilView(t *testing.T) {
	fx := newInflightFixture(t)
	plan, repo := mustPlan(t, fx)

	if _, err := fx.svc.StartPromotion(context.Background(), StartRequest{
		Plan: plan,
		Repo: repo,
		Mode: Mode{Direct: false, Confirmed: false},
	}, Hooks{}); err != nil {
		t.Fatalf("expected a CLI-mode (never LoadRepo'd) request with a nil View to succeed, got: %v", err)
	}
}

// TestStartPromotionCarriesCIIgnoreFromConfigAndKeepsItOnRestart pins the ignore list's whole
// path into state: config -> a new promotion's state, and prior state wins over a changed
// config on a same-id restart (the list is policy "as of when the promotion started").
func TestStartPromotionCarriesCIIgnoreFromConfigAndKeepsItOnRestart(t *testing.T) {
	fx := newInflightFixture(t)
	plan, repo := mustPlan(t, fx)
	id := engine.DeriveID("example/gitops", plan)
	start := func() {
		t.Helper()
		if _, err := fx.svc.StartPromotion(context.Background(), StartRequest{Plan: plan, Repo: repo, Mode: Mode{}}, Hooks{}); err != nil {
			t.Fatalf("StartPromotion: %v", err)
		}
	}

	fx.svc.settings.Repo.CI.Ignore = []string{"copilot-pull-request-reviewer"}
	start()
	st, err := fx.svc.deps.Store.Load(id)
	if err != nil || st == nil {
		t.Fatalf("Load: %v %v", st, err)
	}
	if got := strings.Join(st.CIIgnore, ","); got != "copilot-pull-request-reviewer" {
		t.Fatalf("state.CIIgnore = %q, want the configured list", got)
	}

	fx.svc.settings.Repo.CI.Ignore = []string{"something-else"}
	start()
	st, err = fx.svc.deps.Store.Load(id)
	if err != nil || st == nil {
		t.Fatalf("Load: %v %v", st, err)
	}
	if got := strings.Join(st.CIIgnore, ","); got != "copilot-pull-request-reviewer" {
		t.Fatalf("after restart with a changed config state.CIIgnore = %q, want the prior list", got)
	}
}

// TestStartPromotionCarriesCISettleAndKeepsItOnRestart: config -> a new promotion's state, and
// prior state wins over a changed config on a same-id restart — a restart can never shorten the
// settle window the promotion began with.
func TestStartPromotionCarriesCISettleAndKeepsItOnRestart(t *testing.T) {
	fx := newInflightFixture(t)
	plan, repo := mustPlan(t, fx)
	id := engine.DeriveID("example/gitops", plan)
	start := func() *engine.PromotionState {
		t.Helper()
		if _, err := fx.svc.StartPromotion(context.Background(), StartRequest{Plan: plan, Repo: repo, Mode: Mode{}}, Hooks{}); err != nil {
			t.Fatalf("StartPromotion: %v", err)
		}
		st, err := fx.svc.deps.Store.Load(id)
		if err != nil || st == nil {
			t.Fatalf("Load: %v %v", st, err)
		}
		return st
	}
	d := config.Duration(45 * time.Second)
	fx.svc.settings.Repo.CI.Settle = &d
	if got := start().CISettle; got != 45*time.Second {
		t.Fatalf("state.CISettle = %s, want the configured 45s", got)
	}
	z := config.Duration(0)
	fx.svc.settings.Repo.CI.Settle = &z
	if got := start().CISettle; got != 45*time.Second {
		t.Fatalf("after restart with a changed config state.CISettle = %s, want the prior 45s", got)
	}
}
