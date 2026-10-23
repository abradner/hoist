package matrix

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

const (
	minWidth  = 40
	minHeight = 8
)

// View renders the frame: the subheader, the grid, the notes, the in-flight pane (inside the
// frame) and the footer, with the menu or a chooser dialog over it when one is open.
func (m Model) View() string {
	if m.width > 0 && m.height > 0 && (m.width < minWidth || m.height < minHeight) {
		return fmt.Sprintf("window too small: %d×%d, hoist needs at least %d×%d", m.width, m.height, minWidth, minHeight)
	}
	if len(m.matrix.Envs) == 0 {
		return redact.Strings(m.emptyView())
	}
	m = m.layout()
	frame := ui.Frame{Title: m.title(), Sections: []string{m.subheader(), m.gridSection()}, Footer: m.statusBar()}
	if notes := m.notes(); notes != "" {
		frame.Sections = append(frame.Sections, notes)
	}
	if d := m.detailLines(); len(d) > 0 {
		frame.Sections = append(frame.Sections, strings.Join(d, "\n"))
	}
	if pane := m.inflightPane(m.paneBudget()); pane != "" {
		// the in-flight pane is a Section of this SAME frame now, not a separate
		// ui.Box stacked below it (Frame.Panes, retired) — the mockups draw it inside the one
		// box, ending at the frame's own closing border.
		frame.Sections = append(frame.Sections, pane)
	}
	view := redact.Strings(frame.Render(m.styles, m.width, m.height))
	if m.menuOpen {
		title := fmt.Sprintf("%s · %s", orNoEnv(m.menuFamily), m.menuEnv)
		if m.IsProduction(m.menuEnv) {
			title += productionMarker
		}
		return ui.Dialog(m.styles, view, title, m.menuView(), m.width, m.height)
	}
	if m.chooser != nil {
		title := "deploy"
		if m.chooserKind == chooserOpenPR {
			title = "open PR"
		}
		return ui.Dialog(m.styles, view, title, redact.Strings(m.chooser.View()), m.width, m.height)
	}
	if m.confirmAbandon != nil {
		return ui.Dialog(m.styles, view, "abandon", m.confirmAbandon.View(), m.width, m.height)
	}
	return view
}

// subheader is the one-line row under the title: the promotable root, the base/context, and
// (120-column layout) the env/family counts.
func (m Model) subheader() string {
	line := displayRoot(m.repo.Root)
	if m.base != "" && m.base != "main" {
		line += " · base " + m.base
	} else {
		line += " · base main"
	}
	if m.kubeContext != "" {
		line += " · context " + m.kubeContext
	}
	if m.width >= 110 {
		line += fmt.Sprintf(" · %s · %s", ui.Plural(len(m.matrix.Envs), "env"), pluralFamilies(len(m.matrix.Rows)))
	}
	return line
}

// pluralFamilies is ui.Plural's own "%ss" rule corrected for family's irregular plural
// ("families", never "familys") — the one noun on this screen ui.Plural cannot be used for
// as-is.
func pluralFamilies(n int) string {
	if n == 1 {
		return "1 family"
	}
	return fmt.Sprintf("%d families", n)
}

// emptyView is what the matrix shows when the repo has no envs at all (v2·06b, UX-M18): a
// single-section frame naming where hoist looked and what to fix, rather than a blank table
// with nothing to say about it. Deviation from the mockup: it names m.repo.Root (the only
// path this screen actually holds) rather than the live apps_root value, which is
// cmd/hoist's own config concern and not plumbed into matrix.Model (out of this file's
// scope) — the guidance still names the config key and flag an operator would check, per
// UX-M18's own wording.
func (m Model) emptyView() string {
	body := []string{
		"",
		fmt.Sprintf(" no environments discovered under %s", displayRoot(m.repo.Root)),
		"",
		" hoist reads Argo CD Application wrappers under the apps root and names",
		" each env by its spec.destination.namespace. Check repos[].apps_root in",
		" the config, or pass --apps-root / --repo.",
		"",
		" c shows the config hoist loaded",
	}
	footer := keys.Footer(m.styles, m.width, "", []keys.Hint{
		{B: keys.Refresh, Long: "r refresh", Short: "r refresh", Pri: 0},
		{B: keys.Config, Long: "c config", Short: "c config", Pri: 1},
		{B: keys.Quit, Long: "q quit", Short: "q quit", Pri: 0},
	}, true)
	return ui.Frame{Title: m.title(), Sections: []string{strings.Join(body, "\n")}, Footer: footer}.Render(m.styles, m.width, m.height)
}

