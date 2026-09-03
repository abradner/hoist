package tags

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
)

// ListFunc lists ImageRepo's registry tags and, when the app repo mapping has an entry for it
// AND this call actually managed to fetch that app repo's own git tags with dates (pkg/forge),
// mapped is true and gitTags carries them. cmd/hoist builds this, wrapping whichever registry
// client and (when configured) pkg/forge/github.Client this run already built — this package
// never opens a registry or forge connection itself (AGENTS.md §4.8).
//
// mapped is this call's own, actually-observed answer — never the config's static "is this
// image repo mapped at all" fact alone (finding 3, round 2: cmd/hoist's config check can say
// mapped, but the forge lookup this same call makes can still fail at runtime; a caller that
// returned the config's static answer regardless left the model believing every tag had been
// deliberately left out of a real git-tag match, rather than that ordering had fallen back to
// registry Created entirely — invariant 3's fallback never actually engaging). GitTags is
// always nil/empty when mapped is false, whichever reason made it false.
type ListFunc func(ctx context.Context) (regTags []string, gitTags []forge.GitTag, mapped bool, err error)

// MetaFunc fetches one tag's registry metadata, lazily, only for the row the picker currently
// needs it for (New's own doc comment on laziness, AGENTS.md invariant 4).
type MetaFunc func(ctx context.Context, tag string) (registry.ImageMeta, error)

// BuildFunc is what internal/app.Model calls once per OpenTagsMsg to get imageRepo's own
// ListFunc/MetaFunc and whether RepoConfig.Apps maps it to an app git repo — RepoConfig's own
// static, config-known fact, used only as New's initial guess before the list has loaded.
// ListFunc's own mapped return value (not this one) is what DeriveRows/Reorder actually use
// once the list result arrives (see ListFunc's doc comment) — this one can be true while that
// one later comes back false, when the config names a mapping but the forge lookup itself
// fails at runtime. cmd/hoist builds this, wrapping whichever registry client (and, when
// mapped, pkg/forge/github.Client) the run already built — this package never opens either
// connection itself (AGENTS.md §4.8, mirroring plan.ResolveFunc's own wiring).
type BuildFunc func(imageRepo string) (mapped bool, listFn ListFunc, metaFn MetaFunc)

type state int

const (
	stateLoading state = iota // ListFunc running
	stateReady
)

// BackMsg pops back to whatever's under the tags screen (AGENTS.md §4.8 pattern:
// matrix.OpenPlanMsg/plan.BackMsg's shape, reused here).
type BackMsg struct{}

// SelectedMsg is emitted on Enter, once the chosen row's metadata has loaded: the operator
// picked tag via the normal PR-mode path. This package only reports the choice — AGENTS.md
// §4.8 ("a screen names the transition, the root decides what it means"); wiring this into an
// actual promotion is the next milestone's "deploy new image" flow this picker exists for
// (see model.go's package doc and this repo's own AGENTS.md §8 "building structure where no
// convention is stated is a decision": no write path from the TUI exists anywhere in this
// codebase yet — hoist promote is CLI-only — so this screen stops at reporting the choice
// rather than inventing one).
type SelectedMsg struct {
	ImageRepo, Tag, Digest string
}

// DirectRequestedMsg is emitted only once the operator has completed the keypress + huh.
// Confirm gesture AGENTS.md invariant 5 requires — never on the keypress alone, and never for
// a production target (the 'D' key is not offered at all when Production is true — see
// keyMap/updateReady). This message is UI-side politeness only, exactly like plan.Model's own
// modeLabel/skipNotice: the actual, unbypassable enforcement lives in
// internal/engine.DirectCommitGateStep, which independently refuses a production env even if
// this screen (or any future caller) got this message wrong (invariant 5's "not UI-only
// gating").
type DirectRequestedMsg struct {
	ImageRepo, Tag, Digest string
}

// nextGeneration hands out this process's next tag-picker generation id. Package-level and
// monotonically increasing (never reused, never reset) so that every Model instance New ever
// constructs — for the same image repo or a different one — gets a value no other instance,
// past or future, ever holds. See generation's own doc comment on Model for why imageRepo alone
// cannot serve this purpose.
var nextGeneration atomic.Int64

