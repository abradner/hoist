package ui

import (
	"fmt"
	"testing"

	"charm.land/lipgloss/v2"
)

// fg extracts a style's own foreground colour as a comparable key (its RGBA, color.Color's own
// interface method). Every colour in this package's palette is built through
// lipgloss.LightDark at NewStyles-construction time, so what a style holds is already the
// concrete colour picked for that dark/light build, never an unresolved AdaptiveColor.
func fg(s lipgloss.Style) string {
	c := s.GetForeground()
	if c == nil {
		return ""
	}
	r, g, b, a := c.RGBA()
	return fmt.Sprintf("%d,%d,%d,%d", r, g, b, a)
}

// TestPaletteRolesDistinct asserts T3-02's own reason for existing: Info, Warn and Production
// used to all resolve to the same amber (the audit doc's "today Notice, Warn and
// Production all share one amber"). Each must now render a different foreground, in both a
// dark and a light terminal, or the visual distinction the redesign promises does not exist.
func TestPaletteRolesDistinct(t *testing.T) {
	for _, dark := range []bool{true, false} {
		st := NewStyles(dark)
		roles := map[string]lipgloss.Style{
			"Info":       st.Info,
			"Warn":       st.Warn,
			"Production": st.Production,
		}
		seen := map[string]string{}
		for name, s := range roles {
			c := fg(s)
			if other, ok := seen[c]; ok {
				t.Errorf("dark=%v: %s and %s resolve to the same foreground %q", dark, name, other, c)
			}
			seen[c] = name
		}
	}
}

// TestHelpDistinctFromMuted: Help and Dim used to share one grey; T3-02 gives Help its own
// shade (styles.go's own doc comment).
func TestHelpDistinctFromMuted(t *testing.T) {
	for _, dark := range []bool{true, false} {
		st := NewStyles(dark)
		if fg(st.Help) == fg(st.Dim) {
			t.Errorf("dark=%v: Help and Dim resolve to the same foreground", dark)
		}
	}
}

// TestCursorHasBackground: the cell cursor (T3-04's grid renderer) needs a background fill,
// not just a foreground colour the way Selected reinforces a row — a lone foreground change is
// too easy to miss across a whole highlighted cell.
func TestCursorHasBackground(t *testing.T) {
	for _, dark := range []bool{true, false} {
		st := NewStyles(dark)
		if st.Cursor.GetBackground() == nil {
			t.Errorf("dark=%v: Cursor has no background", dark)
		}
		if !st.Cursor.GetBold() {
			t.Errorf("dark=%v: Cursor is not bold", dark)
		}
	}
}
