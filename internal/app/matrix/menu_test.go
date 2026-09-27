package matrix

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
)

// TestEnterOpensMenu proves enter, on the grid, opens the action menu for the cell under the
// cursor rather than doing anything itself — the retired direct-resume gesture's replacement.
func TestEnterOpensMenu(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	m = uitest.Keys(m, update, "enter")
	if !m.menuOpen {
		t.Fatal("enter did not open the action menu")
	}
	if len(m.menuItems) == 0 {
		t.Fatal("the menu has no items")
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "promote into") {
		t.Errorf("menu view lacks the promote item:\n%s", v)
	}
}

// TestMenuLetterRunsDirectly proves a letter matching one of the listed items runs it without
// moving the menu's own cursor there first (v2·02: "the letter runs it directly").
func TestMenuLetterRunsDirectly(t *testing.T) {
	m := uitest.Keys(newFixture().SetSize(80, 24), update, "enter")
	if !m.menuOpen {
		t.Fatal("setup: enter did not open the menu")
	}
	msg, ok := emitted(t, m, "w").(OpenWatchMsg)
	if !ok {
		t.Fatalf("w in the menu emitted %#v, want OpenWatchMsg", msg)
	}
}

// TestMenuResumeInFlightItemIsReachable is P2-9 from the T3 review: MenuFor built the
// "resume in flight" row as a bare MenuItem{} with no Enabled field set, so runMenuItem's own
// "if !item.Enabled { return m, nil }" guard silently dropped it on every keypress — a probe
// navigating to the item and pressing enter produced no command at all. Now it must actually
// resume.
func TestMenuResumeInFlightItemIsReachable(t *testing.T) {
	p := parked("5pr6sd333t", "b", "a", 12) // targets env "a", the fixture's first (leftmost) column
	m := withPane(80, 24, p)
	if m.CurrentEnv() != "a" {
		t.Fatalf("test setup: cursor column = %q, want the fixture's first env \"a\"", m.CurrentEnv())
	}
	m = uitest.Keys(m, update, "enter") // open the menu for (family, "a")
	if !m.menuOpen {
		t.Fatal("setup: enter did not open the menu")
	}
	found := false
	for i, item := range m.menuItems {
		if strings.Contains(item.Label, "resume in flight") {
			found = true
			if !item.Enabled {
				t.Fatalf("menu item %d (%q) is not Enabled", i, item.Label)
			}
			m.menuCursor = i
		}
	}
	if !found {
		t.Fatal("setup: no \"resume in flight\" item in the menu")
	}
	m2, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the resume item produced no command")
	}
	if _, ok := cmd().(ResumeMsg); !ok {
		t.Fatalf("enter on the resume item emitted %T, want ResumeMsg", cmd())
	}
	if m2.menuOpen {
		t.Error("menu still open after running the item")
	}
}

// TestMenuEscClosesOnly proves esc closes the menu and emits nothing else.
func TestMenuEscClosesOnly(t *testing.T) {
	m := uitest.Keys(newFixture().SetSize(80, 24), update, "enter")
	if !m.menuOpen {
		t.Fatal("setup: enter did not open the menu")
	}
	m, cmd := m.Update(uitest.Key("esc"))
	if cmd != nil {
		t.Fatalf("esc in the menu emitted %v, want nothing", cmd())
	}
	if m.menuOpen {
		t.Fatal("esc did not close the menu")
	}
}

// TestRetiredKeysUnbound proves P, d, C and R (bare, on their own — h is covered by
// TestMatrixHDoesNotMoveColumns) no longer do what they used to (T3-04's own retirement list).
// R alone still restarts, as a legacy shift; P, d and (bare) C must not.
func TestRetiredKeysUnbound(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	if got := emitted(t, m, "P"); got != nil {
		t.Fatalf("P emitted %+v, want nothing (retired)", got)
	}
	if got := emitted(t, m, "d"); got != nil {
		t.Fatalf("d emitted %+v, want nothing (retired: use t)", got)
	}
	if got := emitted(t, m, "C"); got != nil {
		t.Fatalf("capital C emitted %+v, want nothing (retired: use c)", got)
	}
}

// TestCursorCellStyled proves the cell cursor is drawn with the shared Cursor style at exactly
// the (row, col) intersection — the one thing about it that only shows as colour, since
// goldens are ANSI-stripped (train3-design.md's own "Facts" section) and so cannot catch a
// regression here on their own.
func TestCursorCellStyled(t *testing.T) {
	m := newFixture().SetStyles(ui.NewStyles(true)).SetSize(80, 24)
	view := m.View()
	widths := m.gridWidths()
	disp := m.displayTable()
	// Default cursor: row 0, col 0. The styled span is the cell's own formatted text padded to
	// its column width — the cellPad margin on either side sits OUTSIDE the styled run
	// (dataRow's own layout in grid.go), so it is not part of what this checks.
	cellText := disp.Rows[0].Cells[0].Text
	pad := widths[1] - len([]rune(cellText))
	if pad < 0 {
		pad = 0
	}
	want := m.styles.Cursor.Render(cellText + strings.Repeat(" ", pad))
	if !strings.Contains(view, want) {
		t.Errorf("cursor cell (row 0, col 0, width %d) not styled with Cursor:\nwant substring %q\nin view:\n%s", widths[1], want, view)
	}
}

// TestShiftXAbandonThroughConfirm drives the pane's abandon gesture through real keys
// (AGENTS.md §9 entry 6): tab focuses the pane, shift+x opens a confirm, and only "y"/"enter"
// actually emits flight.AbandonMsg — reading the answer via GetValue, never a captured field.
func TestShiftXAbandonThroughConfirm(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	m := withPane(80, 24, p)
	m = uitest.Keys(m, update, "tab", "shift+x")
	if m.confirmAbandon == nil {
		t.Fatal("shift+x on the pane did not open a confirm")
	}
	m2, cmd := m.Update(uitest.Key("esc"))
	if cmd != nil || m2.confirmAbandon != nil {
		t.Fatal("esc must close the confirm without abandoning")
	}
	m, _ = m.Update(uitest.Key("y"))
	m, cmd = m.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter after y produced no command")
	}
	msg, ok := cmd().(flight.AbandonMsg)
	if !ok || msg.ID != "5pr6sd333t" {
		t.Fatalf("enter after y emitted %#v, want flight.AbandonMsg{ID: 5pr6sd333t}", msg)
	}
	if m.confirmAbandon != nil {
		t.Error("the confirm must close once answered")
	}
}

// TestMatrixMenuGolden is the mockup comparison for v2·02 (the action menu).
func TestMatrixMenuGolden(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"c"}}
	m := New(fixture(), []string{"ghcr.io/"}, envs, nil).SetStyles(ui.NewStyles(true)).SetSize(80, 24)
	m = uitest.Keys(m, update, "right", "right", "enter") // cursor on c (production)
	uitest.Golden(t, "matrix-menu", ansi.Strip(m.View()), 80, 24)
}
