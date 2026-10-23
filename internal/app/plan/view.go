// This file is layout, theming and rendering: sizes the panes, then draws the header, totals, left list and right pane.
// Moved out of model.go unchanged; see there for the Model itself.

package plan

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

// CapturesText implements app.Screen (via planScreen's thin delegate in internal/app/screen.go).
// The root queries this before treating "q" as its own global quit key.
// huh.Select and huh.MultiSelect both support their own "/" filter-typing mode (GetFiltering),
// but that mode — like Down/Up/Space navigation generally — is only reachable once
// huh.Field.WithKeyMap has been called on the field. That normally happens automatically inside
// a huh.Form/Group; this screen uses both fields standalone (AGENTS.md §4.7, "no layout
// library", ruled out adopting huh.Form just for its wiring), so buildEnvSelect and
// buildMultiSelect call WithKeyMap directly at construction time instead — plain component
// wiring, not a layout dependency. With that in place, query whichever field is actually live
// for its own real filtering state: the env-select prompt while it's up, or the multiSelect
// while it holds focus and no huh.Confirm dialog is covering it.
func (m Model) CapturesText() bool {
	switch {
	case m.state == stateSelectEnv:
		return m.envSelect != nil && m.envSelect.GetFiltering()
	case m.state == stateReady && m.overriding:
		return true // the o dialog's input takes every character, q included
	case m.state == stateReady && m.confirming:
		// The shift+d direct-mode confirm is a huh.Confirm, not a text field,
		// but this was still false while it was open — so ? and l (both gated on
		// !CapturesText() at the root) opened the help overlay or the activity log OVER the
		// dialog instead of being swallowed by it, the same class of bug the o dialog's own
		// case above exists to prevent.
		return true
	case m.state == stateReady && !m.confirming && m.focus == focusLeft:
		return m.multiSelect != nil && m.multiSelect.GetFiltering()
	default:
		return false
	}
}

// SetSize lays every huh field and the viewport out inside width × height.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m.layout()
}

// layout sizes the two panes to the frame's body: the header and totals section take two
// rows, the notes section (when it shows) its own, and the panes share the rest.
func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	inner := m.width - 2
	sections := 2
	notes := m.notes()
	if notes != "" {
		sections++
	}
	body := max(ui.BodyHeight(m.height, sections)-2-strings.Count(notes, "\n")-ui.BoolInt(notes != ""), 3)
	leftWidth := min(max(inner*2/5, 36), 56)
	rightWidth := max(inner-leftWidth-1, 10)
	if leftWidth != m.leftWidth {
		m.leftWidth = leftWidth
		if m.multiSelect != nil {
			m.rebuildMultiSelect()
			m.multiSelect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
		}
	}

	if m.envSelect != nil {
		m.envSelect.WithWidth(inner)
		m.envSelect.WithHeight(body)
	}
	if m.multiSelect != nil {
		m.multiSelect.WithWidth(m.leftWidth)
		// the greyed no-op section (leftBody's own NoOps loop, above Disabled's) takes
		// rows out of the multiSelect's own height budget exactly as Disabled's already did, or
		// the left pane would overflow past what layout() budgeted for it.
		extra := 0
		if n := len(NoOps(m.rows)); n > 0 {
			extra += n + 1
		}
		if n := len(Disabled(m.rows)); n > 0 {
			extra += n + 1
		}
		m.multiSelect.WithHeight(max(body-extra, 3))
	}
	if m.confirmDirect != nil {
		m.confirmDirect.WithWidth(ui.DialogWidth(m.width))
	}
	if m.overrideInput != nil {
		m.overrideInput.WithWidth(ui.DialogWidth(m.width))
	}
	resized := m.viewport.Width() != rightWidth
	m.viewport.SetWidth(rightWidth)
	m.viewport.SetHeight(body)
	if resized && m.state == stateReady {
		// The pane's text is wrapped to its width when set, so a width change re-renders it.
		m = m.refreshRight()
	}
	return m
}

// SetStyles applies the palette (and its dark/light flag to every huh field via huh's own
// Charm theme — AGENTS.md §4.7: no layout library, but a component's own theming is not one).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	theme := huh.ThemeFunc(huh.ThemeCharm)
	if m.envSelect != nil {
		m.envSelect.WithTheme(theme)
	}
	if m.multiSelect != nil {
		m.multiSelect.WithTheme(theme)
	}
	if m.confirmDirect != nil {
		m.confirmDirect.WithTheme(theme)
	}
	if m.overrideInput != nil {
		m.overrideInput.WithTheme(theme)
	}
	return m.refreshRight()
}

