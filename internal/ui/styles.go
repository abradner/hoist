// Package ui holds what every hoist screen shares: one Styles palette built from a
// light/dark flag, the frame and pane chrome every screen is drawn in (frame.go), the dialog
// compositor (dialog.go), relative-time wording (time.go) and the status-bar line helper. It
// imports Lip Gloss and x/ansi (cell width and ANSI stripping) — no Bubbles components, no
// screen state — so any screen package can depend on it without a cycle.
package ui

import "charm.land/lipgloss/v2"

// Styles is the palette for one terminal background. The root model builds it once from
// tea.BackgroundColorMsg and pushes it into every screen; screens never pick colours
// themselves, so a theme change is one call.
type Styles struct {
	// Dark records which background the palette was built for.
	Dark bool
	// Header styles a table header cell, Cell an ordinary cell, Selected the cursor row.
	Header, Cell, Selected lipgloss.Style
	// Status styles the status-bar summary, Notice a transient message shown in its place,
	// Hint the key hints on the right of the bar.
	Status, Notice, Hint lipgloss.Style
	// Help styles the expanded help line toggled by ?.
	Help lipgloss.Style

	// Chrome (M10). Border colours every frame and pane edge; Title the name in a frame's
	// top edge; Rule a section divider's own text, when it carries one.
	Border, Title, Rule lipgloss.Style
	// Semantic colours, reinforcing a word rather than replacing it (the state is always
	// spelled out; colour is the second channel). Good: pinned, green CI, "in staging".
	// Warn: drifted, split, a migration, a blocked step. Bad: a failed step, an error line.
	// Dim: external, an unreached step, "…10 more". Accent: the cursor, an id, a command
	// the operator should type. Production: the amber temperature a production target
	// gives a header or a mode chip.
	Good, Warn, Bad, Dim, Accent, Production lipgloss.Style
	// Add and Del colour the + and - lines of a diff.
	Add, Del lipgloss.Style
}

// NewStyles returns the palette for a dark or light background.
func NewStyles(dark bool) Styles {
	ld := lipgloss.LightDark(dark)
	accent := ld(lipgloss.Color("62"), lipgloss.Color("212"))
	muted := ld(lipgloss.Color("240"), lipgloss.Color("245"))
	notice := ld(lipgloss.Color("166"), lipgloss.Color("214"))
	border := ld(lipgloss.Color("245"), lipgloss.Color("240"))
	good := ld(lipgloss.Color("28"), lipgloss.Color("78"))
	warn := ld(lipgloss.Color("166"), lipgloss.Color("214"))
	bad := ld(lipgloss.Color("160"), lipgloss.Color("203"))
	return Styles{
		Dark:       dark,
		Header:     lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:       lipgloss.NewStyle().Padding(0, 1),
		Selected:   lipgloss.NewStyle().Bold(true).Foreground(accent),
		Status:     lipgloss.NewStyle().Foreground(muted),
		Notice:     lipgloss.NewStyle().Bold(true).Foreground(notice),
		Hint:       lipgloss.NewStyle().Foreground(muted),
		Help:       lipgloss.NewStyle().Foreground(muted),
		Border:     lipgloss.NewStyle().Foreground(border),
		Title:      lipgloss.NewStyle().Bold(true),
		Rule:       lipgloss.NewStyle().Foreground(muted),
		Good:       lipgloss.NewStyle().Foreground(good),
		Warn:       lipgloss.NewStyle().Foreground(warn),
		Bad:        lipgloss.NewStyle().Foreground(bad),
		Dim:        lipgloss.NewStyle().Foreground(muted),
		Accent:     lipgloss.NewStyle().Foreground(accent),
		Production: lipgloss.NewStyle().Bold(true).Foreground(warn),
		Add:        lipgloss.NewStyle().Foreground(good),
		Del:        lipgloss.NewStyle().Foreground(bad),
	}
}
