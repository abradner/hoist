// Package deploy is the confirm screen for writing one named image into one env — the
// "image bump" half of hoist's problem statement, reached with d on the matrix and a tag
// chosen in internal/app/tags.
//
// It exists as its own screen rather than as a mode of internal/app/plan because the two
// confirm different things. The plan screen confirms a set: several repos, each tickable,
// derived from a source env. A deploy confirms one image the operator named outright, so
// there is nothing to tick and no source to describe — and the questions it should answer
// ("what is in this build", "what is running now") are not the plan screen's.
//
// From M10 it leads with the work (#85 screen 03/10/11): the commits this deploy ships and
// which of them migrate the database, with the YAML diff one key away rather than the
// headline — the diff is the mechanism, and principle 4 already guarantees its shape (one
// image line per occurrence, verified before git add). When the history cannot be resolved
// the diff is the only evidence left and becomes the body again, with the reason stated.
// What it still shares with the plan screen is the rule that no write happens without the
// bytes available to look at: the diff is always one key away, never absent, and enter
// means the same thing from either view — whether the operator looked is their call.
package deploy

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
)

// BackMsg asks whatever composes screens to pop this one.
type BackMsg struct{}

// StartMsg is the operator confirming the deploy. Mode mirrors plan.StartMsg's: ModePR opens
// a pull request, ModeDirect commits straight to the base branch. Confirmed is true only when
// ModeDirect was reached through this screen's own huh.Confirm — engine.DirectCommitGateStep
// reads it as the record of that gesture (internal/engine/direct.go).
type StartMsg struct {
	Plan      gitops.Plan
	Mode      string
	Confirmed bool
	Target    string
	Image     string
}

// Mode values — plan's own, so the root can treat both screens' StartMsgs the same way
// (M10: the two packages had spelled them differently while claiming to mirror each other).
const (
	ModePR     = plan.ModePR
	ModeDirect = plan.ModeDirect
)

// History is what the picker already learned about the build being deployed, carried in so
// this screen leads with it without a second fetch (tags.SelectedMsg). Delta is nil when
// there is none, and Note then says why in a sentence. Declared is what the env declares
// today and Since when its manifest line last changed (zero when unknown).
type History struct {
	Delta    *migrate.Delta
	Note     string
	Declared image.Ref
	Since    time.Time
}

// Model is the deploy confirm screen.
type Model struct {
	styles        ui.Styles
	width, height int

	pl         gitops.Plan
	root       string
	target     string
	image      string
	production bool

	history  History
	now      func() time.Time
	showYAML bool

	commits  viewport.Model
	diff     viewport.Model
	diffText string
	diffErr  error
	mode     string
	confirm  *huh.Confirm
	confirmV bool
	notice   string
	ticked   map[string]bool
}

// New builds the screen for an already-constructed deploy plan. root is the repo checkout the
// diff is read from; envs decides whether the target is production, which forces PR mode.
func New(pl gitops.Plan, root, image string, envs config.EnvsConfig, styles ui.Styles) Model {
	ticked := map[string]bool{}
	for _, e := range pl.Edits {
		ticked[e.Ref.Repo] = true
	}
	m := Model{
		styles:     styles,
		pl:         pl,
		root:       root,
		target:     pl.TargetEnv,
		image:      image,
		production: plan.IsProduction(pl.TargetEnv, envs),
		mode:       ModePR,
		ticked:     ticked,
		diff:       viewport.New(),
		commits:    viewport.New(),
		now:        time.Now,
		showYAML:   true, // until WithHistory supplies commits to lead with
	}
	body, err := plan.RenderDiff(root, pl.Edits, ticked)
	if err != nil {
		m.diffErr = err
	} else {
		m.diffText = body
		m.diff.SetContent(body)
	}
	return m
}

// WithHistory supplies the commit history the picker loaded. With a delta the commits are
// the body and the YAML is behind d; without one the YAML is the body and the header says
// why there is no history.
func (m Model) WithHistory(h History) Model {
	m.history = h
	m.showYAML = h.Delta == nil
	m.commits.SetContent(m.commitLines())
	return m
}

// WithNow fixes the clock (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// WithDirectMode opens the screen already in direct mode, for the picker's own D path: that
// gesture (keypress + huh.Confirm, internal/app/tags) has already been completed, and asking
// for it twice would be ceremony rather than safety. Production is the exception — §4.5 gives
// it no direct path at all, so the request is dropped and the screen says why.
func (m Model) WithDirectMode() Model {
	if m.production {
		m.notice = fmt.Sprintf("%s is a production env — deploys there always open a PR", m.target)
		return m
	}
	m.mode = ModeDirect
	return m
}