// View renders the current state. Every rendered string passes through redact.Strings
// once more here, at the output boundary, in addition to each render point that already
// calls it (Summary's per-repo Detail, the disabled-row Reason, warning messages,
// viewReady's own fatal-error line) — so a display field that forgets to redact itself,
// or a credential registered after an earlier call already built its string, is still
// caught before it reaches the terminal (AGENTS.md §4.4/§4.10, R-002).
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	m = m.layout()
	var out string
	switch m.state {
	case stateSelectEnv:
		out = m.viewSelectEnv()
	case stateLoading:
		out = m.viewLoading()
	default:
		out = m.viewReady()
	}
	if m.confirming && m.confirmDirect != nil {
		out = ui.Dialog(m.styles, out, "direct mode", m.confirmDirect.View(), m.width, m.height)
	}
	if m.overriding && m.overrideInput != nil {
		out = ui.Dialog(m.styles, out, "override digest", m.overrideBody(), m.width, m.height)
	}
	return redact.Strings(out)
}

func (m Model) title() string {
	if m.showYAML && m.state == stateReady {
		return "hoist · promotion · confirm · yaml"
	}
	return "hoist · promotion · confirm"
}

// KeyScreen implements the root's keyed interface (internal/app/screen.go): the help overlay
// and the activity log ("l") only reach this screen once it names its own registry row.
func (m Model) KeyScreen() keys.Screen { return keys.ScrPlan }

// headerSection is "source → target", the shared image-repo prefix when every row has one,
// and the mode chip: amber and named for a production target, which changes this screen's
// temperature.
func (m Model) headerSection() string {
	target := m.target
	if target == "" {
		target = "?"
	}
	left := m.styles.Title.Render(m.source) + "  →  " + m.styles.Title.Render(target)
	if m.prefix != "" && m.state == stateReady {
		left += m.styles.Dim.Render("   images under " + m.prefix)
	}
	return ui.StatusBar(max(m.width-2, 1), left, m.modeChip())
}

func (m Model) modeChip() string {
	switch {
	case m.envs.IsProduction(m.target):
		return m.styles.Production.Render("mode: PR · production")
	case m.mode == ModeDirect:
		return m.styles.Warn.Render("mode: DIRECT")
	default:
		return m.styles.Accent.Render("mode: PR")
	}
}

// modeLabel is the long form of the mode, for the production refusal's own wording.
func (m Model) modeLabel() string {
	if m.envs.IsProduction(m.target) {
		return fmt.Sprintf("direct mode unavailable: %s is production, so every change goes through a PR", m.target)
	}
	if m.mode == ModeDirect {
		return "mode: DIRECT"
	}
	return "mode: PR"
}

// totalsSection is the promotion in one line, over the ticked set: "3 repos · 41 commits ·
// 3 migrations", with what is still loading or missing said rather than counted as zero.
func (m Model) totalsSection() string {
	if m.state != stateReady || m.err != nil {
		return ""
	}
	ticked := map[string]bool{}
	for _, r := range m.ticked {
		ticked[r] = true
	}
	repos, commits, migrations, loading, missing, unresolved, capped := 0, 0, 0, 0, 0, 0, 0
	for _, r := range m.rows {
		if r.Disabled {
			unresolved++
			continue
		}
		if !ticked[r.Repo] {
			continue
		}
		repos++
		st, asked := m.deltas[r.Repo]
		switch {
		case !asked || m.histFn.Delta == nil:
			missing++
		case !st.Loaded:
			loading++
		case st.Err != nil:
			missing++
		default:
			commits += len(st.Delta.Commits)
			migrations += len(st.Delta.Migrations)
			if st.Delta.MigrationsIncomplete {
				capped++
			}
		}
	}
	parts := []string{m.styles.Title.Render(ui.Plural(repos, "repo") + " ticked")}
	if commits > 0 || (loading == 0 && missing == 0 && repos > 0) {
		parts = append(parts, m.styles.Title.Render(ui.Plural(commits, "commit")))
	}
	if migrations > 0 {
		word := ui.Plural(migrations, "migration")
		if capped > 0 {
			word += " (at least)"
		}
		parts = append(parts, m.styles.Warn.Render(word))
	} else if capped > 0 {
		parts = append(parts, m.styles.Warn.Render(fmt.Sprintf("migrations unknown for %d", capped)))
	}
	// (v2·05b): "N image references, M files" — the plan's own version of deploy.scale's
	// identical count, over the same ticked set every other part of this line counts.
	if refs, files := ImageRefStats(m.plan, ticked); refs > 0 {
		parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("%s, %s", ui.Plural(refs, "image reference"), ui.Plural(files, "file"))))
	}
	if loading > 0 {
		parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("history loading for %d", loading)))
	}
	if missing > 0 {
		parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("no history for %d", missing)))
	}
	if unresolved > 0 {
		parts = append(parts, m.styles.Warn.Render(fmt.Sprintf("%d unresolved, not offered", unresolved)))
	}
	line := strings.Join(parts, m.styles.Dim.Render(" · "))
	right := m.styles.Dim.Render("d  see the yaml")
	if m.showYAML {
		right = m.styles.Dim.Render("d  back to impact")
	}
	return ui.StatusBar(max(m.width-2, 1), line, right)
}

