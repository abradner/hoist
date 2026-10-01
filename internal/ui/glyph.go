package ui

import "fmt"

// StepState is one step's state in a pipeline strip (a promotion's branch/commit/push/…/rollout
// steps, a restart target's own progress) — the shape the flight screen and the matrix's
// in-flight rows share.
type StepState uint8

const (
	// StepDone is a completed step.
	StepDone StepState = iota
	// StepActive is the one step currently running.
	StepActive
	// StepPending is a step not yet reached.
	StepPending
	// StepFailed is a step that ended in error.
	StepFailed
	// StepWaiting is a step blocked on something outside hoist (a human's approval comment, a
	// check run) rather than actively failed or simply not yet reached.
	StepWaiting
)

// StepGlyph returns the one glyph set every step strip in this app should use (the audit
// doc): ✓ done, ◐ active, · pending, ✗ failed, ⏸ waiting. It retires the older,
// inconsistent ●/◍/○/⟳ glyphs different screens picked independently. The check that nothing
// drifts back is `git grep -n '"[●◍○⟳]"' internal/ui`, which must stay empty.
func StepGlyph(s StepState) string {
	switch s {
	case StepDone:
		return "✓"
	case StepActive:
		return "◐"
	case StepFailed:
		return "✗"
	case StepWaiting:
		return "⏸"
	default:
		return "·"
	}
}

// Step renders one step's glyph plus label, styled by state: Good for done, Accent for active
// (the one step currently moving), Warn for waiting (a human or an external check, not itself
// a failure), Bad for failed, Dim for a step not yet reached.
func (st Styles) Step(s StepState, label string) string {
	glyph := StepGlyph(s)
	text := glyph
	if label != "" {
		text = glyph + " " + label
	}
	switch s {
	case StepDone:
		return st.Good.Render(text)
	case StepActive:
		return st.Accent.Render(text)
	case StepFailed:
		return st.Bad.Render(text)
	case StepWaiting:
		return st.Warn.Render(text)
	default:
		return st.Dim.Render(text)
	}
}

// Plural renders a count with a word that only takes a bare "s" — "1 Deployment", "2
// Deployments" — replacing every screen's own "Deployment(s)" (AGENTS.md §9,
// the audit doc's UX-L2) with one place that gets English right for both n=1 and n=0
// ("0 Deployments", never "0 Deployment(s)").
func Plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}