// Init implements the screen contract; nothing to load, the plan arrived built.
func (m Model) Init() tea.Cmd { return nil }

// Update implements the screen contract.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		return m.onKey(k)
	}
	return m.scroll(msg)
}

func (m Model) scroll(msg tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	if m.showYAML {
		m.diff, cmd = m.diff.Update(msg)
	} else {
		m.commits, cmd = m.commits.Update(msg)
	}
	return m, cmd
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return BackMsg{} }
	case "enter":
		if m.diffErr != nil {
			// The screen's entire promise is that the bytes are visible before anything is
			// written. When RenderDiff failed there are no bytes on screen, so enter would
			// confirm a write on the operator's behalf against something they were never
			// shown — the one thing this screen exists to prevent (Copilot, PR #72). Esc back
			// and fix the cause; there is no way to force past it, deliberately.
			m.notice = "cannot confirm a deploy whose diff could not be rendered — esc back and retry"
			return m, nil
		}
		return m, m.start(m.mode)
	case "d":
		if m.history.Delta == nil {
			m.notice = "the yaml is already the body: there is no commit history to show instead"
			return m, nil
		}
		m.showYAML = !m.showYAML
		return m, nil
	case "m":
		if m.production {
			// §4.5: production always goes through a PR. Refusing with the reason beats a
			// key that silently does nothing.
			m.notice = fmt.Sprintf("%s is a production env — deploys there always open a PR", m.target)
			return m, nil
		}
		if m.mode == ModeDirect {
			m.mode = ModePR
			return m, nil
		}
		m.confirmV = false
		m.confirm = huh.NewConfirm().
			Title(fmt.Sprintf("Commit %s straight to %s with no PR?", m.image, m.target)).
			Description("Nothing reviews this before it deploys.").
			Value(&m.confirmV)
		// huh.NewConfirm leaves keymap zero-valued, and a zero key.Binding matches nothing: a
		// Confirm used standalone rather than inside a huh.Form ignores every keypress, so
		// without this y/n/←/→ all did nothing and this screen could not be switched to
		// direct mode at all (Copilot, PR #72).
		m.confirm.WithKeyMap(huh.NewDefaultKeyMap())
		m.confirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
		m.confirm.WithWidth(m.dialogWidth())
		return m, tea.Batch(m.confirm.Init(), m.confirm.Focus())
	}
	return m.scroll(msg)
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		m.confirm = nil
		return m, nil
	}
	f, cmd := m.confirm.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirm = c
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" {
		// Read from the widget, not from m.confirmV: Value takes the address of a field in
		// whichever Model copy built the confirm, and every Update since has returned a new
		// copy, so this model's own field never moves however the operator answers.
		agreed, _ := m.confirm.GetValue().(bool)
		m.confirm = nil
		if agreed {
			m.mode = ModeDirect
		}
		return m, nil
	}
	return m, cmd
}

// start emits the confirmation. Confirmed is true only on the direct path, which is only ever
// reached through the huh.Confirm above (or the picker's own, via WithDirectMode).
func (m Model) start(mode string) tea.Cmd {
	pl, target, img := m.pl, m.target, m.image
	confirmed := mode == ModeDirect
	return func() tea.Msg {
		return StartMsg{Plan: pl, Mode: mode, Confirmed: confirmed, Target: target, Image: img}
	}
}

// SetSize implements the screen contract: the body viewport gets what the frame leaves
// after the fixed sections.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	if m.confirm != nil {
		m.confirm.WithWidth(m.dialogWidth())
	}
	return m.layout()
}

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	inner := m.width - 2
	fixed := lipglossHeight(m.headerSection()) + lipglossHeight(m.summarySection()) + lipglossHeight(m.footerSection())
	sections := 4
	if w := m.warningsSection(); w != "" {
		fixed += lipglossHeight(w)
		sections++
	}
	if mig := m.migrationsSection(); mig != "" && !m.showYAML {
		fixed += lipglossHeight(mig)
		sections++
	}
	body := max(ui.BodyHeight(m.height, sections)-fixed, 3)
	m.diff.SetWidth(inner)
	m.diff.SetHeight(body)
	m.commits.SetWidth(inner)
	m.commits.SetHeight(body)
	return m
}

func lipglossHeight(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// SetStyles implements the screen contract.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	m.commits.SetContent(m.commitLines())
	return m
}

// CapturesText reports whether a keypress belongs to this screen's own text input — true only
// while the direct-mode confirmation is open.
func (m Model) CapturesText() bool { return m.confirm != nil }

