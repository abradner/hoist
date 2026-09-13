package flight

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
)

// TestNewBuildingRendersHeaderAndStartingLabel: before any progress line arrives, the screen
// already shows the confirmed plan's own source/target/direct (NewBuilding populates just
// enough of state for headerSection/OrderFor to render correctly, reusing New's rendering
// rather than a parallel path — see NewBuilding's own doc comment) and a spinner with a
// generic "starting" label, never a blank screen.
func TestNewBuildingRendersHeaderAndStartingLabel(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if !m.Building() {
		t.Fatal("Building() = false right after NewBuilding")
	}
	v := m.View()
	if !strings.Contains(v, "app-staging → app-production") {
		t.Errorf("view missing source → target header:\n%s", v)
	}
	if !strings.Contains(v, "starting") {
		t.Errorf("view missing the generic starting label before any progress line:\n%s", v)
	}
}

// TestNewBuildingDirectShowsBadge: the direct badge headerSection already renders for a real
// promotion renders here too, since NewBuilding sets state.Direct from the same bool the
// confirmed plan's own mode carried.
func TestNewBuildingDirectShowsBadge(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("", "app-staging", true, PollDurations{}, ch)
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if v := m.View(); !strings.Contains(v, "direct") {
		t.Errorf("view missing the direct badge:\n%s", v)
	}
}

// TestBuildingSpinnerKeepsTicking: New's own tick-continuation guard (spinner.TickMsg) stops
// the chain once !busy — but NewBuilding sets busy true with driveFn still nil (there is
// nothing to drive yet), so without building's own OR-branch in that guard, the spinner would
// die on its very first tick and the "clearly still doing something" signal this whole PR
// exists for would go dark before the first progress line even arrived.
func TestBuildingSpinnerKeepsTicking(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	_, cmd := m.Update(spinner.TickMsg{})
	if cmd == nil {
		t.Fatal("spinner.TickMsg while building produced no follow-up command — the tick chain died")
	}
}

// TestNewBuildingStreamsProgressLines: a line sent on the channel appears in the log and in
// actionSection's own live label, and listenCmd re-issues itself so the NEXT line is still
// heard — the whole point of defect A/B/C's fix (AGENTS.md-adjacent plan: "show the work").
func TestNewBuildingStreamsProgressLines(t *testing.T) {
	ch := make(chan string, 8)
	ch <- "checking your checkout against origin/main"
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))

	next, cmd := m.Update(progressMsg{line: "checking your checkout against origin/main", ok: true})
	if cmd == nil {
		t.Fatal("progressMsg produced no follow-up command — listenCmd was not re-issued")
	}
	if len(next.buildLog) != 1 {
		t.Fatalf("buildLog = %v, want one entry", next.buildLog)
	}
	v := next.View()
	if !strings.Contains(v, "checking your checkout against origin/main") {
		t.Errorf("view missing the streamed line in the log:\n%s", v)
	}

	// A second line: buildLog accumulates, does not replace.
	ch2 := make(chan string, 8)
	next.progressCh = ch2
	next, cmd = next.Update(progressMsg{line: "claiming app-production", ok: true})
	if cmd == nil {
		t.Fatal("second progressMsg produced no follow-up command")
	}
	if len(next.buildLog) != 2 {
		t.Fatalf("buildLog = %v, want two entries (accumulated, not replaced)", next.buildLog)
	}
	if v := next.View(); !strings.Contains(v, "claiming app-production") {
		t.Errorf("actionSection should show the LAST line received:\n%s", v)
	}
}

// TestProgressChannelCloseStopsListening: the build goroutine closes progressCh when it
// returns (success or failure) — Update must stop re-issuing listenCmd rather than spinning
// on a channel that will only ever return the zero value from here on.
func TestProgressChannelCloseStopsListening(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	next, cmd := m.Update(progressMsg{ok: false})
	if cmd != nil {
		t.Error("progressMsg{ok:false} re-issued a listen command")
	}
	if next.progressCh != nil {
		t.Error("progressCh not cleared on close")
	}
	if next.listenCmd() != nil {
		t.Error("listenCmd should be a no-op once progressCh is nil")
	}
}

