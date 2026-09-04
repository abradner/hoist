package flight

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// TestStepOrderMatchesAllSteps guards StepOrder (a literal, see rows.go's doc comment)
// against drifting from engine.AllSteps' own order. Passing nil for the git.Git/forge.Forge
// parameters is safe here: every Step.Name() implementation in internal/engine ignores its
// receiver's fields, and this test would fail loudly (a nil-pointer panic from some future
// Name() that stopped ignoring them) rather than silently, if that ever changed.
func TestStepOrderMatchesAllSteps(t *testing.T) {
	steps := engine.AllSteps(nil, nil, nil)
	if len(steps) != len(StepOrder) {
		t.Fatalf("engine.AllSteps has %d steps, StepOrder has %d", len(steps), len(StepOrder))
	}
	for i, s := range steps {
		if s.Name() != StepOrder[i] {
			t.Errorf("StepOrder[%d] = %s, want %s (engine.AllSteps' order)", i, StepOrder[i], s.Name())
		}
	}
}

func fixtureState() engine.PromotionState {
	return engine.PromotionState{
		ID:        "abcd1234",
		SourceEnv: "app-staging",
		TargetEnv: "app-production",
		History: []engine.HistoryEntry{
			{Step: engine.StepBranched, At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Detail: "acted"},
		},
	}
}

// stubDrive returns a DriveFunc that always returns the given (done, statuses, err),
// counting calls and recording the state it was invoked with.
type stubDrive struct {
	calls    int
	lastSeen engine.PromotionState
	next     engine.PromotionState
	done     bool
	statuses []engine.StepStatus
	err      error
}

func (s *stubDrive) fn() DriveFunc {
	return func(_ context.Context, state engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		s.calls++
		s.lastSeen = state
		next := s.next
		if next.ID == "" {
			next = state
		}
		return next, s.done, s.statuses, s.err
	}
}

// runBatch drives one tea.Cmd through Update exactly once — mirrors internal/app's own root
// loop (each cmd's message is delivered once, never re-run), matching plan.Model's own
// runInit test helper. cmd may itself be a batch (tea.Batch, e.g. Init's spinner-tick-plus-
// drive-call, or the Reobserve key's own spinner-tick-plus-drive-call after finding #5's
// fix): every sub-command's own message is delivered to Update in order. Whatever new tea.Cmd
// a delivered message's own Update call returns (in particular scheduleTick's tea.Tick, which
// really sleeps for a poll interval) is deliberately never executed here — a test that needs
// to inspect or run that follow-up command does so explicitly itself.
func runBatch(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		m, _ = m.Update(msg)
		return m
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		m, _ = m.Update(c())
	}
	return m
}

// runInit runs Init()'s own command through runBatch.
func runInit(t *testing.T, m Model) Model {
	t.Helper()
	return runBatch(t, m, m.Init())
}

// TestInitDrivesImmediately: New with a non-nil driveFn fires a drive call as part of
// Init — the screen should show real status right away, not a screenful of dots until the
// first poll interval elapses.
func TestInitDrivesImmediately(t *testing.T) {
	drv := &stubDrive{statuses: []engine.StepStatus{
		{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true, Detail: "worktree present"}},
	}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	if !m.busy {
		t.Fatal("busy = false immediately after New with a non-nil driveFn")
	}
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = runInit(t, m)
	if drv.calls != 1 {
		t.Fatalf("driveFn called %d times, want 1", drv.calls)
	}
	if m.busy {
		t.Error("still busy after the drive result landed")
	}
	if got := indexRows(m.rows)[engine.StepBranched]; got.Glyph != GlyphDone {
		t.Errorf("Branched row = %+v, want done", got)
	}
}

