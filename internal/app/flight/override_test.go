package flight

import (
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/app/session"
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

// blockedOnCINone builds a screen already mirrored to the ci.none=prompt blocked state — the
// shape a Controller's own onStep(Blocked) would leave an entry in, expressed directly as a
// Snapshot since this package no longer drives anything to reach it.
func blockedOnCINone(t *testing.T) Model {
	t.Helper()
	s := fixtureState()
	s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
	snap := stepping(s, false, ciNoneStatuses(t, s.ID))
	snap.Phase = session.Stopped
	snap.Busy = false
	m := NewAttached(snap, PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	if !m.stopped || !m.offersCINoneOverride() {
		t.Fatalf("fixture precondition: screen should be blocked on the ci.none reason (stopped=%v offer=%v)", m.stopped, m.offersCINoneOverride())
	}
	return m
}

// TestFlightGoldenBlockedOnCINone pins the offer: the blocked section names `c`, the footer
// hints it, at both harness sizes.
func TestFlightGoldenBlockedOnCINone(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	m := blockedOnCINone(t).WithNow(now)
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
	m := blockedOnCINone(t)
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

	n := blockedOnCINone(t)
	n, _ = n.Update(uitest.Key("c"))
	n, _ = n.Update(uitest.Key("n"))
	n2, cmd := n.Update(uitest.Key("enter"))
	if cmd != nil {
		t.Errorf("answering no must emit nothing, got %v", cmd())
	}
	if n2.confirming {
		t.Error("the confirmation should be closed after no")
	}

	e := blockedOnCINone(t)
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
		waiting  bool
	}{
		{"waiting on approval", []engine.StepStatus{
			st(engine.StepCIGreen, engine.Observation{Satisfied: true}),
			st(engine.StepApproved, engine.Observation{Waiting: true, Detail: "no approval comment yet"}),
		}, true},
		{"a failed check", blockStatuses(engine.StepCIGreen, "1 of 4 checks failed: lint"), false},
		{"ci.none=block", blockStatuses(engine.StepCIGreen, "no checks reported after the grace period and ci.none=block; block has no override"), false},
		{"a branch conflict", blockStatuses(engine.StepCommitted, "branch hoist/app-production/abcd1234 already exists with different content"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := stepping(fixtureState(), false, tc.statuses)
			if !tc.waiting {
				snap.Phase = session.Stopped
			} else {
				snap.Phase = session.Waiting
			}
			snap.Busy = false
			m := NewAttached(snap, PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
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
		})
	}
}

// TestOverrideOnADoneScreenNoticeMirrorsRootRefusal: the flight screen itself no longer applies
// the override (session.Controller.OverrideCINone does — Train 2 design, D3), so there is
// nothing left for THIS package to test about a done/busy screen refusing to re-drive; the offer
// simply is not made once the block is gone. This documents that boundary rather than asserting
// nothing.
func TestOverrideOfferGoneOnceCIIsSatisfied(t *testing.T) {
	m := blockedOnCINone(t)
	snap := stepping(fixtureState(), false, []engine.StepStatus{
		st(engine.StepCIGreen, engine.Observation{Satisfied: true, Detail: "overridden"}),
	})
	snap.Busy = false
	m = m.Mirror(snap)
	if m.offersCINoneOverride() {
		t.Error("offer still made after CI is satisfied")
	}
}
