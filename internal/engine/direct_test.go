package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// TestDirectModeRefusesProductionEnvByConstruction is AGENTS.md's M6 brief's mandatory test:
// "construct a direct-mode attempt against a production-listed env and confirm it refuses
// with a clear error, regardless of what any UI layer would have shown." The attacker here is
// a caller that already got past every UI layer — Confirmed is true, exactly as if the
// operator had completed the keypress + huh.Confirm gesture — and the target env
// (fx.plan.TargetEnv, "app-production") is genuinely listed in ProductionEnvs. Nothing about
// this attempt is a UI mistake; it is what invariant 5 calls "a config bug or a future
// caller mistake": the caller believed direct mode was fine to attempt here. DirectSteps must
// refuse before touching the worktree or the remote at all.
func TestDirectModeRefusesProductionEnvByConstruction(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	if s.TargetEnv != "app-production" {
		t.Fatalf("fixture precondition: TargetEnv = %q, want app-production", s.TargetEnv)
	}

	steps := DirectSteps(git.Exec{}, []string{"app-production"}, true, nil)
	err := Drive(ctx(), steps, s, nil)

	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected a *BlockedError refusing production, got %v", err)
	}
	if blocked.Step != StepDirectGate {
		t.Fatalf("blocked at %q, want %q", blocked.Step, StepDirectGate)
	}
	if blocked.Reason == "" {
		t.Fatal("expected a clear, non-empty refusal reason")
	}
	// The refusal must name the production env so an operator reading it knows exactly why.
	if want := "app-production"; !strings.Contains(blocked.Reason, want) {
		t.Fatalf("refusal %q does not name %q", blocked.Reason, want)
	}

	// The point of the gate: nothing after it ever touched the worktree or the remote.
	g := git.Exec{}
	if _, ok, _ := g.WorktreeBranch(ctx(), fx.cloneDir, wt); ok {
		t.Fatal("no worktree should have been created — the gate must run before BranchedStep")
	}
	remoteSHA, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("origin/main should still exist from the fixture seed")
	}
	if s.CommitSHA != "" || s.PushedSHA != "" {
		t.Fatalf("no commit or push should have happened: CommitSHA=%q PushedSHA=%q", s.CommitSHA, s.PushedSHA)
	}
	_ = remoteSHA
}

// TestDirectModeRefusesWithoutConfirmation is invariant 5's other half: even for a genuinely
// non-production env, direct mode must not proceed on a bare/default flag — the operator's
// distinct keypress+confirm action is required, not assumed.
func TestDirectModeRefusesWithoutConfirmation(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)

	// app-staging is not production in this fixture's config; only Confirmed is false here.
	steps := DirectSteps(git.Exec{}, []string{"app-production"}, false, nil)
	s.TargetEnv = "app-staging"
	err := Drive(ctx(), steps, s, nil)

	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected a *BlockedError refusing an unconfirmed attempt, got %v", err)
	}
	if blocked.Step != StepDirectGate {
		t.Fatalf("blocked at %q, want %q", blocked.Step, StepDirectGate)
	}
}

// TestDirectModeCommitsStraightToBaseForNonProduction is the honest path: a non-production
// env, confirmed, drives clean through to origin's base branch moving to this promotion's own
// commit — no separate branch left on origin, no PR.
func TestDirectModeCommitsStraightToBaseForNonProduction(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	g := git.Exec{}

	// app-production is this fixture's plan's TargetEnv, but ProductionEnvs is deliberately
	// built without it here: the gate is a pure function of the list it's given (DirectSteps'
	// own doc comment on why that list must always be RepoConfig.Envs.Production, unfiltered,
	// in real wiring) — this test exercises the "allowed" branch of that same mechanism with a
	// list that doesn't happen to name this env, exactly as a real repo whose envs.production
	// never listed this env would.
	steps := DirectSteps(g, nil, true, nil)
	if err := Drive(ctx(), steps, s, nil); err != nil {
		t.Fatalf("direct mode should succeed for a non-production, confirmed attempt: %v", err)
	}
	if s.CommitSHA == "" {
		t.Fatal("expected a commit")
	}
	if s.PushedSHA != s.CommitSHA {
		t.Fatalf("PushedSHA = %q, want %q", s.PushedSHA, s.CommitSHA)
	}

	remoteSHA, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", s.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || remoteSHA != s.CommitSHA {
		t.Fatalf("origin/%s = %s ok=%v, want %s", s.Base, remoteSHA, ok, s.CommitSHA)
	}
	// No branch of the promotion's own name exists on origin: direct mode never pushes one.
	if _, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", s.Branch); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("origin/%s should not exist — direct mode pushes straight to base, never its own branch", s.Branch)
	}
}