// TestDriveCmdBoundedByPollDeadline: a DriveFunc that hangs (blocks on ctx.Done() rather than
// ever returning) must not stall driveCmd forever — poll.Deadline bounds it, so the call
// returns (with ctx's own deadline error) once that elapses, instead of the goroutine blocking
// indefinitely. A zero Deadline (the config zero value, not this repo's Normalize-filled
// default) means no bound at all, by design — only the case actually reachable via real config
// (deadline set) is tested here.
func TestDriveCmdBoundedByPollDeadline(t *testing.T) {
	hung := func(ctx context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		return s, false, nil, ctx.Err()
	}
	m := New(fixtureState(), PollDurations{Deadline: 20 * time.Millisecond}, hung)
	cmd := m.driveCmd()
	if cmd == nil {
		t.Fatal("driveCmd returned nil for a non-nil driveFn")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		res, ok := msg.(driveResultMsg)
		if !ok {
			t.Fatalf("got %T, want driveResultMsg", msg)
		}
		if !errors.Is(res.err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("driveCmd did not return within 2s of a 20ms poll.Deadline — a hung DriveFunc call can still stall the screen forever")
	}
}

// TestDriveCmdSharesOneAbsoluteDeadlineAcrossPolls is PR #50 review finding #8 (Codex):
// mapping poll.deadline into a fresh context.WithTimeout(ctx, deadline) on every driveCmd call
// re-derives a brand new deadline-length window each poll, so a promotion stuck re-polling
// CI/approval (each individual wait returns well within the deadline, then schedules another
// call with a full-length timeout again) never actually hits poll.Deadline no matter how long
// it runs — unlike cmd/hoist/drive.go's own driveToCompletion, which bounds its ENTIRE wait by
// one deadline its caller wraps around ctx once. This proves the fix: a driveFn that blocks
// until its own ctx expires, called twice with the SAME Model (so it shares the SAME
// m.deadlineAt, computed once in New), must have its second call's ctx already expired at
// creation — returning near-instantly — rather than getting its own fresh window and blocking
// for another almost-full poll.Deadline.
func TestDriveCmdSharesOneAbsoluteDeadlineAcrossPolls(t *testing.T) {
	hung := func(ctx context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		return s, false, nil, ctx.Err()
	}
	m := New(fixtureState(), PollDurations{Deadline: 60 * time.Millisecond}, hung)

	msg1, ok := m.driveCmd()().(driveResultMsg)
	if !ok || !errors.Is(msg1.err, context.DeadlineExceeded) {
		t.Fatalf("first driveCmd result = %#v, want a driveResultMsg with context.DeadlineExceeded", msg1)
	}

	start := time.Now()
	msg2, ok := m.driveCmd()().(driveResultMsg)
	elapsed := time.Since(start)
	if !ok || !errors.Is(msg2.err, context.DeadlineExceeded) {
		t.Fatalf("second driveCmd result = %#v, want a driveResultMsg with context.DeadlineExceeded", msg2)
	}
	if elapsed > 30*time.Millisecond {
		t.Errorf("second driveCmd call took %v to return — it looks like it got its own fresh poll.Deadline-length timeout instead of sharing one absolute deadline with the first call (want near-instant: the shared deadline had already elapsed)", elapsed)
	}
}

// TestDriveCmdStampsCurrentGen: every driveResultMsg driveCmd produces must carry the issuing
// Model's own generation (m.gen) — the guard onDriveResult uses to drop a stale result from a
// different flight.Model instance (see TestStaleDriveResultFromAnotherModelIsIgnored below,
// PR #50 review finding #4). This is the direct, narrow check that driveCmd actually stamps it.
func TestDriveCmdStampsCurrentGen(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, (&stubDrive{}).fn())
	msg, ok := m.driveCmd()().(driveResultMsg)
	if !ok {
		t.Fatalf("driveCmd's result is %T, want driveResultMsg", msg)
	}
	if msg.gen != m.gen {
		t.Errorf("driveResultMsg.gen = %d, want %d (m.gen)", msg.gen, m.gen)
	}
}

// TestStaleDriveResultFromAnotherModelIsIgnored is PR #50 review finding #4 (Codex): the
// root's message dispatch (internal/app/app.go's Update, "forward everything else to the top
// screen") delivers a message to whichever screen is on top by concrete Go type alone, with no
// notion of which screen instance actually issued the tea.Cmd that produced it. A driveCmd
// still in flight for a flight.Model the operator has since aborted can complete later and
// deliver one more driveResultMsg — which a DIFFERENT flight.Model now on top (driving a
// different promotion) must not silently adopt as its own: doing so would show the wrong
// promotion's state/statuses while continuing to poll and save under the wrong driveFn/state
// closure. This builds a driveResultMsg stamped with one Model's gen ("A", aborted) and feeds
// it to a completely different Model ("B") — B must ignore it outright, unchanged.
func TestStaleDriveResultFromAnotherModelIsIgnored(t *testing.T) {
	a := New(fixtureState(), PollDurations{}, (&stubDrive{}).fn()) // stands in for the aborted promotion
	staleFromA := driveResultMsg{
		gen:   a.gen,
		state: engine.PromotionState{ID: "from-a-not-b"},
		done:  true,
	}

	b := New(engine.PromotionState{ID: "b-own-id", SourceEnv: "x", TargetEnv: "y"}, PollDurations{}, (&stubDrive{}).fn())
	if b.gen == a.gen {
		t.Fatal("setup: two New() calls produced the same gen — this test can't tell stale from current")
	}
	wantState, wantBusy, wantDone := b.state, b.busy, b.done

	got, cmd := b.Update(staleFromA)
	if cmd != nil {
		t.Errorf("a stale driveResultMsg produced a command: %#v", cmd())
	}
	if got.state.ID != wantState.ID {
		t.Errorf("model B adopted A's state: ID = %q, want unchanged %q", got.state.ID, wantState.ID)
	}
	if got.done != wantDone {
		t.Errorf("model B's done changed to %v processing A's stale, unrelated result", got.done)
	}
	if got.busy != wantBusy {
		t.Errorf("busy changed from %v to %v processing a stale result", wantBusy, got.busy)
	}
}

