// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrWatch row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress.
package watch

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

	for _, e := range keys.On(keys.ScrWatch) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			c := &clock{t0}
			// A short terminal so the body overflows, and progressing() has enough workload
			// detail lines to actually scroll.
			m := ready(t, &reader{snaps: []Snapshot{progressing()}}, c, 80, 10)
			if e.Name == keys.PgUp.Name || e.Name == keys.Home.Name {
				m, _ = m.Update(uitest.KeyFor(keys.End.Show))
			}
			if e.Name == keys.Up.Name {
				m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrWatch key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
