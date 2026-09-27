package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func shape(t *testing.T, s string, width, height int) []string {
	t.Helper()
	lines := strings.Split(ansi.Strip(s), "\n")
	if height > 0 && len(lines) != height {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), height, ansi.Strip(s))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Fatalf("line %d is %d wide, over %d: %q", i+1, w, width, l)
		}
	}
	return lines
}

// The footer is the last line whatever the box's height, and the box is exactly the
// terminal's width — the two defects #85 names first.
func TestFrameFooterIsAlwaysTheLastLine(t *testing.T) {
	st := NewStyles(true)
	f := Frame{Title: "hoist · matrix", Sections: []string{"row one\nrow two", "a warning"}, Footer: "left  right"}
	for _, h := range []int{8, 24, 40} {
		lines := shape(t, f.Render(st, 80, h), 80, h)
		if lines[h-1] != "left  right" {
			t.Fatalf("height %d: last line %q, want the footer", h, lines[h-1])
		}
		if !strings.HasPrefix(lines[0], "╭─ hoist · matrix ─") || !strings.HasSuffix(lines[0], "╮") || ansi.StringWidth(lines[0]) != 80 {
			t.Fatalf("height %d: top edge %q", h, lines[0])
		}
		if lines[1] != "│row one"+strings.Repeat(" ", 71)+"│" {
			t.Fatalf("row %q", lines[1])
		}
		if !strings.HasPrefix(lines[3], "├") || !strings.HasSuffix(lines[3], "┤") {
			t.Fatalf("rule %q", lines[3])
		}
		if !strings.HasPrefix(lines[5], "╰") || !strings.HasSuffix(lines[5], "╯") {
			t.Fatalf("bottom %q", lines[5])
		}
	}
}

func TestFrameTruncatesNeverWraps(t *testing.T) {
	st := NewStyles(true)
	long := strings.Repeat("ghcr.io/example/orders:v2026011510@sha256:", 3)
	lines := shape(t, Frame{Sections: []string{long}, Footer: strings.Repeat("hint ", 40)}.Render(st, 40, 5), 40, 5)
	if !strings.HasSuffix(strings.TrimSuffix(lines[1], "│"), "…") {
		t.Fatalf("long line not truncated with …: %q", lines[1])
	}
	if !strings.HasSuffix(lines[4], "…") {
		t.Fatalf("footer not truncated: %q", lines[4])
	}
}

func TestFrameTallerThanTheTerminalIsCutNotOverflowed(t *testing.T) {
	st := NewStyles(true)
	body := strings.Repeat("line\n", 50)
	lines := shape(t, Frame{Sections: []string{body}, Footer: "f"}.Render(st, 20, 10), 20, 10)
	if lines[9] != "f" {
		t.Fatalf("footer lost: %q", lines[9])
	}
}

// The closing border is never cut off the bottom: when content overflows, the box still ends
// with its own ╰…╯ row, with a dim "…" continuation row directly above it marking the cut.
func TestFrameOverflowNeverCutsTheClosingBorder(t *testing.T) {
	st := NewStyles(true)
	body := strings.Repeat("line\n", 50)
	lines := shape(t, Frame{Sections: []string{body}, Footer: "f"}.Render(st, 20, 10), 20, 10)
	if !strings.HasPrefix(lines[8], "╰") || !strings.HasSuffix(lines[8], "╯") {
		t.Fatalf("line %d (height-2) is not the closing border: %q", 8, lines[8])
	}
	if !strings.Contains(lines[7], "…") {
		t.Fatalf("line %d (height-3) does not carry the … continuation marker: %q", 7, lines[7])
	}
}

// TestFrameOverflowDropsTheWholeLastSectionRatherThanEatAnEarlierRow pins t1-review.md P2 #7:
// when trimming the last section down to its own single "…" continuation row still is not
// enough to fit the room above the footer, Render must drop that WHOLE section (its rule
// included) rather than fall back to eating rows out of an earlier section — the defect that
// let a flight screen's own header ("abcd1234 app-staging → app-production") disappear on a
// short terminal while a lower-priority trailing section was reduced to a bare, still-too-tall
// "…" (testdata/golden/flight-approval-80x12.txt, before this fix).
func TestFrameOverflowDropsTheWholeLastSectionRatherThanEatAnEarlierRow(t *testing.T) {
	st := NewStyles(true)
	f := Frame{
		Title:    "hoist · promotion",
		Sections: []string{"abcd1234 app-staging → app-production", "fixed row", strings.Repeat("history line\n", 5)},
		Footer:   "f",
	}
	// height=7: room=6. main (title+header+rule+fixed+rule+5 history lines) is 10 rows, plus
	// the closing border = 11 — trimming the history section to a single marker row (cut=4 of
	// a possible 5, since at least 1 row must remain before a marker replaces it) only gets
	// main+border down to 7, still one over room(6): not enough, so the whole history section
	// (its rule included) must be dropped instead.
	out := f.Render(st, 60, 7)
	lines := shape(t, out, 60, 7)
	if !strings.Contains(lines[1], "abcd1234 app-staging") {
		t.Fatalf("header row was dropped to make room for a trailing section's own marker:\n%s", out)
	}
	if !strings.Contains(out, "fixed row") {
		t.Fatalf("the fixed-content section was dropped, want only the last (history) section gone:\n%s", out)
	}
	if strings.Contains(out, "history line") {
		t.Fatalf("the history section's own content should be gone entirely, not partially shown:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Fatalf("a fully-dropped section must not leave behind a bare \"…\" marker:\n%s", out)
	}
}