// TestNilDriveFuncNeverTicks: a read-only flight screen (driveFn nil, the shape app.go's
// plan.StartMsg handler currently pushes — see app.go's own comment on why) never calls
// anything and never schedules a tick.
func TestNilDriveFuncNeverTicks(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	if m.busy {
		t.Fatal("busy = true with a nil driveFn")
	}
	m2 := runInit(t, m)
	if m2.busy {
		t.Error("busy after Init with a nil driveFn")
	}
	// Every row should still be "not yet reached" — nothing was ever observed.
	for _, r := range m2.rows {
		if r.Glyph != GlyphNotReached {
			t.Errorf("row %+v, want not-reached (nothing to drive)", r)
		}
	}
}

// TestReobserveBypassesTick: R fires a drive call immediately, without waiting for a
// scheduled tick — the whole point of the key (per the design brief: "R re-observe").
func TestReobserveBypassesTick(t *testing.T) {
	drv := &stubDrive{statuses: []engine.StepStatus{
		{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}},
	}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = runInit(t, m) // consume the initial drive so calls resets meaning clearly
	if drv.calls != 1 {
		t.Fatalf("setup: driveFn called %d times, want 1", drv.calls)
	}

	m, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd == nil {
		t.Fatal("R produced no command")
	}
	if !m.busy {
		t.Error("busy = false immediately after R")
	}
	// R's command is now a batch (the drive call and a spinner.Tick to resume the spinner's
	// own animation while busy — see finding #5's fix), so run it through runBatch rather
	// than feeding cmd()'s tea.BatchMsg straight into Update.
	m = runBatch(t, m, cmd)
	if drv.calls != 2 {
		t.Errorf("driveFn called %d times after R, want 2", drv.calls)
	}
}

// TestReobserveIgnoredWhileBusy: a second R while a drive call is already in flight must not
// fire an overlapping DriveFunc call.
func TestReobserveIgnoredWhileBusy(t *testing.T) {
	drv := &stubDrive{}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	// Do not run Init's cmd — m.busy is already true (set by New), simulating "a drive call
	// is in flight".
	if !m.busy {
		t.Fatal("setup: expected busy = true before Init's cmd has landed")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R while busy produced a command")
	}
}

// TestReobserveOnNilDriveFuncShowsNotice: R on a read-only screen shows a notice rather than
// panicking on a nil DriveFunc call.
func TestReobserveOnNilDriveFuncShowsNotice(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	m = m.SetSize(80, 10)
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R with a nil driveFn produced a command")
	}
	if !strings.Contains(m.View(), "nothing to re-observe") {
		t.Errorf("view missing the read-only notice:\n%s", m.View())
	}
}

// TestOpenPRKey: o emits OpenPRMsg naming s.PR's URL once one has been observed; before
// that, it shows a notice instead.
func TestOpenPRKey(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	m = m.SetSize(80, 10)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if cmd != nil {
		t.Fatal("o with no PR yet produced a command")
	}

	m.state.PR = &forge.PR{URL: "https://example.invalid/pr/97"}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if cmd == nil {
		t.Fatal("o with a PR produced no command")
	}
	msg, ok := cmd().(OpenPRMsg)
	if !ok || msg.URL != "https://example.invalid/pr/97" {
		t.Errorf("o's command = %#v, want OpenPRMsg{URL: https://example.invalid/pr/97}", cmd())
	}
}

