package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Frame is the shape every hoist screen is drawn in (AGENTS.md §4.8, M10): a titled,
// rounded box whose Sections are separated by rules, and a Footer — the status bar — that is
// always the terminal's last line no matter how tall the box is. The box is tight to its
// content; a screen that wants it to fill the terminal sizes its own section (a viewport, a
// table) to Frame.BodyHeight. Nothing here draws a box character by hand: the edges, the
// junctions and the title line all come from lipgloss's Border, so the widths are right by
// construction rather than by counting.
//
// A screen's View is `ui.Frame{...}.Render(styles, width, height)`; a sub-pane inside a
// section (the in-flight panel under the matrix) is Box, the same thing without a footer.
type Frame struct {
	Title    string
	Sections []string
	// Panes are full-width blocks (already rendered, a Box each) stacked under the main box
	// and above the footer — the matrix's in-flight pane. Empty strings are skipped.
	Panes  []string
	Footer string
}

// chrome is the number of rows the box's own edges take: top and bottom.
const chrome = 2

// BodyHeight is how many content rows a Frame with n sections can hold on a terminal height
// rows tall: the height minus the footer, the two edges and the n-1 rules between sections.
// A screen sizes its scrolling section to this less the fixed rows of its other sections.
func BodyHeight(height, sections int) int {
	h := height - 1 - chrome - max(sections-1, 0)
	return max(h, 1)
}

// Render draws the frame for a width×height terminal: the box, then blank rows, then the
// footer on the last line. Content wider than the box is truncated with "…"; content taller
// than the space above the footer is cut from the bottom (the screen is expected to have
// sized its scrolling section, see BodyHeight). A width or height too small to hold a box
// (under 4 columns or 3 rows) renders only what fits.
func (f Frame) Render(st Styles, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	footer := ansi.Truncate(f.Footer, width, "…")
	if height == 1 {
		return footer
	}
	box := Box(st, f.Title, f.Sections, width)
	for _, p := range f.Panes {
		if p != "" {
			box += "\n" + p
		}
	}
	lines := strings.Split(box, "\n")
	room := height - 1
	if len(lines) > room {
		lines = lines[:room]
	}
	for len(lines) < room {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, footer), "\n")
}

// Box draws a titled, rounded box exactly width cells wide around sections, with a rule
// between each pair. Every line of every section is truncated to the inner width, never
// wrapped: a version string that wraps mid-token is the defect this exists to end (#85).
func Box(st Styles, title string, sections []string, width int) string {
	if width < 4 {
		return ""
	}
	inner := width - 2
	border := lipgloss.RoundedBorder()
	edge := st.Border
	var out []string
	out = append(out, titleLine(st, border, title, width))
	for i, s := range sections {
		if i > 0 {
			out = append(out, edge.Render(border.MiddleLeft+strings.Repeat(border.Top, inner)+border.MiddleRight))
		}
		for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			line = ansi.Truncate(line, inner, "…")
			pad := inner - ansi.StringWidth(line)
			if pad < 0 {
				pad = 0
			}
			out = append(out, edge.Render(border.Left)+line+strings.Repeat(" ", pad)+edge.Render(border.Right))
		}
	}
	out = append(out, edge.Render(border.BottomLeft+strings.Repeat(border.Bottom, inner)+border.BottomRight))
	return strings.Join(out, "\n")
}

// titleLine is the top edge with the title set into it: "╭─ title ─────╮". An empty title
// is a plain edge. The pieces are the Border's own; only the arrangement is ours.
func titleLine(st Styles, b lipgloss.Border, title string, width int) string {
	inner := width - 2
	if title == "" {
		return st.Border.Render(b.TopLeft + strings.Repeat(b.Top, inner) + b.TopRight)
	}
	t := " " + ansi.Truncate(title, max(inner-3, 0), "…") + " "
	rest := inner - 1 - ansi.StringWidth(t)
	if rest < 0 {
		rest = 0
	}
	return st.Border.Render(b.TopLeft+b.Top) + st.Title.Render(t) + st.Border.Render(strings.Repeat(b.Top, rest)+b.TopRight)
}

// Columns lays left and right side by side with a vertical rule between them, left padded or
// truncated to leftWidth, both aligned to the top. Replaces the string-padding joinPanes the
// plan screen carried (AGENTS.md §4.8 codifies this as the two-pane shape).
func Columns(st Styles, left, right string, leftWidth int) string {
	l := fit(left, leftWidth)
	rows := max(lipgloss.Height(l), lipgloss.Height(right))
	rule := strings.TrimSuffix(strings.Repeat(st.Border.Render(lipgloss.NormalBorder().Left)+"\n", rows), "\n")
	return lipgloss.JoinHorizontal(lipgloss.Top, l, rule, right)
}

// fit truncates or pads every line of s to exactly width cells.
func fit(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = ansi.Truncate(line, width, "…")
		lines[i] = line + strings.Repeat(" ", max(width-ansi.StringWidth(line), 0))
	}
	return strings.Join(lines, "\n")
}
