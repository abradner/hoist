package flight

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// TestStepOrderMatchesAllSteps guards StepOrder (a literal, see rows.go's doc comment)
// against drifting from engine.AllSteps' own order.
func TestStepOrderMatchesAllSteps(t *testing.T) {
	steps := engine.AllSteps(nil, nil, nil, nil, nil)
	if len(steps) != len(StepOrder) {
		t.Fatalf("engine.AllSteps has %d steps, StepOrder has %d", len(steps), len(StepOrder))
	}
	for i, s := range steps {
		if s.Name() != StepOrder[i] {
			t.Errorf("StepOrder[%d] = %s, want %s (engine.AllSteps' order)", i, StepOrder[i], s.Name())
		}
	}
}

// The same guard for direct mode.
func TestDirectStepOrderMatchesAllDirectSteps(t *testing.T) {
	steps := engine.AllDirectSteps(nil, nil, nil, nil, false, nil)
	if len(steps) != len(DirectStepOrder) {
		t.Fatalf("engine.AllDirectSteps has %d steps, DirectStepOrder has %d", len(steps), len(DirectStepOrder))
	}
	for i, s := range steps {
		if s.Name() != DirectStepOrder[i] {
			t.Errorf("DirectStepOrder[%d] = %s, want %s (engine.AllDirectSteps' order)", i, DirectStepOrder[i], s.Name())
		}
	}
}

// Every step either order renders needs a label, or the row draws blank.
func TestEveryOrderedStepHasALabel(t *testing.T) {
	for _, order := range [][]engine.StepName{StepOrder, DirectStepOrder} {
		for _, name := range order {
			if stepLabels[name] == "" {
				t.Errorf("step %q has no label", name)
			}
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

// stepping builds a Snapshot in the ordinary "a Step is not currently outstanding, between
// polls" shape most tests mirror against — the flight-level equivalent of the old stubDrive's
// answer, now expressed as data instead of a fake object (Train 2 design, D3: this screen never
// calls a Driver at all any more, so there is nothing left to fake).
func stepping(s engine.PromotionState, done bool, statuses []engine.StepStatus) session.Snapshot {
	return session.Snapshot{
		Build: 1, ID: s.ID, Phase: session.Stepping,
		Source: s.SourceEnv, Target: s.TargetEnv, Direct: s.Direct,
		State: s, Statuses: statuses, Done: done,
		// Busy defaults true, matching the old New(...)'s own convention of starting busy the
		// moment a non-nil Driver existed (a Step call is always outstanding right after
		// construction/mirroring); a test that wants the gap between polls sets it false.
		Busy: true,
	}
}

func building(source, target string, direct bool) session.Snapshot {
	return session.Snapshot{Build: 1, Phase: session.Building, Source: source, Target: target, Direct: direct, Busy: true}
}

// A direct promotion must render its own steps, not the PR path's.
func TestFlightRendersDirectOrderForADirectPromotion(t *testing.T) {
	st := fixtureState()
	st.Direct = true
	m := NewAttached(stepping(st, false, nil), PollDurations{})
	var names []engine.StepName
	for _, r := range m.rows {
		names = append(names, r.Step)
	}
	if len(names) != len(DirectStepOrder) {
		t.Fatalf("rendered %d rows, want %d (the direct order)", len(names), len(DirectStepOrder))
	}
	for _, unwanted := range []engine.StepName{engine.StepPROpened, engine.StepCIGreen, engine.StepApproved, engine.StepMerged} {
		for _, got := range names {
			if got == unwanted {
				t.Errorf("direct promotion renders %q, a step it never runs", unwanted)
			}
		}
	}
}

// TestFlightNeverCallsDriver pins Train 2 design D3: this package imports no Driver-shaped
// interface and issues no tea.Cmd of its own that could call one — Init and Update only ever
// return the spinner's own tick chain or a plain request message (ReobserveMsg and friends).
// Enforced structurally too (the PR's acceptance grep), this proves it behaviourally: driving a
// screen entirely built and mirrored from data, never touched by anything Driver-shaped, through
// its whole lifecycle produces no panic and no unexpected command.
func TestFlightNeverCallsDriver(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init while building produced no command (the spinner should still tick)")
	}
	msg := cmd()
	if _, ok := msg.(spinner.TickMsg); !ok {
		t.Fatalf("Init's command yielded %T, want spinner.TickMsg", msg)
	}
	snap := stepping(fixtureState(), false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
	})
	snap.Busy = false
	m = m.Mirror(snap)
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd == nil {
		t.Fatal("R produced no command")
	}
	if _, ok := cmd().(ReobserveMsg); !ok {
		t.Fatalf("R's command = %T, want ReobserveMsg — the screen must only ASK, never drive", cmd())
	}
}

// TestMirrorIgnoresOtherIDs is D3's own attacker case: a Snapshot for a different promotion must
// never be silently rendered — the root's own mirrorAttached (app.go) is the real guard (it only
// ever calls Mirror with a matching Build), so this proves what happens if it didn't: Mirror
// itself has no memory of "my own id" to compare against, and the caller is who must not mix
// them up. This is documented, not "fixed" here twice (AGENTS.md §8's deletion test) — the
// routing decision belongs at the one place it is made.
func TestMirrorIgnoresOtherIDs(t *testing.T) {
	a := NewAttached(stepping(fixtureState(), false, nil), PollDurations{})
	other := engine.PromotionState{ID: "other-promotion", SourceEnv: "x", TargetEnv: "y"}
	// The root never does this (mirrorAttached only mirrors a Build match) — proving Mirror
	// itself renders whatever it's given confirms the guard has to live at the call site, not
	// here, which is exactly what app.go's mirrorAttached is.
	b := a.Mirror(stepping(other, false, nil))
	if b.id != "other-promotion" {
		t.Fatalf("Mirror rendered %q; a caller that checked Build first would never reach this state", b.id)
	}
}

// TestNilDriveFuncNeverTicks is NewAttached's read-only-shaped case: nothing busy or building,
// so Init returns nil and the spinner never starts.
func TestNilDriveFuncNeverTicks(t *testing.T) {
	snap := stepping(fixtureState(), false, nil)
	snap.Busy = false
	m := NewAttached(snap, PollDurations{})
	if cmd := m.Init(); cmd != nil {
		t.Errorf("Init produced a command with nothing busy or building: %#v", cmd())
	}
	for _, r := range m.rows {
		if r.Glyph != GlyphNotReached {
			t.Errorf("row %+v, want not-reached (nothing observed yet)", r)
		}
	}
}

// TestReobserveEmitsRequest: R fires ReobserveMsg immediately, without any local wait — the
// controller's own Poke is what decides whether it can actually happen.
func TestReobserveEmitsRequest(t *testing.T) {
	snap := stepping(fixtureState(), false, []engine.StepStatus{st(engine.StepBranched, engine.Observation{Satisfied: true})})
	snap.Busy = false
	m := NewAttached(snap, PollDurations{})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd == nil {
		t.Fatal("R produced no command")
	}
	msg, ok := cmd().(ReobserveMsg)
	if !ok || msg.ID != "abcd1234" {
		t.Fatalf("R's command = %#v, want ReobserveMsg{ID: abcd1234}", cmd())
	}
}

// TestReobserveIgnoredWhileBusy: R while Busy (a Step already outstanding) must not emit — the
// same immediate-feedback guard the old busy check gave, now read off the mirrored Snapshot.
func TestReobserveIgnoredWhileBusy(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}) // Busy defaults true in stepping()
	if !m.busy {
		t.Fatal("setup: expected busy = true")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R while busy produced a command")
	}
}

