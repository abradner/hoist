package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/redact"
)

// failingStep is a minimal Step whose Act always fails with a fixed error — used to exercise
// Drive's error-handling paths without needing a real worktree or forge.
type failingStep struct {
	name StepName
	err  error
}

func (f failingStep) Name() StepName { return f.name }

func (f failingStep) Observe(context.Context, *PromotionState) (Observation, error) {
	return Observation{Satisfied: false}, nil
}

func (f failingStep) Act(context.Context, *PromotionState) error { return f.err }

// TestDriveRedactsRegisteredSecretInHistory reproduces the scenario a git hook or the signing
// helper can trigger: a registered credential (pkg/redact.Register — every adaptor calls this
// the moment it loads a value) ends up echoed into a failed Act's error text (pkg/git folds a
// failed command's stderr into its wrapped error). Drive's own appendHistory is the sink that
// persists that text into PromotionState.History, which SaveState later writes to disk
// verbatim — this must scrub the secret before it ever reaches History, not rely on pkg/git
// having already done so.
func TestDriveRedactsRegisteredSecretInHistory(t *testing.T) {
	const secret = "sekrit-finding-b-ghcr-token-value"
	redact.Register(secret)

	step := failingStep{
		name: StepBranched,
		err:  fmt.Errorf("git worktree add: exit status 1: hook printed %s to stderr", secret),
	}
	s := &PromotionState{}

	if err := Drive(ctx(), []Step{step}, s, nil); err == nil {
		t.Fatal("expected Drive to propagate the step's error")
	}

	if len(s.History) != 1 {
		t.Fatalf("expected exactly one history entry, got %d: %+v", len(s.History), s.History)
	}
	detail := s.History[0].Detail
	if strings.Contains(detail, secret) {
		t.Fatalf("History entry leaked the registered secret verbatim: %q", detail)
	}
	if !strings.Contains(detail, redact.Redacted) {
		t.Fatalf("History entry should carry the redaction marker, got %q", detail)
	}
}

// TestStepErrorMessageContainsWrappedErrorText is the regression for a format-verb bug: Error()
// used to build its message with fmt.Sprintf's %s verb against e.Err, a plain error value.
// %s does correctly invoke Error() for a value satisfying the error interface (verified: this
// exact construction, unchanged, does not reproduce "%!s(...)" garbage), but %v is the
// conventional, defensive verb for formatting an error and was what the fix switched to — this
// test pins the actual contract (the wrapped error's real message text appears verbatim in the
// output), not the specific verb used to get there, so it stays meaningful regardless.
func TestStepErrorMessageContainsWrappedErrorText(t *testing.T) {
	const wrapped = "signing timed out after 120s waiting for interactive approval"
	e := &StepError{Step: StepPushed, Op: "act", Err: fmt.Errorf("%s", wrapped)}
	got := e.Error()
	if !strings.Contains(got, wrapped) {
		t.Fatalf("StepError.Error() = %q, want it to contain the wrapped error's message %q", got, wrapped)
	}
	if strings.Contains(got, "%!") {
		t.Fatalf("StepError.Error() = %q, contains a malformed fmt verb", got)
	}
	const want = "pushed: act: " + wrapped
	if got != want {
		t.Fatalf("StepError.Error() = %q, want %q", got, want)
	}
}

// countingStub is a minimal Step, like stepStub, that also counts how many times Observe and
// Act are each called on it — used below to prove Drive's Merged short-circuit really does skip
// re-Observing and re-Acting on earlier steps, not just happen to produce the right end state.
type countingStub struct {
	name     StepName
	obs      Observation
	observes *int
	acts     *int
}

func (c countingStub) Name() StepName { return c.name }

func (c countingStub) Observe(context.Context, *PromotionState) (Observation, error) {
	if c.observes != nil {
		*c.observes++
	}
	return c.obs, nil
}

func (c countingStub) Act(context.Context, *PromotionState) error {
	if c.acts != nil {
		*c.acts++
	}
	return nil
}