// TestDirectModeIsIdempotentOnResume mirrors CommittedStep's own resume property: driving the
// exact same steps a second time (a fresh PromotionState, as a restarted process would build)
// must recognise the work is already done and make no further commit or push.
func TestDirectModeIsIdempotentOnResume(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}

	first := newState(fx, wt)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), first, nil); err != nil {
		t.Fatalf("first Drive: %v", err)
	}

	resumed := newState(fx, wt)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), resumed, nil); err != nil {
		t.Fatalf("resumed Drive: %v", err)
	}
	if resumed.CommitSHA != first.CommitSHA {
		t.Fatalf("resumed produced a different commit: %s vs %s", resumed.CommitSHA, first.CommitSHA)
	}
	shas, err := g.Log(ctx(), wt, "origin/"+resumed.Base)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's origin starts with exactly one commit ("seed"); direct mode adds exactly
	// one more, and resuming must not add a second.
	if len(shas) != 2 {
		t.Fatalf("origin/%s has %d commits, want 2 (seed + exactly one direct commit)", resumed.Base, len(shas))
	}
}

// TestDirectModeReObserveToleratesBaseAdvancingFurther is finding 3's own regression test
// (AGENTS.md gotcha, same class as the exact-tip-vs-ancestry bug MergedStep's own revert check
// was fixed for elsewhere in this milestone's history): after a direct promotion pushes
// successfully, a distinct, later, legitimate commit advances Base further. Re-observing the
// ORIGINAL promotion — a fresh PromotionState built the same way a restarted process would,
// reusing the same worktree — must still recognise its own commit as satisfied (still genuinely
// pushed, merely superseded), rather than attempting a doomed non-fast-forward replay and
// reporting a conflict that isn't real.
func TestDirectModeReObserveToleratesBaseAdvancingFurther(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}

	first := newState(fx, wt)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), first, nil); err != nil {
		t.Fatalf("first Drive: %v", err)
	}

	// A distinct, later, legitimate change advances origin/main further, built ON TOP of this
	// promotion's own commit (a genuine fast-forward, not a rewrite) — from a separate clone,
	// never touching fx.cloneDir.
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "commit", "-q", "--allow-empty", "-m", "someone else's later, legitimate change")
	runHost(t, other, "push", "-q", "origin", "main")

	// Re-observe the exact same promotion, as a restarted `hoist promote` process would build
	// it: a fresh PromotionState, same worktree.
	resumed := newState(fx, wt)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), resumed, nil); err != nil {
		t.Fatalf("resumed Drive should tolerate Base having moved further with a legitimate later change, not report a conflict: %v", err)
	}
	if resumed.CommitSHA != first.CommitSHA {
		t.Fatalf("resumed produced a different commit: %s vs %s", resumed.CommitSHA, first.CommitSHA)
	}
	// PushedSHA is the base revision that CARRIES this promotion's content, not the commit
	// object it originally created. This assertion used to demand the latter, which reads
	// better as a field name and is unobservable in the world: Argo tracks the branch and
	// reports the tip, and ArgoSyncedStep compares status.sync.revision against LandedSHA()
	// exactly — so pinning the superseded commit made a direct promotion whose base had moved
	// wait out its entire deadline for a SHA the branch would never report again.
	tip, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", "main")
	if err != nil || !ok {
		t.Fatalf("reading origin/main: %v (ok=%v)", err, ok)
	}
	if tip == first.CommitSHA {
		t.Fatal("fixture precondition: origin/main should have moved past this promotion's own commit")
	}
	if resumed.PushedSHA != tip {
		t.Fatalf("PushedSHA = %q, want the base tip carrying this promotion's content %q (its own superseded commit was %q)",
			resumed.PushedSHA, tip, first.CommitSHA)
	}
	if resumed.LandedSHA() != tip {
		t.Fatalf("LandedSHA = %q, want %q: this is the revision ArgoSyncedStep waits for", resumed.LandedSHA(), tip)
	}
}

