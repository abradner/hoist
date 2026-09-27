package flight

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
)

// TestAbandonKeyNoticeWhenNotAttached mirrors TestAbortKeyNoticeWhenNotAttached: X is a
// no-op-with-notice, never opening the confirm dialog, while still Building (no real id yet).
func TestAbandonKeyNoticeWhenNotAttached(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{})
	// height 11, not this file's usual 10: at 10, header+strip+action+notice (4 sections) is
	// one row over room, and the notice — already a single line, so ui.Frame's backward walk
	// (internal/ui/frame.go) cannot trim it to a shorter marker — is dropped whole rather than
	// left as a bare "…" that would not have closed the gap by itself. One more row removes
	// the overflow entirely, matching internal/app/flight/model_test.go's own precedent for
	// this exact interaction (TestDriveErrorOnNonRetryableStepStopsPolling's history).
	m = m.SetSize(80, 11).SetStyles(ui.NewStyles(true))
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if cmd != nil {
		t.Fatal("X produced a command when there is nothing to abandon")
	}
	if m.confirmingAbandon {
		t.Fatal("X opened the confirm dialog when there is nothing to abandon")
	}
	if !strings.Contains(m.View(), "nothing to abandon") {
		t.Errorf("view missing the not-driving notice:\n%s", m.View())
	}
}

// TestAbandonKeyRefusedOnADoneScreen: a finished promotion cannot be abandoned — abandoning is
// not a rollback — so X refuses before ever opening the dialog.
func TestAbandonKeyRefusedOnADoneScreen(t *testing.T) {
	snap := stepping(fixtureState(), true, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepRolledOut, engine.Observation{Satisfied: true}),
	})
	snap.Busy = false
	m := NewAttached(snap, PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	if !m.done {
		t.Fatal("fixture precondition: the screen should be done")
	}
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if cmd != nil {
		t.Fatal("X produced a command on a done screen")
	}
	if m.confirmingAbandon {
		t.Fatal("X opened the confirm dialog on a done screen")
	}
	if !strings.Contains(m.View(), "already landed") {
		t.Errorf("view missing the already-landed notice:\n%s", m.View())
	}
}

// TestAbandonGestureCompletesThroughRealInput is the regression shape AGENTS.md §9 entry 6
// exists for: huh.NewConfirm ships a zero keymap and a standalone field's answer must be read
// back through GetValue, never a bool the widget's own Update happens to touch — driven only
// through real keypresses, never by setting m.confirmAbandonValue directly.
func TestAbandonGestureCompletesThroughRealInput(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m, _ = m.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if !m.confirmingAbandon {
		t.Fatal("X did not open the confirmation")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m2, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("y then enter produced no command: the confirmation never saw the keypress")
	}
	msg, ok := cmd().(AbandonMsg)
	if !ok || msg.ID != "abcd1234" {
		t.Fatalf("y then enter emitted %#v, want AbandonMsg{ID: abcd1234}", cmd())
	}
	if m2.confirmingAbandon {
		t.Error("the confirmation should be closed after enter")
	}

	// The asymmetry that makes the above mean something: answering no must emit nothing.
	n := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	n, _ = n.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	n, _ = n.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if _, cmd := n.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Errorf("answering no must not abandon anything, got %v", cmd())
	}
}

// TestAbandonEscClosesDialogWithoutEmitting mirrors the tag picker/CI-none dialog's own
// round-3 fix: Esc leaves the dialog without answering it, rather than falling into huh's own
// widget update (which swallows Esc).
func TestAbandonEscClosesDialogWithoutEmitting(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m, _ = m.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if !m.confirmingAbandon {
		t.Fatal("fixture precondition: X should open the confirm dialog")
	}
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil {
		t.Errorf("esc should emit nothing, got %v", cmd())
	}
	if m.confirmingAbandon {
		t.Error("confirmingAbandon should be cleared once Esc is handled")
	}
}
