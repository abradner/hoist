package ui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestStatusBar(t *testing.T) {
	cases := []struct {
		name        string
		width       int
		left, right string
		want        string
	}{
		{"fits", 20, "left", "right", "left           right"},
		{"exact", 10, "left", "right", "left right"},
		{"left truncated", 12, "a long left side", "right", "a lon… right"},
		{"only right fits", 5, "left", "right", "right"},
		{"right cut", 3, "left", "right", "rig"},
		{"no left", 8, "", "right", "   right"},
		{"zero width", 0, "left", "right", ""},
		{"styled widths", 12, NewStyles(true).Notice.Render("left"), "right", ""},
	}
	for _, tc := range cases {
		got := StatusBar(tc.width, tc.left, tc.right)
		if tc.want != "" && got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if w := ansi.StringWidth(got); tc.width > 0 && w != tc.width {
			t.Errorf("%s: width %d, want %d: %q", tc.name, w, tc.width, got)
		}
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
