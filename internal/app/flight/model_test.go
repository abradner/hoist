package flight

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/config"
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
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
	m := New(fixtureState(), config.PollConfig{Deadline: config.Duration(20 * time.Millisecond)}, hung)
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

// TestNilDriveFuncNeverTicks: a read-only flight screen (driveFn nil, the shape app.go's
// plan.StartMsg handler currently pushes — see app.go's own comment on why) never calls
// anything and never schedules a tick.
func TestNilDriveFuncNeverTicks(t *testing.T) {
	m := New(fixtureState(), config.PollConfig{}, nil)
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
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
	m := New(fixtureState(), config.PollConfig{}, nil)
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
	m := New(fixtureState(), config.PollConfig{}, nil)
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
			m := New(tc.state, config.PollConfig{}, tc.driveFn)
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
	m := New(state, config.PollConfig{}, (&stubDrive{}).fn())
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
	m := New(fixtureState(), config.PollConfig{}, nil)
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
	m := New(fixtureState(), config.PollConfig{}, nil)
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

// TestDriveErrorShowsNoticeAndKeepsPolling: a plumbing error from DriveFunc must not stop
// the screen from scheduling another attempt (mirrors cmd/hoist/drive.go's own retry
// behaviour for a transient Checks/Comments failure).
func TestDriveErrorShowsNoticeAndKeepsPolling(t *testing.T) {
	drv := &stubDrive{err: errors.New("GET check-runs: 404")}
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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

// TestDriveErrorRedactsRegisteredSecret: a registered credential embedded in a DriveFunc
// error must not reach View() verbatim, mirroring plan.Model's TestViewRedactsRegisteredSecrets.
func TestDriveErrorRedactsRegisteredSecret(t *testing.T) {
	const secret = "SEKRIT-FLIGHT-TOKEN"
	redact.Register(secret)
	drv := &stubDrive{err: errors.New("checking CI status: token " + secret + " rejected")}
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
	m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
		m := New(fixtureState(), config.PollConfig{}, nil)
		if cmd := m.Init(); cmd != nil {
			t.Errorf("Init on a read-only screen returned a command, want nil (got %#v)", cmd())
		}
	})

	t.Run("busy: spinner.TickMsg reschedules", func(t *testing.T) {
		drv := &stubDrive{}
		m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
		m := New(fixtureState(), config.PollConfig{}, drv.fn())
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
		m := New(fixtureState(), config.PollConfig{}, drv.fn())
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

// TestPollIntervalUsesConfig: scheduleTick's own pollInterval reads config.PollConfig for
// CI/Approval rather than a hand-copied literal.
func TestPollIntervalUsesConfig(t *testing.T) {
	poll := config.PollConfig{CI: config.Duration(7 * time.Second), Approval: config.Duration(11 * time.Second)}
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

// TestViewFixedSize checks View() at a fixed 100x30 terminal across a few step-states: mid-
// CIGreen waiting, blocked with a reason, and fully done — matching plan/matrix's own
// fixed-size, string-contains test style rather than a golden file (neither of those
// packages' own View tests need one for every state; plan's single golden test covers one
// state exhaustively, this covers several states with targeted assertions instead).
func TestViewFixedSize(t *testing.T) {
	styles := ui.NewStyles(true)

	t.Run("mid CI waiting", func(t *testing.T) {
		m := New(fixtureState(), config.PollConfig{}, nil)
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
		m := New(fixtureState(), config.PollConfig{}, nil)
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
		m := New(fixtureState(), config.PollConfig{}, nil)
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
