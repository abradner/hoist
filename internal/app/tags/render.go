package tags

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
)

// Fixed rows the frame spends outside the tag table: the meta section's lines and the
// commit pane's, so pageSize and the render agree on what is visible.
const (
	paneRowsWide   = 8 // head + up to 7 commit lines, when the terminal is tall
	paneRowsNarrow = 4
	minPageSize    = 3
)

// paneRows is how many lines the commit pane gets: none when there is no way to have one,
// one for a gap sentence, else a share of the height.
func (m Model) paneRows() int {
	if m.histFn.Delta == nil || m.declared == nil {
		return 1
	}
	if m.height >= 30 {
		return paneRowsWide
	}
	return paneRowsNarrow
}

// metaLines is the meta section's own line count: the repo→target line, the declared line
// when there is one, the staging note when there is one, the filter line when active.
func (m Model) metaLines() int {
	n := 1
	if m.declared != nil {
		n += lipgloss.Height(m.declaredLine()) // a split env's line may wrap
	}
	if m.hasStagingMismatch {
		n += lipgloss.Height(m.wrap(m.stagingNote()))
	}
	if m.filtering || m.filterQuery != "" {
		n++
	}
	return n
}

// pageSize is how many tag rows the table shows and fetchVisible keeps warm around the
// cursor: what the frame has left after its chrome, the meta section and the commit pane —
// with a floor, and a fixed default before the screen has a size at all (a snapshot test).
func (m Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	body := ui.BodyHeight(m.height, 3) - m.metaLines() - m.paneRows() - 1 // header row
	if m.notice != "" {
		body--
	}
	return max(body-m.dividerRows(), minPageSize)
}

// dividerRows is how many table rows the dividers can take from the page: one per group
// present beyond the release group (#91), and one for the "unordered" rule when a mapped
// repo has a release tag with no git date. Counted over the whole filtered list rather than
// the window — the window depends on the page size — so a divider outside the window
// over-reserves a row rather than pushing the cursor's row off the bottom of the frame.
func (m Model) dividerRows() int {
	classes := map[Class]bool{}
	unordered := false
	for _, r := range m.filtered() {
		classes[r.Class] = true
		if m.mapped && r.Class == ClassRelease && !r.HasGitDate {
			unordered = true
		}
	}
	n := 0
	for c := range classes {
		if c != ClassRelease {
			n++
		}
	}
	if unordered {
		n++
	}
	return n
}

// visibleWindow computes the half-open [start,end) slice of rows this screen keeps warm and
// draws, centered on (or otherwise including) the cursor — the single source of truth both
// fetchVisible (what gets a MetaFunc call) and the render share, so the two can never drift
// apart (AGENTS.md §8, layered checks: one definition of "visible", not two that can disagree
// about what's on screen). rows is the caller's own m.filtered() result, passed in rather
// than recomputed here so a caller that already has it doesn't pay for it twice.
func (m Model) visibleWindow(rows []Row) (start, end int) {
	if len(rows) == 0 {
		return 0, 0
	}
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		idx = 0
	}
	page := m.pageSize()
	start = idx - page/2
	if start < 0 {
		start = 0
	}
	end = start + page
	if end > len(rows) {
		end = len(rows)
		start = max(0, end-page)
	}
	return start, end
}

// View renders the current state in the M10 frame. Every rendered string passes through
// redact.Strings once more here (AGENTS.md §4.4/R-002), matching plan.Model.View's own
// final-boundary pattern.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		// A screen the root has not sized yet (a test calling View directly) still renders
		// something readable rather than nothing.
		m.width, m.height = 80, 24
	}
	var out string
	switch {
	case m.err != nil:
		out = m.viewErr()
	case m.state == stateLoading:
		out = m.viewLoading()
	case m.reading:
		out = m.viewReading()
	default:
		out = m.viewReady()
	}
	return redact.Strings(out)
}

func (m Model) title() string { return fmt.Sprintf("hoist · tags · %s", m.target) }

// KeyScreen implements the root's keyed interface (internal/app/screen.go).
// this always answered ScrTags, even while m.reading showed the commit-detail pane — so ? (the
// help overlay) named the list's own keys (Right, Filter, Tab…) while none of them did
// anything and the reader's actual keys (Home/End/PgUp/PgDn/Left/Enter) were nowhere on it.
func (m Model) KeyScreen() keys.Screen {
	if m.reading {
		return keys.ScrTagsReader
	}
	return keys.ScrTags
}