// listLoadedMsg and metaLoadedMsg both carry imageRepo and gen, the picker instance that
// requested them: loadCmd/fetchCmd close over m.imageRepo and m.generation at the moment the
// command is created, and onListLoaded/onMetaLoaded discard a result whose gen doesn't match
// this model's own current one. internal/app's root routes a message to whatever screen is
// currently on top of its stack by type alone, not by which Model instance produced the
// tea.Cmd that resolves to it — if an operator leaves this picker while its own list/meta
// commands are still in flight and opens a picker for a different image repo, a stale result
// landing in the new picker would otherwise silently populate it with another repo's
// rows/metadata (common tag names like "latest" make this look plausible rather than obviously
// wrong).
//
// gen, not imageRepo, is what actually discriminates instances: imageRepo scopes only by
// REPOSITORY, and two picker instances for the SAME repo are common — closing this picker while
// its commands are still in flight, then immediately reopening a new picker for the identical
// repo, gives the new instance the identical imageRepo value. A stale result from the OLD
// instance would still land on the NEW one under an imageRepo-only check (this was finding 2's
// round-1 gap: the fix for cross-repo leaks scoped by imageRepo alone, and this doc comment
// used to claim — incorrectly — that reopening a model was isolated by repo alone). gen is
// assigned fresh by New for every instance regardless of repo, so it discriminates same-repo
// reopens exactly as it discriminates cross-repo ones; imageRepo is kept on these messages for
// context/debugging only, not as part of the discard decision.
type listLoadedMsg struct {
	imageRepo string
	gen       int64
	regTags   []string
	gitTags   []forge.GitTag
	mapped    bool // this call's own observed answer — see ListFunc's doc comment.
	err       error
}

type metaLoadedMsg struct {
	imageRepo string
	gen       int64
	tag       string
	meta      registry.ImageMeta
	err       error
}

type keyMap struct {
	Up, Down, Filter, Direct, Enter, Back key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Direct: key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "direct commit")),
		Enter:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		Back:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	}
}

// Model is the tag picker screen for one image repo. It is a value: Update, SetSize and
// SetStyles return the updated model, matching internal/app/plan's convention.
type Model struct {
	imageRepo  string
	target     string
	mapped     bool // RepoConfig.Apps has an entry for imageRepo
	production bool

	// generation uniquely identifies this Model instance, assigned once by New from
	// nextGeneration. Two instances for the SAME imageRepo (closing this picker while its
	// commands are still in flight, then immediately reopening a new picker for the identical
	// repo) get different generations — imageRepo alone cannot tell them apart, since a
	// reopened picker's imageRepo is, by definition, identical to the one it replaced. See
	// listLoadedMsg/metaLoadedMsg's own doc comment.
	generation int64

	stagingEnv, stagingTag string
	hasStagingMismatch     bool

	listFn ListFunc
	metaFn MetaFunc

	state state
	err   error

	rows        []Row
	selectedTag string

	filtering   bool
	filterInput textinput.Model
	filterQuery string

	confirming    bool
	confirmDirect *huh.Confirm
	confirmValue  bool

	spinner spinner.Model
	notice  string

	styles        ui.Styles
	keys          keyMap
	width, height int
}

// New builds the tag picker for imageRepo, choosing a tag for target. mapped is whether
// RepoConfig.Apps has an entry for imageRepo (a config-known fact, passed in rather than
// inferred — see rows.go's DeriveRows) — used only as the model's initial value until the
// first list result arrives; onListLoaded then overwrites it with that call's own observed
// mapped value (ListFunc's doc comment), since the config can say mapped while the forge
// lookup itself still fails at runtime. production is whether target is listed in
// envs.production, which gates the 'D' (direct commit) key entirely — the UI-side half of
// AGENTS.md invariant 5; internal/engine.DirectCommitGateStep is the half that actually
// matters. stagingEnv/stagingTag/hasMismatch are rows.StagingMismatch's own result, computed
// by the caller (cmd/hoist) once up front from data already discovered rather than a second
// registry call — see StagingMismatch's doc comment. listFn/metaFn are nil-safe: a nil listFn
// immediately reports an error state rather than hanging in stateLoading forever.
func New(imageRepo, target string, mapped, production bool, stagingEnv, stagingTag string, hasStagingMismatch bool, listFn ListFunc, metaFn MetaFunc) Model {
	return Model{
		imageRepo:          imageRepo,
		target:             target,
		mapped:             mapped,
		production:         production,
		generation:         nextGeneration.Add(1),
		stagingEnv:         stagingEnv,
		stagingTag:         stagingTag,
		hasStagingMismatch: hasStagingMismatch,
		listFn:             listFn,
		metaFn:             metaFn,
		state:              stateLoading,
		keys:               defaultKeyMap(),
		spinner:            spinner.New(spinner.WithSpinner(spinner.Line)),
		filterInput:        textinput.New(),
	}
}

