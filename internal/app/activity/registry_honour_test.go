// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrActivity row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress (this screen has no `l`
// row of its own — the activity log does not open itself).
package activity

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

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

	for _, e := range keys.On(keys.ScrActivity) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			m := newFixture().SetSize(30, 6)
			if e.Name == keys.PgUp.Name || e.Name == keys.Home.Name {
				m, _ = m.Update(uitest.KeyFor(keys.End.Show))
			}
			if e.Name == keys.Up.Name {
				m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrActivity key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
