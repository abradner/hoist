// TestRegistryKeysAreHonoured (T3 review, commit 1 of the fixups pass) walks internal/ui/keys'
// registry row for this package's screens (ScrMatrix and ScrMenu) and presses every listed key
// through a real tea.KeyPressMsg (uitest.KeyFor), asserting the screen actually reacts — either
// the rendered View changes or a tea.Cmd comes back non-nil. A row the screen doesn't honour any
// more (or never did) is a registry lie the help overlay and every footer repeat; this is the
// mechanical check that catches one going stale, rather than relying on someone noticing in
// review (AGENTS.md §10 meta-rule 5).
//
// Quit, Help, Log and ctrl+c are skipped for every screen in this walk (and every other
// screen's own copy of this test): AGENTS.md §4.8's root intercepts them generically in
// internal/app/app.go before a screen's own Update ever sees the keypress (`?`/`l`/`q` as keys,
// not *Msgs, and ctrl+c quits immediately from app.go's own top-level switch) — a screen-level
// Update test can only ever see them do nothing, which would not be a regression, just this
// test asking the wrong layer. Every other skip below states its own reason at the point it is
// skipped.
package matrix

import (
	"context"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/image"
)

// rootHandled is Quit/Help/Log/CtrlC — see the package doc above.
func rootHandled(name string) bool {
	switch name {
	case keys.Quit.Name, keys.Help.Name, keys.Log.Name, keys.CtrlC.Name:
		return true
	}
	return false
}

func TestRegistryKeysAreHonoured(t *testing.T) {
	press := func(t *testing.T, m Model, b keys.Binding) (Model, string, bool) {
		t.Helper()
		before := ansi.Strip(m.View())
		nm, cmd := m.Update(uitest.KeyFor(b.Show))
		after := ansi.Strip(nm.View())
		return nm, after, after != before || cmd != nil
	}

	t.Run("ScrMatrix", func(t *testing.T) {
		for _, e := range keys.On(keys.ScrMatrix) {
			e := e
			if rootHandled(e.Name) {
				continue
			}
			t.Run(e.Name, func(t *testing.T) {
				var m Model
				switch e.Name {
				case keys.Esc.Name:
					// Dialog-conditional (registry: "close menu/overlay"): the bare grid has
					// nothing to close, so this needs the menu open first.
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
					m, _ = m.Update(uitest.KeyFor(keys.Enter.Show))
					if !m.menuOpen {
						t.Fatal("setup: enter did not open the menu")
					}
				case keys.Abandon.Name, keys.Tab.Name:
					// Dialog/pane-conditional (registry: "abandon row" / "table/flight pane"):
					// both need a real in-flight pane to focus, which New() alone never has.
					m = withPane(80, 24, parked("5pr6sd333t", "app-staging", "app-production", 12))
					if e.Name == keys.Abandon.Name {
						m, _ = m.Update(uitest.KeyFor(keys.Tab.Show))
						if m.focus != FocusPane {
							t.Fatal("setup: tab did not focus the in-flight pane")
						}
					}
				case keys.Open.Name:
					// "open PR (chooser)": with nothing in flight this only sets a notice
					// (still a real reaction), but a real PR proves the actual command path.
					m = withPane(80, 24, parked("5pr6sd333t", "app-staging", "app-production", 12))
				case keys.Refresh.Name:
					// Re-reading the cluster is a real no-op without a cluster to ask (the
					// product behaviour, not a registry gap) — a drift func makes it real.
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, func(context.Context, string) (map[string][]image.Ref, error) {
						return map[string][]image.Ref{}, nil
					}).SetSize(80, 24)
				case keys.Up.Name:
					// Spatial, at the top-left cell already: move away first so up has
					// somewhere to move back to.
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
					m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
				case keys.Left.Name:
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
					m, _ = m.Update(uitest.KeyFor(keys.Right.Show))
				case keys.PgUp.Name, keys.Home.Name:
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
					m, _ = m.Update(uitest.KeyFor(keys.End.Show))
				default:
					m = New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
				}
				if _, _, ok := press(t, m, e.Binding); !ok {
					t.Errorf("ScrMatrix key %q (%s) produced no visible change and no command", e.Show, e.Name)
				}
			})
		}
	})

	t.Run("ScrMenu", func(t *testing.T) {
		open := func() Model {
			m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
			m, _ = m.Update(uitest.KeyFor(keys.Enter.Show))
			if !m.menuOpen {
				t.Fatal("setup: enter did not open the menu")
			}
			return m
		}
		for _, e := range keys.On(keys.ScrMenu) {
			e := e
			if rootHandled(e.Name) {
				continue
			}
			t.Run(e.Name, func(t *testing.T) {
				m := open()
				if e.Name == keys.Up.Name {
					// The cursor opens at item 0 already; move down first so up has
					// somewhere to move back to.
					m, _ = m.Update(uitest.KeyFor(keys.Down.Show))
				}
				if _, _, ok := press(t, m, e.Binding); !ok {
					t.Errorf("ScrMenu key %q (%s) produced no visible change and no command", e.Show, e.Name)
				}
			})
		}
	})
}
