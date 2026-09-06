package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Dialog draws body in a titled box centred over under, for a width×height terminal — the
// shape every huh.Confirm takes from M10 on. The screen underneath stays visible (dimmed, its
// own colours stripped) so the decision keeps its context: the operator confirming a direct
// commit is still looking at the commits it ships. Built on lipgloss's compositor (Canvas and
// Layer), which is the one place the TUI uses it: a dialog is the case where covering the
// parent is the point, and everything else composes with JoinVertical (docs/tui/README.md).
//
// The dialog is at least wide enough for its longest line plus the box, capped at the
// terminal; body lines wider than that are truncated. A terminal too small for the box gets
// the dialog alone, uncentred, so the question is never lost behind the chrome.
func Dialog(st Styles, under, title, body string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	boxWidth := 0
	for _, line := range strings.Split(body, "\n") {
		boxWidth = max(boxWidth, ansi.StringWidth(line)+2)
	}
	boxWidth = max(boxWidth, ansi.StringWidth(title)+6)
	boxWidth = min(boxWidth, width)
	box := Box(st, title, []string{body}, boxWidth)
	bw, bh := lipgloss.Size(box)
	if bw > width || bh > height {
		return box
	}
	// Layer.Draw alone ignores a layer's X/Y — positioning is the Compositor's job — and a
	// Canvas.Compose of two bare layers paints the second over the first at the origin. The
	// Compositor is what places the box; the parent, dimmed, is its bottom layer.
	dimmed := st.Dim.Render(ansi.Strip(under))
	comp := lipgloss.NewCompositor(
		lipgloss.NewLayer(dimmed),
		lipgloss.NewLayer(box).X((width-bw)/2).Y((height-bh)/2).Z(1),
	)
	return comp.Render()
}
