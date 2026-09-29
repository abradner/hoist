// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrPlan row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress.
package plan

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/internal/ui/uitest"
)

func TestRegistryKeysAreHonoured(t *testing.T) {
	press := func(t *testing.T, m Model, b keys.Binding) bool {
		t.Helper()
		before := ansi.Strip(m.View())
		nm, cmd := m.Update(uitest.KeyFor(b.Show))
		after := ansi.Strip(nm.View())
		return after != before || cmd != nil
	}

	for _, e := range keys.On(keys.ScrPlan) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			m := readyModel(t, config.EnvsConfig{})
			switch e.Name {
			case keys.Up.Name:
				// The cursor opens on the first row; move down first so up has somewhere to
				// move back to.
				m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
			case keys.PgUp.Name, keys.PgDn.Name, keys.Home.Name, keys.End.Name:
				// These four page/jump the impact viewport, which only has its own case
				// (model.go's updateReady) when focus is on the right pane — reached with tab
				// first (registry: "repos ⇄ impact pane") — and only actually scrolls when the
				// content overflows a short terminal.
				m, _ = m.Update(uitest.KeyFor(keys.Tab.Show))
				if m.focus != focusRight {
					t.Fatal("setup: tab did not move focus to the impact pane")
				}
				m = m.SetSize(80, 10)
				if e.Name == keys.PgUp.Name || e.Name == keys.Home.Name {
					// Already at the top; scroll to the bottom first so the reverse jump has
					// somewhere to go back to.
					m, _ = m.Update(uitest.KeyFor(keys.End.Show))
				}
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrPlan key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