// TestDirectModeDoesNotTreatRevertedContentAsSatisfied is the flip side of
// TestDirectModeReObserveToleratesBaseAdvancingFurther, and the reason ancestry alone was
// replaced with a content comparison (DirectPushedStep.Observe's own doc comment): a direct
// promotion's own commit remains an ancestor of Base's tip FOREVER once pushed, even after
// someone `git revert`s it — a revert commit undoes the file content but never removes the
// reverted commit from history. The old ancestor-only check could not tell "Base advanced with
// an unrelated later change" (content still matches — genuinely still satisfied) from "this
// exact promotion was reverted" (content no longer matches — must not read as satisfied): both
// have "this promotion's commit is still an ancestor" equally true. Re-running the identical
// promotion after a revert must not exit as if it were already done — it planned digest is no
// longer actually present at the tip, so Observe must report unsatisfied, and Act's own push
// must then either restore it or surface a real conflict, never silently do nothing.
func TestDirectModeDoesNotTreatRevertedContentAsSatisfied(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}

	first := newState(fx, wt)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), first, nil); err != nil {
		t.Fatalf("first Drive: %v", err)
	}

	// Someone reverts the promotion's own commit, from a separate clone, and pushes the revert
	// straight to main — first.CommitSHA remains an ancestor of the new tip (revert commits never
	// remove history), but the file content it changed is now back to what it was before.
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "revert", "--no-edit", first.CommitSHA)
	runHost(t, other, "push", "-q", "origin", "main")

	// Re-observe the exact same promotion, as a restarted `hoist promote` process would build
	// it: a fresh PromotionState, same worktree (still holding its own single, un-reverted
	// commit — nothing here touches fx.cloneDir or the worktree itself).
	resumed := newState(fx, wt)
	// CommittedStep must run first in a real Drive to populate ExpectedBlobs/CommitSHA; do that
	// here too, exactly as Drive would, before asking DirectPushedStep about it.
	if _, err := (CommittedStep{Git: g}).Observe(ctx(), resumed); err != nil {
		t.Fatalf("CommittedStep.Observe: %v", err)
	}
	obs, err := (DirectPushedStep{Git: g}).Observe(ctx(), resumed)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Satisfied {
		t.Fatalf("should not be satisfied: the promotion's own commit was reverted, its planned content is no longer at the tip — got %+v", obs)
	}
	if resumed.PushedSHA != "" {
		t.Fatalf("PushedSHA must not be set when the planned content isn't actually present, got %q", resumed.PushedSHA)
	}

	// Driving the whole thing end to end: Act must not silently succeed doing nothing — the
	// worktree's own HEAD (first.CommitSHA, unreverted, based on the ORIGINAL pre-revert base)
	// is no longer a fast-forward of the current tip (the revert moved main to a new commit
	// whose tree differs), so the push is rejected as a real conflict rather than replayed.
	err = Drive(ctx(), DirectSteps(g, nil, true, nil), newState(fx, wt), nil)
	if err == nil {
		t.Fatal("Drive should not report success after this promotion's commit was reverted upstream — nothing restores the promoted digest silently")
	}
}

