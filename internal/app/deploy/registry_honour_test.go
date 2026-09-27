// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrDeploy row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress.
package deploy

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

	for _, e := range keys.On(keys.ScrDeploy) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			m := unsizedFixture(t, config.EnvsConfig{})
			switch e.Name {
			case keys.PgUp.Name, keys.Home.Name:
				// A short terminal so the commit list actually overflows, and scrolled to the
				// bottom first so the reverse jump has somewhere to go back to.
				m = m.SetSize(80, 10)
				m, _ = m.Update(uitest.KeyFor(keys.End.Show))
			case keys.Up.Name:
				m = m.SetSize(80, 10)
				m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
			case keys.PgDn.Name, keys.Down.Name, keys.End.Name:
				m = m.SetSize(80, 10)
			default:
				m = m.SetSize(120, 40)
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrDeploy key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
