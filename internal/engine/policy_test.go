package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/rollout"
)

// TestPollIntervalPicksTheConfiguredKnobPerStep is the regression test for the M5 gap:
// pollInterval switched on StepName for CIGreen/Approved but fell through to the hardcoded 2s
// default for every other phase, silently including the three M5 steps (StepArgoRefreshed,
// StepArgoSynced, StepRolledOut) — meaning `promote`/`resume`'s live drive loop ignored
// poll.argo/poll.rollout entirely and always polled Argo/rollout status every 2s, regardless of
// what the operator configured. Each of the three step names must map to its own configured
// interval, not the fallback, and the fallback itself must still answer for a step with
// genuinely no config knob (AGENTS.md §4.9: a knob with no real use is a knob nobody needed).
// Moved here from cmd/hoist/drive_test.go and internal/app/flight/model_test.go when both
// callers' own copies of this function were retired in favour of this one.
func TestPollIntervalPicksTheConfiguredKnobPerStep(t *testing.T) {
	p := PollIntervals{CI: 11 * time.Second, Approval: 22 * time.Second, Argo: 33 * time.Second, Rollout: 44 * time.Second}
	cases := []struct {
		phase StepName
		want  time.Duration
	}{
		{StepCIGreen, 11 * time.Second},
		{StepApproved, 22 * time.Second},
		{StepArgoRefreshed, 33 * time.Second},
		{StepArgoSynced, 33 * time.Second},
		{StepRolledOut, 44 * time.Second},
		// A step with no configured knob still falls back to the fixed 2s interval — proves the
		// three M5 cases are additions, not a rewrite that broke the pre-existing fallback.
		{StepBranched, 2 * time.Second},
		{StepCommitted, 2 * time.Second},
		{StepPushed, 2 * time.Second},
		{StepPROpened, 2 * time.Second},
		{StepMerged, 2 * time.Second},
	}
	for _, c := range cases {
		if got := PollInterval(p, c.phase); got != c.want {
			t.Errorf("PollInterval(%s) = %s, want %s", c.phase, got, c.want)
		}
	}
}

// TestPollIntervalReturnsAConfiguredZeroUnchanged: unlike the flight screen's own UI-tick floor
// (minTick, internal/app/flight/model.go), PollInterval itself applies no fallback for a
// configured knob — a zero or negative value is returned exactly as given, because a caller
// that needs a floor applies it at its own boundary (cmd/hoist's driveToCompletion sleeps in a
// plain loop that a zero duration merely skips; only a UI tick loop can spin on one).
func TestPollIntervalReturnsAConfiguredZeroUnchanged(t *testing.T) {
	p := PollIntervals{CI: 0, Approval: -time.Second}
	if got := PollInterval(p, StepCIGreen); got != 0 {
		t.Errorf("PollInterval(CI=0) = %s, want 0 unchanged", got)
	}
	if got := PollInterval(p, StepApproved); got != -time.Second {
		t.Errorf("PollInterval(Approval=-1s) = %s, want -1s unchanged", got)
	}
}

// TestRetryableStepIncludesM5PollingSteps is round-1's regression: retryableStep only listed
// CIGreen and Approved, so a single transient Kubernetes API error (a connection reset, a
// timeout) reading an Argo Application or Deployment status made a drive loop abort
// promote/resume (or stop the flight screen) immediately instead of retrying at
// poll.argo/poll.rollout until poll.deadline — exactly the same shape of problem
// CIGreen/Approved were already carved out for.
func TestRetryableStepIncludesM5PollingSteps(t *testing.T) {
	cases := []struct {
		step StepName
		want bool
	}{
		{StepCIGreen, true},
		{StepApproved, true},
		{StepArgoRefreshed, true},
		{StepArgoSynced, true},
		{StepRolledOut, true},
		// Every other step's error is still terminal — proves this is an addition, not a
		// rewrite that made everything retryable.
		{StepBranched, false},
		{StepCommitted, false},
		{StepPushed, false},
		{StepPROpened, false},
		{StepMerged, false},
	}
	for _, c := range cases {
		if got := RetryableStep(c.step); got != c.want {
			t.Errorf("RetryableStep(%s) = %v, want %v", c.step, got, c.want)
		}
	}
}

// TestIsNotFound covers both adaptors' sentinels, plain-wrapped, plus a negative control so a
// broken predicate reading "any error" as not-found can't pass unnoticed.
func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"argo", argo.ErrNotFound, true},
		{"rollout", rollout.ErrNotFound, true},
		{"wrapped argo", fmt.Errorf("reading Argo Application app-production: %w", argo.ErrNotFound), true},
		{"wrapped rollout", fmt.Errorf("reading Deployment app: %w", rollout.ErrNotFound), true},
		{"unrelated", errors.New("connection reset"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsNotFound(c.err); got != c.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestRetryable is the full decision both cmd/hoist/drive.go's driveToCompletion and
// internal/app/flight's onDriveResult make: retryable only for a *StepError naming a
// RetryableStep whose own error is not one of IsNotFound's structural sentinels.
func TestRetryable(t *testing.T) {
	t.Run("StepError on a retryable step is retryable", func(t *testing.T) {
		err := &StepError{Step: StepArgoSynced, Op: "observe", Err: errors.New("GET applications: connection reset")}
		if !Retryable(err) {
			t.Error("want retryable")
		}
	})

	t.Run("StepError on a non-retryable step is terminal", func(t *testing.T) {
		err := &StepError{Step: StepPushed, Op: "act", Err: errors.New("rejected: non-fast-forward")}
		if Retryable(err) {
			t.Error("want terminal")
		}
	})

	// TestNotFoundOnARetryableStepIsTerminal, moved from internal/app/flight/model_test.go:
	// a missing Application or Deployment is structural, and no amount of waiting brings it
	// back. Observe reports it as Blocked, but Act cannot produce a BlockedError of its own, so
	// an Act that races a deletion after a successful Observe surfaces the sentinel as a plain
	// error — which a naive retryable-step check alone would poll until poll.Deadline.
	t.Run("a retryable step wrapping a not-found sentinel is terminal", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"argo", argo.ErrNotFound},
			{"rollout", rollout.ErrNotFound},
			{"wrapped", fmt.Errorf("reading Argo Application app-production: %w", argo.ErrNotFound)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := &StepError{Step: StepArgoRefreshed, Op: "act", Err: tc.err}
				if Retryable(err) {
					t.Errorf("a %s sentinel on a retryable step must be terminal, not polled until the deadline", tc.name)
				}
			})
		}
	})

	t.Run("a plain error that is not a StepError is terminal", func(t *testing.T) {
		if Retryable(errors.New("boom")) {
			t.Error("want terminal")
		}
	})

	t.Run("nil is terminal", func(t *testing.T) {
		if Retryable(nil) {
			t.Error("want terminal (Retryable is only ever asked about a non-nil error in practice, but nil must not read as retryable)")
		}
	})
}
