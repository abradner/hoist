package flight

import (
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
)

// ciNoneBlocked is the reason CIGreenStep produces under ci.none: prompt, built through the
// engine's own predicate rather than copied here, so this test fails if the wording drifts
// away from what the screen recognises.
func ciNoneBlocked(t *testing.T, id string) string {
	t.Helper()
	// CIGreenStep with a PR older than a zero grace and a forge reporting no checks.
	f := &forge.Fake{}
	st := &engine.PromotionState{ID: id, CINone: "prompt", PR: &forge.PR{Number: 7, CreatedAt: time.Now().Add(-time.Hour)}, PushedSHA: "abc"}
	obs, err := engine.CIGreenStep{Forge: f}.Observe(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" || !engine.IsCINonePromptBlock(obs.Blocked) {
		t.Fatalf("fixture precondition: expected the ci.none=prompt block, got %+v", obs)
	}
	return obs.Blocked
}

func ciNoneStatuses(t *testing.T, id string) []engine.StepStatus {
	t.Helper()
	return []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #103"}),
		st(engine.StepCIGreen, engine.Observation{Blocked: ciNoneBlocked(t, id)}),
	}
}

// blockedOnCINone drives a real DriveFunc result through Update so the screen reaches the
// blocked state the way it does in use, not by poking rows.
func blockedOnCINone(t *testing.T, drv *stubDrive) Model {
	t.Helper()
	s := fixtureState()
	s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
	drv.statuses = ciNoneStatuses(t, s.ID)
	m := New(s, PollDurations{}, drv.fn()).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = uitest.Drain(m, m.Init(), Model.Update)
	if !m.stopped || !m.offersCINoneOverride() {
		t.Fatalf("fixture precondition: screen should be blocked on the ci.none reason (stopped=%v offer=%v)", m.stopped, m.offersCINoneOverride())
	}
	return m
}

// TestFlightGoldenBlockedOnCINone pins the offer: the blocked section names `c`, the footer
// hints it, at both harness sizes.
func TestFlightGoldenBlockedOnCINone(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	m := blockedOnCINone(t, &stubDrive{}).WithNow(now)
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		v := m.SetSize(size[0], size[1]).View()
		for _, want := range []string{"press c to treat no checks as green", "c treat as green"} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d: view missing %q:\n%s", size[0], size[1], want, v)
			}
		}
		uitest.Golden(t, "flight-ci-none", v, size[0], size[1])
	}
	// The dialog itself, drawn over the dimmed screen.
	d := uitest.Keys(m, Model.Update, "c")
	uitest.Golden(t, "flight-ci-none-confirm", d.View(), 80, 24)
}

// TestOverrideGestureCompletesThroughRealInput: c, y, enter emits OverrideCINoneMsg for this
// promotion; c, n, enter emits nothing; c, esc leaves the dialog. Driven through keypresses
// only — nothing here touches confirmValue (AGENTS.md §9 entry 6).
func TestOverrideGestureCompletesThroughRealInput(t *testing.T) {
	m := blockedOnCINone(t, &stubDrive{})
	if m.CapturesText() {
		t.Fatal("positive control: nothing captures text before c")
	}
	m, _ = m.Update(uitest.Key("c"))
	if !m.confirming {
		t.Fatal("c did not open the confirmation")
	}
	if !m.CapturesText() {
		t.Fatal("the open dialog must capture text, or the root's q quits mid-decision")
	}
	m, _ = m.Update(uitest.Key("y"))
	m2, cmd := m.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("y then enter produced no command: the confirmation never saw the keypress")
	}
	got, ok := cmd().(OverrideCINoneMsg)
	if !ok {
		t.Fatalf("y then enter emitted %T, want OverrideCINoneMsg", cmd())
	}
	if got.ID != "abcd1234" {
		t.Errorf("OverrideCINoneMsg.ID = %q, want the screen's own promotion", got.ID)
	}
	if m2.confirming {
		t.Error("the confirmation should be closed after enter")
	}

	n := blockedOnCINone(t, &stubDrive{})
	n, _ = n.Update(uitest.Key("c"))
	n, _ = n.Update(uitest.Key("n"))
	n2, cmd := n.Update(uitest.Key("enter"))
	if cmd != nil {
		t.Errorf("answering no must emit nothing, got %v", cmd())
	}
	if n2.confirming {
		t.Error("the confirmation should be closed after no")
	}

	e := blockedOnCINone(t, &stubDrive{})
	e, _ = e.Update(uitest.Key("c"))
	e, cmd = e.Update(uitest.Key("esc"))
	if cmd != nil || e.confirming {
		t.Errorf("esc should close the dialog silently: cmd=%v confirming=%v", cmd, e.confirming)
	}
}