// Init starts the spinner and the async tag/git-tag list load. Listing talks to a registry
// (and, when mapped, a forge), so it is a tea.Cmd here, never run inside Update (AGENTS.md
// §4.3).
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadCmd())
}

func (m Model) loadCmd() tea.Cmd {
	listFn := m.listFn
	imageRepo := m.imageRepo
	gen := m.generation
	return func() tea.Msg {
		if listFn == nil {
			return listLoadedMsg{imageRepo: imageRepo, gen: gen, err: fmt.Errorf("no registry configured for %s", "this repo")}
		}
		regTags, gitTags, mapped, err := listFn(context.Background())
		return listLoadedMsg{imageRepo: imageRepo, gen: gen, regTags: regTags, gitTags: gitTags, mapped: mapped, err: err}
	}
}

func (m Model) fetchCmd(tag string) tea.Cmd {
	metaFn := m.metaFn
	imageRepo := m.imageRepo
	gen := m.generation
	return func() tea.Msg {
		if metaFn == nil {
			return metaLoadedMsg{imageRepo: imageRepo, gen: gen, tag: tag, err: fmt.Errorf("no registry configured")}
		}
		meta, err := metaFn(context.Background(), tag)
		return metaLoadedMsg{imageRepo: imageRepo, gen: gen, tag: tag, meta: meta, err: err}
	}
}

// Update handles the async loads, the spinner tick, and the screen's own keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case listLoadedMsg:
		return m.onListLoaded(msg)
	case metaLoadedMsg:
		return m.onMetaLoaded(msg)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m Model) onListLoaded(msg listLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.generation {
		// A stale result from a picker instance this model is not (a closed-and-reopened
		// picker for the same repo, or a different repo entirely) — discard it without
		// touching any of this model's own state (see listLoadedMsg's own doc comment; gen,
		// not imageRepo, is what actually discriminates instances here).
		return m, nil
	}
	m.state = stateReady
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	// msg.mapped is this call's own observed answer (ListFunc's doc comment), which supersedes
	// New's constructor-time guess: the config can say this image repo is mapped while the
	// forge lookup this same call made still fails at runtime (finding 3, round 2) — m.mapped
	// must reflect that, or DeriveRows/Reorder below keep treating every row as "mapped but
	// unmatched" instead of falling back to Created-based ordering as invariant 3 requires.
	m.mapped = msg.mapped
	m.rows = DeriveRows(msg.regTags, msg.gitTags, m.mapped)
	if len(m.rows) > 0 {
		m.selectedTag = m.rows[0].Tag
	}
	return m.fetchVisible()
}

func (m Model) onMetaLoaded(msg metaLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.generation {
		// Same stale-result guard as onListLoaded — see listLoadedMsg's own doc comment.
		return m, nil
	}
	for i, r := range m.rows {
		if r.Tag == msg.tag {
			m.rows[i].MetaLoading = false
			m.rows[i].MetaLoaded = msg.err == nil
			m.rows[i].MetaErr = msg.err
			if msg.err == nil {
				m.rows[i].Meta = msg.meta
			}
			break
		}
	}
	m.rows = Reorder(m.rows, m.mapped)
	return m.fetchVisible()
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.confirming {
		return m.updateConfirm(msg)
	}
	if m.filtering {
		return m.updateFilter(msg)
	}
	if key.Matches(msg, m.keys.Back) {
		return m, func() tea.Msg { return BackMsg{} }
	}
	if m.state != stateReady {
		return m, nil
	}
	m.notice = ""
	switch {
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.CursorEnd()
		return m, m.filterInput.Focus()
	case key.Matches(msg, m.keys.Up):
		return m.moveCursor(-1)
	case key.Matches(msg, m.keys.Down):
		return m.moveCursor(1)
	case key.Matches(msg, m.keys.Enter):
		return m.selectCurrent(false)
	case key.Matches(msg, m.keys.Direct):
		if m.production {
			m.notice = fmt.Sprintf("direct mode is not offered for %s: it is a production env (AGENTS.md §4.5)", m.target)
			return m, nil
		}
		return m.selectCurrent(true)
	}
	return m, nil
}

func (m Model) filtered() []Row { return Filter(m.rows, m.filterQuery) }

