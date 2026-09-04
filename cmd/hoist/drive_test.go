package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/rollout"
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

// TestRetryableStepIncludesM5PollingSteps is round-1's regression: retryableStep only listed
// CIGreen and Approved, so a single transient Kubernetes API error (a connection reset, a
// timeout) reading an Argo Application or Deployment status made driveToCompletion abort
// promote/resume immediately instead of retrying at poll.argo/poll.rollout until poll.deadline —
// exactly the same shape of problem CIGreen/Approved were already carved out for.
func TestRetryableStepIncludesM5PollingSteps(t *testing.T) {
	cases := []struct {
		step engine.StepName
		want bool
	}{
		{engine.StepCIGreen, true},
		{engine.StepApproved, true},
		{engine.StepArgoRefreshed, true},
		{engine.StepArgoSynced, true},
		{engine.StepRolledOut, true},
		// Every other step's error is still terminal — proves this is an addition, not a
		// rewrite that made everything retryable.
		{engine.StepBranched, false},
		{engine.StepCommitted, false},
		{engine.StepPushed, false},
		{engine.StepPROpened, false},
		{engine.StepMerged, false},
	}
	for _, c := range cases {
		if got := retryableStep(c.step); got != c.want {
			t.Errorf("retryableStep(%s) = %v, want %v", c.step, got, c.want)
		}
	}
}

// TestDriveToCompletionRetriesTransientRolloutErrors is retryableStep's sibling end-to-end
// regression, exercising the actual loop rather than just the classification function: a
// RolledOutStep whose Rollout.Deployment call always fails with a transient (non-ErrNotFound)
// error must make driveToCompletion retry at poll.rollout until ctx's deadline elapses — never
// abort on the first hiccup. The observable proof is the returned error's *type*: with the fix,
// driveToCompletion keeps calling Drive until ctx.Done() fires, so the loop's own
// context.DeadlineExceeded is what comes back; without it (retryableStep not listing
// StepRolledOut), the very first *engine.StepError from Drive would be returned immediately
// instead, after exactly one call to Rollout.Deployment.
func TestDriveToCompletionRetriesTransientRolloutErrors(t *testing.T) {
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
	poll := config.PollConfig{Rollout: config.Duration(5 * time.Millisecond)}
	err = driveToCompletion(ctx, steps, s, func(*engine.PromotionState) error { return nil }, poll, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (a transient rollout error must be retried until poll.deadline, not aborted on the first hiccup)", err)
	}
	if len(ro.Calls) < 2 {
		t.Fatalf("Rollout.Deployment called %d time(s), want at least 2 (proves the retry loop actually retried instead of returning after one call)", len(ro.Calls))
	}
}

// TestDriveToCompletionDoesNotRetryArgoRefreshNotFound is Copilot's PR #51 review finding:
// ArgoRefreshedStep.Observe already Blocks cleanly the moment its own Get call reports
// argo.ErrNotFound, so that race (an Application deleted or moved) never reaches this loop as a
// plain StepError at all — but Act's own Refresh call can independently discover the SAME
// absence (a race between Observe succeeding and Act running moments later), and Drive always
// wraps an Act error as a plain *StepError, with no way for Act to produce a *BlockedError of
// its own. Before isNotFoundErr, that StepError's step name (StepArgoRefreshed) was on the
// retryable list unconditionally, so this raced-Refresh case silently retried every poll.argo
// interval instead of reporting immediately — this proves it does not: the fake's Refresh call
// count stays at exactly one, and the loop returns right away rather than running out the clock
// on ctx's deadline.
func TestDriveToCompletionDoesNotRetryArgoRefreshNotFound(t *testing.T) {
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
	// so Drive actually proceeds to call Act — the race this test exercises only exists on the
	// path through Act, not the one Observe already guards on its own.
	fake.SetStatus(app, argo.Status{ReconciledAt: time.Now().Add(-time.Hour)})
	steps := []engine.Step{engine.ArgoRefreshedStep{Argo: fake}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	poll := config.PollConfig{Argo: config.Duration(5 * time.Millisecond)}
	start := time.Now()
	err := driveToCompletion(ctx, steps, s, func(*engine.PromotionState) error { return nil }, poll, io.Discard)
	elapsed := time.Since(start)

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want an immediate terminal error, not a retry loop that ran out the deadline", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("driveToCompletion took %s, want well under the 200ms deadline (a genuine ErrNotFound must not be retried)", elapsed)
	}
	refreshCalls := 0
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "Refresh ") {
			refreshCalls++
		}
	}
	if refreshCalls != 1 {
		t.Fatalf("Refresh called %d time(s), want exactly 1 (a retry loop would call it again every poll.argo interval)", refreshCalls)
	}
}