// View implements the screen contract.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	// Laid out on this copy at render time, so the body viewport is sized against exactly the
	// sections about to be drawn (a notice set since SetSize, the yaml/commits toggle).
	m = m.layout()
	title := "hoist · confirm deploy"
	if m.showYAML && m.history.Delta != nil {
		title += " · yaml"
	}
	sections := []string{m.headerSection(), m.summarySection()}
	if w := m.warningsSection(); w != "" {
		sections = append(sections, w)
	}
	switch {
	case m.diffErr != nil && m.showYAML:
		sections = append(sections, m.styles.Bad.Render(ansi.Wordwrap("could not render the diff: "+m.diffErr.Error(), max(m.width-2, 20), "")))
	case m.showYAML:
		sections = append(sections, m.diff.View())
	default:
		sections = append(sections, m.commits.View())
		if mig := m.migrationsSection(); mig != "" {
			sections = append(sections, mig)
		}
	}
	sections = append(sections, m.footerSection())
	view := ui.Frame{Title: title, Sections: sections, Footer: m.hints()}.Render(m.styles, m.width, m.height)
	if m.confirm != nil {
		view = ui.Dialog(m.styles, view, "direct commit", m.confirm.View(), m.width, m.height)
	}
	// The same final-boundary scrub every other screen applies (internal/app/plan's own View,
	// and tags'): the diff carries three lines of context from files this screen never chose,
	// and the warnings, commit text and render errors are rendered verbatim — so a credential
	// registered with pkg/redact would otherwise reach the terminal through the one screen
	// that skipped it (Copilot, PR #72/#73). Applied once at the boundary rather than per
	// field, so a field added later cannot forget.
	return redact.Strings(view)
}

// headerSection is "image → env" with the mode chip flush right; production turns the chip
// amber and says so, since it is what changes this screen's temperature.
func (m Model) headerSection() string {
	// repo:tag in the header; the digest is the mechanism's detail and sits in the footer line,
	// where it does not push the target off the edge at 80 columns.
	shown := m.image
	if ref, err := image.Parse(m.image); err == nil && ref.Tag != "" {
		shown = ref.Repo + ":" + ref.Tag
	}
	left := m.styles.Title.Render(shown) + "   →   " + m.styles.Title.Render(m.target)
	chip := "mode: " + strings.ToUpper(m.mode)
	switch {
	case m.production:
		chip = m.styles.Production.Render(chip + " · production")
	case m.mode == ModeDirect:
		chip = m.styles.Warn.Render(chip)
	default:
		chip = m.styles.Accent.Render(chip)
	}
	return ui.StatusBar(max(m.width-2, 1), left, chip)
}

// summarySection is the sentence: "rolling out 14 commits · 2 migrations · replacing v1,
// live 34 days" — or why there is no history, with the diff's own scale.
func (m Model) summarySection() string {
	d := m.history.Delta
	if d == nil {
		note := m.history.Note
		if note == "" {
			note = "no commit history for this build"
		}
		return m.styles.Dim.Render(ansi.Wordwrap(note+" — the yaml below is the change", max(m.width-2, 20), ""))
	}
	var parts []string
	switch d.Direction {
	case migrate.DirectionSame:
		parts = append(parts, m.styles.Title.Render("rolling out the build already declared"))
	case migrate.DirectionRollback:
		parts = append(parts, m.styles.Warn.Render(fmt.Sprintf("rolling back %s", countCommits(d))))
	default:
		parts = append(parts, m.styles.Title.Render("rolling out "+countCommits(d)))
	}
	if n := len(d.Migrations); n > 0 {
		word := "migrations"
		if n == 1 {
			word = "migration"
		}
		if d.Direction == migrate.DirectionRollback {
			word += " reverted"
		}
		if d.MigrationsIncomplete {
			word += " (at least)"
		}
		parts = append(parts, m.styles.Warn.Render(fmt.Sprintf("%d %s", n, word)))
	} else if d.Prefix == "" {
		parts = append(parts, m.styles.Dim.Render("migrations not tracked"))
	} else if d.MigrationsIncomplete {
		parts = append(parts, m.styles.Warn.Render("migrations unknown — the history is incomplete"))
	}
	if m.history.Declared.Repo != "" {
		replacing := "replacing " + tagOrDigest(m.history.Declared)
		if !m.history.Since.IsZero() {
			replacing += ", live " + ui.Span(m.now().Sub(m.history.Since))
		}
		parts = append(parts, m.styles.Warn.Render(replacing))
	}
	return strings.Join(parts, m.styles.Dim.Render(" · "))
}

