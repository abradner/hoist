// Package ui holds what every hoist screen shares: one Styles palette built from a
// light/dark flag, and the status-bar line helper. It imports Lip Gloss only — no Bubbles
// components, no screen state — so any screen package can depend on it without a cycle.
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
}

// NewStyles returns the palette for a dark or light background.
func NewStyles(dark bool) Styles {
	ld := lipgloss.LightDark(dark)
	accent := ld(lipgloss.Color("62"), lipgloss.Color("212"))
	muted := ld(lipgloss.Color("240"), lipgloss.Color("245"))
	notice := ld(lipgloss.Color("166"), lipgloss.Color("214"))
	return Styles{
		Dark:     dark,
		Header:   lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Bold(true).Foreground(accent),
		Status:   lipgloss.NewStyle().Foreground(muted),
		Notice:   lipgloss.NewStyle().Bold(true).Foreground(notice),
		Hint:     lipgloss.NewStyle().Foreground(muted),
		Help:     lipgloss.NewStyle().Foreground(muted),
	}
}
