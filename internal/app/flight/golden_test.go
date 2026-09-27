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

// The flight screen at both harness sizes, in the three states an operator stares at:
// parked on approval (the command to type is on screen), blocked, and done — and the
// direct-mode shape, which renders only the steps a direct promotion runs.
func TestFlightGolden(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	base := func(statuses []engine.StepStatus) Model {
		s := fixtureState()
		s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
		snap := stepping(s, false, statuses)
		snap.Phase = session.Waiting
		snap.Busy = false
		return NewAttached(snap, PollDurations{}).WithNow(now).SetStyles(ui.NewStyles(true))
	}
	parked := base([]engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #103"}),
		st(engine.StepCIGreen, engine.Observation{Satisfied: true, Detail: "4/4 checks green"}),
		st(engine.StepApproved, engine.Observation{Waiting: true, Detail: "no approval comment yet"}),
	})
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		uitest.Golden(t, "flight-approval", parked.SetSize(size[0], size[1]).View(), size[0], size[1])
	}
	// The short terminal: the list degrades to the strip and the command survives.
	uitest.Golden(t, "flight-approval", parked.SetSize(80, 12).View(), 80, 12)

	blockedSnap := func() Model {
		s := fixtureState()
		s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
		snap := stepping(s, false, []engine.StepStatus{
			st(engine.StepBranched, engine.Observation{Satisfied: true}),
			st(engine.StepCommitted, engine.Observation{Blocked: "branch hoist/app-production/abcd1234 already exists with different content"}),
		})
		snap.Phase = session.Stopped
		snap.Busy = false
		return NewAttached(snap, PollDurations{}).WithNow(now).SetStyles(ui.NewStyles(true))
	}
	uitest.Golden(t, "flight-blocked", blockedSnap().SetSize(80, 24).View(), 80, 24)

	doneSnap := func() Model {
		s := fixtureState()
		s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
		snap := stepping(s, true, []engine.StepStatus{st(engine.StepRolledOut, engine.Observation{Satisfied: true, Detail: "2 deployments rolled out"})})
		snap.Busy = false
		return NewAttached(snap, PollDurations{}).WithNow(now).SetStyles(ui.NewStyles(true))
	}
	uitest.Golden(t, "flight-done", doneSnap().SetSize(80, 24).View(), 80, 24)

	s := fixtureState()
	s.Direct, s.SourceEnv = true, ""
	directSnap := stepping(s, false, []engine.StepStatus{
		st(engine.StepDirectGate, engine.Observation{Satisfied: true}),
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepDirectPushed, engine.Observation{Waiting: true, Detail: "pushing to main"}),
	})
	directSnap.Phase = session.Waiting
	directSnap.Busy = false
	direct := NewAttached(directSnap, PollDurations{}).WithNow(now).SetStyles(ui.NewStyles(true))
	v := direct.SetSize(80, 24).View()
	for _, never := range []string{"· PR", "· CI", "· approval", "· merge"} {
		if contains(v, never) {
			t.Errorf("a direct promotion must not render %q:\n%s", never, v)
		}
	}
	uitest.Golden(t, "flight-direct", v, 80, 24)
}

// TestFlightWaitingGolden is Train 2 design PR 8's own answer to the ordinary "nothing wrong,
// just waiting for the next scheduled check" case: before this PR actionSection said nothing at
// all here (Summary.Action's own default branch returns a non-empty Detail with no command, and
// the switch in actionSection had no case for that combination, so it fell through to the final
// `return ""`) — indistinguishable from a hang for however long the poll interval is. The last
// step here is Active and Waiting (on CI, not approval — StepApproved's own special case in
// Summary.Action is deliberately not exercised, since TestFlightGolden's "flight-approval"
// already covers that one), so this is a genuinely different case: no PR-comment command to
// show, just a countdown.
func TestFlightWaitingGolden(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	s := fixtureState()
	s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
	snap := stepping(s, false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #103"}),
		st(engine.StepCIGreen, engine.Observation{Waiting: true, Detail: "2/4 checks reported"}),
	})
	snap.Phase = session.Waiting
	snap.Busy = false
	snap.NextPoll = now().Add(12 * time.Second)
	m := NewAttached(snap, PollDurations{}).WithNow(now).SetStyles(ui.NewStyles(true))
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		uitest.Golden(t, "flight-waiting", m.SetSize(size[0], size[1]).View(), size[0], size[1])
	}
}

// TestActionSectionShowsCountdownWhileWaiting is TestFlightWaitingGolden's non-golden pin: the
// exact text and its rounding to the second, and that it disappears once busy (a poll fired) or
// stopped, so a mutant that always shows it (or never rounds) fails a clear assertion rather
// than only a byte-diff.
func TestActionSectionShowsCountdownWhileWaiting(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	snap := stepping(fixtureState(), false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Waiting: true, Detail: "waiting on the branch"}),
	})
	snap.Busy = false
	snap.NextPoll = now().Add(12500 * time.Millisecond)
	m := NewAttached(snap, PollDurations{}).WithNow(now)
	if got := m.actionSection(); !strings.Contains(got, "next check in 13s") || !strings.Contains(got, "R now") {
		t.Fatalf("actionSection() = %q, want a rounded 13s countdown naming R", got)
	}

	m.busy = true
	if got := m.actionSection(); got != "" {
		t.Errorf("busy must not show the countdown: %q", got)
	}

	m.busy = false
	m.stopped = true
	if got := m.actionSection(); strings.Contains(got, "next check in") {
		t.Errorf("stopped must not show the countdown: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