func (m Model) title() string {
	return "hoist · matrix · " + displayRoot(m.repo.Root)
}

func (m Model) gridSection() string {
	disp := m.displayTable()
	widths := m.gridWidths()
	lines := Grid(m.styles, disp, GridState{Row: m.row, Col: m.col, Offset: m.offset, Height: m.gridHeight(), Widths: widths, Focus: m.focus})
	return strings.Join(lines, "\n")
}

// displayTable is the Table Grid renders: env names decorated with the cursor marker and the
// production warning (Grid itself has no config to consult), and each cell's text already
// formatted (text plus its right-aligned state word) to its column's fitted width, since Grid
// only lays out plain strings.
func (m Model) displayTable() Table {
	src := m.matrix
	widths := m.gridWidths()
	disp := Table{Envs: make([]string, len(src.Envs)), Rows: make([]Row, len(src.Rows))}
	for i, e := range src.Envs {
		name := strings.ToUpper(e)
		if i == m.col {
			name = selectedMarker + name
		}
		if m.IsProduction(e) {
			name += productionMarker
		}
		disp.Envs[i] = name
	}
	for ri, r := range src.Rows {
		row := Row{Family: r.Family, Cells: make([]Cell, len(r.Cells))}
		for ci, c := range r.Cells {
			if !c.Present {
				continue
			}
			word := m.stateWord(c, src.Envs[ci])
			row.Cells[ci] = Cell{Present: true, Text: formatCell(c.Text, word, widths[ci+1])}
		}
		disp.Rows[ri] = row
	}
	return disp
}

// formatCell lays out one cell's text with its state word right-aligned to width, truncating
// the tag first (the state word is what tells a promotion apart from a problem).
func formatCell(text, word string, width int) string {
	if word == "" {
		return ansi.Truncate(text, width, "…")
	}
	room := width - ansi.StringWidth(word) - 2
	if room < 1 {
		return ansi.Truncate(word, width, "…")
	}
	t := ansi.Truncate(text, room, "…")
	return fmt.Sprintf("%-*s  %s", room, t, word)
}

// gridWidths is FAMILY's width, then each env column's, fitted to the terminal (fitWidths).
func (m Model) gridWidths() []int {
	t := m.matrix
	famW := len("FAMILY")
	for _, r := range t.Rows {
		famW = max(famW, ansi.StringWidth(selectedMarker)+ansi.StringWidth(r.Family))
	}
	widths := make([]int, 0, len(t.Envs)+1)
	widths = append(widths, famW)
	for i, e := range t.Envs {
		title := strings.ToUpper(e)
		if i == m.col {
			title = selectedMarker + title
		}
		if m.IsProduction(e) {
			title += productionMarker
		}
		w := ansi.StringWidth(title)
		tw, sw := m.cellWidths(i)
		w = max(w, tw+2+sw)
		widths = append(widths, min(w, maxCellWidth))
	}
	return fitWidths(widths, max(m.width-2, 0))
}

// minCellWidth is the narrowest an env column shrinks to before the grid simply overflows.
const minCellWidth = 10

