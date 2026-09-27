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
// than the space above the footer is trimmed — but never by dropping the closing border off
// the bottom, and never by deleting a screen's header (or any other earlier section)
// to keep a bare "…" marker for the LAST section alone. When the assembled box (plus any
// Panes) is too tall for the room above the footer, rows are cut from the bottom of the box's
// LAST section only — the section nearest the closing border, never an earlier one — down to
// a single dim "…" continuation row. If even THAT (its own rule included) is not enough to
// close the gap, the whole section is dropped outright instead of leaving an orphaned "…"
// that was never going to be enough on its own; the box then ends at whatever section is now
// last. Only once every section has been considered do Panes get cut whole from the bottom,
// and only as an absolute last resort — one that should never fire while any section still had
// something left to give — does an earlier row get dropped at all; the closing border itself
// is never a candidate for removal. A width or height too small to hold a box (under 4
// columns or 3 rows) renders only what fits.
func (f Frame) Render(st Styles, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	footer := ansi.Truncate(f.Footer, width, "…")
	if height == 1 {
		return footer
	}
	room := height - 1
	if width < 4 {
		lines := make([]string, room)
		return strings.Join(append(lines, footer), "\n")
	}

	main, starts := boxLines(st, f.Title, f.Sections, width)
	bottom := bottomBorder(st, width)
	var panes []string
	for _, p := range f.Panes {
		if p != "" {
			panes = append(panes, strings.Split(p, "\n")...)
		}
	}

	// Work backward from the last section: trim it to a single "…" marker if that closes the
	// gap, otherwise drop it whole (rule included) and reconsider the section now last. This
	// repeats until the box fits or only one section remains — so a run of several short
	// trailing sections gives way one at a time before an earlier, higher-priority section
	// (the header) is ever touched.
	for n := len(starts); n > 0; {
		total := len(main) + 1 + len(panes)
		if total <= room {
			break
		}
		overflow := total - room
		sectionStart := starts[n-1]
		avail := len(main) - sectionStart // content rows currently in this section
		cut := min(overflow, max(avail-1, 0))
		if cut < overflow && n > 1 {
			// Reducing this section to its own single marker row still would not close the
			// gap: dropping it entirely (rule included) is strictly better than leaving a
			// bare "…" that was never going to be enough by itself, and moves the box's
			// closing border up to whatever section is now last, rather than reaching past
			// this section into an earlier, higher-priority one.
			main = main[:sectionStart-1]
			n--
			continue
		}
		if cut > 0 {
			main = main[:len(main)-cut]
		}
		if avail > 0 {
			main[len(main)-1] = continuationRow(st, width)
		}
		break
	}

	if total := len(main) + 1 + len(panes); total > room {
		overflow := total - room
		if len(panes) > 0 {
			paneCut := min(overflow, len(panes))
			panes = panes[:len(panes)-paneCut]
			overflow -= paneCut
		}
		// Pathological remainder (room too small even for title + the header's own one
		// marker row + the panes already cut to nothing): fall back to dropping from the
		// earliest content row after the title. This never fires while the header still has
		// more than one row to give — the loop above already reduced it to its own single
		// continuation row first.
		for overflow > 0 && len(main) > 1 {
			main = append(main[:1], main[2:]...)
			overflow--
		}
	}

	lines := append(main, bottom) //nolint:gocritic // main is never read again after this
	lines = append(lines, panes...)
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
	lines, _ := boxLines(st, title, sections, width)
	lines = append(lines, bottomBorder(st, width))
	return strings.Join(lines, "\n")
}

// boxLines renders every line of a box except its closing border: the title edge, then each
// section's content with a rule between sections. starts[i] is the index into lines where
// section i's own content begins (after its rule, or after the title for section 0) — the
// boundary Frame.Render needs to trim or drop sections from the end on overflow, without ever
// reaching into an earlier one while a later one still has something left to give.
func boxLines(st Styles, title string, sections []string, width int) (lines []string, starts []int) {
	inner := width - 2
	border := lipgloss.RoundedBorder()
	edge := st.Border
	lines = append(lines, titleLine(st, border, title, width))
	starts = make([]int, len(sections))
	for i, s := range sections {
		if i > 0 {
			lines = append(lines, edge.Render(border.MiddleLeft+strings.Repeat(border.Top, inner)+border.MiddleRight))
		}
		starts[i] = len(lines)
		for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			line = ansi.Truncate(line, inner, "…")
			pad := inner - ansi.StringWidth(line)
			if pad < 0 {
				pad = 0
			}
			lines = append(lines, edge.Render(border.Left)+line+strings.Repeat(" ", pad)+edge.Render(border.Right))
		}
	}
	return lines, starts
}

// bottomBorder is a box's closing edge alone — the row Frame.Render never cuts.
func bottomBorder(st Styles, width int) string {
	inner := width - 2
	border := lipgloss.RoundedBorder()
	return st.Border.Render(border.BottomLeft + strings.Repeat(border.Bottom, inner) + border.BottomRight)
}

// continuationRow is a box content row reading a single dim "…", used in place of the last
// row Frame.Render keeps from a section it had to cut short.
func continuationRow(st Styles, width int) string {
	inner := width - 2
	border := lipgloss.RoundedBorder()
	edge := st.Border
	mark := st.Dim.Render("…")
	pad := inner - ansi.StringWidth("…")
	if pad < 0 {
		pad = 0
	}
	return edge.Render(border.Left) + mark + strings.Repeat(" ", pad) + edge.Render(border.Right)
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

// NoticeMaxLines caps how many terminal rows a root-level notice may take. A notice is
// reserved space stolen from the screen under it (app.Model.View), so an unbounded one — a
// git or forge transport error can run to several hundred characters — would push the screen
// it is explaining off the terminal instead.
const NoticeMaxLines = 3

// NoticeLines renders a transient message as at most NoticeMaxLines rows of exactly width
// cells, word-wrapped, with the overflow marked "…" on the last row. It returns nil for an
// empty message so the caller can size the screen above it by len(lines).
//
// The caller is expected to shrink whatever it draws above by exactly this many rows: a
// Frame renders exactly `height` lines (Render, above), so a notice appended after one lands
// on row height+1 and the alternate screen buffer never shows it — the whole defect this
// exists to close (#164).
func NoticeLines(st Styles, text string, width int) []string {
	if text == "" || width <= 0 {
		return nil
	}
	wrapped := strings.Split(ansi.Wrap(text, width, " -"), "\n")
	if len(wrapped) > NoticeMaxLines {
		wrapped = wrapped[:NoticeMaxLines]
		last := len(wrapped) - 1
		wrapped[last] = ansi.Truncate(wrapped[last], max(width-1, 0), "") + "…"
	}
	out := make([]string, len(wrapped))
	for i, line := range wrapped {
		out[i] = st.Notice.Render(fit(line, width))
	}
	return out
}
