package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// StatusBar renders one line exactly width cells wide: left text, then right text flush
// against the right edge. When both do not fit, left is truncated with an ellipsis so the
// key hints on the right stay visible; when even right alone does not fit, right is cut.
// Both arguments may carry ANSI styling; widths are measured on the visible text.
func StatusBar(width int, left, right string) string {
	if width <= 0 {
		return ""
	}
	rw := ansi.StringWidth(right)
	if rw > width {
		return ansi.Truncate(right, width, "")
	}
	// One cell of gap between the two halves, when there is a left half at all.
	avail := width - rw - 1
	if avail <= 0 {
		return strings.Repeat(" ", width-rw) + right
	}
	lw := ansi.StringWidth(left)
	if lw > avail {
		left = ansi.Truncate(left, avail, "…")
		lw = ansi.StringWidth(left)
	}
	return left + strings.Repeat(" ", width-lw-rw) + right
}