// notes is the section under the panes: the transient notice, or the production skip
// warning (AGENTS.md §4.5; it never blocks, principle 5).
func (m Model) notes() string {
	inner := max(m.width-2, 20)
	switch {
	case m.notice != "":
		return m.styles.Notice.Render(ansi.Wordwrap(m.notice, inner, ""))
	case m.state == stateReady && m.err == nil:
		if skip := m.skipNotice(); skip != "" {
			// (UX-M17): a sentence, never a bare "!" marker.
			return m.styles.Warn.Render(ansi.Wordwrap(skip, inner, ""))
		}
	}
	return ""
}

// hints is the screen's own footer, built through keys.Footer like every other
// migrated screen rather than a hand-joined string, so a narrow terminal drops the lowest-
// priority keys first instead of wrapping or truncating (the audit doc's own Footer rules).
func (m Model) hints() string {
	switch {
	case m.state == stateSelectEnv:
		return keys.Footer(m.styles, m.width, "", []keys.Hint{
			{B: keys.Up, Long: "↑/↓ choose", Short: "↑/↓ choose", Pri: 1},
			{B: keys.Enter, Long: "enter confirm", Pri: 0},
			{B: keys.Esc, Long: "esc back", Pri: -1},
		}, true)
	case m.state == stateLoading:
		return keys.Footer(m.styles, m.width, "", []keys.Hint{{B: keys.Esc, Long: "esc back", Pri: -1}}, true)
	case m.overriding:
		return keys.Footer(m.styles, m.width, "", []keys.Hint{
			{B: keys.Enter, Long: "enter apply", Pri: 0},
			{B: keys.Esc, Long: "esc cancel", Pri: -1},
		}, true)
	}
	hints := []keys.Hint{
		{B: keys.Enter, Long: "enter promote", Pri: 0},
		{B: keys.Space, Long: "space tick", Pri: 1},
		{B: keys.Diff, Long: "d yaml", Pri: 2},
		{B: keys.Edit, Long: "e edit digest", Short: "e digest", Pri: 3},
	}
	if !m.envs.IsProduction(m.target) {
		hints = append(hints, keys.Hint{B: keys.Direct, Long: "shift+d direct", Pri: 4})
	}
	hints = append(hints,
		keys.Hint{B: keys.Refresh, Long: "r fresh origin", Short: "r fresh", Pri: 5},
		keys.Hint{B: keys.Tab, Long: "tab pane", Pri: 6},
		keys.Hint{B: keys.Log, Long: "l activity", Pri: 7},
		keys.Hint{B: keys.Esc, Long: "esc back", Pri: -1},
	)
	return keys.Footer(m.styles, m.width, "", hints, true)
}

// render draws a frame at the screen's size, or a default one before the root has sized it
// (a test calling a view helper directly).
func (m Model) render(title string, sections []string) string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		w, h = 80, 24
	}
	return ui.Frame{Title: title, Sections: sections, Footer: m.hints()}.Render(m.styles, w, h)
}