// TestFrameOverflowWalksBackThroughMultipleTrailingSections pins the codex finding on top of
// TestFrameOverflowDropsTheWholeLastSectionRatherThanEatAnEarlierRow: dropping the last section
// alone is not always enough. When a 4-section frame overflows badly enough that both trailing
// sections have to give way, Render must walk backward — drop the last section whole, then
// re-apply the same trim-or-drop judgement to the section that is now last — rather than falling
// straight through to the pathological "eat rows after the title" fallback the moment the first
// drop doesn't close the gap. That fallback starts cutting at index 1 (right after the title),
// which reaches into the header before a still-trimmable third section is ever considered.
func TestFrameOverflowWalksBackThroughMultipleTrailingSections(t *testing.T) {
	st := NewStyles(true)
	f := Frame{
		Title: "hoist · promotion",
		Sections: []string{
			"HEADER abcd1234 app-staging → app-production",
			"row one",
			"keep-a\nkeep-b\nkeep-c",
			strings.Repeat("history line\n", 5),
		},
		Footer: "f",
	}
	// height=8: room=7. main is 14 rows + border = 15, five over. Dropping the history section
	// whole (rule included) removes 6, leaving 9 (one over room=7... walked through below) — not
	// enough by itself; the third section then must also give way, trimmed to its own single "…"
	// marker rather than dropped, since trimming alone closes the remaining gap.
	out := f.Render(st, 60, 8)
	lines := shape(t, out, 60, 8)
	if !strings.Contains(lines[1], "HEADER abcd1234") {
		t.Fatalf("header row was dropped instead of a trailing section: %q\n%s", lines[1], out)
	}
	if !strings.Contains(out, "row one") {
		t.Fatalf("the second section was dropped, want only the two trailing sections affected:\n%s", out)
	}
	if strings.Contains(out, "history line") {
		t.Fatalf("the history section's own content should be gone entirely:\n%s", out)
	}
	if strings.Contains(out, "keep-b") || strings.Contains(out, "keep-c") {
		t.Fatalf("the third section should be trimmed to a single marker row, not left partially shown:\n%s", out)
	}
	closing := lines[len(lines)-2]
	if !strings.HasPrefix(closing, "╰") || !strings.HasSuffix(closing, "╯") {
		t.Fatalf("closing border not at height-2 (row %d): %q", len(lines)-2, closing)
	}
	above := lines[len(lines)-3]
	if strings.HasPrefix(above, "├") || strings.HasSuffix(above, "┤") {
		t.Fatalf("a bare rule sits directly above the closing border, detached from any content: %q", above)
	}
	if !strings.Contains(above, "…") {
		t.Fatalf("expected the row above the closing border to carry the trimmed section's \"…\" marker: %q", above)
	}
}

func TestBodyHeight(t *testing.T) {
	// 24 rows: footer 1, edges 2, one rule between two sections = 20 content rows.
	if got := BodyHeight(24, 2); got != 20 {
		t.Fatalf("BodyHeight(24,2) = %d", got)
	}
	if got := BodyHeight(24, 1); got != 21 {
		t.Fatalf("BodyHeight(24,1) = %d", got)
	}
	if got := BodyHeight(2, 5); got != 1 {
		t.Fatalf("BodyHeight floors at 1, got %d", got)
	}
}

func TestFrameTinySizes(t *testing.T) {
	st := NewStyles(true)
	if got := (Frame{Footer: "f"}).Render(st, 10, 1); got != "f" {
		t.Fatalf("height 1 renders only the footer, got %q", got)
	}
	if got := (Frame{}).Render(st, 0, 5); got != "" {
		t.Fatalf("width 0 renders nothing, got %q", got)
	}
	if got := Box(st, "t", []string{"x"}, 3); got != "" {
		t.Fatalf("a 3-wide box cannot exist, got %q", got)
	}
}

func TestColumnsPadsLeftAndRulesBetween(t *testing.T) {
	st := NewStyles(true)
	got := ansi.Strip(Columns(st, "a\nbb\nccc", "right\nr2", 5))
	// lipgloss pads the right block to its widest line; compare without trailing spaces.
	var trimmed []string
	for _, l := range strings.Split(got, "\n") {
		trimmed = append(trimmed, strings.TrimRight(l, " "))
	}
	got = strings.Join(trimmed, "\n")
	want := "a    │right\nbb   │r2\nccc  │"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