// TestReobserveOnUnattachedScreenShowsNotice: R while still Building (no real id yet) shows a
// notice instead of emitting a request nothing could answer.
func TestReobserveOnUnattachedScreenShowsNotice(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	// height 11: see abandon_test.go's TestAbandonKeyNoticeWhenNotAttached comment — at this
	// file's usual 10 there is one row too little for header+strip+action+notice, and the
	// notice (already a single line) is dropped whole rather than trimmed.
	m = m.SetSize(80, 11)
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R with no real id yet produced a command")
	}
	if !strings.Contains(m.View(), "nothing to re-observe") {
		t.Errorf("view missing the read-only notice:\n%s", m.View())
	}
}

// TestOpenPRKey: o emits OpenPRMsg naming s.PR's URL once one has been observed; before
// that, it shows a notice instead.
func TestOpenPRKey(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{})
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

// TestAbortKeyNoticeWhenNotAttached: x is a no-op-with-notice, never emitting AbortMsg, while
// still Building (no real promotion id yet).
func TestAbortKeyNoticeWhenNotAttached(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd != nil {
		t.Fatal("x produced a command when there is nothing to abort")
	}
	if !strings.Contains(m.View(), "nothing to abort") {
		t.Errorf("view missing the not-driving notice:\n%s", m.View())
	}
}

// TestAbortKeyEmitsWhenAttached: once a real, non-empty promotion id has landed, x emits AbortMsg.
func TestAbortKeyEmitsWhenAttached(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}) // ID: "abcd1234"
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
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Errorf("esc's command = %T, want BackMsg", cmd())
	}
}

