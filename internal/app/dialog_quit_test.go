package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
)

// quits reports whether cmd yields tea.QuitMsg — the root's own answer to q.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// pressRoot types one key through the root's Update, the way the runtime would.
func pressRoot(m tea.Model, k string) (tea.Model, tea.Cmd) {
	return m.Update(uitest.Key(k))
}

// TestQuitKeyWhileFlightOverrideDialogIsOpenDoesNotQuit is the composition the flight
// screen's own CapturesText test cannot prove: with the flight screen on top, blocked on
// ci.none and its c dialog up, q through the ROOT is handed to the dialog rather than
// treated as the global quit — an operator mid-decision cannot quit the program by
// mistake. Positive control: the same q, with no dialog up, quits.
func TestQuitKeyWhileFlightOverrideDialogIsOpenDoesNotQuit(t *testing.T) {
	drv := &recordingDrive{blocked: ciNoneBlocked(t, "abcd1234")}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, drv.fn())}
	root = root.push(fs)
	tm := landFirstDrive(t, root, fs.Init())
	if !strings.Contains(plain(tm), "c treat as green") {
		t.Fatalf("setup: the flight screen should offer c:\n%s", plain(tm))
	}
	if _, cmd := pressRoot(tm, "q"); !quits(cmd) {
		t.Fatal("positive control: q with no dialog open must quit")
	}

	tm, _ = pressRoot(tm, "c")
	if !strings.Contains(plain(tm), "treat no checks as green") {
		t.Fatalf("c did not open the flight dialog:\n%s", plain(tm))
	}
	tm, cmd := pressRoot(tm, "q")
	if quits(cmd) {
		t.Fatal("q quit the program while the flight screen's c dialog was open")
	}
	if !tm.(Model).capturesText() {
		t.Error("the flight dialog closed on q")
	}
	if got := plain(tm); !strings.Contains(got, "treat no checks as green") {
		t.Errorf("the dialog should still be drawn after q:\n%s", got)
	}
}

// TestQuitKeyWhilePlanOverrideDialogIsOpenDoesNotQuit: the same composition for the plan
// screen's o dialog, a text input that takes q as a character. Positive control: q on the
// ready plan screen with no dialog up quits.
func TestQuitKeyWhilePlanOverrideDialogIsOpenDoesNotQuit(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	pm := plan.New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, nil, history.Funcs{})
	pm = uitest.Drain(pm, pm.Init(), plan.Model.Update)
	root := sized(t).(Model).push(planScreen{pm})
	var tm tea.Model = root
	if _, cmd := pressRoot(tm, "q"); !quits(cmd) {
		t.Fatal("positive control: q on the plan screen with no dialog open must quit")
	}

	tm, _ = pressRoot(tm, "o")
	if !strings.Contains(plain(tm), "override the digest for") {
		t.Fatalf("o did not open the plan override dialog:\n%s", plain(tm))
	}
	tm, cmd := pressRoot(tm, "q")
	if quits(cmd) {
		t.Fatal("q quit the program while the plan screen's o dialog was open")
	}
	if !tm.(Model).capturesText() {
		t.Error("the plan override dialog closed on q")
	}
	if got := plain(tm); !strings.Contains(got, "override the digest for") {
		t.Errorf("the dialog should still be drawn after q:\n%s", got)
	}
}
