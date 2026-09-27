package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/rollout"
)

// TestDriverRunRetriesTransientRolloutErrors is engine.RetryableStep's sibling end-to-end
// regression, exercising the actual loop rather than just the classification function: a
// RolledOutStep whose Rollout.Deployment call always fails with a transient (non-ErrNotFound)
// error must make Driver.Run retry at the configured Rollout interval until ctx's deadline
// elapses — never abort on the first hiccup. The observable proof is the returned error's
// *type*: with the fix, Run keeps calling Step until ctx.Done() fires, so the loop's own
// context.DeadlineExceeded is what comes back; without it (RetryableStep not listing
// StepRolledOut), the very first *engine.StepError would be returned immediately instead, after
// exactly one call to Rollout.Deployment. Moved from cmd/hoist/drive_test.go's own
// TestDriveToCompletionRetriesTransientRolloutErrors when driveToCompletion became Driver.Run
// (service, engine: one Driver for the CLI and the flight screen).
func TestDriverRunRetriesTransientRolloutErrors(t *testing.T) {
	ref, err := image.Parse("ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		TargetEnv: "app-production",
		MergeSHA:  "deadbeef",
		Edits: []gitops.Edit{{
			Occurrence: gitops.Occurrence{
				File: "cluster/apps/app-production/app/deployment.yaml", Kind: "Deployment", Name: "app", Container: "app",
			},
			New: ref,
		}},
	}
	ro := &rollout.Fake{DeploymentErr: errors.New("transient: connection reset")}
	steps := []engine.Step{engine.RolledOutStep{Rollout: ro}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	d := NewDriver(steps, s, func(*engine.PromotionState) error { return nil }, engine.PollIntervals{Rollout: 5 * time.Millisecond}, DriverHooks{})
	err = d.Run(ctx, RunHooks{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (a transient rollout error must be retried until the deadline, not aborted on the first hiccup)", err)
	}
	if len(ro.Calls) < 2 {
		t.Fatalf("Rollout.Deployment called %d time(s), want at least 2 (proves the retry loop actually retried instead of returning after one call)", len(ro.Calls))
	}
}

// TestDriverRunDoesNotRetryArgoRefreshNotFound is Copilot's PR #51 review finding, re-proved
// against Driver.Run: ArgoRefreshedStep.Observe already Blocks cleanly the moment its own Get
// call reports argo.ErrNotFound, so that race (an Application deleted or moved) never reaches
// this loop as a plain StepError at all — but Act's own Refresh call can independently discover
// the SAME absence (a race between Observe succeeding and Act running moments later), and
// DriveStatus always wraps an Act error as a plain *StepError, with no way for Act to produce a
// *BlockedError of its own. Before engine.IsNotFound, that StepError's step name
// (StepArgoRefreshed) was on the retryable list unconditionally, so this raced-Refresh case
// silently retried every poll interval instead of reporting immediately — this proves it does
// not: the fake's Refresh call count stays at exactly one, and the loop returns right away
// rather than running out the clock on ctx's deadline. Moved from cmd/hoist/drive_test.go.
func TestDriverRunDoesNotRetryArgoRefreshNotFound(t *testing.T) {
	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	s := &engine.PromotionState{
		TargetEnv:     "app-production",
		MergeSHA:      "deadbeef",
		ArgoNamespace: "argocd",
		ArgoApps:      []string{"app-app-production"},
		History:       []engine.HistoryEntry{{Step: engine.StepMerged, At: time.Now().Add(-time.Minute)}},
	}
	fake := &argo.Fake{RefreshErr: argo.ErrNotFound}
	// Observe's own Get must succeed and report "not yet reconciled" (never Blocked/Satisfied)
	// so Step actually proceeds to call Act — the race this test exercises only exists on the
	// path through Act, not the one Observe already guards on its own.
	fake.SetStatus(app, argo.Status{ReconciledAt: time.Now().Add(-time.Hour)})
	steps := []engine.Step{engine.ArgoRefreshedStep{Argo: fake}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d := NewDriver(steps, s, func(*engine.PromotionState) error { return nil }, engine.PollIntervals{Argo: 5 * time.Millisecond}, DriverHooks{})
	start := time.Now()
	err := d.Run(ctx, RunHooks{})
	elapsed := time.Since(start)

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want an immediate terminal error, not a retry loop that ran out the deadline", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Run took %s, want well under the 200ms deadline (a genuine ErrNotFound must not be retried)", elapsed)
	}
	refreshCalls := 0
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "Refresh ") {
			refreshCalls++
		}
	}
	if refreshCalls != 1 {
		t.Fatalf("Refresh called %d time(s), want exactly 1 (a retry loop would call it again every poll interval)", refreshCalls)
	}
}

