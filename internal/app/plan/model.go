// Package plan is the plan/confirm screen: it runs discovery + digest resolution +
// gitops.BuildPlan for one source/target env pair, shows a tickable list of image repos on
// the left and their unified diff on the right, and reports (never writes — M2 has no write
// path) what a promotion would do. rows.go derives the rows, the diff and the warning text
// from a gitops.Plan and resolve.Resolution map with no terminal dependency, so it is
// unit-testable as plain values (AGENTS.md §4.8); model.go lays that data out with huh
// fields (MultiSelect for the repo list, Select for the target-env prompt, Confirm for the
// direct-mode switch) and a bubbles/v2 spinner + viewport.
package plan

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/resolve"
)

// Mode is the write path this plan would use; M2 never writes either one (issue #2).
const (
	ModePR     = "pr"
	ModeDirect = "direct"
)

// ResolveOutcome is what one ResolveFunc call returns: the resolution per repo plus which
// kube context and registry auth source were actually consulted, by name only (AGENTS.md
// §4.4) — the same facts `hoist plan --dry-run` prints in its Resolution section. A zero
// value is "digest sources: none": BuildPlan then plans from the manifests alone, exactly
// as M1 did.
type ResolveOutcome struct {
	Resolutions  map[string]resolve.Resolution
	KubeContext  string
	RegistryAuth string
}

// ResolveFunc resolves the source env's promotable occurrences to digests. cmd/hoist
// supplies it, wrapping whichever cluster and registry adaptors the CLI's own plan command
// builds (kube context, registry credential chain) — so this package never opens a
// cluster or registry connection itself (AGENTS.md §4.3) and never imports cmd (AGENTS.md
// §4.8). It always talks to a cluster/registry when called, so model.go calls it only from
// inside a tea.Cmd, never from Update directly. A nil ResolveFunc means "digest sources:
// none" from the start (no config, or resolution deliberately turned off); an error from a
// non-nil one degrades the same way, with a warning, rather than failing the screen.
type ResolveFunc func(ctx context.Context, repo *gitops.Repo, source string) (ResolveOutcome, error)

// state is which part of the screen is showing.
type state int

const (
	stateSelectEnv state = iota // prompting for the target env (huh.Select)
	stateLoading                // resolving + building the plan (spinner)
	stateReady                  // rows + diff shown
)

type focusPane int

const (
	focusLeft focusPane = iota
	focusRight
)

// BackMsg is emitted when the screen wants the root to pop back to whatever was
// underneath it. internal/app/doc.go notes that pop arrives with the first screen pushed on
// top of the matrix; this is that screen, so the root recognizes BackMsg by its concrete
// type in its own Update switch — screens still never import app (AGENTS.md §4.8).
type BackMsg struct{}

// loadedMsg is delivered once the async discovery+resolution+BuildPlan cmd finishes.
type loadedMsg struct {
	plan    gitops.Plan
	outcome ResolveOutcome
	warn    string // resolveFn's error text, "" when resolution was skipped or succeeded
	err     error  // a BuildPlan failure; fatal for this screen (rendered, never panics)
}

type keyMap struct {
	SwitchPane, Mode, Enter, Back key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		SwitchPane: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch pane")),
		Mode:       key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mode")),
		Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	}
}

// Model is the plan screen. It is a value: Update, SetSize and SetStyles return the
// updated model, matching internal/app/matrix's convention.
type Model struct {
	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	resolveFn  ResolveFunc

	source, target string

	state state
	err   error

	envSelect *huh.Select[string]

	spinner spinner.Model
	status  string

	plan    gitops.Plan
	outcome ResolveOutcome
	rows    []Row

	multiSelect *huh.MultiSelect[string]
	ticked      []string // bound to multiSelect's accessor

	viewport viewport.Model
	diff     string

	confirming    bool
	confirmDirect *huh.Confirm
	confirmValue  bool

	focus  focusPane
	mode   string
	notice string

	styles        ui.Styles
	keys          keyMap
	width, height int
	leftWidth     int // set by layout(); joinPanes pads the left pane to exactly this
}

