package matrix

import "testing"

// TestFitWidthsBudgetsSeparators is P1-4 from the T3 review: fitWidths' own total() summed
// each column's width plus its cellPad*2, but never the "│" that dataRow/ruleRow actually join
// every pair of columns with — one per boundary, len(widths)-1 of them for N columns. So a
// budget of 78 (an 80-column terminal minus the frame's own 2-cell border, gridWidths' own
// "m.width-2") could pass fitWidths' fit check while the row it actually renders is 78 plus the
// separators wide — 80 for a 3-column row (FAMILY + 2 envs) — 2 cells over the terminal.
// Frame's own line cropping then ate into whatever sat at the row's right edge, which is the
// last column's right-aligned state word (helped shrink to "pinn…", "extern…", "spl…" at 80
// columns in testdata/golden/matrix-root-80x24.txt before the fix).
func TestFitWidthsBudgetsSeparators(t *testing.T) {
	widths := []int{12, 44, 44} // gridWidths' own pre-fit widths for the app-level fixture at 80 cols
	budget := 78                // gridWidths' own max(m.width-2, 0) for an 80-column terminal
	out := fitWidths(widths, budget)

	rendered := 0
	for _, w := range out {
		rendered += w + cellPad*2
	}
	if len(out) > 1 {
		rendered += len(out) - 1 // the "│" separators dataRow/ruleRow actually emit between columns
	}
	if rendered > budget {
		t.Errorf("fitWidths(%v, %d) = %v: the row this actually renders is %d cells wide, over the %d-cell budget", widths, budget, out, rendered, budget)
	}
}
