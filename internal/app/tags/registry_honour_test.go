// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// ScrTags and ScrTagsReader rows and presses every listed key through a real tea.KeyPressMsg
// (uitest.KeyFor), asserting the screen actually reacts — either the rendered View changes or a
// tea.Cmd comes back non-nil. Quit, Help, Log, ctrl+c are skipped: AGENTS.md §4.8's root
// intercepts them generically before this screen's own Update ever sees the keypress.
package tags

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

	t.Run("ScrTags", func(t *testing.T) {
		for _, e := range keys.On(keys.ScrTags) {
			e := e
			switch e.Name {
			case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
				continue
			}
			t.Run(e.Name, func(t *testing.T) {
				m := historyModel(t, fourteenAhead, liveAge34Days).SetSize(80, 24)
				switch e.Name {
				case keys.Up.Name:
					// The cursor opens on the first row; move down first so up has somewhere
					// to move back to.
					m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
				case keys.Home.Name:
					m, _ = m.Update(uitest.KeyFor(keys.End.Show))
				}
				if !press(t, m, e.Binding) {
					t.Errorf("ScrTags key %q (%s) produced no visible change and no command", e.Show, e.Name)
				}
			})
		}
	})

	t.Run("ScrTagsReader", func(t *testing.T) {
		open := func(t *testing.T) Model {
			t.Helper()
			m := historyModelOver(t, unsplitRepo(), longBody, liveAge34Days).SetSize(80, 24)
			m = uitest.Keys(m, updateFn, "tab", "right")
			if !m.reading {
				t.Fatal("setup: tab, → did not open the commit reader")
			}
			return m
		}
		for _, e := range keys.On(keys.ScrTagsReader) {
			e := e
			switch e.Name {
			case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
				continue
			}
			t.Run(e.Name, func(t *testing.T) {
				m := open(t)
				switch e.Name {
				case keys.PgUp.Name, keys.Home.Name:
					// longBody's own body is long enough to page (TestReadingScrollsALongBody);
					// scroll to the end first so the reverse jump has somewhere to go back to.
					m, _ = m.Update(uitest.KeyFor(keys.End.Show))
				case keys.Up.Name:
					// "switch commit" — at the first commit already, so move to the next one
					// first so up has somewhere to move back to.
					m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
				}
				if !press(t, m, e.Binding) {
					t.Errorf("ScrTagsReader key %q (%s) produced no visible change and no command", e.Show, e.Name)
				}
			})
		}
	})
}
