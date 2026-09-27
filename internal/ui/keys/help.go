package keys

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// column names each help-overlay group, in the v2·03 mockup's own left-to-right, top-to-bottom
// order (NAVIGATE, ACT, VIEW, APP).
var column = []struct {
	g     Group
	title string
}{
	{Navigate, "NAVIGATE"},
	{Act, "ACT"},
	{View, "VIEW"},
	{AppGroup, "APP"},
}

// HelpTitle is the overlay's own dialog title for screen s — "help · matrix" — the same string
// ui.Dialog draws around HelpView's body, kept as its own function so app.go's View never
// hand-formats it a second way.
func HelpTitle(s Screen) string { return fmt.Sprintf("help · %s", s) }

// HelpView renders the full-key overlay's BODY for one screen (T3-03): every binding On(s)
// lists, grouped under the mockup's own four headings, the "shift+ keys always ask before they
// write" line whenever the screen has at least one Write binding, and one line naming whether
// this run can tell a caps-lock letter from a real shift — recorded once at the root from
// tea.KeyboardEnhancementsMsg rather than threaded into every value-typed screen
// (train3-design.md's own "needs your decision", resolved that way). The caller (app.go) draws
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
	for _, col := range column {
		es := byGroup[col.g]
		if len(es) == 0 {
			continue
		}
		if !first {
			body.WriteString("\n")
		}
		first = false
		body.WriteString(col.title)
		for _, e := range es {
			fmt.Fprintf(&body, "\n%-8s %s", e.Show, e.Desc)
		}
	}
	if hasWrite {
		fmt.Fprintf(&body, "\n\nshift+ keys always ask before they write")
	}
	fmt.Fprintf(&body, "\n\n%s", kbdLine(kbd))

	return body.String()
}

// kbdLine is the one line HelpView always shows naming what this run's terminal reported: only
// a terminal that both supports the Kitty keyboard protocol and was asked for flag 8
// (ReportAllKeysAsEscapeCodes — the View sent every run, T3-01) can tell a caps-lock letter apart
// from a real shift; every other terminal (the legacy path every session runs today unless that
// flag was granted) reports both identically, so a bare capital counts as shift there
// (Binding.Matches, point 4).
func kbdLine(kbd tea.KeyboardEnhancementsMsg) string {
	if kbd.SupportsAllKeysAsEscapeCodes() {
		return "caps lock ignored"
	}
	return "a capital counts as shift"
}
