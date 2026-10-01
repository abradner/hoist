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
	// Help styles the expanded help line toggled by ? — its own shade, distinct
	// from Dim's grey (previously identical), so the two read as different things when a
	// screen shows both at once (the help overlay's own body over a dimmed frame).
	Help lipgloss.Style

	// Chrome (M10). Border colours every frame and pane edge; Title the name in a frame's
	// top edge; Rule a section divider's own text, when it carries one.
	Border, Title, Rule lipgloss.Style
	// Semantic colours, reinforcing a word rather than replacing it (the state is always
	// spelled out; colour is the second channel). Good: pinned, green CI, "in staging".
	// Warn: drifted, split, a migration, a blocked step. Bad: a failed step, an error line.
	// Dim: external, an unreached step, "…10 more". Accent: the cursor, an id, a command
	// the operator should type. Production: its own hue, distinct from Warn — a
	// production target is a fact about the env, not a warning about the current change.
	Good, Warn, Bad, Dim, Accent, Production lipgloss.Style
	// Info is its own colour, split out of what used to be Notice/Warn/Production
	// sharing one amber: a plain "this happened" report (a promotion started, a URL shown),
	// never itself a warning or a failure. Notice keeps its own field and colour — the
	// transient-message role frame.go's NoticeLines still defaults to — so existing callers
	// are unaffected; Info is what a caller now picks explicitly to colour by kind (the
	// root's activity row, AGENTS.md §4.8/§9 — ui.NoticeLinesStyled takes the style a caller
	// chose instead of always reaching for Notice).
	Info lipgloss.Style
	// Cursor is the cell-cursor style the matrix's grid renderer needs: a background fill
	// (not just a foreground colour, the way Selected reinforces a
	// row) plus bold, so a single highlighted cell reads clearly against a table of many.
	Cursor lipgloss.Style
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
	// Info, Production and Help/Muted each get their own hue instead of sharing Warn's
	// amber or Dim's grey (the audit doc's own list of what today collapses to one colour).
	// info is a calm blue — a plain report, never itself an exception. production is the
	// mockups' own violet (docs/tui/mockups.html's `.c-prod`, #d9a0f0), picked specifically to
	// read as "a fact about this env" rather than "something is wrong" the way Warn's amber
	// would. help is a step lighter than muted so the two are still distinguishable side by
	// side (the help overlay's own body drawn over a dimmed frame).
	info := ld(lipgloss.Color("25"), lipgloss.Color("117"))
	production := ld(lipgloss.Color("91"), lipgloss.Color("183"))
	help := ld(lipgloss.Color("244"), lipgloss.Color("250"))
	cursorBG := ld(lipgloss.Color("189"), lipgloss.Color("24"))
	cursorFG := ld(lipgloss.Color("0"), lipgloss.Color("255"))
	return Styles{
		Dark:       dark,
		Header:     lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:       lipgloss.NewStyle().Padding(0, 1),
		Selected:   lipgloss.NewStyle().Bold(true).Foreground(accent),
		Status:     lipgloss.NewStyle().Foreground(muted),
		Notice:     lipgloss.NewStyle().Bold(true).Foreground(notice),
		Hint:       lipgloss.NewStyle().Foreground(muted),
		Help:       lipgloss.NewStyle().Foreground(help),
		Border:     lipgloss.NewStyle().Foreground(border),
		Title:      lipgloss.NewStyle().Bold(true),
		Rule:       lipgloss.NewStyle().Foreground(muted),
		Good:       lipgloss.NewStyle().Foreground(good),
		Warn:       lipgloss.NewStyle().Foreground(warn),
		Bad:        lipgloss.NewStyle().Foreground(bad),
		Dim:        lipgloss.NewStyle().Foreground(muted),
		Accent:     lipgloss.NewStyle().Foreground(accent),
		Production: lipgloss.NewStyle().Bold(true).Foreground(production),
		Info:       lipgloss.NewStyle().Bold(true).Foreground(info),
		Cursor:     lipgloss.NewStyle().Bold(true).Background(cursorBG).Foreground(cursorFG),
		Add:        lipgloss.NewStyle().Foreground(good),
		Del:        lipgloss.NewStyle().Foreground(bad),
	}
}