// New builds the plan screen for one source env. target is the configured pair for source
// (envs.pairs[source]), or "" when there is none; forcePrompt is true when the matrix
// screen's P (rather than p) opened it, which always prompts even when a pair is
// configured. resolveFn is nil in "digest sources: none" mode.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, source, target string, forcePrompt bool, resolveFn ResolveFunc) Model {
	m := Model{
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		resolveFn:  resolveFn,
		source:     source,
		target:     target,
		keys:       defaultKeyMap(),
		mode:       ModePR,
		spinner:    spinner.New(spinner.WithSpinner(spinner.Line)),
		viewport:   viewport.New(),
	}
	if target == "" || forcePrompt {
		m.state = stateSelectEnv
		m.buildEnvSelect()
	} else {
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", source)
	}
	return m
}

func (m *Model) buildEnvSelect() {
	candidates := TargetsFor(m.repo, m.source)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote %s to…", m.source)).Value(&m.target)
	if len(candidates) > 0 {
		opts := make([]huh.Option[string], 0, len(candidates))
		for _, e := range candidates {
			opts = append(opts, huh.NewOption(e, e))
		}
		sel = sel.Options(opts...)
	} else {
		m.err = fmt.Errorf("no other env to promote %s to", m.source)
	}
	m.envSelect = sel
}

func (m *Model) buildMultiSelect() {
	sel := Selectable(m.rows)
	m.ticked = make([]string, 0, len(sel))
	opts := make([]huh.Option[string], 0, len(sel))
	for _, r := range sel {
		m.ticked = append(m.ticked, r.Repo)
		opts = append(opts, huh.NewOption(r.Label(), r.Repo))
	}
	ms := huh.NewMultiSelect[string]().Value(&m.ticked)
	if len(opts) > 0 {
		ms = ms.Options(opts...)
	}
	ms.Focus()
	m.multiSelect = ms
}

// Init kicks off whatever the starting state needs: the env-select prompt's focus, or the
// spinner tick plus the async load. Resolution talks to a cluster/registry, so it is a
// tea.Cmd here, never run inside Update.
func (m Model) Init() tea.Cmd {
	switch m.state {
	case stateSelectEnv:
		return tea.Batch(m.envSelect.Init(), m.envSelect.Focus())
	case stateLoading:
		return tea.Batch(m.spinner.Tick, m.loadCmd())
	default:
		return nil
	}
}

// loadCmd runs discovery-derived data already in repo, resolution and BuildPlan off the
// Update call stack (AGENTS.md §4.3: resolution opens a cluster/registry connection).
func (m Model) loadCmd() tea.Cmd {
	repo, source, target, promotable, resolveFn := m.repo, m.source, m.target, m.promotable, m.resolveFn
	return func() tea.Msg {
		var outcome ResolveOutcome
		var warn string
		if resolveFn != nil {
			out, err := resolveFn(context.Background(), repo, source)
			if err != nil {
				warn = err.Error()
			} else {
				outcome = out
			}
		}
		digests := resolve.Digests(outcome.Resolutions)
		pl, err := gitops.BuildPlan(repo, source, target, promotable, digests)
		if err == nil {
			pl.Warnings = append(resolve.Warnings(outcome.Resolutions), pl.Warnings...)
		}
		return loadedMsg{plan: pl, outcome: outcome, warn: warn, err: err}
	}
}

// Update handles the screen's own keys, the loading messages, and forwards everything else
// to whichever huh field or bubbles component owns the current state.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loadedMsg:
		return m.onLoaded(msg), nil
	case spinner.TickMsg:
		if m.state == stateLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyPressMsg:
		if key.Matches(msg, m.keys.Back) {
			return m, func() tea.Msg { return BackMsg{} }
		}
	}

	switch m.state {
	case stateSelectEnv:
		return m.updateSelectEnv(msg)
	case stateReady:
		return m.updateReady(msg)
	default:
		return m, nil
	}
}

func (m Model) onLoaded(msg loadedMsg) Model {
	m.state = stateReady
	m.status = ""
	if msg.err != nil {
		m.err = msg.err
		return m
	}
	m.plan = msg.plan
	m.outcome = msg.outcome
	if msg.warn != "" {
		m.plan.Warnings = append([]gitops.Warning{{
			Code:    "resolution-unavailable",
			Message: "digest resolution failed, planning from manifests alone: " + msg.warn,
		}}, m.plan.Warnings...)
	}
	m.rows = DeriveRows(m.plan, m.outcome.Resolutions)
	m.buildMultiSelect()
	m = m.recomputeDiff()
	return m.layout()
}