func (m Model) viewSelectEnv() string {
	sections := []string{m.headerSection()}
	if m.envSelect != nil {
		sections = append(sections, m.envSelect.View())
	}
	if m.err != nil {
		sections = append(sections, m.styles.Bad.Render(m.err.Error()))
	}
	return m.render(m.title(), sections)
}

func (m Model) viewLoading() string {
	return m.render(m.title(), []string{m.headerSection(), m.spinner.View() + " " + m.status})
}

func (m Model) viewReady() string {
	if m.err != nil {
		return m.render(m.title(), []string{m.headerSection(), m.styles.Bad.Render(ansi.Wordwrap(m.err.Error(), max(m.width-2, 20), ""))})
	}
	sections := []string{
		m.headerSection() + "\n" + m.totalsSection(),
		ui.Columns(m.styles, m.leftBody(), m.viewport.View(), m.leftWidth),
	}
	if n := m.notes(); n != "" {
		sections = append(sections, n)
	}
	return m.render(m.title(), sections)
}

// skipNotice is the "deploying straight to production, skipping <staging>" warning
// (AGENTS.md §4.5); it never blocks (principle 5).
func (m Model) skipNotice() string {
	staging, skip := SkippedStaging(m.source, m.target, m.envs)
	if !skip {
		return ""
	}
	return fmt.Sprintf("deploying straight to production, skipping %s", staging)
}