// stubObserveErrStep is a minimal engine.Step whose Observe always fails with a fixed error —
// just enough for Driver.Step to wrap it into a *engine.StepError naming this step, without
// pulling in any real adaptor. Act is never reached in these tests (Observe always errors
// first), so it is a no-op.
type stubObserveErrStep struct {
	name engine.StepName
	err  error
}

func (s stubObserveErrStep) Name() engine.StepName { return s.name }
func (s stubObserveErrStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	return engine.Observation{}, s.err
}
func (s stubObserveErrStep) Act(context.Context, *engine.PromotionState) error { return nil }

// TestDriverStepRetryWaitsAtTheFailedStepsCadence is
// TestRetryAfterAStatusErrorPollsAtTheFailedStepsCadence's (internal/app/flight/model_test.go,
// pre-Driver) equivalent against Driver.Step directly: moved, not just renamed, because the old
// two-call flight.Model.tickDelay shape this proved is gone, but the same failure mode is still
// reachable through Driver.Step's own phase computation (see its doc comment: "the failing
// step, when there was one, else the promotion's own current phase"). A promotion's
// *engine.PromotionState.Phase is only an advisory hint left over from whichever step the LAST
// poll stopped at — here deliberately seeded to a step with its own distinct cadence before the
// step under test ever runs — and Driver.Step must compute this poll's Tick.Wait from the step
// *this* call's StepError actually names, never from that stale hint. Each configured interval
// below is distinct and non-zero so a Wait that reads the wrong knob, or the 2s unconfigured
// default, cannot pass by coincidence.
func TestDriverStepRetryWaitsAtTheFailedStepsCadence(t *testing.T) {
	poll := engine.PollIntervals{
		CI:       11 * time.Millisecond,
		Approval: 13 * time.Millisecond,
		Argo:     17 * time.Millisecond,
		Rollout:  19 * time.Millisecond,
	}

	cases := []struct {
		name      string
		failStep  engine.StepName
		stepErr   error
		wantRetry bool
		wantWait  time.Duration
	}{
		{
			name:      "retryable step error waits at the failed step's own cadence, not the stale phase's",
			failStep:  engine.StepArgoRefreshed,
			stepErr:   errors.New("transient: connection reset"),
			wantRetry: true,
			wantWait:  poll.Argo,
		},
		{
			name:      "a StepError wrapping argo.ErrNotFound is never retried",
			failStep:  engine.StepArgoRefreshed,
			stepErr:   argo.ErrNotFound,
			wantRetry: false,
			wantWait:  poll.Argo, // still the failed step's own cadence — only Retry differs
		},
		{
			name:      "a non-retryable step's error is never retried and gets the unconfigured 2s default",
			failStep:  engine.StepBranched,
			stepErr:   errors.New("worktree: permission denied"),
			wantRetry: false,
			wantWait:  2 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Seed Phase to StepCIGreen (poll.CI = 11ms) in every case: distinct from every
			// wantWait above, so a Wait computed from this stale hint instead of the failing
			// step is caught regardless of which case is running.
			s := &engine.PromotionState{Phase: engine.StepCIGreen}
			steps := []engine.Step{stubObserveErrStep{name: tc.failStep, err: tc.stepErr}}
			d := NewDriver(steps, s, func(*engine.PromotionState) error { return nil }, poll, DriverHooks{})

			tick, err := d.Step(context.Background())

			var stepErr *engine.StepError
			if !errors.As(err, &stepErr) || stepErr.Step != tc.failStep {
				t.Fatalf("err = %v, want a *engine.StepError naming %s", err, tc.failStep)
			}
			if tick.Retry != tc.wantRetry {
				t.Fatalf("tick.Retry = %v, want %v", tick.Retry, tc.wantRetry)
			}
			if tick.Wait != tc.wantWait {
				t.Fatalf("tick.Wait = %v, want %v (the failed step %s's own cadence, not the stale Phase's poll.CI=%v)", tick.Wait, tc.wantWait, tc.failStep, poll.CI)
			}
		})
	}
}
