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
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
)

// runDriveForTest wires service.Driver.Run exactly as promote.go/deploy.go/resume.go now wire
// it, kept as one helper here so this file's own waiting/heartbeat tests (runHooksForCLI's
// actual consumers) read the same as before the PR that replaced the old direct-call CLI drive
// loop with this Driver.Run call.
func runDriveForTest(ctx context.Context, steps []engine.Step, s *engine.PromotionState, poll config.PollConfig, stderr io.Writer) error {
	d := service.NewDriver(steps, s, nil, pollIntervals(poll), service.DriverHooks{})
	return d.Run(ctx, runHooksForCLI(stderr))
}

// TestPollIntervalsConvertsEveryKnob is the CLI-boundary regression: pollIntervals must carry
// every one of config.PollConfig's four knobs into engine.PollIntervals unchanged — the actual
// per-step mapping and fallback are engine.PollInterval's own responsibility, covered in
// internal/engine/policy_test.go.
func TestPollIntervalsConvertsEveryKnob(t *testing.T) {
	poll := config.PollConfig{
		CI:       config.Duration(11 * time.Second),
		Approval: config.Duration(22 * time.Second),
		Argo:     config.Duration(33 * time.Second),
		Rollout:  config.Duration(44 * time.Second),
	}
	got := pollIntervals(poll)
	want := engine.PollIntervals{CI: 11 * time.Second, Approval: 22 * time.Second, Argo: 33 * time.Second, Rollout: 44 * time.Second}
	if got != want {
		t.Errorf("pollIntervals(%+v) = %+v, want %+v", poll, got, want)
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
	err := runDriveForTest(ctx, []engine.Step{step}, s, poll, &errOut)
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

// A poll interval longer than the heartbeat still shows the run is alive: the sleep is taken
// in heartbeat-sized pieces, each followed by a report that prints only when a heartbeat is
// due and never observes the remote (Copilot, PR #94). Here the heartbeat is shrunk far below
// the poll interval, so several heartbeats land inside one sleep while the step is observed
// only once.
func TestDriveToCompletionHeartbeatsInsideALongPollSleep(t *testing.T) {
	prev := heartbeatEvery
	heartbeatEvery = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = prev })
	s := &engine.PromotionState{ID: "abc123", TargetEnv: "app-production", PR: &forge.PR{Number: 7, URL: "https://github.com/me/my-gitops/pull/7"}}
	step := &waitingStep{name: engine.StepApproved, details: []string{"waiting for `hoist approve abc123` from an approver"}}
	var errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	poll := config.PollConfig{Approval: config.Duration(time.Hour)}
	if err := runDriveForTest(ctx, []engine.Step{step}, s, poll, &errOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if step.calls != 1 {
		t.Errorf("step observed %d times inside one hour-long poll interval, want 1", step.calls)
	}
	if got := strings.Count(errOut.String(), "hoist: still approved:"); got < 2 {
		t.Errorf("only %d heartbeats printed during a poll sleep longer than the heartbeat:\n%s", got, errOut.String())
	}
}

// A retried StepError records no new wait, so no heartbeat is printed for it: a heartbeat off
// an older "waiting:" entry would claim the run is still waiting on a step it has moved past
// (Copilot, PR #99). The step here errors transiently on every observe; History carries an
// old CI wait; nothing but the retry notice may be printed.
func TestDriveToCompletionDoesNotHeartbeatOnARetriedError(t *testing.T) {
	prev := heartbeatEvery
	heartbeatEvery = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = prev })
	s := &engine.PromotionState{TargetEnv: "app-production", History: []engine.HistoryEntry{{Step: engine.StepCIGreen, Detail: "waiting: CI: 1/3 checks complete"}}}
	step := &erroringStep{name: engine.StepApproved, err: errors.New("GET /comments: 502")}
	var errOut bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	poll := config.PollConfig{Approval: config.Duration(time.Hour)}
	if err := runDriveForTest(ctx, []engine.Step{step}, s, poll, &errOut); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if strings.Contains(errOut.String(), "still ci-green") || strings.Contains(errOut.String(), "hoist: ci-green:") {
		t.Errorf("a retried error produced a heartbeat off an older wait:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "(retrying)") {
		t.Errorf("the retry notice itself is missing:\n%s", errOut.String())
	}
}

// erroringStep is a Step whose Observe always fails with err.
type erroringStep struct {
	name engine.StepName
	err  error
}

func (e *erroringStep) Name() engine.StepName { return e.name }
func (e *erroringStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	return engine.Observation{}, e.err
}
func (e *erroringStep) Act(context.Context, *engine.PromotionState) error { return nil }
