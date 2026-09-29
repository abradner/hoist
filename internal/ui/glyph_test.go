package ui

import "testing"

// TestStepGlyphSet pins the exact glyph set T3-02 introduces, retiring the older ●/◍/○/⟳ set
// different screens picked independently (AGENTS.md §9).
func TestStepGlyphSet(t *testing.T) {
	cases := []struct {
		s    StepState
		want string
	}{
		{StepDone, "✓"},
		{StepActive, "◐"},
		{StepPending, "·"},
		{StepFailed, "✗"},
		{StepWaiting, "⏸"},
	}
	seen := map[string]StepState{}
	for _, c := range cases {
		if got := StepGlyph(c.s); got != c.want {
			t.Errorf("StepGlyph(%v) = %q, want %q", c.s, got, c.want)
		}
		if other, ok := seen[c.want]; ok {
			t.Errorf("glyph %q used by both %v and %v", c.want, other, c.s)
		}
		seen[c.want] = c.s
	}
}

// TestStepStylesDiffer: each state should render through a different style, so the strip
// carries state in colour as well as glyph (T3-02's Step method).
func TestStepStylesDiffer(t *testing.T) {
	st := NewStyles(true)
	states := []StepState{StepDone, StepActive, StepPending, StepFailed, StepWaiting}
	seen := map[string]StepState{}
	for _, s := range states {
		out := st.Step(s, "label")
		if other, ok := seen[out]; ok {
			t.Errorf("Step(%v) renders identically to Step(%v): %q", s, other, out)
		}
		seen[out] = s
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		one  string
		want string
	}{
		{0, "Deployment", "0 Deployments"},
		{1, "Deployment", "1 Deployment"},
		{2, "Deployment", "2 Deployments"},
		{1, "migration", "1 migration"},
		{14, "migration", "14 migrations"},
	}
	for _, c := range cases {
		if got := Plural(c.n, c.one); got != c.want {
			t.Errorf("Plural(%d, %q) = %q, want %q", c.n, c.one, got, c.want)
		}
	}
}