func (m Model) moveCursor(delta int) (Model, tea.Cmd) {
	rows := m.filtered()
	if len(rows) == 0 {
		return m, nil
	}
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		idx = 0
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	m.selectedTag = rows[idx].Tag
	return m.fetchVisible()
}

// selectCurrent emits SelectedMsg or, once the operator has confirmed, opens the huh.Confirm
// gesture direct mode requires (invariant 5's keypress-then-confirm shape) before a
// DirectRequestedMsg is ever emitted. Neither message is emitted until the current row's
// metadata (digest) has actually loaded — a selection without a resolved digest would promote
// a tag hoist cannot yet pin, which AGENTS.md principle 3 refuses at the manifest-write layer
// anyway; refusing it here just gives an earlier, clearer notice.
func (m Model) selectCurrent(direct bool) (Model, tea.Cmd) {
	rows := m.filtered()
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		return m, nil
	}
	r := rows[idx]
	if !r.MetaLoaded {
		m.notice = fmt.Sprintf("still loading metadata for %s — try again in a moment", r.Tag)
		return m, nil
	}
	if !direct {
		tag, digest := r.Tag, r.Meta.Digest
		return m, func() tea.Msg { return SelectedMsg{ImageRepo: m.imageRepo, Tag: tag, Digest: digest} }
	}
	m.confirming = true
	m.confirmValue = false
	verb := fmt.Sprintf("Commit %s directly to %s's base branch — no PR, no review? This is only offered for non-production envs.", r.Tag, m.target)
	m.confirmDirect = huh.NewConfirm().Title(verb).Value(&m.confirmValue)
	m.confirmDirect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	if m.width > 0 {
		m.confirmDirect.WithWidth(m.width)
	}
	return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirming = false
		if !m.confirmValue {
			return m, nil
		}
		rows := m.filtered()
		idx := IndexOf(rows, m.selectedTag)
		if idx < 0 {
			return m, nil
		}
		r := rows[idx]
		tag, digest := r.Tag, r.Meta.Digest
		return m, func() tea.Msg { return DirectRequestedMsg{ImageRepo: m.imageRepo, Tag: tag, Digest: digest} }
	}
	_, cmd := m.confirmDirect.Update(msg)
	return m, cmd
}

func (m Model) updateFilter(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		switch kmsg.String() {
		case "enter":
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		case "esc":
			m.filtering = false
			m.filterQuery = ""
			m.filterInput.SetValue("")
			m.filterInput.Blur()
			return m.fetchVisible()
		}
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	if m.filterInput.Value() != m.filterQuery {
		m.filterQuery = m.filterInput.Value()
		rows := m.filtered()
		if IndexOf(rows, m.selectedTag) < 0 && len(rows) > 0 {
			m.selectedTag = rows[0].Tag
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.fetchVisible()
		cmd = tea.Batch(cmd, fetchCmd)
	}
	return m, cmd
}

// pageSize is how many rows fetchVisible keeps warm around the cursor: the visible body
// height when known, else a small fixed window so a picker that hasn't been sized yet (a
// snapshot test at a fixed size, in particular) still fetches something sane.
func (m Model) pageSize() int {
	if m.height > 4 {
		return max(m.height-4, 5)
	}
	return 15
}

// visibleWindow computes the half-open [start,end) slice of rows this screen keeps warm and
// draws, centered on (or otherwise including) the cursor — the single source of truth both
// fetchVisible (what gets a MetaFunc call) and viewReady (what actually gets rendered) share, so
// the two can never drift apart (AGENTS.md §8, layered checks: one definition of "visible", not
// two that can disagree about what's on screen). rows is the caller's own m.filtered() result,
// passed in rather than recomputed here so a caller that already has it doesn't pay for it
// twice.
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

// fetchVisible fires MetaFunc for every row within the current visibleWindow that isn't already
// loaded or loading — AGENTS.md invariant 4's "fetch on demand as rows become visible/
// selected", never every tag up front. Called after the list loads, on cursor movement, on a
// filter change, and after a metadata load reorders the list (which can shift what's near the
// cursor for the unmapped/Created-sorted case).
func (m Model) fetchVisible() (Model, tea.Cmd) {
	rows := m.filtered()
	if len(rows) == 0 {
		return m, nil
	}
	start, end := m.visibleWindow(rows)

	byTag := make(map[string]int, len(m.rows))
	for i, r := range m.rows {
		byTag[r.Tag] = i
	}
	var cmds []tea.Cmd
	for _, r := range rows[start:end] {
		i := byTag[r.Tag]
		if m.rows[i].MetaLoaded || m.rows[i].MetaLoading {
			continue
		}
		m.rows[i].MetaLoading = true
		cmds = append(cmds, m.fetchCmd(r.Tag))
	}
	return m, tea.Batch(cmds...)
}

// SetSize lays the screen out to width × height.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	if m.confirmDirect != nil {
		m.confirmDirect.WithWidth(width)
	}
	m.filterInput.SetWidth(max(width-10, 10))
	return m
}

