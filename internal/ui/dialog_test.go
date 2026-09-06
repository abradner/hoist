package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestDialogIsCentredOverADimmedParent(t *testing.T) {
	st := NewStyles(true)
	under := Frame{Title: "parent", Sections: []string{strings.Repeat("context line\n", 20)}, Footer: "hints"}.Render(st, 80, 24)
	got := Dialog(st, under, "direct commit", "Commit straight to main, no PR?\n\n  ▸ Yes    No", 80, 24)
	lines := strings.Split(ansi.Strip(got), "\n")
	if len(lines) > 24 {
		t.Fatalf("%d lines, over 24", len(lines))
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > 80 {
			t.Fatalf("line %d over 80 wide: %q", i, l)
		}
	}
	// The parent is still there, around the dialog.
	if !strings.HasPrefix(lines[0], "╭─ parent ") || !strings.Contains(lines[1], "context line") {
		t.Fatalf("parent lost:\n%s", ansi.Strip(got))
	}
	// The dialog is on the middle rows, its edges inset from the parent's.
	var top int
	for i, l := range lines {
		if strings.Contains(l, "╭─ direct commit ") {
			top = i
			break
		}
	}
	if top < 8 || top > 12 {
		t.Fatalf("dialog top edge on row %d, not centred:\n%s", top, ansi.Strip(got))
	}
	col := strings.Index(lines[top], "╭─ direct commit")
	if col < 15 || col > 30 {
		t.Fatalf("dialog left edge at column %d, not centred", col)
	}
	if !strings.Contains(lines[top+1], "Commit straight to main, no PR?") {
		t.Fatalf("question missing:\n%s", ansi.Strip(got))
	}
}

func TestDialogTooBigForTheTerminalStillShowsTheQuestion(t *testing.T) {
	st := NewStyles(true)
	got := ansi.Strip(Dialog(st, "under", "t", "a question that is wider than the terminal it is asked in", 20, 3))
	if !strings.Contains(got, "a question") {
		t.Fatalf("question lost: %q", got)
	}
	if strings.Contains(got, "under") {
		t.Fatalf("when the box cannot be composed the dialog stands alone: %q", got)
	}
}