// TestAdoptBuiltTransitionsToDriving: once cmd/hoist's StartPromotionFunc actually returns a
// real PromotionState and DriveFunc, AdoptBuilt is what turns this same screen instance into
// a normal, driving one — building clears, buildLog clears (state.History is authoritative
// from here), and the returned command actually drives (proven by running it and checking the
// stub was called, not merely that a non-nil command came back — a command that does nothing
// would pass a nil check just as well).
func TestAdoptBuiltTransitionsToDriving(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	m, _ = m.Update(progressMsg{line: "claiming app-production", ok: true})
	if len(m.buildLog) == 0 {
		t.Fatal("setup: expected a buildLog entry before AdoptBuilt")
	}

	drv := &stubDrive{statuses: []engine.StepStatus{
		{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true, Detail: "worktree present"}},
	}}
	realState := fixtureState()
	next, cmd := m.AdoptBuilt(realState, drv.fn())
	if next.Building() {
		t.Error("Building() still true after AdoptBuilt")
	}
	// buildLog persists across AdoptBuilt, deliberately: the preflight lines it was showing
	// stay visible until a real driveResultMsg actually supersedes them (AdoptBuilt's own doc
	// comment) — clearing here would blank the log for however long the first drive call
	// takes, the exact laggy gap this whole mechanism exists to close.
	if len(next.buildLog) == 0 {
		t.Error("buildLog cleared by AdoptBuilt — the preflight lines it was showing vanished before any drive result arrived to replace them")
	}
	if next.state.ID != realState.ID {
		t.Errorf("state not adopted: ID = %q, want %q", next.state.ID, realState.ID)
	}
	if !next.busy {
		t.Error("busy = false right after AdoptBuilt with a real driveFn")
	}

	// Close the channel before running cmd: it now includes listenCmd (AdoptBuilt keeps the
	// listener alive across the transition, its own doc comment) — an empty, unclosed test
	// channel would block that sub-command forever inside runBatch. Closing it here is exactly
	// what a real build goroutine's own return does NOT do anymore (the panic this method
	// exists to have stopped causing) — but ending the flow here is a legitimate way for this
	// test's own fake to say "nothing more is coming."
	close(ch)
	next = runBatch(t, next, cmd)
	if drv.calls != 1 {
		t.Errorf("driveFn called %d times via AdoptBuilt's own command, want 1", drv.calls)
	}
	if drv.lastSeen.ID != realState.ID {
		t.Errorf("driveFn was called with the wrong state: ID = %q, want %q", drv.lastSeen.ID, realState.ID)
	}

	// The drive result that runBatch just delivered (driveResultMsg, from the driveCmd
	// sub-command) is what actually clears buildLog — onDriveResult's own job, not AdoptBuilt's.
	if len(next.buildLog) != 0 {
		t.Errorf("buildLog = %v after a real drive result landed, want cleared — state.History is authoritative from here", next.buildLog)
	}
}

// TestAdoptBuiltWithNilDriveFunc: mirrors New's own nil-driveFn convention — AdoptBuilt must
// not panic or claim to be driving something it was handed no way to drive.
func TestAdoptBuiltWithNilDriveFunc(t *testing.T) {
	ch := make(chan string, 8)
	m := NewBuilding("app-staging", "app-production", false, PollDurations{}, ch)
	next, cmd := m.AdoptBuilt(fixtureState(), nil)
	if next.busy {
		t.Error("busy = true after AdoptBuilt with a nil driveFn")
	}
	// cmd is listenCmd, not nil: the listener outlives AdoptBuilt regardless of whether a
	// real driveFn came with it (AdoptBuilt's own doc comment) — there is still a screen on
	// screen that could still receive a straggling progress line.
	if cmd == nil {
		t.Error("AdoptBuilt with a nil driveFn produced no command — the listener should still be running")
	}
	close(ch)
	if msg := cmd(); msg != (progressMsg{ok: false}) {
		t.Errorf("cmd() = %#v, want progressMsg{ok: false} from the now-closed channel", msg)
	}
}
