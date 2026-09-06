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
