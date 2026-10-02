// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrFlight row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress.
package flight

import (
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/internal/ui/uitest"
)

// manyHistoryState is fixtureState with enough History entries that the log viewport actually
// overflows a short terminal — logView()'s content comes straight from state.History, and
// layout() rebuilds it on every render (unlike a plain viewport, it cannot be hand-set once).
func manyHistoryState() engine.PromotionState {
	s := fixtureState()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		s.History = append(s.History, engine.HistoryEntry{
			Step: engine.StepBranched, At: base.Add(time.Duration(i) * time.Minute), Detail: fmt.Sprintf("entry %02d", i),
		})
	}
	return s
}

// notBusy is blockedOnCINone's own recipe, generalised: a screen with a real id, a real PR, not
// busy and not done, so refresh/open/abandon/CINone's own guards all pass and every entry gets
// tested against its real command path rather than an early "nothing to do yet" notice.
func notBusy(t *testing.T, s engine.PromotionState) Model {
	t.Helper()
	snap := stepping(s, false, ciNoneStatuses(t, s.ID))
	snap.Phase = session.Stopped
	snap.Busy = false
	return NewAttached(snap, PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
}

func TestRegistryKeysAreHonoured(t *testing.T) {
	press := func(t *testing.T, m Model, b keys.Binding) bool {
		t.Helper()
		before := ansi.Strip(m.View())
		nm, cmd := m.Update(uitest.KeyFor(b.Show))
		after := ansi.Strip(nm.View())
		return after != before || cmd != nil
	}

	for _, e := range keys.On(keys.ScrFlight) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			var m Model
			switch e.Name {
			case keys.Watch.Name:
				// "watch family/target" needs a real family to watch (families() reads
				// state.Edits — TestWEmitsWatchMsg's own setup).
				m = notBusy(t, fixtureState())
				m = withEdits(m, m.state.TargetEnv, "web")
			case keys.PgUp.Name, keys.PgDn.Name, keys.Up.Name, keys.Down.Name, keys.Home.Name, keys.End.Name:
				// Tall enough that the history section keeps a few rows rather than
				// collapsing to "…" entirely (AGENTS.md §9 entry 10's own degrade-by-height).
				m = notBusy(t, manyHistoryState()).SetSize(80, 18)
				// The log opens on its newest line, so the keys that scroll DOWN start from the top.
				switch e.Name {
				case keys.PgDn.Name, keys.Down.Name, keys.End.Name:
					m, _ = m.Update(uitest.KeyFor(keys.Home.Show))
				case keys.PgUp.Name, keys.Home.Name:
					m, _ = m.Update(uitest.KeyFor(keys.End.Show))
				case keys.Up.Name:
					m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
				}
			default:
				m = notBusy(t, fixtureState())
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrFlight key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
