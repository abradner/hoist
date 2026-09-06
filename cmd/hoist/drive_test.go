package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
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

// waitingStep is a Step that reports Waiting with whatever detail the test has queued,
// one per Observe call (the last one repeats), and never Acts.
type waitingStep struct {
	name    engine.StepName
	details []string
	calls   int
}

func (w *waitingStep) Name() engine.StepName { return w.name }
func (w *waitingStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	i := w.calls
	if i >= len(w.details) {
		i = len(w.details) - 1
	}
	w.calls++
	return engine.Observation{Waiting: true, Detail: w.details[i]}, nil
}
func (w *waitingStep) Act(context.Context, *engine.PromotionState) error { return nil }

// A run parked on a Waiting step prints why, once, and again only when the reason changes —
// never once per tick, and never nothing at all (issue #68: the first real promotion sat
// silently at Approved for what looked like a hang). The approval wait also prints the exact
// comment to post and where, once.
func TestDriveToCompletionPrintsEachWaitingReasonOnce(t *testing.T) {
	s := &engine.PromotionState{ID: "abc123", TargetEnv: "app-production", PR: &forge.PR{Number: 7, URL: "https://github.com/me/my-gitops/pull/7"}}
	step := &waitingStep{name: engine.StepApproved, details: []string{"waiting for `hoist approve abc123` from an approver", "waiting for `hoist approve abc123` from an approver", "a rejection was posted; waiting for a newer approval"}}
	var errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	poll := config.PollConfig{Approval: config.Duration(5 * time.Millisecond)}
	err := driveToCompletion(ctx, []engine.Step{step}, s, nil, poll, &errOut)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if step.calls < 4 {
		t.Fatalf("only %d observes; the loop did not keep polling", step.calls)
	}
	out := errOut.String()
	first := "hoist: approved: waiting: waiting for `hoist approve abc123` from an approver\n"
	if got := strings.Count(out, first); got != 1 {
		t.Errorf("first reason printed %d times, want exactly once:\n%s", got, out)
	}
	if got := strings.Count(out, "hoist: approved: waiting: a rejection was posted; waiting for a newer approval\n"); got != 1 {
		t.Errorf("changed reason printed %d times, want exactly once:\n%s", got, out)
	}
	if got := strings.Count(out, "hoist: to approve, comment `hoist approve abc123` on https://github.com/me/my-gitops/pull/7\n"); got != 1 {
		t.Errorf("approval instructions printed %d times, want exactly once:\n%s", got, out)
	}
}

// An unchanged reason still gets a heartbeat, on a cadence far slower than any poll interval,
// so an hour-long wait shows something recent without filling the scrollback.
func TestWaitingReporterHeartbeatsOnUnchangedReason(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	r := waitingReporter{w: &out, now: func() time.Time { return now }}
	s := &engine.PromotionState{History: []engine.HistoryEntry{{Step: engine.StepCIGreen, Detail: "waiting: CI: 1/3 checks complete"}}}
	for i := 0; i < 5; i++ {
		r.report(s)
		now = now.Add(time.Minute)
	}
	if got := out.String(); got != "hoist: ci-green: waiting: CI: 1/3 checks complete\n" {
		t.Fatalf("five minutes of the same reason printed:\n%s", got)
	}
	now = now.Add(heartbeatEvery)
	r.report(s)
	if !strings.Contains(out.String(), "hoist: still ci-green: waiting: CI: 1/3 checks complete (15m0s so far)\n") {
		t.Errorf("no heartbeat after %s:\n%s", heartbeatEvery, out.String())
	}
	r.report(s)
	if got := strings.Count(out.String(), "still"); got != 1 {
		t.Errorf("heartbeat repeated %d times within one interval", got)
	}
	// "so far" counts from when the reason first appeared, not from the previous heartbeat:
	// a three-hour approval wait must not keep reporting ten minutes.
	now = now.Add(heartbeatEvery)
	r.report(s)
	if !strings.Contains(out.String(), "(25m0s so far)") {
		t.Errorf("second heartbeat does not count from the start of the wait:\n%s", out.String())
	}
	// Drive saves after recording the wait, and a failed save appends its own entry after it:
	// the reason reported is still the wait, never the save failure read off the end.
	s.History = append(s.History, engine.HistoryEntry{Step: engine.StepCIGreen, Detail: "state save failed: disk full"})
	r.report(s)
	if strings.Contains(out.String(), "state save failed") {
		t.Errorf("a save-failure entry was reported as the waiting reason:\n%s", out.String())
	}
}