func (m Model) updateSelectEnv(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		if m.target == "" {
			return m, nil
		}
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", m.source)
		return m, m.Init()
	}
	_, cmd := m.envSelect.Update(msg)
	return m, cmd
}

func (m Model) updateReady(msg tea.Msg) (Model, tea.Cmd) {
	if m.confirming {
		return m.updateConfirm(msg)
	}
	kmsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	m.notice = ""
	switch {
	case key.Matches(kmsg, m.keys.Enter):
		m.notice = "promotion lands in M3"
		return m, nil
	case key.Matches(kmsg, m.keys.Mode):
		if IsProduction(m.target, m.envs) {
			m.notice = fmt.Sprintf("direct mode is not offered for %s: it is a production env (AGENTS.md §4.5)", m.target)
			return m, nil
		}
		m.confirming = true
		m.confirmValue = m.mode == ModeDirect
		m.buildConfirm()
		return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
	case key.Matches(kmsg, m.keys.SwitchPane):
		if m.focus == focusLeft {
			m.focus = focusRight
			if m.multiSelect != nil {
				m.multiSelect.Blur()
			}
		} else {
			m.focus = focusLeft
			if m.multiSelect != nil {
				m.multiSelect.Focus()
			}
		}
		return m, nil
	}
	if m.focus == focusRight {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	if m.multiSelect == nil {
		return m, nil
	}
	before := append([]string(nil), m.ticked...)
	_, cmd := m.multiSelect.Update(msg)
	if !equalSets(before, m.ticked) {
		m = m.recomputeDiff()
	}
	return m, cmd
}

func (m *Model) buildConfirm() {
	verb := "Switch to direct mode (commit straight to the default branch, no PR)?"
	if m.mode == ModeDirect {
		verb = "Switch back to PR mode?"
	}
	m.confirmDirect = huh.NewConfirm().Title(verb).Value(&m.confirmValue)
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirming = false
		if m.confirmValue {
			m.mode = ModeDirect
		} else {
			m.mode = ModePR
		}
		return m, nil
	}
	_, cmd := m.confirmDirect.Update(msg)
	return m, cmd
}

func (m Model) recomputeDiff() Model {
	ticked := map[string]bool{}
	for _, r := range m.ticked {
		ticked[r] = true
	}
	diff, err := RenderDiff(m.repo.Root, m.plan.Edits, ticked)
	if err != nil {
		m.err = err
		return m
	}
	m.diff = diff
	m.viewport.SetContent(m.rightBody())
	return m
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

// SetSize lays every huh field and the viewport out inside width × height.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m.layout()
}

func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	bodyHeight := max(m.height-2, 1) // header line + status bar
	m.leftWidth = min(max(m.width*2/5, 28), 56)
	rightWidth := max(m.width-m.leftWidth-1, 10)

	if m.envSelect != nil {
		m.envSelect.WithWidth(m.width)
		m.envSelect.WithHeight(bodyHeight)
	}
	if m.multiSelect != nil {
		m.multiSelect.WithWidth(m.leftWidth)
		m.multiSelect.WithHeight(bodyHeight)
	}
	if m.confirmDirect != nil {
		m.confirmDirect.WithWidth(m.width)
	}
	m.viewport.SetWidth(rightWidth)
	m.viewport.SetHeight(bodyHeight)
	return m
}

// SetStyles applies the palette's dark/light flag to every huh field via huh's own Charm
// theme (AGENTS.md §4.7: no layout library, but a component's own theming is not one).
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
	return m
}

// View renders the current state.
func (m Model) View() string {
	switch m.state {
	case stateSelectEnv:
		return m.viewSelectEnv()
	case stateLoading:
		return m.viewLoading()
	default:
		return m.viewReady()
	}
}

func (m Model) header() string {
	target := m.target
	if target == "" {
		target = "?"
	}
	left := fmt.Sprintf("hoist plan: %s -> %s", m.source, target)
	right := m.modeLabel()
	return ui.StatusBar(m.width, left, right)
}