// metaSection is the frame's first section: the repo and target, what the env declares
// today and for how long, the staging note, and the filter line.
func (m Model) metaSection() string {
	lines := []string{m.styles.Title.Render(m.imageRepo) + "  →  " + m.styles.Title.Render(m.target) + m.productionChip()}
	if m.declared != nil {
		lines = append(lines, m.declaredLine())
	}
	if m.hasStagingMismatch {
		lines = append(lines, m.styles.Notice.Render(m.wrap(m.stagingNote())))
	}
	switch {
	case m.filtering:
		lines = append(lines, "filter: "+m.filterInput.View())
	case m.filterQuery != "":
		lines = append(lines, m.styles.Dim.Render(fmt.Sprintf("filter: %q (esc in filter mode to clear)", m.filterQuery)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) productionChip() string {
	if !m.production {
		return ""
	}
	return "   " + m.styles.Production.Render("production")
}

// declaredLine words what the env declares: "app-production declares  v1 · 111111111111 ·
// since 34 days ago". "Declares", never "runs": this is the manifest, not the cluster. A
// split env (one image repo at several references, #119) names every reference and says
// which one the commit pane measures from — the first by file then line (Declared.Ref) —
// rather than claiming a single declared build; BuildDeployPlan rewrites every one of them.
func (m Model) declaredLine() string {
	ref := m.declared.Ref
	parts := []string{m.styles.Dim.Render(m.target + " declares"), "  " + m.styles.Accent.Render(tagOrDigest(ref))}
	if ref.Digest != "" {
		parts = append(parts, " · "+ShortDigest(ref.Digest))
	}
	if m.declared.Split() {
		for _, r := range m.declared.Refs[1:] {
			parts = append(parts, m.styles.Dim.Render(" and ")+m.styles.Accent.Render(tagOrDigest(r)))
			if r.Digest != "" {
				parts = append(parts, " · "+ShortDigest(r.Digest))
			}
		}
		parts = append(parts, m.styles.Warn.Render(" — split"), m.styles.Dim.Render("; commits are counted from "+tagOrDigest(ref)))
	}
	switch {
	case m.ageKnown && m.ageErr == nil:
		since := "since " + ui.Ago(m.now(), m.age.Since)
		if m.age.Approximate {
			since += " (approximately)"
		}
		parts = append(parts, m.styles.Dim.Render(" · "+since))
	case m.ageKnown:
		parts = append(parts, m.styles.Dim.Render(" · age unknown"))
	case m.histFn.LiveAge != nil:
		parts = append(parts, m.styles.Dim.Render(" · since …"))
	}
	return m.wrap(strings.Join(parts, ""))
}

func tagOrDigest(r image.Ref) string {
	if r.Tag != "" {
		return r.Tag
	}
	return ShortDigest(r.Digest)
}

// stagingNote renders the paired staging env's committed manifest tag(s) for this image repo:
// the single agreed tag in the common case, or an explicit disagreement
// when StagingMismatch found more than one distinct tag across the staging env's own
// families/occurrences — gitops.BuildPlan's WarnSourceDisagrees is the same "warn, don't
// block" shape for the analogous disagreement among one env's SOURCE occurrences; this
// mirrors it rather than silently rendering an arbitrary one of them and hiding the rest.
// Only ever called when m.hasStagingMismatch is true, which StagingMismatch never reports
// alongside an empty m.stagingTags (see its own doc comment).
//
// Known gap, deliberately not closed here (issue #74): when the paired staging env has no
// occurrence of this image repo AT ALL, StagingMismatch reports ok=false and the note is
// suppressed entirely — so the strongest "this has never been through staging" case is the
// one case that gets no verdict. Closing it means StagingMismatch distinguishing "no pair
// configured" from "pair exists, nothing of this repo in it", which is the same return
// reshaping #74 already needs for digests. Until then nothing here, and nothing in
// docs/repo-map.md, may describe this note as covering that case.
func (m Model) stagingNote() string {
	// The env's own committed state comes first and is described in exactly the terms it is
	// read in — a manifest occurrence, never a live cluster read (this package has no cluster
	// connection wired in at all), and never collapsed to one tag when the env disagrees with
	// itself.
	base := fmt.Sprintf("note: %s (paired staging) disagrees with itself on this image's committed manifest tag: %s",
		m.stagingEnv, strings.Join(m.stagingTags, ", "))
	if len(m.stagingTags) == 1 {
		base = fmt.Sprintf("note: %s (paired staging)'s committed manifest tag is %s", m.stagingEnv, m.stagingTags[0])
	}
	// The comparison this note existed to enable but never made. StagingMismatch has always
	// fetched the paired staging env's committed tags; until now the screen printed them and
	// left the operator to check the tag under their own cursor against the list by eye —
	// which is the actual question ("has this build been anywhere first?") on the screen where
	// it is being answered. Appended rather than substituted so the honest description of what
	// was read survives the verdict.
	tag := m.cursorTag()
	switch {
	case tag == "":
		return base
	case m.stagingRuns(tag):
		// Named as a tag comparison, because that is all it is: StagingMismatch carries the
		// staging env's committed tag STRINGS, and a tag is mutable — staging may have
		// committed v1 when v1 meant one digest and the registry may point v1 at another now.
		// Saying "v1 is committed there" would let that read as "this build went through
		// staging", which this data cannot support (issue #74 tracks carrying the digests
		// through and comparing those instead).
		return base + fmt.Sprintf("; %s is the tag committed there — tags move, so this is not proof of the same build", tag)
	default:
		return base + fmt.Sprintf("; warning: %s (under the cursor) is not committed there — it has not been through %s",
			tag, m.stagingEnv)
	}
}

// cursorTag is the tag the operator is looking at, or "" when nothing is selected yet and
// there is nothing to compare.
func (m Model) cursorTag() string { return m.selectedTag }

// stagingRuns reports whether the paired staging env's committed manifest carries tag.
func (m Model) stagingRuns(tag string) bool {
	for _, t := range m.stagingTags {
		if t == tag {
			return true
		}
	}
	return false
}

func (m Model) wrap(s string) string { return ansi.Wordwrap(s, max(m.width-2, 20), "") }

func (m Model) viewErr() string {
	// Wrapped, not left as one long line. A registry failure is a list — one clause per
	// credential source — and printing it unwrapped means the terminal clips it after the first
	// clause, which is the one least likely to be the actionable one: an operator sees
	// "HOIST_GHCR_TOKEN is not set" and never reaches "cluster: not configured", which is the
	// clause that tells them what to fix.
	body := m.styles.Notice.Render(wrapError(m.err.Error(), max(m.width-2, 20)))
	return ui.Frame{Title: m.title(), Sections: []string{m.metaSection(), body}, Footer: m.errFooter()}.Render(m.styles, m.width, m.height)
}

// errFooter is the error state's own footer (v2·06c): r retries (reload), esc goes back.
func (m Model) errFooter() string {
	hints := []keys.Hint{
		{B: keys.Refresh, Long: "r retry", Pri: 1},
		{B: keys.Esc, Long: "esc back", Pri: -1},
	}
	return keys.Footer(m.styles, m.width, "", hints, true)
}

// wrapError breaks an error across lines at its own clause separators first, then at spaces,
// so a credential chain's "source: reason; source: reason" reads one source per line.
func wrapError(msg string, width int) string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for _, clause := range strings.Split(msg, "; ") {
		out = append(out, ansi.Wordwrap(clause, width, ""))
	}
	return strings.Join(out, "\n")
}

func (m Model) viewLoading() string {
	hints := []keys.Hint{{B: keys.Esc, Long: "esc back", Pri: -1}}
	return ui.Frame{Title: m.title(), Sections: []string{m.metaSection(), m.spinner.View() + " listing tags…"}, Footer: keys.Footer(m.styles, m.width, "", hints, true)}.Render(m.styles, m.width, m.height)
}

func (m Model) viewReady() string {
	sections := []string{m.metaSection(), m.tableSection(), m.paneSection()}
	return ui.Frame{Title: m.title(), Sections: sections, Footer: m.footer()}.Render(m.styles, m.width, m.height)
}

// tableSection is the header row and the visible window of tag rows, plus the notices that
// belong to the table.
func (m Model) tableSection() string {
	rows := m.filtered()
	widths := m.columnWidths(rows)
	var b strings.Builder
	b.WriteString(m.styles.Header.Render(m.tableRow(widths, "", "TAG", "BUILT", "DIGEST", "")))
	if len(rows) == 0 {
		b.WriteString("\n  (no matching tags)")
	}
	// Windowed to the same [start,end) fetchVisible uses (visibleWindow), so the cursor's row
	// is always among what's drawn — moving the cursor past one screen's worth of rows must
	// scroll the window with it, never leave the selected row off-screen.
	start, end := m.visibleWindow(rows)
	// Groups (#91): a divider above the first drawn row of the digest and moving groups —
	// also when the window opens mid-group, so a scrolled page still says what it is
	// looking at. The release group is the default view and gets none; the "unordered"
	// divider (a mapped repo's releases with no matching git tag) lives inside it only,
	// since a digest or moving tag never matches a git tag and would always be "unordered".
	dividerShown := false
	prev := ClassRelease
	for i, r := range rows[start:end] {
		if r.Class != prev || (i == 0 && r.Class != ClassRelease) {
			b.WriteString("\n" + m.styles.Dim.Render(ansi.Truncate(r.Class.Divider(), max(m.width-2, 10), "…")))
		}
		prev = r.Class
		if m.mapped && r.Class == ClassRelease && !r.HasGitDate && !dividerShown {
			b.WriteString("\n" + m.styles.Dim.Render("── unordered (no matching git tag) ──"))
			dividerShown = true
		}
		b.WriteString("\n" + m.rowLine(widths, r))
	}
	// For an unmapped repo, Created-based ordering (invariant 3's fallback)
	// is only actually established among rows fetchVisible has already loaded — AGENTS.md
	// invariant 4's deliberate laziness (New's own doc comment) means a row outside every window
	// the cursor has visited so far may never be fetched at all, so a genuinely newer tag sitting
	// there can never be sorted to the top. Rather than eagerly fetching every row up front
	// (which would defeat that laziness) or silently claiming a Created-sort this screen hasn't
	// actually established, count and name how many rows outside the current window are still
	// unevaluated so the operator can tell the sort is provisional, not complete.
	if !m.mapped {
		var pending int
		for i, r := range rows {
			if i >= start && i < end {
				continue
			}
			if !r.MetaLoaded {
				pending++
			}
		}
		if pending > 0 {
			b.WriteString("\n" + m.styles.Notice.Render(fmt.Sprintf(
				"%s outside the visible window haven't been evaluated yet — created order isn't fully established",
				ui.Plural(pending, "tag"),
			)))
		}
	}
	if m.notice != "" {
		b.WriteString("\n" + m.styles.Notice.Render(m.wrap(m.notice)))
	}
	// Fewer tags than the page has room for (or fewer dividers than dividerRows() budgeted
	// for) must not shrink the box below what pageSize/BodyHeight reserved for it — every
	// other screen fills its scrolling section to the room it was given so the closing
	// border always lands directly above the footer; a table with real content
	// still fits within its budget, so this only ever adds rows, never removes any.
	want := 1 + m.pageSize() + m.dividerRows() // header + rows + dividers
	if have := strings.Count(b.String(), "\n") + 1; have < want {
		// A trailing " " rather than "" on the last padded row: Box's boxLines trims a
		// section's trailing "\n"s before splitting it into lines, which would otherwise
		// silently eat every blank row this loop just added right back off the end.
		b.WriteString(strings.Repeat("\n", want-have-1) + "\n ")
	}
	return b.String()
}

// columnWidths sizes the four columns to the visible rows: the tag column to its widest
// tag (bounded), BUILT and DIGEST fixed, the provenance column with the rest.
func (m Model) columnWidths(rows []Row) [4]int {
	tag := len("TAG")
	for _, r := range rows {
		tag = max(tag, ansi.StringWidth(r.Tag))
	}
	tag = min(tag, max(m.width/3, 12))
	return [4]int{tag, 13, 12, 0}
}

func (m Model) tableRow(w [4]int, marker, tag, built, digest, prov string) string {
	return fmt.Sprintf("%-2s%-*s  %-*s  %-*s  %s", marker, w[0], ansi.Truncate(tag, w[0], "…"), w[1], built, w[2], digest, prov)
}

func (m Model) rowLine(w [4]int, r Row) string {
	marker := "  "
	if r.Tag == m.selectedTag {
		marker = "▸ "
	}
	line := m.tableRow(w, marker, r.Tag, m.builtCell(r), m.digestCell(r), m.provenance(r))
	if r.Tag == m.selectedTag && m.focus == focusTags {
		return m.styles.Selected.Render(line)
	}
	return line
}

// provenance is the fourth column: where else this tag is — "in app-staging" when the
// paired staging env's committed manifest carries it, "◂ declared here" for what the target
// env declares now (any of its references, when split). Both are tag comparisons, and the
// staging note says what that proves.
func (m Model) provenance(r Row) string {
	var parts []string
	if m.declared != nil && m.declaresTag(r.Tag) {
		parts = append(parts, m.styles.Accent.Render("◂ declared here"))
	}
	if m.hasStagingMismatch && m.stagingRuns(r.Tag) {
		parts = append(parts, m.styles.Good.Render("in "+m.stagingEnv))
	}
	return strings.Join(parts, "  ")
}

// declaresTag reports whether any of the target env's declared references carries tag.
func (m Model) declaresTag(tag string) bool {
	if m.declared == nil {
		return false
	}
	if len(m.declared.Refs) == 0 {
		return m.declared.Ref.Tag == tag // a Declared built by hand, without Refs
	}
	for _, r := range m.declared.Refs {
		if r.Tag == tag {
			return true
		}
	}
	return false
}

// builtCell is when the build was made, relative to now: the app repo's git tag date when
// the repo is mapped, else the registry's Created — the same source DeriveRows/Reorder order
// the rows by.
func (m Model) builtCell(r Row) string {
	switch {
	case r.HasGitDate:
		return ui.Ago(m.now(), r.GitDate)
	case r.MetaLoaded:
		return ui.Ago(m.now(), r.Meta.Created)
	case r.MetaErr != nil:
		return "load failed"
	case r.MetaLoading:
		return m.spinner.View()
	default:
		return "…"
	}
}

func (m Model) digestCell(r Row) string {
	switch {
	case r.MetaLoaded:
		return ShortDigest(r.Meta.Digest)
	case r.MetaErr != nil:
		return "—"
	case r.MetaLoading:
		return m.spinner.View()
	default:
		return "…"
	}
}

// paneSection is the commit pane for the cursor tag.
func (m Model) paneSection() string {
	if m.selectedTag == "" {
		return m.styles.Dim.Render("no tag under the cursor")
	}
	declared := "what " + m.target + " declares"
	if m.declared != nil {
		declared = tagOrDigest(m.declared.Ref)
	}
	if m.declared == nil {
		return m.styles.Dim.Render(fmt.Sprintf("no commit history — %s does not declare %s yet, so there is nothing to compare with", m.target, m.imageRepo))
	}
	if m.histFn.Delta == nil {
		return m.styles.Dim.Render(fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", m.imageRepo))
	}
	at := -1
	if m.focus == focusCommits {
		at = m.commitIdx
	}
	lines := history.Lines(m.deltas[m.selectedTag], m.selectedTag, declared, m.target, m.imageRepo, m.histFn.Mapped == nil || m.histFn.Mapped(m.imageRepo), m.paneRows(), at)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		text := m.wrap(l.Text)
		switch l.Role {
		case "head":
			if strings.Contains(l.Text, "migration") && !strings.Contains(l.Text, "not tracked") {
				head, rest, _ := strings.Cut(l.Text, " · ")
				text = m.styles.Title.Render(head) + " · " + m.styles.Warn.Render(rest)
			} else {
				text = m.styles.Title.Render(l.Text)
			}
		case "commit", "migration":
			marker := "  "
			if m.focus == focusCommits && l.Index == m.commitIdx {
				marker = "▸ "
			}
			text = marker + l.Text
			if l.Role == "migration" {
				text = marker + m.styles.Warn.Render(ansi.Truncate(l.Text, max(m.width-2-2-12, 10), "…")) + "  " + m.styles.Warn.Render("migration")
			}
			if marker == "▸ " {
				text = m.styles.Selected.Render(ansi.Strip(text))
			}
		case "more":
			// (v2·06a): "↓ N more commits" sits flush right, mirroring the mockup's own
			// rjust — a trailer naming what the pane's own room left out reads as a footnote,
			// not another row of content.
			pad := max(m.width-2-ansi.StringWidth(text), 0)
			text = m.styles.Dim.Render(strings.Repeat(" ", pad) + text)
		case "gap", "wait":
			text = m.styles.Dim.Render(text)
		}
		out = append(out, text)
	}
	return strings.Join(out, "\n")
}

// readingHead is the commit-detail view's fixed section: subject and position. The second
// line says how much of the body is on screen when it does not all fit, so a clipped body
// is never mistaken for the whole of it.
func (m Model) readingHead(c migrate.Commit, n int) string {
	st := m.deltas[m.selectedTag]
	// Forward: the commit is in the cursor tag and not in what the env declares. A rollback
	// lists the commits being removed, so the containment reads the other way round.
	in, notIn := m.selectedTag, tagOrDigest(st.Delta.From.Ref)
	if st.Delta.Direction == migrate.DirectionRollback {
		in, notIn = notIn, in
	}
	pos := fmt.Sprintf("%d of %d in %s · not in %s · %s · %s", m.commitIdx+1, n, in, notIn, c.Author, ui.Ago(m.now(), c.Date))
	if m.body.TotalLineCount() > m.body.Height() {
		pos += fmt.Sprintf(" · body %d%% (pgdn scrolls)", int(m.body.ScrollPercent()*100))
	}
	return m.styles.Title.Render(history.ShortSHA(c.SHA)+"   "+c.Subject) + "\n" + m.styles.Dim.Render(pos)
}

// readingBody is what the viewport scrolls: the commit message, wrapped, and the migration
// files the commit carries.
func (m Model) readingBody(c migrate.Commit) string {
	body := c.Body
	if strings.TrimSpace(body) == "" {
		body = m.styles.Dim.Render("(no body)")
	} else {
		body = m.wrap(body)
	}
	if len(c.Migrations) > 0 {
		body += "\n\n" + m.styles.Warn.Render("migrations in this commit:\n  "+strings.Join(c.Migrations, "\n  "))
	}
	return body
}

// layoutReading sizes the body viewport to what the frame leaves after the head, and loads
// the cursor commit into it — the offset is kept, so a scrolled body stays scrolled across
// a redraw. A no-op when there is no commit to read.
func (m Model) layoutReading() Model {
	commits := m.currentCommits()
	if len(commits) == 0 || m.commitIdx >= len(commits) {
		return m
	}
	c := commits[m.commitIdx]
	m.body.SetWidth(m.width - 2)
	m.body.SetHeight(max(ui.BodyHeight(m.height, 2)-2, 1)) // the head is two lines
	m.body.SetContent(m.readingBody(c))
	return m
}

// viewReading is the commit-detail view (mockup 08): subject and position, then the body and
// the commit's migration files in a viewport — PageUp/PageDown, ctrl+u/ctrl+d and home/end
// scroll it when the message is longer than the terminal (replacing g/G, #120); ↑/↓
// move to the next commit, ← (or esc) back to the list.
func (m Model) viewReading() string {
	commits := m.currentCommits()
	if len(commits) == 0 || m.commitIdx >= len(commits) {
		m.reading = false
		return m.viewReady()
	}
	m = m.layoutReading()
	sections := []string{m.readingHead(commits[m.commitIdx], len(commits)), m.body.View()}
	return ui.Frame{Title: fmt.Sprintf("hoist · tags · %s · commit", m.target), Sections: sections, Footer: m.readingFooter()}.Render(m.styles, m.width, m.height)
}

// readingFooter is the commit-detail view's own footer (ScrTagsReader's registry row).
func (m Model) readingFooter() string {
	hints := []keys.Hint{
		{B: keys.Up, Long: "↑/↓ switch commit", Short: "↑/↓ commit", Pri: 3},
		{B: keys.PgUp, Long: "pgup/pgdn scroll", Short: "pgup/pgdn", Pri: 2},
		{B: keys.Home, Long: "home/end scroll ends", Short: "home/end", Pri: 4},
		{B: keys.Enter, Long: "enter review the change", Short: "enter review", Pri: 1},
		{B: keys.Left, Long: "← back to the list", Short: "← back", Pri: 0},
	}
	return keys.Footer(m.styles, m.width, "", hints, true)
}

// footer is the tag list's own footer (v2·06a).
func (m Model) footer() string {
	hints := []keys.Hint{
		{B: keys.Enter, Long: "enter review " + m.selectedTag, Short: "enter review", Pri: 0},
	}
	if len(m.currentCommits()) > 0 {
		hints = append(hints, keys.Hint{B: keys.Right, Long: "→ read commit", Short: "→ read", Pri: 3})
	}
	hints = append(hints,
		keys.Hint{B: keys.Filter, Long: "/ filter", Pri: 2},
		keys.Hint{B: keys.Refresh, Long: "r reload", Pri: 4},
		keys.Hint{B: keys.Log, Long: "l activity", Pri: 5},
		keys.Hint{B: keys.Esc, Long: "esc back", Pri: -1},
	)
	return keys.Footer(m.styles, m.width, "", hints, true)
}
