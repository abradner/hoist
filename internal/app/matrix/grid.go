package matrix

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
)

// grid.go is the matrix's own table renderer (T3-04, UX-M9): a cell cursor — one family row
// AND one env column intersecting at a single highlighted cell — is something a whole-row
// highlight (the bubbles table.Model this replaces) cannot express, so the grid is drawn by
// hand here instead. cells.go stays the pure derivation of what a cell says; grid.go is only
// concerned with how those strings are laid into a bordered block of text, with no notion of
// promotion, drift or config.

// Focus says which part of the matrix screen currently owns the cursor keys (tab moves it,
// T3-04's own new binding): the grid itself, or the in-flight pane underneath it.
type Focus uint8

const (
	// FocusGrid is the default: up/down move the family row, left/right the env column.
	FocusGrid Focus = iota
	// FocusPane means up/down move the in-flight pane's own cursor instead, and enter/shift+x
	// act on the promotion under IT rather than opening the action menu for a cell.
	FocusPane
)

// GridState is the grid renderer's own input beyond the Table text itself (train3-design.md's
// T3-04 shape): the cell cursor (Row, Col — Col is an index into Widths' env columns, i.e.
// Table.Envs, not into Widths itself, which carries one extra leading entry for FAMILY),
// the vertical scroll Offset and how many data rows fit (Height), each column's fitted width
// (Widths[0] is FAMILY; Widths[i+1] is Table.Envs[i]), and which side of the screen currently
// has the cursor (Focus) — the grid dims nothing based on Focus itself; a screen that wants to
// show the pane focused draws that separately. Cursor cell styling instead only applies to the
// exact (Row, Col) intersection when Focus is FocusGrid, so tabbing to the pane visibly moves
// the highlight off the table rather than leaving a stale cell lit.
type GridState struct {
	Row, Col, Offset, Height int
	Widths                   []int
	Focus                    Focus
}

// cellPad is the horizontal padding either side of a cell's text — the same convention the
// old bubbles-table styling used, kept so column widths read the same width a screen already
// picked via fit().
const cellPad = 1

// Grid renders the table's own bordered block: a header-divider rule, the header row (one
// cell per Table.Envs entry, already carrying whatever cursor/production marker the caller
// baked into the display copy of t — Grid itself knows nothing about config or which column is
// "selected" beyond g.Col, which only ever drives the CELL highlight), a header/body divider,
// then g.Height data rows starting at g.Offset (blank-padded if the table has fewer rows than
// that). It returns the block as a slice of lines rather than a joined string so a caller
// composing it into a larger section (the matrix's own View) can measure it with
// lipgloss.Height the same way every other section is measured.
func Grid(st ui.Styles, t Table, g GridState) []string {
	if len(g.Widths) == 0 {
		return nil
	}
	var out []string
	out = append(out, ruleRow(g.Widths, '┬'))
	out = append(out, dataRow(st, headerCells(t), g.Widths, -1, -1, g))
	out = append(out, ruleRow(g.Widths, '┼'))
	height := max(g.Height, 0)
	for i := 0; i < height; i++ {
		row := g.Offset + i
		if row >= len(t.Rows) {
			out = append(out, dataRow(st, blankCells(len(g.Widths)), g.Widths, -1, -1, g))
			continue
		}
		out = append(out, dataRow(st, t.Rows[row].cellTexts(), g.Widths, row, g.Row, g))
	}
	return out
}

// Neither ruleRow nor dataRow draws the grid's own OUTER left/right edge — internal/ui.Frame's
// own boxLines wraps every section line in the frame's "│ … │" automatically (AGENTS.md §4.8:
// a screen never hand-assembles the outer border), so the grid supplies only its interior:
// the "┬"/"┼" column joints and the "│" cell separators. The one visible trade-off is the
// frame's own outer edge glyph staying a plain "│" on a rule row rather than becoming "├"/"┤"
// the way a hand-drawn, fully self-bordered table would — Frame has no per-line notion of
// "this row wants different corner glyphs", and giving it one is a bigger change than this
// screen's own file scope, so the mockup-vs-golden diff notes this one cosmetically, never
// fixes it by having the grid draw its own outer border (which would double it, T3-04's own
// first attempt at this and the bug this comment now heads off).

// headerCells is FAMILY plus every env's display name (Table.Envs — a caller wanting the
// cursor marker or the production warning in the header bakes it into the env name string
// before calling Grid, since Grid itself has no config to consult).
func headerCells(t Table) []string {
	out := make([]string, 0, len(t.Envs)+1)
	out = append(out, "FAMILY")
	out = append(out, t.Envs...)
	return out
}

// cellTexts is one row's own text per column: the family name, then Cell.String() (or blank
// for an absent cell) for each env.
func (r Row) cellTexts() []string {
	out := make([]string, 0, len(r.Cells)+1)
	out = append(out, r.Family)
	for _, c := range r.Cells {
		out = append(out, c.String())
	}
	return out
}

func blankCells(n int) []string {
	return make([]string, n)
}

// ruleRow draws a horizontal divider the width of every column, joined at each boundary by
// joint — '┬' above the header, '┼' between the header and the body.
func ruleRow(widths []int, joint rune) string {
	parts := make([]string, len(widths))
	for i, w := range widths {
		parts[i] = strings.Repeat("─", w+cellPad*2)
	}
	return strings.Join(parts, string(joint))
}

// dataRow draws one row of cell text, left-aligned and padded/truncated to each column's
// width, separated by "│". rowIdx/cursorRow decide the family-column marker ("▸ ", the same
// convention the header's own column marker uses — goldens are ANSI-stripped, so the cursor
// ROW has to be visible as text, not only as a colour, the same reasoning the pre-existing
// header column marker was built on); colIdx is unused by dataRow directly (kept for a future
// per-cell marker) — the CELL cursor itself is the one thing that only ever shows as colour
// (st.Cursor), applied to cells[g.Col+1] when g.Focus is FocusGrid and rowIdx == g.Row.
func dataRow(st ui.Styles, cells []string, widths []int, rowIdx, cursorRow int, g GridState) string {
	parts := make([]string, len(cells))
	for i, w := range cells {
		text := w
		if i == 0 && rowIdx >= 0 && rowIdx == cursorRow {
			text = selectedMarker + text
		}
		text = ansi.Truncate(text, widths[i], "…")
		pad := widths[i] - ansi.StringWidth(text)
		if pad < 0 {
			pad = 0
		}
		cell := text + strings.Repeat(" ", pad)
		if i > 0 && rowIdx >= 0 && rowIdx == g.Row && i-1 == g.Col && g.Focus == FocusGrid {
			cell = st.Cursor.Render(cell)
		}
		parts[i] = strings.Repeat(" ", cellPad) + cell + strings.Repeat(" ", cellPad)
	}
	return strings.Join(parts, "│")
}