// TestAbortKeyNoticeWhenNotDriving: x is a no-op-with-notice, never emitting AbortMsg, when
// there is nothing real to abort — no DriveFunc wired (read-only, the shape app.go's
// plan.StartMsg handler currently pushes) and/or an empty promotion ID (the same stub
// state). PR #39 review finding #2: emitting AbortMsg unconditionally risked a future
// handler mishandling an abort with an ID nothing downstream could safely act on.
func TestAbortKeyNoticeWhenNotDriving(t *testing.T) {
	cases := []struct {
		name    string
		state   engine.PromotionState
		driveFn DriveFunc
	}{
		{"nil driveFn, non-empty ID (today's actual stub shape has driveFn nil)", fixtureState(), nil},
		{"real driveFn, empty ID", engine.PromotionState{SourceEnv: "app-staging", TargetEnv: "app-production"}, (&stubDrive{}).fn()},
		{"nil driveFn, empty ID", engine.PromotionState{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.state, PollDurations{}, tc.driveFn)
			m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))
			m, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			if cmd != nil {
				t.Fatal("x produced a command when there is nothing to abort")
			}
			if !strings.Contains(m.View(), "nothing to abort") {
				t.Errorf("view missing the not-driving notice:\n%s", m.View())
			}
		})
	}
}

// TestAbortKeyEmitsWhenDriving: once the model has a real DriveFunc and a real, non-empty
// promotion ID (constructed directly here — app.go's own wiring doesn't produce this shape
// yet), x still emits AbortMsg exactly as before the finding #2 guard: the guard only blocks
// the currently-always-true stub case, it does not change the general contract.
func TestAbortKeyEmitsWhenDriving(t *testing.T) {
	state := fixtureState() // ID: "abcd1234"
	m := New(state, PollDurations{}, (&stubDrive{}).fn())
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd == nil {
		t.Fatal("x produced no command")
	}
	msg, ok := cmd().(AbortMsg)
	if !ok || msg.ID != "abcd1234" {
		t.Errorf("x's command = %#v, want AbortMsg{ID: abcd1234}", cmd())
	}
}

// TestBackKey: esc emits BackMsg.
func TestBackKey(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Errorf("esc's command = %T, want BackMsg", cmd())
	}
}