func (m Model) modeLabel() string {
	if IsProduction(m.target, m.envs) {
		return fmt.Sprintf("direct mode unavailable: %s is production (AGENTS.md §4.5)", m.target)
	}
	if m.mode == ModeDirect {
		return "mode: DIRECT"
	}
	return "mode: PR"
}

func (m Model) viewSelectEnv() string {
	parts := []string{m.header()}
	if m.envSelect != nil {
		parts = append(parts, m.envSelect.View())
	}
	if m.err != nil {
		parts = append(parts, m.styles.Notice.Render(m.err.Error()))
	}
	parts = append(parts, ui.StatusBar(m.width, "", "enter confirm · esc back"))
	return strings.Join(parts, "\n")
}

func (m Model) viewLoading() string {
	parts := []string{m.header(), m.spinner.View() + " " + m.status}
	parts = append(parts, ui.StatusBar(m.width, "", "esc back"))
	return strings.Join(parts, "\n")
}

func (m Model) viewReady() string {
	if m.err != nil {
		parts := []string{m.header(), m.styles.Notice.Render(m.err.Error())}
		parts = append(parts, ui.StatusBar(m.width, "", "esc back"))
		return strings.Join(parts, "\n")
	}
	if skip := m.skipNotice(); skip != "" && m.notice == "" {
		m.notice = skip
	}
	parts := []string{m.header()}
	if m.confirming && m.confirmDirect != nil {
		parts = append(parts, m.confirmDirect.View())
	} else {
		parts = append(parts, joinPanes(m.leftBody(), m.viewport.View(), m.leftWidth))
	}
	left := m.notice
	right := "tab pane · x toggle · m mode · enter confirm · esc back"
	parts = append(parts, ui.StatusBar(m.width, left, right))
	return strings.Join(parts, "\n")
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
	if dis := Disabled(m.rows); len(dis) > 0 {
		b.WriteString("\n")
		for _, r := range dis {
			marker := "  "
			if len(r.Warnings) > 0 {
				marker = "! "
			}
			fmt.Fprintf(&b, "%s%s  (%s)\n", marker, r.Repo, r.Reason)
		}
	}
	return b.String()
}

func (m Model) rightBody() string {
	var b strings.Builder
	if m.diff != "" {
		b.WriteString(m.diff)
	} else {
		b.WriteString("(no ticked edits)\n")
	}
	fmt.Fprintf(&b, "\nUntouched (%d):\n", len(m.plan.Untouched))
	for _, ref := range m.plan.Untouched {
		fmt.Fprintf(&b, "  %s\n", ref)
	}
	fmt.Fprintf(&b, "\nWarnings (%d):\n", len(m.plan.Warnings))
	for _, w := range m.plan.Warnings {
		fmt.Fprintf(&b, "  [%s] %s\n", w.Code, strings.ReplaceAll(w.Message, "\n", "\n  "))
	}
	b.WriteString("\nResolution:\n")
	for _, line := range Summary(m.outcome) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

// joinPanes lays the left and right pane text side by side, one gutter column apart,
// padding every left line out to leftWidth (the width the multiSelect was itself sized to
// in layout()) so the gutter stays a straight column regardless of how a huh field wraps
// its own text. Width is measured with ansi.StringWidth, not len(), so an SGR-styled line
// pads by its visible width rather than its byte or rune count — the same convention
// internal/app/matrix/cells.go and internal/ui/statusbar.go use. No layout library
// (AGENTS.md §4.7): plain string composition, like internal/app/matrix's own View.
func joinPanes(left, right string, leftWidth int) string {
	ll := strings.Split(strings.TrimRight(left, "\n"), "\n")
	rl := strings.Split(strings.TrimRight(right, "\n"), "\n")
	n := max(len(ll), len(rl))
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		var l, r string
		if i < len(ll) {
			l = ll[i]
		}
		if i < len(rl) {
			r = rl[i]
		}
		pad := leftWidth - ansi.StringWidth(l)
		if pad < 0 {
			pad = 0
		}
		lines[i] = l + strings.Repeat(" ", pad) + " │ " + r
	}
	return strings.Join(lines, "\n")
}
