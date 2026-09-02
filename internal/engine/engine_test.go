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