// newTwoFamilyOrigin is like newFixtureOrigin but with two independent families (distinct
// registry prefixes, so BuildPlan can be scoped to exactly one of them per plan): "app" under
// ghcr.io/example/, and "app2" under ghcr.io/example2/. Used only by
// TestDirectModeSecondIndependentPromotionSeesFirstsPushedContent, which needs two genuinely
// disjoint promotions (different families, no shared file) rather than the single-family
// fixture every other test in this file shares — touching the same occurrence twice would hit
// doc.go's own separately-documented assumption ("the clone is assumed to match Base") rather
// than isolate finding 2's own worktree-ancestry bug.
func newTwoFamilyOrigin(t *testing.T) (cloneDir string) {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	const cfg = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(gitconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))

	seed := t.TempDir()
	runHost(t, "", "init", "-q", "-b", "main", seed)
	write := func(rel, content string) {
		p := filepath.Join(seed, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := func(env, family string) string {
		return "" +
			"apiVersion: argoproj.io/v1alpha1\n" +
			"kind: Application\n" +
			"metadata:\n" +
			"  name: " + family + "-" + env + "\n" +
			"  namespace: argocd\n" +
			"spec:\n" +
			"  project: default\n" +
			"  source:\n" +
			"    repoURL: https://git.example.test/example/gitops.git\n" +
			"    targetRevision: main\n" +
			"    path: cluster/apps/" + env + "/" + family + "\n" +
			"  destination:\n" +
			"    server: https://kubernetes.default.svc\n" +
			"    namespace: " + env + "\n"
	}
	deployment := func(family, ref string) string {
		return "" +
			"apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: " + family + "\n" +
			"  namespace: irrelevant\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: " + family + "\n" +
			"          image: " + ref + "\n"
	}
	digestOld := "sha256:" + strings.Repeat("0", 64)
	digestNew := "sha256:" + strings.Repeat("1", 64)
	for _, f := range []struct{ family, prefix string }{{"app", "ghcr.io/example"}, {"app2", "ghcr.io/example2"}} {
		write("cluster/apps/app-staging-"+f.family+".yaml", wrapper("app-staging", f.family))
		write("cluster/apps/app-production-"+f.family+".yaml", wrapper("app-production", f.family))
		write("cluster/apps/app-staging/"+f.family+"/deployment.yaml", deployment(f.family, f.prefix+"/"+f.family+":v2@"+digestNew))
		write("cluster/apps/app-production/"+f.family+"/deployment.yaml", deployment(f.family, f.prefix+"/"+f.family+":v1@"+digestOld))
	}

	runHost(t, seed, "add", ".")
	runHost(t, seed, "commit", "-q", "-m", "seed")

	originDir := filepath.Join(t.TempDir(), "origin.git")
	runHost(t, "", "init", "-q", "--bare", "-b", "main", originDir)
	runHost(t, seed, "remote", "add", "origin", originDir)
	runHost(t, seed, "push", "-q", "origin", "main")

	cloneDir = filepath.Join(t.TempDir(), "clone")
	runHost(t, "", "clone", "-q", originDir, cloneDir)
	return cloneDir
}

// TestDirectModeSecondIndependentPromotionSeesFirstsPushedContent is finding 2's own regression
// test: after one direct promotion completes, a second, independent promotion (a different
// family, a different digest set, same Base) must branch its own worktree from what the first
// promotion actually pushed — not from the clone's own local "main", which direct mode never
// advances directly (AGENTS.md §4.6) and which would otherwise still point at the
// pre-first-promotion history, producing a wrong diff or a rejected push.
func TestDirectModeSecondIndependentPromotionSeesFirstsPushedContent(t *testing.T) {
	cloneDir := newTwoFamilyOrigin(t)
	g := git.Exec{}

	repo, err := gitops.Discover(cloneDir, "cluster/apps")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	plan1, err := gitops.BuildPlan(repo, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatalf("BuildPlan (app): %v", err)
	}
	if len(plan1.Edits) != 1 || plan1.Edits[0].NoOp() {
		t.Fatalf("first plan should have exactly one real edit for the app family, got %+v", plan1.Edits)
	}
	plan2, err := gitops.BuildPlan(repo, "app-staging", "app-production", []string{"ghcr.io/example2/"}, nil)
	if err != nil {
		t.Fatalf("BuildPlan (app2): %v", err)
	}
	if len(plan2.Edits) != 1 || plan2.Edits[0].NoOp() {
		t.Fatalf("second plan should have exactly one real edit for the app2 family, got %+v", plan2.Edits)
	}

	newPromotionState := func(plan gitops.Plan, worktreeDir string) *PromotionState {
		id := DeriveID(repoFullName, plan)
		return &PromotionState{
			ID:            id,
			RepoFullName:  repoFullName,
			SourceEnv:     plan.SourceEnv,
			TargetEnv:     plan.TargetEnv,
			Branch:        BranchName(plan.TargetEnv, id),
			CloneDir:      cloneDir,
			WorktreeDir:   worktreeDir,
			Base:          "main",
			Edits:         plan.Edits,
			CommitMessage: RenderCommitMessage(id, plan),
			PRTitle:       PRTitle(plan),
			PRBody:        RenderPRBody(id, plan),
		}
	}

	wt1 := filepath.Join(t.TempDir(), "wt1")
	first := newPromotionState(plan1, wt1)
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), first, nil); err != nil {
		t.Fatalf("first Drive: %v", err)
	}

	wt2 := filepath.Join(t.TempDir(), "wt2")
	second := newPromotionState(plan2, wt2)
	if first.ID == second.ID {
		t.Fatalf("fixture setup bug: the two promotions' ids should differ, got the same %q", first.ID)
	}
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), second, nil); err != nil {
		t.Fatalf("second, independent Drive: %v", err)
	}

	// The whole point: the second promotion's worktree must have branched from what the first
	// promotion actually pushed, not from the clone's own stale local "main" — proven here by
	// confirming the first promotion's own commit is present in the second worktree's history.
	shas, err := g.Log(ctx(), wt2, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sha := range shas {
		if sha == first.CommitSHA {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("second promotion's worktree history %v should include the first promotion's own commit %s — it branched from a stale view of Base", shas, first.CommitSHA)
	}

	// End to end: origin/main now carries both commits in sequence, and the second promotion's
	// own push landed cleanly (never rejected, never replayed over stale content).
	remoteSHA, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || remoteSHA != second.CommitSHA {
		t.Fatalf("origin/main = %s ok=%v, want the second promotion's own commit %s", remoteSHA, ok, second.CommitSHA)
	}
}

// TestDirectModeBlocksOnGenuineConflict is DirectPushedStep's counterpart to
// TestPushedStepBlocksOnGenuineConflict: someone else moves the base branch on origin after
// this promotion's worktree branched from it, and the push must fail loudly rather than
// force-pushing over it.
func TestDirectModeBlocksOnGenuineConflict(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	g := git.Exec{}
	if err := (BranchedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if err := (CommittedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}

	// Someone else commits straight to origin's base branch before we push.
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "commit", "-q", "--allow-empty", "-m", "someone else, direct to main")
	runHost(t, other, "push", "-q", "origin", "main")

	step := DirectPushedStep{Git: g}
	err := step.Act(ctx(), s)
	if err == nil {
		t.Fatal("expected the push to be rejected as a non-fast-forward conflict")
	}
	if !strings.Contains(err.Error(), "real conflict") {
		t.Fatalf("error should name this a real conflict, not a generic failure: %v", err)
	}
	if s.PushedSHA != "" {
		t.Fatal("PushedSHA must not be set after a rejected push")
	}
}