func countCommits(d *migrate.Delta) string {
	n := len(d.Commits)
	if d.Truncated {
		return fmt.Sprintf("%d of %d commits", n, d.Total)
	}
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

func tagOrDigest(r image.Ref) string {
	if r.Tag != "" {
		return r.Tag
	}
	d := strings.TrimPrefix(r.Digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// commitLines is the commit viewport's content: one line per commit, migration-carrying
// ones marked, newest first as the delta lists them.
func (m Model) commitLines() string {
	d := m.history.Delta
	if d == nil {
		return ""
	}
	if len(d.Commits) == 0 {
		return m.styles.Dim.Render("no commits between the two builds")
	}
	width := max(m.width-4, 20)
	lines := make([]string, 0, len(d.Commits)+1)
	for _, c := range d.Commits {
		sha := c.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		line := fmt.Sprintf("  %s  %s", sha, c.Subject)
		if len(c.Migrations) > 0 {
			line = m.styles.Warn.Render(ansi.Truncate(line, width-12, "…")) + "  " + m.styles.Warn.Render("migration")
		} else {
			line = ansi.Truncate(line, width, "…")
		}
		lines = append(lines, line)
	}
	if d.Truncated {
		lines = append(lines, m.styles.Dim.Render(fmt.Sprintf("  …and %d older commits the forge did not list", d.Total-len(d.Commits))))
	}
	return strings.Join(lines, "\n")
}

// migrationsSection names every migration file this deploy runs — called out twice,
// deliberately: inline against the commit that adds it, and here as the list that will run.
// It is the one class of change that is not trivially reversible.
func (m Model) migrationsSection() string {
	d := m.history.Delta
	if d == nil || len(d.Migrations) == 0 {
		return ""
	}
	verb := "run on this deploy"
	if d.Direction == migrate.DirectionRollback {
		verb = "are reverted by this deploy"
	}
	word := "migrations"
	if len(d.Migrations) == 1 {
		word = "migration"
	}
	qualifier := ""
	if d.MigrationsIncomplete {
		qualifier = " (at least)" // the forge capped the history; this list is a floor
	}
	lines := []string{m.styles.Warn.Render(fmt.Sprintf("%d %s%s %s:", len(d.Migrations), word, qualifier, verb))}
	for _, f := range d.Migrations {
		lines = append(lines, m.styles.Dim.Render("  "+ansi.Truncate(f, max(m.width-4, 20), "…")))
	}
	return strings.Join(lines, "\n")
}

// warningsSection is the plan's own warnings — above the fold, on the one surface where the
// operator is about to press enter. A warning the CLI's dry run and the PR body both carry
// (plan.WarnDeployIntoProduction) has no business being invisible here. Informational, never
// blocking (AGENTS.md §4.5): enter still works.
func (m Model) warningsSection() string {
	var lines []string
	for _, w := range m.pl.Warnings {
		lines = append(lines, m.styles.Warn.Render(ansi.Wordwrap("! "+w.Message, max(m.width-2, 20), "")))
	}
	if m.notice != "" {
		lines = append(lines, m.styles.Notice.Render(ansi.Wordwrap(m.notice, max(m.width-2, 20), "")))
	}
	return strings.Join(lines, "\n")
}

// footerSection is the mechanism's own line: what is written, and the key that shows it.
func (m Model) footerSection() string {
	digest := ""
	if ref, err := image.Parse(m.image); err == nil && ref.Digest != "" {
		digest = " · digest " + tagOrDigest(image.Ref{Digest: ref.Digest})
	}
	left := m.styles.Dim.Render("writes " + scale(m.pl) + digest)
	right := ""
	switch {
	case m.history.Delta == nil:
		left = m.styles.Dim.Render(scale(m.pl) + digest + " · verified before commit")
	case m.showYAML:
		left = m.styles.Dim.Render(scale(m.pl) + digest + " · verified before commit")
		right = m.styles.Dim.Render("d  back to commits")
	default:
		right = m.styles.Dim.Render("d  see the yaml")
	}
	return ui.StatusBar(max(m.width-2, 1), left, right)
}

func (m Model) hints() string {
	help := "enter deploy · ↑/↓ scroll"
	if m.history.Delta != nil {
		if m.showYAML {
			help += " · d back to commits"
		} else {
			help += " · d yaml diff"
		}
	}
	if !m.production {
		help += " · m mode"
	}
	help += " · esc back"
	return ui.StatusBar(m.width, "", m.styles.Hint.Render(help))
}

// scale is the one-line summary of what will be written — the sentence an operator would say
// out loud before pressing enter.
func scale(pl gitops.Plan) string {
	files := map[string]bool{}
	n := 0
	for _, e := range pl.Edits {
		if e.NoOp() {
			continue
		}
		n++
		files[e.File] = true
	}
	return fmt.Sprintf("%s in %s", plural(n, "occurrence"), plural(len(files), "file"))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
