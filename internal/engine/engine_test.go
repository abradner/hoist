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