// fitWidths shrinks the widest env columns, one cell at a time, until the grid fits width.
// The family column keeps its natural width.
//
// total must also count the "│" grid.go's own dataRow/ruleRow join every
// column with — one separator between each pair of columns, len(out)-1 of them — not just each
// column's own text-plus-padding. Missing that made fitWidths believe a row fit when it was
// actually len(out)-1 cells too wide, so the row Frame received was wider than the terminal;
// Frame's own line cropping then ate into whatever sat at the row's right edge, which is the
// last column's right-aligned state word ("pinn…", "extern…", "spl…" at 80 columns — the ref
// should truncate before the state word ever does, per formatCell's own room calculation, but
// that calculation was never reached because the column was never actually shrunk to make
// room).
func fitWidths(widths []int, width int) []int {
	out := append([]int(nil), widths...)
	total := func() int {
		n := 0
		for _, w := range out {
			n += w + cellPad*2
		}
		if len(out) > 1 {
			n += len(out) - 1 // the "│" separators between columns
		}
		return n
	}
	for total() > width {
		widest := -1
		for i := 1; i < len(out); i++ {
			if out[i] > minCellWidth && (widest < 0 || out[i] > out[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		out[widest]--
	}
	return out
}

func (m Model) cellWidths(i int) (text, state int) {
	for _, r := range m.matrix.Rows {
		c := r.Cells[i]
		text = max(text, ansi.StringWidth(c.Text))
		state = max(state, ansi.StringWidth(m.stateWord(c, m.matrix.Envs[i])))
	}
	return text, state
}

func (m Model) stateWord(c Cell, env string) string {
	if !c.Present {
		return ""
	}
	if m.pending[env] && (c.State == StatePinned || c.State == StateUnpinned) {
		return "resolving…"
	}
	return string(c.State)
}

// hasPane reports whether the in-flight pane occupies a Section at all (it is one of
// Frame's own Sections now, not a separate Pane appended below the box — see View), so its own
// inter-section rule has to be counted alongside the subheader's and the notes' whenever one is
// present. It says nothing about whether the pane ultimately finds room to render non-empty at
// the current height — paneBudget/inflightPane decide that — so a height too tight for even the
// compact form is, at worst, one row off here; the same approximation the earlier code lived
// with when the pane cost 0 rather than 1 extra rule row.
func (m Model) hasPane() bool {
	return len(m.inflight) > 0 || m.inflightErr != ""
}

// detailLines is the detail pane's own content, "" when the terminal is too narrow for it or
// there is nothing to show — the same lines View appends as a Section, computed once here so
// gridHeight/paneBudget size around it instead of guessing its height.
func (m Model) detailLines() []string {
	if m.width < detailMinWidth {
		return nil
	}
	return Detail(m.matrix, m.CurrentFamily(), m.CurrentEnv(), m.IsProduction(m.CurrentEnv()), m.inflight)
}

// gridHeight is how many data rows the grid draws — the terminal's own body height, less the
// subheader, the notes, the detail pane, the in-flight pane and the grid's own 3 fixed rows
// (top rule, header, mid rule).
func (m Model) gridHeight() int {
	sections := 2 // title's subheader + grid, at least
	notes := m.notes()
	if notes != "" {
		sections++
	}
	detail := m.detailLines()
	if len(detail) > 0 {
		sections++
	}
	if m.hasPane() {
		sections++
	}
	rows := ui.BodyHeight(m.height, sections) - 1 /* subheader */
	rows -= lipgloss.Height(notes) * ui.BoolInt(notes != "")
	rows -= len(detail)
	rows -= m.paneRows(m.paneBudget())
	return max(rows-3, 1)
}

// paneBudget is how many rows the in-flight pane may take: what is left after the frame's
// chrome (the subheader plus the grid section, always present, the base notes and the detail
// pane when there are any, and each one's own inter-section rule) and a grid tall enough to
// keep its families on screen.
func (m Model) paneBudget() int {
	notes := len(m.baseNotes())
	detail := len(m.detailLines())
	sections := 2
	if notes > 0 {
		sections++
	}
	if detail > 0 {
		sections++
	}
	if m.hasPane() {
		sections++
	}
	rows := ui.BodyHeight(m.height, sections) - notes - detail - 1 /* subheader */
	grid := len(m.matrix.Rows) + 3                                 // + top rule, header row, header/body rule
	return rows - max(grid, minTableRows+3)
}

func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	height := m.gridHeight()
	if m.row < m.offset {
		m.offset = m.row
	}
	if m.row >= m.offset+height {
		m.offset = m.row - height + 1
	}
	maxOffset := max(len(m.matrix.Rows)-height, 0)
	m.offset = clamp(m.offset, maxOffset)
	if m.chooser != nil {
		m.chooser.WithWidth(max(m.width-8, 20))
	}
	return m
}

// notes is the section under the grid: the transient notice or hint first, else what the
// cluster said about the cursor's column, then the in-flight fold when the pane itself has no
// room (paneBudget's own decision). The bubbles help.Model line this used to carry is retired —
// the root's own overlay (internal/ui/keys.HelpView) replaces it entirely.
func (m Model) notes() string {
	lines := m.baseNotes()
	if m.paneRows(m.paneBudget()) == 0 {
		if l := m.inflightLine(); l != "" {
			lines = append(lines, ansi.Truncate(l, max(m.width-2, 1), "…"))
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) baseNotes() []string {
	inner := max(m.width-2, 1)
	var lines []string
	switch {
	case m.refreshingRepo:
		lines = append(lines, m.styles.Notice.Render(fmt.Sprintf("re-reading origin/%s…", m.baseName())))
	case m.notice != "":
		lines = append(lines, m.styles.Notice.Render(ansi.Wordwrap(m.notice, inner, "")))
	case m.hint != "":
		lines = append(lines, m.styles.Hint.Render(m.hint))
	default:
		env := m.CurrentEnv()
		if env != "" && m.IsProduction(env) {
			lines = append(lines, m.styles.Production.Render(fmt.Sprintf("⚠ %s is a production env: writes there always open a PR", env)))
		}
		switch {
		case m.pending[env]:
			lines = append(lines, m.styles.Dim.Render(fmt.Sprintf("… asking the cluster what %s is running", env)))
		case m.driftErr[env] != "":
			lines = append(lines, m.styles.Warn.Render(ansi.Wordwrap(fmt.Sprintf("! %s: cluster not asked: %s", env, m.driftErr[env]), inner, "")))
		default:
			for _, d := range m.driftLines(env) {
				lines = append(lines, m.styles.Warn.Render(ansi.Wordwrap(d, inner, "")))
			}
		}
	}
	return lines
}

func (m Model) driftLines(env string) []string {
	col := -1
	for i, e := range m.matrix.Envs {
		if e == env {
			col = i
		}
	}
	if col < 0 {
		return nil
	}
	const capLines = 3
	var out []string
	cursor := clamp(m.row, max(len(m.matrix.Rows)-1, 0))
	order := make([]int, 0, len(m.matrix.Rows))
	if cursor >= 0 && cursor < len(m.matrix.Rows) {
		order = append(order, cursor)
	}
	for i := range m.matrix.Rows {
		if i != cursor {
			order = append(order, i)
		}
	}
	for _, i := range order {
		c := m.matrix.Rows[i].Cells[col]
		if c.State != StateDrifted {
			continue
		}
		if len(out) == capLines {
			out = append(out, m.styles.Dim.Render("…and more drift in this env"))
			break
		}
		suffix := ""
		if c.Compared != "" {
			suffix = " (compared " + c.Compared + ")"
		}
		out = append(out, fmt.Sprintf("! %s runs %s in %s; manifest says %s%s", m.matrix.Rows[i].Family, c.Running, env, c.Text, suffix))
	}
	return out
}

const selectedMarker = "▸ "

const productionMarker = " ⚠"

// statusBar is the matrix's own footer, through keys.Footer: the env under the
// cursor on the left, the writes and verbs an operator is actually looking for the key of on
// the right, in priority order so a narrow terminal drops the least useful first. The cursor's
// own production marker is productionMarker — the same "⚠" the header
// and the notes sentence already use — rather than the spelled-out " (production)" this used to
// read: at 120 columns the write-verb hints leave the status too little room, and
// ui.StatusBar's own truncation-with-ellipsis rule was cutting "env b (production)" down to
// "env b (produc…", a fragment that reads worse than no suffix at all. The shorter marker fits,
// and the notes sentence below is still what actually spells "production" out for the operator.
func (m Model) statusBar() string {
	env := m.CurrentEnv()
	status := "env " + orNoEnv(env)
	if env != "" && m.IsProduction(env) {
		status = "env " + env + productionMarker
	}
	target := m.CurrentEnv()
	promoteLong := "p promote into"
	if target != "" {
		promoteLong = "p promote into " + target
	}
	hints := []keys.Hint{
		{B: keys.Enter, Long: "enter actions", Short: "enter actions", Pri: 0},
		{B: keys.Promote, Long: promoteLong, Short: "p promote", Pri: 1},
		{B: keys.Tag, Long: "t deploy tag", Short: "t tag", Pri: 2},
		{B: keys.Watch, Long: "w watch", Short: "w watch", Pri: 3},
		{B: keys.Refresh, Long: "r refresh", Short: "r refresh", Pri: 4},
		{B: keys.Restart, Long: "shift+r restart", Short: "shift+r restart", Pri: 6},
	}
	// "tab in flight" is only true when there is an in-flight pane to focus (toggleFocus is a
	// no-op otherwise) — advertising it with nothing in flight is a hint for a key that
	// does nothing.
	if len(m.inflight) > 0 {
		hints = append(hints, keys.Hint{B: keys.Tab, Long: "tab in flight", Short: "tab in flight", Pri: 7})
	}
	hints = append(hints, keys.Hint{B: keys.Quit, Long: "q quit", Short: "q quit", Pri: 0})
	// help true: the root's own overlay — Footer appends "? help"/"? more" itself.
	return keys.Footer(m.styles, m.width, status, hints, true)
}

func orNoEnv(env string) string {
	if env == "" {
		return "(none)"
	}
	return env
}
