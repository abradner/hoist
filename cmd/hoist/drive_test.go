package main

import (
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
)

// TestPollIntervalPicksTheConfiguredKnobPerStep is the regression test for the M5 gap:
// pollInterval switched on engine.StepName for CIGreen/Approved but fell through to the
// hardcoded 2s default for every other phase, silently including the three M5 steps
// (StepArgoRefreshed, StepArgoSynced, StepRolledOut) — meaning `promote`/`resume`'s live drive
// loop ignored poll.argo/poll.rollout entirely and always polled Argo/rollout status every 2s,
// regardless of what the operator configured. Each of the three new step names must map to its
// own configured interval, not the fallback, and the fallback itself must still answer for a
// step with genuinely no config knob (AGENTS.md §4.9: a knob with no real use is a knob nobody
// needed).
func TestPollIntervalPicksTheConfiguredKnobPerStep(t *testing.T) {
	poll := config.PollConfig{
		CI:       config.Duration(11 * time.Second),
		Approval: config.Duration(22 * time.Second),
		Argo:     config.Duration(33 * time.Second),
		Rollout:  config.Duration(44 * time.Second),
	}

	cases := []struct {
		phase engine.StepName
		want  time.Duration
	}{
		{engine.StepCIGreen, 11 * time.Second},
		{engine.StepApproved, 22 * time.Second},
		{engine.StepArgoRefreshed, 33 * time.Second},
		{engine.StepArgoSynced, 33 * time.Second},
		{engine.StepRolledOut, 44 * time.Second},
		// A step with no configured knob still falls back to the fixed 2s interval — proves the
		// new cases are additions, not a rewrite that broke the pre-existing fallback.
		{engine.StepBranched, 2 * time.Second},
	}
	for _, c := range cases {
		if got := pollInterval(poll, c.phase); got != c.want {
			t.Errorf("pollInterval(%s) = %s, want %s", c.phase, got, c.want)
		}
	}
}