// TestDriveSkipsPushedAndMergedOncePastMergedOnAPriorPass is this round's regression for the
// churn TestResumeRebuildsArgoAppsForALegacyStateFile (cmd/hoist/resume_test.go) first flagged
// and worked around locally rather than fixed: once a promotion has merged and is waiting on an
// M5 Argo/rollout step, Drive's own "re-observe every step from the top" rule used to make
// PushedStep re-push the branch MergedStep had just deleted, and MergedStep delete it again — on
// every single poll tick for as long as the rollout takes to converge, not just once. This drives
// with s.Phase already set to a step after Merged (the same hint pollInterval already reads from
// a prior Drive call) and asserts PushedStep's Observe/Act and MergedStep's Act are never called
// this pass — only MergedStep's Observe, exactly once (the short-circuit probe) — while the
// pipeline still correctly reports ErrWaiting on the still-unsatisfied step after it.
func TestDriveSkipsPushedAndMergedOncePastMergedOnAPriorPass(t *testing.T) {
	var pushedObserves, pushedActs, mergedObserves, mergedActs int
	steps := []Step{
		countingStub{name: StepBranched, obs: Observation{Satisfied: true}},
		countingStub{name: StepCommitted, obs: Observation{Satisfied: true}},
		countingStub{name: StepPushed, obs: Observation{Satisfied: false}, observes: &pushedObserves, acts: &pushedActs},
		countingStub{name: StepPROpened, obs: Observation{Satisfied: true}},
		countingStub{name: StepCIGreen, obs: Observation{Satisfied: true}},
		countingStub{name: StepApproved, obs: Observation{Satisfied: true}},
		countingStub{name: StepMerged, obs: Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}, observes: &mergedObserves, acts: &mergedActs},
		countingStub{name: StepArgoSynced, obs: Observation{Waiting: true, Detail: "waiting for argo to sync"}},
	}
	s := &PromotionState{Phase: StepArgoSynced}

	if err := Drive(ctx(), steps, s, nil); err != ErrWaiting {
		t.Fatalf("expected ErrWaiting, got %v", err)
	}
	if s.Phase != StepArgoSynced {
		t.Fatalf("expected to stop at %s, stopped at %s", StepArgoSynced, s.Phase)
	}
	if pushedObserves != 0 || pushedActs != 0 {
		t.Fatalf("PushedStep should not be re-Observed or re-Acted once past Merged: observes=%d acts=%d", pushedObserves, pushedActs)
	}
	if mergedObserves != 1 {
		t.Fatalf("MergedStep's Observe should be called exactly once (the short-circuit probe), got %d", mergedObserves)
	}
	if mergedActs != 0 {
		t.Fatalf("MergedStep's Act should never be called once its Observe reports Satisfied, got %d", mergedActs)
	}
	if len(s.History) != 1 || !strings.Contains(s.History[0].Detail, "waiting") {
		t.Fatalf("expected exactly one history entry, for the waiting Argo step, got %+v", s.History)
	}
}

// TestDriveDoesNotShortCircuitBeforeMergedIsFirstReached is the short-circuit's own guard rail:
// on any pass where s.Phase has not yet reached Merged (still waiting on, say, CI), the probe
// must not run at all — Merged cannot possibly be satisfied yet, so probing it would only add a
// wasted Observe call on every earlier-phase poll tick too.
func TestDriveDoesNotShortCircuitBeforeMergedIsFirstReached(t *testing.T) {
	var mergedObserves int
	steps := []Step{
		countingStub{name: StepBranched, obs: Observation{Satisfied: true}},
		countingStub{name: StepCommitted, obs: Observation{Satisfied: true}},
		countingStub{name: StepPushed, obs: Observation{Satisfied: true}},
		countingStub{name: StepPROpened, obs: Observation{Satisfied: true}},
		countingStub{name: StepCIGreen, obs: Observation{Waiting: true, Detail: "waiting for CI"}},
		countingStub{name: StepApproved, obs: Observation{Satisfied: false}},
		countingStub{name: StepMerged, obs: Observation{Satisfied: false}, observes: &mergedObserves},
		countingStub{name: StepArgoSynced, obs: Observation{Satisfied: false}},
	}
	s := &PromotionState{Phase: StepApproved}

	if err := Drive(ctx(), steps, s, nil); err != ErrWaiting {
		t.Fatalf("expected ErrWaiting, got %v", err)
	}
	if s.Phase != StepCIGreen {
		t.Fatalf("expected to stop at %s, stopped at %s", StepCIGreen, s.Phase)
	}
	if mergedObserves != 0 {
		t.Fatalf("Merged should not be probed before a prior pass has actually reached it, got %d Observe call(s)", mergedObserves)
	}
}

// TestObserveAllObservesFinalStepExactlyOnce is round-6's regression: ObserveAll had the same
// double-observe bug Status was already fixed for (PR #39 review finding #3) — the short-circuit
// probe on the final step, when not satisfied, got observed a SECOND time when the ordinary walk
// reached it. Every not-yet-done call to findInFlight/hoist promotions/hoist resume --env (all
// of which call ObserveAll, not Status) paid one extra, wasted remote/git call on the final step
// — real work for MergedStep, which talks to the forge and fetches origin. Uses the same
// observeCountingStub helper status_test.go already defines in this package.
func TestObserveAllObservesFinalStepExactlyOnce(t *testing.T) {
	calls := 0
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true}},
		stepStub{name: StepCommitted, obs: Observation{Satisfied: true}},
		observeCountingStub{stepStub{name: StepMerged, obs: Observation{Satisfied: false, Detail: "not yet merged"}}, &calls},
	}
	done, last, err := ObserveAll(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("done = true, want false: the final step reported not satisfied")
	}
	if calls != 1 {
		t.Fatalf("final step's Observe called %d times, want exactly 1", calls)
	}
	if last.Step != StepMerged || last.Detail != "not yet merged" {
		t.Fatalf("last = %+v, want the final step's own Observation", last)
	}
}