// TestOverrideKeyDoesNothingForOtherBlocks: c on a screen blocked for any other reason — an
// approval wait, a failed check, ci.none=block, a branch conflict — opens nothing and emits
// nothing, and the offer is not on screen.
func TestOverrideKeyDoesNothingForOtherBlocks(t *testing.T) {
	blockStatuses := func(step engine.StepName, reason string) []engine.StepStatus {
		return []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(step, engine.Observation{Blocked: reason}),
		}
	}
	cases := []struct {
		name     string
		statuses []engine.StepStatus
	}{
		{"waiting on approval", []engine.StepStatus{
			st(engine.StepCIGreen, engine.Observation{Satisfied: true}),
			st(engine.StepApproved, engine.Observation{Waiting: true, Detail: "no approval comment yet"}),
		}},
		{"a failed check", blockStatuses(engine.StepCIGreen, "1 of 4 checks failed: lint")},
		{"ci.none=block", blockStatuses(engine.StepCIGreen, "no checks reported after the grace period and ci.none=block; block has no override")},
		{"a branch conflict", blockStatuses(engine.StepCommitted, "branch hoist/app-production/abcd1234 already exists with different content")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drv := &stubDrive{statuses: tc.statuses}
			m := New(fixtureState(), PollDurations{}, drv.fn()).SetSize(80, 24).SetStyles(ui.NewStyles(true))
			m = runInit(t, m) // not Drain: a waiting result schedules a real tick Drain would follow
			if m.offersCINoneOverride() {
				t.Fatal("the offer must not be made for this block")
			}
			if v := m.View(); strings.Contains(v, "treat as green") {
				t.Errorf("view offers the override:\n%s", v)
			}
			m, cmd := m.Update(uitest.Key("c"))
			if m.confirming || cmd != nil {
				t.Errorf("c opened a dialog (%v) or emitted (%v)", m.confirming, cmd)
			}
			// And the full gesture emits nothing either — a dialog that never opened
			// cannot be confirmed.
			m = uitest.Keys(m, Model.Update, "y")
			if _, cmd := m.Update(uitest.Key("enter")); cmd != nil {
				if _, ok := cmd().(OverrideCINoneMsg); ok {
					t.Error("c y enter emitted OverrideCINoneMsg for a block with no override")
				}
			}
		})
	}
}

// TestApplyCINoneOverrideRedrivesWithTheFlagSet: the state the next DriveFunc call receives
// carries CINoneOverride — the only way the engine step ever learns of the override — and the
// drive resumes from its stopped state.
func TestApplyCINoneOverrideRedrivesWithTheFlagSet(t *testing.T) {
	drv := &stubDrive{}
	m := blockedOnCINone(t, drv)
	if drv.lastSeen.CINoneOverride {
		t.Fatal("fixture precondition: the first drive must not carry the override")
	}
	before := drv.calls
	m, cmd := m.ApplyCINoneOverride()
	if !m.busy || m.stopped {
		t.Errorf("after apply: busy=%v stopped=%v, want busy and not stopped", m.busy, m.stopped)
	}
	// The stub now answers "satisfied" so the screen moves on. runBatch, not Drain: a
	// satisfied-but-not-done result schedules a real tea.Tick, which Drain would sleep on and
	// follow forever.
	drv.statuses = []engine.StepStatus{st(engine.StepCIGreen, engine.Observation{Satisfied: true, Detail: "overridden"})}
	m = runBatch(t, m, cmd)
	if drv.calls != before+1 {
		t.Fatalf("drive calls = %d, want %d (one re-drive)", drv.calls, before+1)
	}
	if !drv.lastSeen.CINoneOverride {
		t.Error("the re-drive's state did not carry CINoneOverride — the engine step would never see it")
	}
	if m.stopped {
		t.Error("still stopped after the re-drive reported CI satisfied")
	}
	// The offer is gone once the block is.
	if m.offersCINoneOverride() {
		t.Error("offer still made after CI is satisfied")
	}
}

// TestApplyCINoneOverrideReadOnly: a screen with nothing driving it records the wish and says
// so, rather than calling a nil DriveFunc.
func TestApplyCINoneOverrideReadOnly(t *testing.T) {
	m := New(fixtureState(), PollDurations{}, nil)
	m, cmd := m.ApplyCINoneOverride()
	if cmd != nil {
		t.Error("a read-only screen must not schedule a drive")
	}
	if !strings.Contains(m.notice, "hoist resume abcd1234 --override-ci-none") {
		t.Errorf("notice should name the CLI command, got %q", m.notice)
	}
	if !m.state.CINoneOverride {
		t.Error("the state should still record the override")
	}
}

// TestApplyCINoneOverrideRefusesOnADoneScreen: a finished promotion cannot be re-driven, so
// the override is not recorded either — recording without a re-drive would leave a flag no
// engine step reads until some later R applied it without the c gesture. The screen says so
// and schedules nothing. The blocked screen from blockedOnCINone is the positive control.
func TestApplyCINoneOverrideRefusesOnADoneScreen(t *testing.T) {
	drv := &stubDrive{done: true, statuses: []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepRolledOut, engine.Observation{Satisfied: true}),
	}}
	m := New(fixtureState(), PollDurations{}, drv.fn()).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = runInit(t, m)
	if !m.done {
		t.Fatal("fixture precondition: the screen should be done")
	}
	m, cmd := m.ApplyCINoneOverride()
	if cmd != nil {
		t.Error("a done screen must not schedule a drive")
	}
	if m.state.CINoneOverride {
		t.Error("a done screen recorded CINoneOverride with nothing to re-drive")
	}
	if !strings.Contains(m.notice, "not applied") {
		t.Errorf("notice should say the override was not applied, got %q", m.notice)
	}

	ctl, cmd := blockedOnCINone(t, &stubDrive{}).ApplyCINoneOverride()
	if cmd == nil || !ctl.state.CINoneOverride {
		t.Errorf("positive control: a blocked screen must record and re-drive (cmd=%v flag=%v)", cmd != nil, ctl.state.CINoneOverride)
	}
}