// TestLogToggle: l shows/hides PromotionState.History in View().
func TestLogToggle(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if strings.Contains(m.View(), "History:") {
		t.Fatal("history shown before l was pressed")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !strings.Contains(m.View(), "History:") || !strings.Contains(m.View(), "acted") {
		t.Errorf("history not shown after l:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if strings.Contains(m.View(), "History:") {
		t.Error("history still shown after a second l")
	}
}

// TestDriveErrorShowsNoticeAndKeepsPolling: a plumbing error from DriveFunc on a retryable step
// (StepCIGreen/StepApproved — Known bug classes: a transient 404/permissions hiccup on
// Checks/Comments) must not stop the screen from scheduling another attempt (mirrors
// cmd/hoist/drive.go's own driveToCompletion retry behaviour, via retryableErr/retryableStep).
// The error is a properly-shaped *engine.StepError on StepCIGreen, matching what engine.Drive
// itself actually returns for this scenario (Drive wraps an Observe/Act error in *StepError
// before ever handing it back) — see TestDriveErrorOnNonRetryableStepStopsPolling below for the
// terminal-step counterpart PR #50 review finding #7 added.
func TestDriveErrorShowsNoticeAndKeepsPolling(t *testing.T) {
	drv := &stubDrive{err: &engine.StepError{Step: engine.StepCIGreen, Op: "observe", Err: errors.New("GET check-runs: 404")}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	cmd := m.Init()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("Init's command = %#v, want a 2-command batch (spinner tick, drive)", msg)
	}
	// batch[1] is the drive cmd (see Init: tea.Batch(m.spinner.Tick, m.driveCmd())).
	driveResult := batch[1]()
	m, tickCmd := m.Update(driveResult)
	if m.busy {
		t.Error("busy = true after an errored drive result")
	}
	if !strings.Contains(m.View(), "404") {
		t.Errorf("view missing the plumbing-error notice:\n%s", m.View())
	}
	if tickCmd == nil {
		t.Fatal("no further tick scheduled after a plumbing error")
	}
}

// TestDriveErrorOnNonRetryableStepStopsPolling is PR #50 review finding #7 (Codex):
// cmd/hoist/drive.go's own driveToCompletion retries a *engine.StepError only on
// StepCIGreen/StepApproved; every other step's error — a rejected push, a failed signing
// commit — is terminal there and returned immediately, never retried. Before this fix,
// onDriveResult scheduled another poll for literally any non-nil err regardless of which step
// it came from, so the screen would silently repeat a terminal Act failure every ~2s until
// poll.Deadline elapsed instead of stopping and surfacing it as a real failure. This uses
// StepPushed (not CIGreen/Approved) to prove the terminal path: no further tick, either as the
// immediate result of processing the error or for a tick arriving afterward.
func TestDriveErrorOnNonRetryableStepStopsPolling(t *testing.T) {
	drv := &stubDrive{err: &engine.StepError{Step: engine.StepPushed, Op: "act", Err: errors.New("rejected: non-fast-forward")}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	cmd := m.Init()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("Init's command is not a 2-command batch")
	}
	m, tickCmd := m.Update(batch[1]())
	if m.busy {
		t.Error("busy = true after a terminal drive error")
	}
	if !strings.Contains(m.View(), "rejected: non-fast-forward") {
		t.Errorf("view missing the terminal-error notice:\n%s", m.View())
	}
	if tickCmd != nil {
		t.Error("a terminal (non-retryable) step error still scheduled another poll")
	}
	// A stray tick arriving later (one already scheduled before the failure, say) must not
	// fire another drive call either.
	if _, cmd := m.Update(tickMsg{}); cmd != nil {
		t.Error("a stray tick after a terminal step error still fired another drive call")
	}
}

// TestDriveResultAdoptsStateOnError is PR #50 round-4 review finding #7 (Codex): driveFn
// always returns however far a single drive iteration actually got, even one that ends in
// error — a branch, commit, push and PR can all have already succeeded (and been persisted by
// driveFn's own save callback) before a later step's Act then fails. Before this fix,
// onDriveResult's error branch returned without ever adopting msg.state, so the screen kept
// showing whatever it was constructed with — here, a state with no PR at all — even though the
// state driveFn actually returned (and drove.lastSeen would be re-driven from on a manual R)
// already has one. This proves m.state is the returned state, not the constructor's original
// one, once an error result has been processed.
func TestDriveResultAdoptsStateOnError(t *testing.T) {
	original := fixtureState() // no PR set
	withPR := original
	withPR.PR = &forge.PR{Number: 7, URL: "https://example.invalid/pull/7"}
	drv := &stubDrive{
		next: withPR,
		err:  &engine.StepError{Step: engine.StepPushed, Op: "act", Err: errors.New("rejected: non-fast-forward")},
	}
	m := New(original, PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	batch := m.Init()().(tea.BatchMsg)
	m, _ = m.Update(batch[1]())

	if url, ok := PRURL(m.state); !ok || url != withPR.PR.URL {
		t.Errorf("m.state after an errored drive result has PR %v, want the returned state's PR %v (o would report no PR to open otherwise)", m.state.PR, withPR.PR)
	}
}

// TestDriveResultUpdatesRowsOnError is PR #50 round-5 review finding (Copilot): DriveFunc
// always calls engine.Status after engine.Drive regardless of whether Drive itself errored
// (cmd/hoist/wiring.go), so msg.statuses reflects the real, current per-step standing even on
// a failed poll — but before this fix, onDriveResult's error branch never derived m.rows from
// it, only m.state. A PR already opened before a later step's Act fails would leave the step
// list still showing "PR: not yet opened" even though m.state (fixed in round 4) correctly
// showed the PR. This proves m.rows reflects msg.statuses even when the same result carries a
// (retryable) error.
func TestDriveResultUpdatesRowsOnError(t *testing.T) {
	drv := &stubDrive{
		statuses: []engine.StepStatus{
			{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}},
			{Step: engine.StepCommitted, Observation: engine.Observation{Satisfied: true}},
			{Step: engine.StepPushed, Observation: engine.Observation{Satisfied: true}},
			{Step: engine.StepPROpened, Observation: engine.Observation{Satisfied: true}},
			{Step: engine.StepCIGreen, Observation: engine.Observation{Waiting: true, Detail: "pending"}},
		},
		err: &engine.StepError{Step: engine.StepCIGreen, Op: "observe", Err: errors.New("GET check-runs: 404")},
	}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	batch := m.Init()().(tea.BatchMsg)
	m, _ = m.Update(batch[1]())

	v := m.View()
	if !strings.Contains(v, GlyphDone) {
		t.Errorf("view after an errored-but-informative drive result shows no completed step:\n%s", v)
	}
	for _, done := range []engine.StepName{engine.StepBranched, engine.StepCommitted, engine.StepPushed, engine.StepPROpened} {
		row, ok := indexRows(m.rows)[done]
		if !ok || row.Glyph != GlyphDone {
			t.Errorf("row for %s = %+v (ok=%v), want done — m.rows should reflect msg.statuses even on an errored poll", done, row, ok)
		}
	}
}

// TestRetryableErrorAfterPriorStopClearsStoppedAndResumesPolling is PR #50 round-5 review
// finding (Copilot): a retryable error must clear m.stopped, not just schedule a tick — if
// this poll followed a manual R retry after an EARLIER, unrelated terminal stop (R bypasses
// the m.stopped gate on purpose, see onDriveResult's own comment), m.stopped was still true
// from that prior stop; without clearing it here, the very tick this call schedules would be
// immediately suppressed by tickMsg's own busy||done||stopped||driveFn==nil check the moment
// it fires — silently breaking automatic re-polling even though this error is exactly the
// transient kind that's supposed to keep retrying on its own.
func TestRetryableErrorAfterPriorStopClearsStoppedAndResumesPolling(t *testing.T) {
	terminal := &stubDrive{err: &engine.StepError{Step: engine.StepPushed, Op: "act", Err: errors.New("rejected: non-fast-forward")}}
	m := New(fixtureState(), PollDurations{}, terminal.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	batch := m.Init()().(tea.BatchMsg)
	m, tickCmd := m.Update(batch[1]())
	if !m.stopped || tickCmd != nil {
		t.Fatalf("setup: expected the terminal error to stop polling first (stopped=%v, tickCmd=%v)", m.stopped, tickCmd)
	}

	// The operator presses R (Reobserve) to retry by hand; this time the poll comes back with
	// a retryable error instead (a transient CI-endpoint hiccup).
	retryable := &stubDrive{err: &engine.StepError{Step: engine.StepCIGreen, Op: "observe", Err: errors.New("GET check-runs: 404")}}
	m.driveFn = retryable.fn()
	m, retryCmd := m.handleKey(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if retryCmd == nil {
		t.Fatal("R produced no command")
	}
	rBatch, ok := retryCmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("R's command = %#v, want a spinner-tick-plus-drive batch", retryCmd())
	}
	// handleKey's Reobserve case orders its batch driveCmd-first (tea.Batch(m.driveCmd(),
	// m.spinner.Tick)) — the opposite order from Init's (spinner.Tick, then driveCmd) — so
	// index 0, not the last element, is the drive result here.
	m, tickCmd = m.Update(rBatch[0]())
	if m.stopped {
		t.Error("stopped = true after a retryable error, want false (a stale stop must not survive a retryable result)")
	}
	if tickCmd == nil {
		t.Fatal("no tick scheduled after a retryable error")
	}

	// The scheduled tick itself must not be suppressed by a stale m.stopped.
	if _, cmd := m.Update(tickMsg{}); cmd == nil {
		t.Error("a tick after a retryable error was suppressed — m.stopped was left true from the earlier terminal stop")
	}
}

// TestDriveResultBlockedStopsPolling is PR #50 round-4 review finding #5 (Codex):
// cmd/hoist/wiring.go's DriveFunc deliberately suppresses *engine.BlockedError as msg.err (it
// is read from the statuses engine.Status produces instead, the same way Waiting already is —
// see DriveFunc's own comment), so msg.err is nil even when the active step is genuinely
// Blocked. Before this fix, onDriveResult only ever stopped automatic polling by inspecting
// msg.err, so a Blocked result — terminal until an operator resolves the underlying conflict,
// per engine.BlockedError's own doc comment ("retrying will not help") — fell through to the
// success path and scheduled another tick, repeating the identical blocked observation and
// state save every poll interval forever. This proves polling stops once the active status is
// Blocked, exactly like a non-retryable StepError already does, and that a stray tick
// afterward still does not fire another drive call.
func TestDriveResultBlockedStopsPolling(t *testing.T) {
	drv := &stubDrive{statuses: []engine.StepStatus{
		{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}},
		{Step: engine.StepCommitted, Observation: engine.Observation{Satisfied: true}},
		{Step: engine.StepPushed, Observation: engine.Observation{
			Blocked: "origin/promo-1 already exists with different content",
		}},
	}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	batch := m.Init()().(tea.BatchMsg)
	m, tickCmd := m.Update(batch[1]())
	if m.busy {
		t.Error("busy = true after a blocked drive result")
	}
	if !m.stopped {
		t.Error("stopped = false after a blocked drive result, want true (terminal until an operator resolves it)")
	}
	if !strings.Contains(m.View(), "already exists with different content") {
		t.Errorf("view missing the blocked reason:\n%s", m.View())
	}
	if tickCmd != nil {
		t.Error("a blocked drive result still scheduled another poll")
	}
	if _, cmd := m.Update(tickMsg{}); cmd != nil {
		t.Error("a stray tick after a blocked drive result still fired another drive call")
	}
}

// TestDriveErrorRedactsRegisteredSecret: a registered credential embedded in a DriveFunc
// error must not reach View() verbatim, mirroring plan.Model's TestViewRedactsRegisteredSecrets.
func TestDriveErrorRedactsRegisteredSecret(t *testing.T) {
	const secret = "SEKRIT-FLIGHT-TOKEN"
	redact.Register(secret)
	drv := &stubDrive{err: errors.New("checking CI status: token " + secret + " rejected")}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))

	cmd := m.Init()
	batch := cmd().(tea.BatchMsg)
	m, _ = m.Update(batch[1]())
	if strings.Contains(m.View(), secret) {
		t.Errorf("view leaked the registered secret:\n%s", m.View())
	}
	if !strings.Contains(m.View(), redact.Redacted) {
		t.Error("view should carry the redaction marker")
	}
}

// TestDoneStopsTicking: once DriveFunc reports done, the screen must not schedule another
// tick, and R is a no-op (nothing left to re-observe).
func TestDoneStopsTicking(t *testing.T) {
	drv := &stubDrive{done: true, statuses: []engine.StepStatus{
		{Step: engine.StepMerged, Observation: engine.Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}},
	}}
	m := New(fixtureState(), PollDurations{}, drv.fn())
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))
	m = runInit(t, m)
	if !m.done {
		t.Fatal("done = false after a done drive result")
	}
	if !strings.Contains(m.View(), "promotion complete") {
		t.Errorf("view missing the done status:\n%s", m.View())
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R after done produced a command")
	}
	_, cmd = m.Update(tickMsg{})
	if cmd != nil {
		t.Error("a stray tick after done produced a command")
	}
}

