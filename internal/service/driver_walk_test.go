package service

import (
	"context"
	"testing"

	"github.com/abradner/hoist/internal/engine"
)

// countingStep is a fake engine.Step that counts how many times Observe is called and always
// answers obs — never Blocked/Satisfied-inconsistently, since these fixtures only ever need one
// fixed answer per step for the whole test.
type countingStep struct {
	name    engine.StepName
	obs     engine.Observation
	observe *int
}

func (s countingStep) Name() engine.StepName { return s.name }

func (s countingStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	*s.observe++
	return s.obs, nil
}

func (s countingStep) Act(context.Context, *engine.PromotionState) error { return nil }

// TestDriverStepIsOneWalk is the design's own acceptance test (service-design.md §4, PR C): a
// healthy poll that stops Waiting must Observe each step at most once per Driver.Step call.
// Before DriveStatus existed, cmd/hoist/wiring.go's driveFuncFor called engine.Drive and then,
// unconditionally, engine.Status over the same steps — two full walks per poll, real remote/git
// calls doubled on every tick a TUI or CLI driver made. Driver.Step (internal/service/driver.go)
// replaces that pair with one engine.DriveStatus call, falling back to a second engine.Status
// walk only on a genuine failure (never for Waiting) — see Driver.Step's own doc comment.
//
// This test was run once against a deliberately reintroduced two-walk Step (calling
// engine.Status unconditionally after engine.DriveStatus, mirroring the old driveFuncFor) to
// confirm it actually fails that shape: with two steps it counted 4 Observe calls instead of the
// 2 asserted below, then was reverted once that failure was confirmed — see this PR's commit
// message and report for the exact numbers.
func TestDriverStepIsOneWalk(t *testing.T) {
	var obs1, obs2 int
	steps := []engine.Step{
		countingStep{name: engine.StepBranched, obs: engine.Observation{Satisfied: true, Detail: "already branched"}, observe: &obs1},
		countingStep{name: engine.StepCommitted, obs: engine.Observation{Waiting: true, Detail: "waiting for signing approval"}, observe: &obs2},
	}
	s := &engine.PromotionState{ID: "test-one-walk"}
	d := NewDriver(steps, s, nil, engine.PollIntervals{})

	tick, err := d.Step(context.Background())
	if err != nil {
		t.Fatalf("Step returned an error for a Waiting poll: %v", err)
	}
	if !tick.Waiting {
		t.Fatalf("expected Tick.Waiting, got %+v", tick)
	}
	if obs1 != 1 {
		t.Errorf("step 1 (already satisfied) Observed %d times, want exactly 1 — a healthy poll must be one walk, not Drive-then-Status", obs1)
	}
	if obs2 != 1 {
		t.Errorf("step 2 (the Waiting step) Observed %d times, want exactly 1 — a healthy poll must be one walk, not Drive-then-Status", obs2)
	}
	if got := len(tick.Statuses); got != 2 {
		t.Errorf("Tick.Statuses has %d entries, want 2 (one per step reached)", got)
	}
}
