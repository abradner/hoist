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
