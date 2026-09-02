package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/git"
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