// TestSpinnerStopsWhenNotBusy covers PR #39 review finding #5: the spinner's own tick chain
// must not run forever in the background regardless of whether anything is actually
// animating. Before the fix, Init always started the spinner (even read-only) and every
// spinner.TickMsg unconditionally rescheduled another one (even once done) — a permanent,
// invisible animation loop for as long as the screen stayed open.
func TestSpinnerStopsWhenNotBusy(t *testing.T) {
	t.Run("nil driveFn: Init never starts the spinner", func(t *testing.T) {
		m := New(fixtureState(), PollDurations{}, nil)
		if cmd := m.Init(); cmd != nil {
			t.Errorf("Init on a read-only screen returned a command, want nil (got %#v)", cmd())
		}
	})

	t.Run("busy: spinner.TickMsg reschedules", func(t *testing.T) {
		drv := &stubDrive{}
		m := New(fixtureState(), PollDurations{}, drv.fn())
		if !m.busy {
			t.Fatal("setup: expected busy = true (New sets it for a non-nil driveFn)")
		}
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd == nil {
			t.Error("spinner.TickMsg while busy produced no command")
		}
	})

	t.Run("not busy: spinner.TickMsg does not reschedule", func(t *testing.T) {
		drv := &stubDrive{}
		m := New(fixtureState(), PollDurations{}, drv.fn())
		m.busy = false // simulate the gap between polls, waiting on scheduleTick's timer
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd != nil {
			t.Error("spinner.TickMsg while not busy produced a command")
		}
	})

	t.Run("done: spinner.TickMsg does not reschedule", func(t *testing.T) {
		drv := &stubDrive{done: true, statuses: []engine.StepStatus{
			{Step: engine.StepMerged, Observation: engine.Observation{Satisfied: true, Detail: "merged"}},
		}}
		m := New(fixtureState(), PollDurations{}, drv.fn())
		m = runInit(t, m)
		if !m.done {
			t.Fatal("setup: expected done = true")
		}
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd != nil {
			t.Error("spinner.TickMsg after done produced a command")
		}
	})
}