// SetStyles applies the palette's dark/light flag to huh's own Charm theme (AGENTS.md §4.7:
// a component's own theming is not a layout library).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	if m.confirmDirect != nil {
		m.confirmDirect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	return m
}

// View renders the current state. Every rendered string passes through redact.Strings once
// more here (AGENTS.md §4.4/R-002), matching plan.Model.View's own final-boundary pattern.
func (m Model) View() string {
	var out string
	switch {
	case m.err != nil:
		out = m.viewErr()
	case m.state == stateLoading:
		out = m.viewLoading()
	case m.confirming:
		out = strings.Join([]string{m.header(), m.confirmDirect.View(), ui.StatusBar(m.width, "", "enter confirm · esc back")}, "\n")
	default:
		out = m.viewReady()
	}
	return redact.Strings(out)
}

func (m Model) header() string {
	left := fmt.Sprintf("hoist tags: %s -> %s", m.imageRepo, m.target)
	right := "revision: " + Revision + " (pkg/migrate, later)"
	return ui.StatusBar(m.width, left, right)
}

func (m Model) viewErr() string {
	parts := []string{m.header(), m.styles.Notice.Render(m.err.Error())}
	parts = append(parts, ui.StatusBar(m.width, "", "esc back"))
	return strings.Join(parts, "\n")
}

func (m Model) viewLoading() string {
	parts := []string{m.header(), m.spinner.View() + " listing tags…"}
	parts = append(parts, ui.StatusBar(m.width, "", "esc back"))
	return strings.Join(parts, "\n")
}

func (m Model) viewReady() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	if m.filtering {
		fmt.Fprintf(&b, "filter: %s\n", m.filterInput.View())
	} else if m.filterQuery != "" {
		fmt.Fprintf(&b, "filter: %q (esc in filter mode to clear)\n", m.filterQuery)
	}
	if m.hasStagingMismatch {
		b.WriteString(m.styles.Notice.Render(fmt.Sprintf("note: %s (paired staging) is currently running %s", m.stagingEnv, m.stagingTag)))
		b.WriteString("\n")
	}
	b.WriteString(m.tableHeader())
	b.WriteString("\n")

	rows := m.filtered()
	if len(rows) == 0 {
		b.WriteString("  (no matching tags)\n")
	}
	// Windowed to the same [start,end) fetchVisible uses (visibleWindow), so the cursor's row
	// is always among what's drawn — moving the cursor past one screen's worth of rows must
	// scroll the window with it, never leave the selected row off-screen (finding 5).
	start, end := m.visibleWindow(rows)
	dividerShown := false
	for _, r := range rows[start:end] {
		if m.mapped && !r.HasGitDate && !dividerShown {
			b.WriteString(strings.Repeat("─", 4) + " unordered (no matching git tag) " + strings.Repeat("─", 4) + "\n")
			dividerShown = true
		}
		b.WriteString(m.rowLine(r))
		b.WriteString("\n")
	}

	if m.notice != "" {
		b.WriteString(m.styles.Notice.Render(m.notice))
		b.WriteString("\n")
	}
	help := "↑/↓ move · / filter · enter select"
	if !m.production {
		help += " · D direct commit"
	}
	b.WriteString(ui.StatusBar(m.width, "", help))
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) tableHeader() string {
	return fmt.Sprintf("  %-30s %-25s %-14s %s", "TAG", "CREATED", "DIGEST", "REV")
}

func (m Model) rowLine(r Row) string {
	marker := "  "
	if r.Tag == m.selectedTag {
		marker = "> "
	}
	created := m.createdCell(r)
	digest := m.digestCell(r)
	return fmt.Sprintf("%s%-30s %-25s %-14s %s", marker, r.Tag, created, digest, Revision)
}

func (m Model) createdCell(r Row) string {
	switch {
	case r.HasGitDate:
		return r.GitDate.Format("2006-01-02 15:04")
	case r.MetaLoaded:
		return r.Meta.Created.Format("2006-01-02 15:04")
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