// TestLogToggle: the log is visible by default, and l still toggles it off and back on.
func TestLogToggle(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if !strings.Contains(m.View(), "history") || !strings.Contains(m.View(), "acted") {
		t.Fatalf("history not shown by default:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if strings.Contains(m.View(), "acted") {
		t.Error("history still shown after l")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !strings.Contains(m.View(), "history") || !strings.Contains(m.View(), "acted") {
		t.Errorf("history not shown after a second l:\n%s", m.View())
	}
}

// TestMirrorShowsPlumbingError: a Snapshot carrying Err renders it (redacted) as a notice.
func TestMirrorShowsPlumbingError(t *testing.T) {
	snap := stepping(fixtureState(), false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
	})
	snap.Err = &engine.StepError{Step: engine.StepCIGreen, Op: "observe", Err: errors.New("GET check-runs: 404")}
	m := NewAttached(snap, PollDurations{}).SetSize(80, 10).SetStyles(ui.NewStyles(true))
	if !strings.Contains(m.View(), "404") {
		t.Errorf("view missing the plumbing-error notice:\n%s", m.View())
	}
	for _, done := range []engine.StepName{engine.StepBranched} {
		row, ok := indexRows(m.rows)[done]
		if !ok || row.Glyph != GlyphDone {
			t.Errorf("row for %s = %+v (ok=%v), want done", done, row, ok)
		}
	}
}

// TestMirrorRedactsRegisteredSecret: a registered credential embedded in Snapshot.Err must not
// reach View() verbatim, mirroring plan.Model's TestViewRedactsRegisteredSecrets.
func TestMirrorRedactsRegisteredSecret(t *testing.T) {
	const secret = "SEKRIT-FLIGHT-TOKEN"
	redact.Register(secret)
	snap := stepping(fixtureState(), false, nil)
	snap.Err = errors.New("checking CI status: token " + secret + " rejected")
	m := NewAttached(snap, PollDurations{}).SetSize(80, 10).SetStyles(ui.NewStyles(true))
	if strings.Contains(m.View(), secret) {
		t.Errorf("view leaked the registered secret:\n%s", m.View())
	}
	if !strings.Contains(m.View(), redact.Redacted) {
		t.Error("view should carry the redaction marker")
	}
}

// TestDoneShowsCompleteAndStopsOfferingReobserve: a Done snapshot shows "promotion complete" and
// R is a no-op (nothing left to re-observe).
func TestDoneShowsCompleteAndStopsOfferingReobserve(t *testing.T) {
	snap := stepping(fixtureState(), true, []engine.StepStatus{
		st(engine.StepMerged, engine.Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}),
	})
	snap.Busy = false
	m := NewAttached(snap, PollDurations{}).SetSize(80, 10).SetStyles(ui.NewStyles(true))
	if !m.done {
		t.Fatal("done = false after a done snapshot")
	}
	if !strings.Contains(m.View(), "promotion complete") {
		t.Errorf("view missing the done status:\n%s", m.View())
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		t.Error("R after done produced a command")
	}
}

// TestSpinnerStopsWhenNotBusy covers PR #39 review finding #5, re-pointed at the mirrored
// architecture: the spinner's own tick chain must not run forever regardless of whether
// anything is actually animating.
func TestSpinnerStopsWhenNotBusy(t *testing.T) {
	t.Run("not busy, not building: Init never starts the spinner", func(t *testing.T) {
		snap := stepping(fixtureState(), false, nil)
		snap.Busy = false
		m := NewAttached(snap, PollDurations{})
		if cmd := m.Init(); cmd != nil {
			t.Errorf("Init returned a command, want nil (got %#v)", cmd())
		}
	})

	t.Run("busy: spinner.TickMsg reschedules", func(t *testing.T) {
		m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{})
		if !m.busy {
			t.Fatal("setup: expected busy = true")
		}
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd == nil {
			t.Error("spinner.TickMsg while busy produced no command")
		}
	})

	t.Run("not busy: spinner.TickMsg does not reschedule", func(t *testing.T) {
		snap := stepping(fixtureState(), false, nil)
		snap.Busy = false
		m := NewAttached(snap, PollDurations{})
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd != nil {
			t.Error("spinner.TickMsg while not busy produced a command")
		}
	})

	t.Run("done: spinner.TickMsg does not reschedule", func(t *testing.T) {
		snap := stepping(fixtureState(), true, []engine.StepStatus{
			st(engine.StepMerged, engine.Observation{Satisfied: true, Detail: "merged"}),
		})
		snap.Busy = false
		m := NewAttached(snap, PollDurations{})
		if !m.done {
			t.Fatal("setup: expected done = true")
		}
		_, cmd := m.Update(spinner.TickMsg{})
		if cmd != nil {
			t.Error("spinner.TickMsg after done produced a command")
		}
	})
}

