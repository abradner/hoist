package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestStatusBar(t *testing.T) {
	cases := []struct {
		name        string
		width       int
		left, right string
		want        string // the visible text: got with ANSI sequences stripped
		styled      bool   // the input carried styling, and the output must still
	}{
		{"fits", 20, "left", "right", "left           right", false},
		{"exact", 10, "left", "right", "left right", false},
		{"left truncated", 12, "a long left side", "right", "a lon… right", false},
		{"only right fits", 5, "left", "right", "right", false},
		{"right cut", 3, "left", "right", "rig", false},
		{"no left", 8, "", "right", "   right", false},
		{"zero width", 0, "left", "right", "", false},
		{"styled widths", 12, NewStyles(true).Notice.Render("left"), "right", "left   right", true},
	}
	for _, tc := range cases {
		got := StatusBar(tc.width, tc.left, tc.right)
		// Compare the visible text for every case, the styled one included: a regression
		// that padded a styled left with spaces would keep the width and lose the words.
		if plain := ansi.Strip(got); plain != tc.want {
			t.Errorf("%s: visible text %q, want %q (raw %q)", tc.name, plain, tc.want, got)
		}
		if w := ansi.StringWidth(got); tc.width > 0 && w != tc.width {
			t.Errorf("%s: width %d, want %d: %q", tc.name, w, tc.width, got)
		}
		if tc.styled == !strings.Contains(got, "\x1b[") {
			t.Errorf("%s: styled=%v but output is %q", tc.name, tc.styled, got)
		}
	}
	// The styled case is only a control if Render actually emitted styling.
	if !strings.Contains(NewStyles(true).Notice.Render("left"), "\x1b[") {
		t.Fatal("Notice.Render produced no ANSI sequence; the styled case proves nothing")
	}
}

func TestNewStylesFollowsBackground(t *testing.T) {
	if !NewStyles(true).Dark || NewStyles(false).Dark {
		t.Error("Dark does not record the flag")
	}
	if NewStyles(true).Selected.GetForeground() == NewStyles(false).Selected.GetForeground() {
		t.Error("light and dark palettes share the accent colour")
	}
}