func (m Model) leftBody() string {
	var b strings.Builder
	if m.multiSelect != nil {
		b.WriteString(m.multiSelect.View())
	}
	// (UX-M17, v2·05b): rows already current — nothing to tick, since promoting them
	// would write nothing (Row.NoOp) — shown greyed with the reason, "· <repo> · already
	// current", the mockup's own wording. Listed before the unresolved rows below: a repo with
	// nothing to write is not a failure the way an unresolved digest is.
	if noops := NoOps(m.rows); len(noops) > 0 {
		b.WriteString("\n")
		for _, r := range noops {
			line := fmt.Sprintf("  · %s · already current", strings.TrimPrefix(r.Repo, m.prefix))
			fmt.Fprintf(&b, "%s\n", m.styles.Dim.Render(ansi.Wordwrap(line, max(m.leftWidth, 20), "")))
		}
	}
	if dis := Disabled(m.rows); len(dis) > 0 {
		b.WriteString("\n")
		for _, r := range dis {
			line := fmt.Sprintf("  %s  unresolved: %s", strings.TrimPrefix(r.Repo, m.prefix), redact.Strings(r.Reason))
			fmt.Fprintf(&b, "%s\n", m.styles.Dim.Render(ansi.Wordwrap(line, max(m.leftWidth, 20), "")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// hoveredRow is the row the left pane's cursor is on, when there is one.
func (m Model) hoveredRow() (Row, bool) {
	if m.multiSelect == nil {
		return Row{}, false
	}
	repo, ok := m.multiSelect.Hovered()
	if !ok {
		return Row{}, false
	}
	for _, r := range m.rows {
		if r.Repo == repo {
			return r, true
		}
	}
	return Row{}, false
}

// impactBody is the right pane's default content: what the hovered repo ships. The repo
// and its versions on one line (never wrapped: the version is what is being approved), the
// commit history against what the target declares, the migrations that run, and the
// repo's own warnings as sentences rather than a bare "!".
func (m Model) impactBody() string {
	r, ok := m.hoveredRow()
	if !ok {
		return m.styles.Dim.Render("no repo under the cursor")
	}
	width := m.viewport.Width()
	if width <= 0 {
		width = 40
	}
	var lines []string
	lines = append(lines, m.styles.Title.Render(ansi.Truncate(strings.TrimPrefix(r.Repo, m.prefix), width, "…")))
	lines = append(lines, ansi.Truncate(m.styles.Accent.Render(tagOrDigest(r.Old))+" → "+m.styles.Accent.Render(tagOrDigest(r.New)), width, "…"))
	// "image reference(s)", never "occurrence(s)" — the latter is this codebase's own
	// internal term (AGENTS.md's domain-noun glossary) and reads as jargon on the one screen an
	// operator is about to press enter on (mirrors deploy.scale's identical wording).
	lines = append(lines, m.styles.Dim.Render(ansi.Wrap(fmt.Sprintf("%s · %s · digest from %s", ui.Plural(r.Count, "image reference"), ui.Plural(r.Files, "file"), r.Source), width, "")))
	lines = append(lines, "")
	mapped := m.histFn.Mapped == nil || m.histFn.Mapped(r.Repo)
	if m.histFn.Delta == nil {
		lines = append(lines, m.styles.Dim.Render(ansi.Wrap(fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", r.Repo), width, "")))
	} else {
		st := m.deltas[r.Repo]
		for _, l := range history.Lines(st, tagOrDigest(r.New), tagOrDigest(r.Old), m.target, r.Repo, mapped, 200, -1) {
			switch l.Role {
			case "head":
				lines = append(lines, m.styles.Title.Render(ansi.Wrap(l.Text, width, "")))
			case "migration":
				lines = append(lines, m.styles.Warn.Render("  "+ansi.Truncate(l.Text, width-14, "…")+"  migration"))
			case "commit":
				lines = append(lines, "  "+ansi.Truncate(l.Text, width-2, "…"))
			default:
				lines = append(lines, m.styles.Dim.Render(ansi.Wrap(l.Text, width, "")))
			}
		}
		if st.Loaded && st.Err == nil && len(st.Delta.Migrations) > 0 {
			qualifier := ""
			if st.Delta.MigrationsIncomplete {
				qualifier = " (at least)" // the forge capped the history; this list is a floor
			}
			lines = append(lines, "", m.styles.Warn.Render(ui.Plural(len(st.Delta.Migrations), "migration")+qualifier+" run on this promotion:"))
			for _, f := range st.Delta.Migrations {
				lines = append(lines, m.styles.Dim.Render("  "+ansi.Truncate(f, width-2, "…")))
			}
		}
	}
	if len(r.Warnings) > 0 {
		lines = append(lines, "")
		for _, w := range r.Warnings {
			// (UX-M17): a sentence, never a bare "!" marker.
			lines = append(lines, m.styles.Warn.Render(ansi.Wrap(redact.Strings(w.Message), width, "")))
		}
	}
	if r.Disabled {
		lines = append(lines, "", m.styles.Warn.Render(ansi.Wrap("not offered: "+redact.Strings(r.Reason), width, "")))
	}
	if r.NoOp {
		lines = append(lines, "", m.styles.Dim.Render("already current in "+m.target+": promoting this repo would write nothing"))
	}
	return strings.Join(lines, "\n")
}

// rightBody is the yaml view: the diff of the ticked edits, then the untouched images, the
// plan's warnings and the resolution report — the mechanism, one key away from the impact.
func (m Model) rightBody() string {
	var b strings.Builder
	if m.diff != "" {
		b.WriteString(colourDiff(m.styles, m.diff))
	} else {
		b.WriteString("(no ticked edits)\n")
	}
	fmt.Fprintf(&b, "\nUntouched (%d):\n", len(m.plan.Untouched))
	for _, ref := range m.plan.Untouched {
		fmt.Fprintf(&b, "  %s\n", ref)
	}
	fmt.Fprintf(&b, "\nWarnings (%d):\n", len(m.plan.Warnings))
	for _, w := range m.plan.Warnings {
		fmt.Fprintf(&b, "  [%s] %s\n", w.Code, redact.Strings(strings.ReplaceAll(w.Message, "\n", "\n  ")))
	}
	b.WriteString("\nResolution:\n")
	for _, line := range Summary(m.outcome) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

// colourDiff colours a unified diff's added and removed lines and dims its headers.
func colourDiff(st ui.Styles, diff string) string {
	lines := strings.Split(diff, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "@@"):
			lines[i] = st.Dim.Render(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = st.Add.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = st.Del.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// CommonPrefix is the image-repo prefix every row shares, up to and including its last
// "/", so the repo names on the rows can drop it and never wrap mid-token (#85: the version
// you are approving was being split across lines at an arbitrary column). "" when the rows
// share nothing, or there is only one row (no prefix is shorter than the whole name).
func CommonPrefix(rows []Row) string {
	if len(rows) < 2 {
		return ""
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Repo)
	}
	sort.Strings(names)
	first, last := names[0], names[len(names)-1]
	i := 0
	for i < len(first) && i < len(last) && first[i] == last[i] {
		i++
	}
	common := first[:i]
	if cut := strings.LastIndex(common, "/"); cut >= 0 {
		return common[:cut+1]
	}
	return ""
}