// TestViewFixedSize checks View() at a fixed 100x30 terminal across a few step-states: mid-
// CIGreen waiting, blocked with a reason, and fully done.
func TestViewFixedSize(t *testing.T) {
	styles := ui.NewStyles(true)

	t.Run("mid CI waiting", func(t *testing.T) {
		snap := stepping(fixtureState(), false, []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(engine.StepCommitted, engine.Observation{Satisfied: true}),
			st(engine.StepPushed, engine.Observation{Satisfied: true}),
			st(engine.StepPROpened, engine.Observation{Satisfied: true}),
			st(engine.StepCIGreen, engine.Observation{Waiting: true, Detail: "CI: 2/3 checks complete"}),
		})
		snap.Busy = false
		m := NewAttached(snap, PollDurations{})
		m.state.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"} // a PR exists, so o is offered
		m = m.SetSize(100, 30).SetStyles(styles)
		got := m.View()
		for _, want := range []string{"app-staging → app-production", "abcd1234", "CI: 2/3 checks complete", "o open PR", "R re-observe", "x abort", "l log"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
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
		snap := stepping(fixtureState(), false, []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(engine.StepCommitted, engine.Observation{Satisfied: true}),
			st(engine.StepPushed, engine.Observation{Satisfied: true}),
			st(engine.StepPROpened, engine.Observation{Satisfied: true}),
			st(engine.StepCIGreen, engine.Observation{Blocked: "2 of 3 checks failed: lint, test"}),
		})
		snap.Phase = session.Stopped
		snap.Busy = false
		m := NewAttached(snap, PollDurations{}).SetSize(100, 30).SetStyles(styles)
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
		snap := stepping(fixtureState(), true, []engine.StepStatus{
			st(engine.StepMerged, engine.Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}),
		})
		snap.Busy = false
		m := NewAttached(snap, PollDurations{}).SetSize(100, 30).SetStyles(styles)
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
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d wide, over %d: %q", i+1, w, width, l)
		}
	}
}

// TestHeaderNamesADeployAsADeploy: a deploy state has no source env, so the promotion header's
// "A -> B" rendered as "hoist promote:  -> app-production" — a hole where the source belongs.
func TestHeaderNamesADeployAsADeploy(t *testing.T) {
	s := fixtureState()
	s.SourceEnv = ""
	m := NewAttached(stepping(s, false, nil), PollDurations{}).SetSize(120, 20).SetStyles(ui.NewStyles(true))
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "hoist · deploy · in flight") || !strings.Contains(v, "deploy → "+s.TargetEnv) {
		t.Errorf("the header should name the deploy and its env, with no source:\n%s", v)
	}

	// A promotion keeps both, so this is a discrimination and not a flattening.
	p := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(120, 20).SetStyles(ui.NewStyles(true))
	if pv := ansi.Strip(p.View()); !strings.Contains(pv, "hoist · promotion · in flight") || !strings.Contains(pv, "app-staging → app-production") {
		t.Errorf("a promotion still moves between two envs:\n%s", pv)
	}
}

// o is offered only when there is a PR to open.
func TestHintOffersOpenPROnlyWithAPR(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if v := ansi.Strip(m.View()); strings.Contains(v, "o open PR") {
		t.Fatalf("no PR yet, but the hint offers o:\n%s", v)
	}
	m.state.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "o open PR") {
		t.Fatalf("a PR exists, but the hint does not offer o:\n%s", v)
	}
}

// The log is a viewport that the arrow keys scroll.
func TestLogScrollsByKeypress(t *testing.T) {
	s := fixtureState()
	for i := 0; i < 60; i++ {
		s.History = append(s.History, engine.HistoryEntry{Step: engine.StepBranched, Detail: fmt.Sprintf("entry %d", i)})
	}
	m := NewAttached(stepping(s, false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	if !m.showLog || m.log.YOffset() != 0 {
		t.Fatalf("log visible by default: showLog=%v offset=%d", m.showLog, m.log.YOffset())
	}
	m = uitest.Keys(m, updateFn, "down", "down", "down")
	if m.log.YOffset() != 3 {
		t.Fatalf("three downs scrolled to %d; want 3", m.log.YOffset())
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "entry 3") {
		t.Fatalf("view does not show the scrolled log:\n%s", v)
	}
}

func updateFn(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) }
