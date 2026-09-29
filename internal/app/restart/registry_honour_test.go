// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrRestart row and presses every listed key through a real tea.KeyPressMsg (uitest.KeyFor),
// asserting the screen actually reacts — either the rendered View changes or a tea.Cmd comes
// back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root intercepts them
// generically before this screen's own Update ever sees the keypress.
package restart

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/internal/ui/uitest"
)

// numberedLines is 40 distinct lines, so a viewport scrolled by one line renders visibly
// different text — a fixture of identical repeated lines (e.g. "line\n" × 40) would scroll
// internally (YOffset moves) while looking pixel-for-pixel the same, a false negative this
// walk's before/after View() comparison cannot tell from an unhonoured key.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	return b.String()
}

func TestRegistryKeysAreHonoured(t *testing.T) {
	press := func(t *testing.T, m Model, b keys.Binding) bool {
		t.Helper()
		before := ansi.Strip(m.View())
		nm, cmd := m.Update(uitest.KeyFor(b.Show))
		after := ansi.Strip(nm.View())
		return after != before || cmd != nil
	}

	for _, e := range keys.On(keys.ScrRestart) {
		e := e
		switch e.Name {
		case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			f := &fakeFuncs{plan: onePlan()}
			m := ready(t, f, false).SetSize(80, 10)
			switch e.Name {
			case keys.PgUp.Name, keys.PgDn.Name, keys.Home.Name, keys.End.Name, keys.Up.Name, keys.Down.Name:
				// TestHomeEndScrollBody's own pattern: enough content for the body to
				// actually overflow at this height.
				m.body.SetContent(numberedLines(40))
			}
			if e.Name == keys.PgUp.Name || e.Name == keys.Home.Name || e.Name == keys.Up.Name {
				m, _ = m.Update(uitest.KeyFor(keys.End.Show))
			}
			if !press(t, m, e.Binding) {
				t.Errorf("ScrRestart key %q (%s) produced no visible change and no command", e.Show, e.Name)
			}
		})
	}
}
