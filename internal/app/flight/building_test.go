package flight

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/ui"
)

// TestBuildingRendersHeaderAndStartingLabel: before any progress line arrives, the screen
// already shows the confirmed plan's own source/target/direct (a Building Snapshot's
// Source/Target/Direct, mirrored the same way NewBuilding's old stub state was) and a spinner
// with a generic "starting" label, never a blank screen.
func TestBuildingRendersHeaderAndStartingLabel(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if !m.building {
		t.Fatal("building = false right after NewAttached with a Building snapshot")
	}
	v := m.View()
	if !strings.Contains(v, "app-staging → app-production") {
		t.Errorf("view missing source → target header:\n%s", v)
	}
	if !strings.Contains(v, "starting") {
		t.Errorf("view missing the generic starting label before any progress line:\n%s", v)
	}
}

// TestBuildingDirectShowsBadge: the direct badge headerSection already renders for a real
// promotion renders here too, since the Building snapshot carries the same Direct bool the
// confirmed plan's own mode carried.
func TestBuildingDirectShowsBadge(t *testing.T) {
	m := NewAttached(building("", "app-staging", true), PollDurations{})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	if v := m.View(); !strings.Contains(v, "direct") {
		t.Errorf("view missing the direct badge:\n%s", v)
	}
}

// TestBuildingSpinnerKeepsTicking: a Building snapshot has Busy true with nothing yet to poll —
// without building's own OR-branch in the spinner guard, the spinner would die on its very first
// tick and the "clearly still doing something" signal would go dark before the first progress
// line even arrived.
func TestBuildingSpinnerKeepsTicking(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	_, cmd := m.Update(spinner.TickMsg{})
	if cmd == nil {
		t.Fatal("spinner.TickMsg while building produced no follow-up command — the tick chain died")
	}
}

// TestMirrorStreamsProgressLines: a line the controller has accumulated in Snapshot.Log appears
// in the log and in actionSection's own live label — the mirrored equivalent of the old
// progressCh-driven buildLog.
func TestMirrorStreamsProgressLines(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))

	snap := building("app-staging", "app-production", false)
	snap.Log = []session.LogLine{{Text: "checking your checkout against origin/main"}}
	m = m.Mirror(snap)
	if len(m.buildLog) != 1 {
		t.Fatalf("buildLog = %v, want one entry", m.buildLog)
	}
	v := m.View()
	if !strings.Contains(v, "checking your checkout against origin/main") {
		t.Errorf("view missing the streamed line in the log:\n%s", v)
	}

	snap.Log = append(snap.Log, session.LogLine{Text: "claiming app-production"})
	m = m.Mirror(snap)
	if len(m.buildLog) != 2 {
		t.Fatalf("buildLog = %v, want two entries (accumulated, not replaced)", m.buildLog)
	}
	if v := m.View(); !strings.Contains(v, "claiming app-production") {
		t.Errorf("actionSection should show the LAST line received:\n%s", v)
	}
}

// TestMirrorTransitionsFromBuildingToDriving: once the controller's own onBuilt has landed a
// real PromotionState, the very next Mirror call — no separate "adopt" step — turns this same
// screen instance into a normal, driving one: building clears, and the header/rows reflect the
// real state.
func TestMirrorTransitionsFromBuildingToDriving(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))

	m = m.Mirror(stepping(fixtureState(), false, nil))
	if m.building {
		t.Error("building still true after mirroring a Stepping snapshot")
	}
	if m.id != fixtureState().ID {
		t.Errorf("id not adopted: %q, want %q", m.id, fixtureState().ID)
	}
	if !m.busy {
		t.Error("busy = false right after mirroring an in-flight Stepping snapshot")
	}
}
