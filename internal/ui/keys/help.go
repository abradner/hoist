package keys

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// pair is one row of the v2·03 mockup's own 2×2 grid of groups: NAVIGATE beside ACT, VIEW
// beside APP — laid out side by side rather than stacked, which is what let a
// write-heavy screen's overlay (the matrix, with its restart/abandon bindings) overflow past
// 24 rows when every group was simply one more block underneath the last.
var pairs = [][2]struct {
	g     Group
	title string
}{
	{{Navigate, "NAVIGATE"}, {Act, "ACT"}},
	{{View, "VIEW"}, {AppGroup, "APP"}},
}

// helpColWidth is each column's fixed width in the pair layout — wide enough for the longest
// entry this train's screens actually use ("shift+r  restart the cell's family → restart
// screen" truncates past this, which is an accepted trade-off of a fixed two-column grid over
// one column that could grow to fit anything).
const helpColWidth = 30

// HelpTitle is the overlay's own dialog title for screen s — "help · matrix" — the same string
// ui.Dialog draws around HelpView's body, kept as its own function so app.go's View never
// hand-formats it a second way.
func HelpTitle(s Screen) string { return fmt.Sprintf("help · %s", s) }

// HelpView renders the full-key overlay's BODY for one screen every binding On(s)
// lists, grouped under the mockup's own four headings, the "shift+ keys always ask before they
// write" line whenever the screen has at least one Write binding, and one line naming whether
// this run can tell a caps-lock letter from a real shift — recorded once at the root from
// tea.KeyboardEnhancementsMsg rather than threaded into every value-typed screen
// (the audit doc's own "needs your decision", resolved that way). The caller (app.go) draws
// this inside ui.Dialog, which supplies the box, the title (HelpTitle) and the dimmed
// background itself — HelpView returns plain content, never its own box, so the two are never
// nested one inside the other.
func HelpView(s Screen, kbd tea.KeyboardEnhancementsMsg) string {
	entries := On(s)
	byGroup := map[Group][]Entry{}
	hasWrite := false
	for _, e := range entries {
		if e.Name == Help.Name {
			// The overlay never lists its own key — pressing ? again just closes it, per the
			// mockup's own "? this help" line sitting in APP without a second "? close" row.
			continue
		}
		byGroup[e.Group] = append(byGroup[e.Group], e)
		if e.Class == Write {
			hasWrite = true
		}
	}

	var body strings.Builder
	first := true
	for _, row := range pairs {
		left := groupBlock(row[0].title, byGroup[row[0].g])
		right := groupBlock(row[1].title, byGroup[row[1].g])
		if left == "" && right == "" {
			continue
		}
		if !first {
			body.WriteString("\n")
		}
		first = false
		body.WriteString(joinCols(left, right, helpColWidth))
	}
	if hasWrite {
		fmt.Fprintf(&body, "\n\nshift+ keys always ask before they write")
	}
	fmt.Fprintf(&body, "\n\n%s", kbdLine(kbd))

	return body.String()
}

// groupBlock is one group's own heading plus its entries — "" for an empty group, so a screen
// missing a whole group (watch has no ACT bindings) leaves that side of the pair blank rather
// than printing a bare heading over nothing.
func groupBlock(title string, es []Entry) string {
	if len(es) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(title)
	for _, e := range es {
		desc := e.Desc
		if e.Class == Write {
			// v2·02/v2·03: a write binding's own row names that it asks first, right where the
			// operator is reading the key, rather than only in the shared "shift+ keys always
			// ask" line at the very bottom.
			desc += " (asks)"
		}
		fmt.Fprintf(&b, "\n%-8s %s", e.Show, desc)
	}
	return b.String()
}

// joinCols lays two already-built blocks side by side, left padded/truncated to width, joined
// by a plain rule — HelpView's own column pairing (v2·03), kept local rather than
// reusing ui.Columns so this package's HelpView signature stays exactly what shipped
// (no ui.Styles parameter to thread through call sites and tests that don't otherwise need
// one just to draw an unstyled dialog body).
func joinCols(left, right string, width int) string {
	ll := strings.Split(left, "\n")
	rl := strings.Split(right, "\n")
	n := len(ll)
	if len(rl) > n {
		n = len(rl)
	}
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(ll) {
			l = ll[i]
		}
		if i < len(rl) {
			r = rl[i]
		}
		lines[i] = padTrunc(l, width) + " │ " + padTrunc(r, width)
	}
	return strings.Join(lines, "\n")
}

// padTrunc pads or truncates s to exactly width runes.
func padTrunc(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	return s + strings.Repeat(" ", width-len(r))
}

// kbdLine is the one line HelpView always shows naming what this run's terminal reported: only
// a terminal that both supports the Kitty keyboard protocol and was asked for flag 8
// (ReportAllKeysAsEscapeCodes — the View sent every run) can tell a caps-lock letter apart
// from a real shift; every other terminal (the legacy path every session runs today unless that
// flag was granted) reports both identically, so a bare capital counts as shift there
// (Binding.Matches, point 4).
func kbdLine(kbd tea.KeyboardEnhancementsMsg) string {
	if kbd.SupportsAllKeysAsEscapeCodes() {
		return "caps lock ignored"
	}
	return "a capital counts as shift"
}