// TestPollIntervalUsesConfig: scheduleTick's own pollInterval reads the caller-supplied
// PollDurations for CI/Approval rather than a hand-copied literal.
func TestPollIntervalUsesConfig(t *testing.T) {
	poll := PollDurations{CI: 7 * time.Second, Approval: 11 * time.Second}
	cases := []struct {
		phase engine.StepName
		want  time.Duration
	}{
		{engine.StepCIGreen, 7 * time.Second},
		{engine.StepApproved, 11 * time.Second},
		{engine.StepBranched, 2 * time.Second},
		{engine.StepMerged, 2 * time.Second},
	}
	for _, tc := range cases {
		if got := pollInterval(poll, tc.phase); got != tc.want {
			t.Errorf("pollInterval(%s) = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

// TestPollIntervalZeroValueFallsBackToDefault is round-9's regression: PollDurations{} (the zero
// value app.go's current stub actually passes) must fall back to the same 2s default every
// other step already gets, not return a literal 0 — tea.Tick(0, ...) fires immediately/tightly,
// a real CPU-spin risk if a caller ever leaves CI/Approval unset (exactly what the doc comment
// on PollDurations claims happens, but the code didn't actually do until this fix).
func TestPollIntervalZeroValueFallsBackToDefault(t *testing.T) {
	var poll PollDurations
	for _, phase := range []engine.StepName{engine.StepCIGreen, engine.StepApproved} {
		if got := pollInterval(poll, phase); got != 2*time.Second {
			t.Errorf("pollInterval(zero value, %s) = %v, want the 2s default", phase, got)
		}
	}
}

// TestViewFixedSize checks View() at a fixed 100x30 terminal across a few step-states: mid-
// CIGreen waiting, blocked with a reason, and fully done — matching plan/matrix's own
// fixed-size, string-contains test style rather than a golden file (neither of those
// packages' own View tests need one for every state; plan's single golden test covers one
// state exhaustively, this covers several states with targeted assertions instead).
func TestViewFixedSize(t *testing.T) {
	styles := ui.NewStyles(true)

	t.Run("mid CI waiting", func(t *testing.T) {
		m := New(fixtureState(), PollDurations{}, nil)
		m = m.SetSize(100, 30).SetStyles(styles)
		m.rows = DeriveRows(StepOrder, false, []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(engine.StepCommitted, engine.Observation{Satisfied: true}),
			st(engine.StepPushed, engine.Observation{Satisfied: true}),
			st(engine.StepPROpened, engine.Observation{Satisfied: true}),
			st(engine.StepCIGreen, engine.Observation{Waiting: true, Detail: "CI: 2/3 checks complete"}),
		})
		got := m.View()
		for _, want := range []string{"app-staging -> app-production", "abcd1234", "CI: 2/3 checks complete", "o open PR", "R re-observe", "x abort", "l log"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		// Glyph assertions are scoped to the step list itself, not the whole view: the
		// status bar's key hints are joined with "·", the same rune as GlyphNotReached, so
		// checking the full view for that glyph would pass regardless of whether Approved/
		// Merged actually rendered as not-reached.
		list := m.stepList()
		if !strings.Contains(list, GlyphWaiting) {
			t.Errorf("missing waiting glyph:\n%s", list)
		}
		if !strings.Contains(list, GlyphNotReached) {
			t.Errorf("missing not-reached glyph:\n%s", list)
		}
		assertFits(t, got, 100)
	})

	t.Run("blocked", func(t *testing.T) {
		m := New(fixtureState(), PollDurations{}, nil)
		m = m.SetSize(100, 30).SetStyles(styles)
		m.rows = DeriveRows(StepOrder, false, []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(engine.StepCommitted, engine.Observation{Satisfied: true}),
			st(engine.StepPushed, engine.Observation{Satisfied: true}),
			st(engine.StepPROpened, engine.Observation{Satisfied: true}),
			st(engine.StepCIGreen, engine.Observation{Blocked: "2 of 3 checks failed: lint, test"}),
		})
		got := m.View()
		if !strings.Contains(got, "2 of 3 checks failed: lint, test") {
			t.Errorf("missing the blocked reason:\n%s", got)
		}
		if !strings.Contains(m.stepList(), GlyphBlocked) {
			t.Errorf("missing blocked glyph:\n%s", m.stepList())
		}
		assertFits(t, got, 100)
	})

	t.Run("done", func(t *testing.T) {
		m := New(fixtureState(), PollDurations{}, nil)
		m = m.SetSize(100, 30).SetStyles(styles)
		m.done = true
		m.rows = DeriveRows(StepOrder, true, []engine.StepStatus{
			st(engine.StepMerged, engine.Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}),
		})
		got := m.View()
		for _, want := range []string{"promotion complete", "merged as abc123; branch deleted"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		list := m.stepList()
		if strings.Contains(list, GlyphNotReached) || strings.Contains(list, GlyphActive) || strings.Contains(list, GlyphWaiting) || strings.Contains(list, GlyphBlocked) {
			t.Errorf("a done step list still shows a non-done glyph:\n%s", list)
		}
		assertFits(t, got, 100)
	})
}

func assertFits(t *testing.T, view string, width int) {
	t.Helper()
	for i, l := range strings.Split(view, "\n") {
		if w := len([]rune(l)); w > width+8 { // generous slack: no ansi width helper needed here, just a sanity bound
			t.Errorf("line %d is suspiciously wide (%d): %q", i+1, w, l)
		}
	}
}
