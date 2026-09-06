// Package plan is the plan/confirm screen: it runs discovery + digest resolution +
// gitops.BuildPlan for one source/target env pair, shows a tickable list of image repos on
// the left and, on the right, what the promotion ships for the repo under the cursor — the
// commits between what the target declares and what the source resolved to, and the
// migrations among them (M10, #85 screen 04/12) — with the unified diff one key away.
// rows.go derives the rows, the diff and the warning text from a gitops.Plan and
// resolve.Resolution map with no terminal dependency, so it is unit-testable as plain
// values (AGENTS.md §4.8); model.go lays that data out with huh fields (MultiSelect for the
// repo list, Select for the target-env prompt, Confirm for the direct-mode switch) and a
// bubbles/v2 spinner + viewport, inside the M10 frame.
package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/resolve"
)

// Mode is the write path this plan would use.
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
	Resolutions map[string]resolve.Resolution
	KubeContext string
	// RegistryAuth names the credential source that authenticated, "" when none did.
	RegistryAuth string
	// RegistryConsulted is true when the registry was asked at all (win or lose) —
	// distinct from RegistryAuth == "", which is also true when the registry was never
	// consulted in the first place. Summary uses the two together so "not consulted" and
	// "consulted, every source failed" are never confused, the same distinction
	// cmd/hoist's own resolutionReport.print makes (AGENTS.md §4.10).
	RegistryConsulted bool
	// RegistryAuthTried names the configured credential chain, for the "all failed"
	// wording when RegistryConsulted is true and RegistryAuth is "".
	RegistryAuthTried []string
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

// StartMsg is emitted when the operator confirms this plan (Enter, in stateReady) —
// whichever ticked repos are selected should now start driving as a promotion. It carries
// plan-shaped data only: this package has no repoFullName (RepoConfig.GitHub), no CI/
// approval policy, and no git.Git/forge.Forge adaptor to build a real
// engine.PromotionState or a flight.DriveFunc from (AGENTS.md §4.3/§4.8 — a screen never
// imports those adaptor packages). The root recognizes StartMsg by concrete type
// (AGENTS.md §4.8) and pushes internal/app/flight.
type StartMsg struct {
	Plan    gitops.Plan
	Outcome ResolveOutcome
	Mode    string
	// Ticked is the repo set the operator selected in the multiSelect, unmodified — the
	// same set recomputeDiff already filters Plan.Edits by.
	Ticked         []string
	Source, Target string
}

// loadedMsg is delivered once the async discovery+resolution+BuildPlan cmd finishes.
type loadedMsg struct {
	plan    gitops.Plan
	outcome ResolveOutcome
	// err is fatal for this screen (rendered, never panics): either resolveFn failed —
	// which AGENTS.md §4.10 states is a whole-operation failure whenever resolution was
	// attempted at all, the same asymmetry cmd/hoist's runPlan enforces for the CLI — or
	// BuildPlan itself failed. The screen must not have a looser gate on that rule than
	// the command line does; it never plans from unverified manifest values when the
	// cluster it was told to consult could not be reached.
	err error
}

// historyMsg is one repo's delta (what the target declares → what the source resolved to).
// gen drops an answer from a plan screen since popped and reopened.
type historyMsg struct {
	gen   uint64
	repo  string
	delta migrate.Delta
	err   error
}

var nextGen atomic.Uint64

type keyMap struct {
	SwitchPane, Mode, YAML, Enter, Back key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		SwitchPane: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch pane")),
		Mode:       key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mode")),
		YAML:       key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "yaml")),
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
	histFn     history.Funcs
	now        func() time.Time
	gen        uint64

	source, target string

	state state
	err   error

	envSelect *huh.Select[string]

	spinner spinner.Model
	status  string

	plan    gitops.Plan
	outcome ResolveOutcome
	rows    []Row
	prefix  string // the image-repo prefix every row shares, shown once in the header
	deltas  map[string]history.State

	multiSelect *huh.MultiSelect[string]
	ticked      []string // bound to multiSelect's accessor

	viewport viewport.Model
	diff     string
	showYAML bool

	// ctx scopes every history request this screen issues; cancel runs when the screen is
	// left (esc, or enter handing off to the flight screen), so a delta still loading does
	// not keep calling the forge for a screen nobody is looking at.
	ctx    context.Context
	cancel context.CancelFunc

	confirming    bool
	confirmDirect *huh.Confirm
	// confirmValue is huh's write target only; the answer is read from the widget
	// (confirmAgreed) — see internal/app/tags for why the field itself never moves.
	confirmValue bool

	focus  focusPane
	mode   string
	notice string

	styles        ui.Styles
	keys          keyMap
	width, height int
	leftWidth     int
}

// New builds the plan screen for one source env. target is the configured pair for source
// (envs.pairs[source]), or "" when there is none; forcePrompt is true when the matrix
// screen's P (rather than p) opened it, which always prompts even when a pair is
// configured. resolveFn is nil in "digest sources: none" mode. hist is the commit-history
// bundle (M10); a zero value degrades every repo to a named gap.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, source, target string, forcePrompt bool, resolveFn ResolveFunc, hist history.Funcs) Model {
	m := Model{
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		resolveFn:  resolveFn,
		histFn:     hist,
		now:        time.Now,
		gen:        nextGen.Add(1),
		source:     source,
		target:     target,
		keys:       defaultKeyMap(),
		mode:       ModePR,
		spinner:    spinner.New(spinner.WithSpinner(spinner.Line)),
		viewport:   viewport.New(),
		deltas:     map[string]history.State{},
		styles:     ui.NewStyles(true),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if target == "" || forcePrompt {
		m.state = stateSelectEnv
		m.buildEnvSelect()
	} else {
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", source)
	}
	return m
}

// leave cancels the screen's outstanding history requests; every path off the screen calls it.
func (m Model) leave() Model {
	if m.cancel != nil {
		m.cancel()
	}
	return m
}

// WithNow fixes the clock relative dates are worded against (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

func (m *Model) buildEnvSelect() {
	candidates := TargetsFor(m.repo, m.source)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote %s to…", m.source)).Value(&m.target)
	// Wires Down/Up/"/" filtering the same way a huh.Form/Group would, without adopting either
	// (AGENTS.md §4.7 — this is component wiring, not layout). See CapturesText's own doc
	// comment for why this call has to happen here rather than being left to a Form/Group.
	sel.WithKeyMap(huh.NewDefaultKeyMap())
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

// labelWidth is what a left-pane option may take before huh's own "> ✓ " marker.
func (m Model) labelWidth() int {
	if m.leftWidth <= 0 {
		return 34
	}
	return max(m.leftWidth-6, 12)
}

func (m *Model) buildMultiSelect() {
	sel := Selectable(m.rows)
	m.ticked = make([]string, 0, len(sel))
	for _, r := range sel {
		m.ticked = append(m.ticked, r.Repo)
	}
	m.rebuildMultiSelect()
}

// rebuildMultiSelect builds the left pane's field for the current width, keeping the ticked
// set (bound through Value) — called at load and again when the pane's width changes, since
// the labels are truncated to it and huh would otherwise wrap them.
func (m *Model) rebuildMultiSelect() {
	sel := Selectable(m.rows)
	opts := make([]huh.Option[string], 0, len(sel))
	ticked := map[string]bool{}
	for _, t := range m.ticked {
		ticked[t] = true
	}
	for _, r := range sel {
		opts = append(opts, huh.NewOption(r.ShortLabel(m.prefix, m.labelWidth()), r.Repo).Selected(ticked[r.Repo]))
	}
	ms := huh.NewMultiSelect[string]().Value(&m.ticked)
	// Same wiring as buildEnvSelect's own WithKeyMap call — see CapturesText's doc comment.
	// This is what makes Down/Space("x")/"/" actually reach the field's Update.
	ms.WithKeyMap(huh.NewDefaultKeyMap())
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
		if resolveFn != nil {
			out, err := resolveFn(context.Background(), repo, source)
			if err != nil {
				// A resolveFn error means digest resolution was attempted and failed
				// outright — the cluster was unreachable, or the resolution
				// configuration itself was invalid. It is never a per-repo registry
				// miss (pkg/resolve handles that as an unresolved Resolution, not an
				// error return), so there is nothing safe left to plan from: fail the
				// screen exactly as cmd/hoist's plan command fails the whole run,
				// rather than building a selectable plan from manifest values nobody
				// has confirmed against the running environment.
				return loadedMsg{err: fmt.Errorf("digest resolution: %w", err)}
			}
			outcome = out
		}
		digests := resolve.Digests(outcome.Resolutions)
		pl, err := gitops.BuildPlanWith(repo, source, target, promotable, digests, resolve.Reasons(outcome.Resolutions))
		if err == nil {
			pl.Warnings = append(resolve.Warnings(outcome.Resolutions), pl.Warnings...)
		}
		return loadedMsg{plan: pl, outcome: outcome, err: err}
	}
}

// historyCmds asks for every selectable row's delta at once (the adaptor caches, and the
// confirm screen wants all of them for its totals), or nil when history is not wired.
func (m Model) historyCmds() tea.Cmd {
	if m.histFn.Delta == nil {
		return nil
	}
	delta, gen, ctx := m.histFn.Delta, m.gen, m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmds := make([]tea.Cmd, 0, len(m.rows))
	for _, r := range m.rows {
		if r.Disabled {
			continue
		}
		from, to, repo := r.Old, r.New, r.Repo
		m.deltas[repo] = history.State{}
		cmds = append(cmds, func() tea.Msg {
			d, err := delta(ctx, from, to)
			return historyMsg{gen: gen, repo: repo, delta: d, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// Update handles the screen's own keys, the loading messages, and forwards everything else
// to whichever huh field or bubbles component owns the current state.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loadedMsg:
		return m.onLoaded(msg)
	case historyMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.deltas[msg.repo] = history.State{Loaded: true, Delta: msg.delta, Err: msg.err}
		return m.refreshRight(), nil
	case spinner.TickMsg:
		if m.state == stateLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyPressMsg:
		if key.Matches(msg, m.keys.Back) && !m.confirming {
			return m.leave(), func() tea.Msg { return BackMsg{} }
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

func (m Model) onLoaded(msg loadedMsg) (Model, tea.Cmd) {
	m.state = stateReady
	m.status = ""
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.plan = msg.plan
	m.outcome = msg.outcome
	m.rows = DeriveRows(m.plan, m.outcome.Resolutions)
	m.prefix = CommonPrefix(m.rows)
	m.buildMultiSelect()
	m = m.recomputeDiff()
	m = m.layout()
	return m.refreshRight(), m.historyCmds()
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
	// Re-read m.target from the field itself rather than trusting buildEnvSelect's
	// Value(&m.target) binding to have kept it current: this Model is a value passed by copy
	// through every Update in the chain, so the pointer that binding captured addresses a
	// Model snapshot that stopped being "the" model the instant New returned. Without this
	// resync, Down could move the highlighted option while m.target silently stayed pinned
	// to whichever option construction time happened to default to.
	if v, ok := m.envSelect.GetValue().(string); ok {
		m.target = v
	}
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
		if len(m.ticked) == 0 {
			m.notice = "nothing ticked to promote"
			return m, nil
		}
		ticked := append([]string(nil), m.ticked...)
		plan, outcome, mode, source, target := m.plan, m.outcome, m.mode, m.source, m.target
		return m.leave(), func() tea.Msg {
			return StartMsg{Plan: plan, Outcome: outcome, Mode: mode, Ticked: ticked, Source: source, Target: target}
		}
	case key.Matches(kmsg, m.keys.Mode):
		if IsProduction(m.target, m.envs) {
			m.notice = fmt.Sprintf("direct mode is not offered for %s: it is a production env, so every change goes through a PR", m.target)
			return m, nil
		}
		m.confirming = true
		m.buildConfirm()
		return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
	case key.Matches(kmsg, m.keys.YAML):
		m.showYAML = !m.showYAML
		m = m.refreshRight()
		m.viewport.GotoTop() // two unrelated documents; a scroll offset from one hides the other's head
		return m, nil
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
	hoveredBefore, _ := m.multiSelect.Hovered()
	_, cmd := m.multiSelect.Update(msg)
	// Re-read m.ticked from the field itself — see updateSelectEnv's own resync comment for
	// why buildMultiSelect's Value(&m.ticked) binding alone can't keep this Model's own copy
	// current across Toggle presses (space/x), only View()'s rendering (which reads the
	// field's own internal option state directly, not m.ticked).
	if v, ok := m.multiSelect.GetValue().([]string); ok {
		m.ticked = v
	}
	if !equalSets(before, m.ticked) {
		m = m.recomputeDiff()
	}
	if hovered, _ := m.multiSelect.Hovered(); hovered != hoveredBefore || !equalSets(before, m.ticked) {
		m = m.refreshRight()
	}
	return m, cmd
}

// buildConfirm builds the direct-mode dialog. WithKeyMap is not decoration: huh.NewConfirm
// leaves its keymap zero-valued, and a zero key.Binding matches nothing, so a standalone
// Confirm ignored y, n and the arrows — this screen's m gesture could not be completed by a
// real operator until M10, and its test had set the bound bool directly (#85's named trap;
// internal/app/tags and internal/app/deploy had already been fixed the same way).
func (m *Model) buildConfirm() {
	verb := "Switch to direct mode (commit straight to the default branch, no PR)?"
	if m.mode == ModeDirect {
		verb = "Switch back to PR mode?"
	}
	m.confirmValue = false
	m.confirmDirect = huh.NewConfirm().Title(verb).Value(&m.confirmValue)
	m.confirmDirect.WithKeyMap(huh.NewDefaultKeyMap())
	m.confirmDirect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmDirect.WithWidth(m.dialogWidth())
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		switch kmsg.String() {
		case "esc":
			m.confirming = false
			return m, nil
		case "enter":
			m.confirming = false
			// Read from the widget, never from confirmValue: Value(&m.confirmValue) captured a
			// field on the Model copy that built the widget, which every Update since has
			// superseded.
			if m.confirmAgreed() {
				if m.mode == ModeDirect {
					m.mode = ModePR
				} else {
					m.mode = ModeDirect
				}
			}
			return m, nil
		}
	}
	f, cmd := m.confirmDirect.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmDirect = c
	}
	return m, cmd
}

func (m Model) confirmAgreed() bool {
	if m.confirmDirect == nil {
		return false
	}
	v, _ := m.confirmDirect.GetValue().(bool)
	return v
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
	return m
}

// refreshRight re-renders the right pane's content into the viewport: the impact for the
// hovered repo, or the yaml view.
func (m Model) refreshRight() Model {
	if m.showYAML {
		m.viewport.SetContent(m.rightBody())
	} else {
		m.viewport.SetContent(m.impactBody())
	}
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

// CapturesText implements app.Screen (via planScreen's thin delegate in internal/app/screen.go).
// The root queries this before treating "q" as its own global quit key (round 5, finding 3).
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

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

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
	body := max(ui.BodyHeight(m.height, sections)-2-strings.Count(notes, "\n")-boolInt(notes != ""), 3)
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
		m.multiSelect.WithHeight(max(body-len(Disabled(m.rows))-boolInt(len(Disabled(m.rows)) > 0), 3))
	}
	if m.confirmDirect != nil {
		m.confirmDirect.WithWidth(m.dialogWidth())
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	return redact.Strings(out)
}

func (m Model) title() string {
	if m.showYAML && m.state == stateReady {
		return "hoist · confirm promotion · yaml"
	}
	return "hoist · confirm promotion"
}

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
	case IsProduction(m.target, m.envs):
		return m.styles.Production.Render("mode: PR · production")
	case m.mode == ModeDirect:
		return m.styles.Warn.Render("mode: DIRECT")
	default:
		return m.styles.Accent.Render("mode: PR")
	}
}

// modeLabel is the long form of the mode, for the production refusal's own wording.
func (m Model) modeLabel() string {
	if IsProduction(m.target, m.envs) {
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
	repos, commits, migrations, loading, missing, unresolved := 0, 0, 0, 0, 0, 0
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
		}
	}
	parts := []string{m.styles.Title.Render(plural(repos, "repo") + " ticked")}
	if commits > 0 || (loading == 0 && missing == 0 && repos > 0) {
		parts = append(parts, m.styles.Title.Render(plural(commits, "commit")))
	}
	if migrations > 0 {
		parts = append(parts, m.styles.Warn.Render(plural(migrations, "migration")))
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

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
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
			return m.styles.Warn.Render(ansi.Wordwrap("! "+skip, inner, ""))
		}
	}
	return ""
}

func (m Model) hints() string {
	var help string
	switch {
	case m.state == stateSelectEnv:
		help = "↑/↓ choose · enter confirm · esc back"
	case m.state == stateLoading:
		help = "esc back"
	case IsProduction(m.target, m.envs):
		help = "tab pane · x toggle · d yaml · enter confirm · esc back"
	default:
		help = "tab pane · x toggle · d yaml · m mode · enter confirm · esc back"
	}
	return ui.StatusBar(m.width, "", m.styles.Hint.Render(help))
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
	lines = append(lines, m.styles.Dim.Render(ansi.Wrap(fmt.Sprintf("%s · %s · digest from %s", plural(r.Count, "occurrence"), plural(r.Files, "file"), r.Source), width, "")))
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
			lines = append(lines, "", m.styles.Warn.Render(plural(len(st.Delta.Migrations), "migration")+" run on this promotion:"))
			for _, f := range st.Delta.Migrations {
				lines = append(lines, m.styles.Dim.Render("  "+ansi.Truncate(f, width-2, "…")))
			}
		}
	}
	if len(r.Warnings) > 0 {
		lines = append(lines, "")
		for _, w := range r.Warnings {
			lines = append(lines, m.styles.Warn.Render(ansi.Wrap("! "+redact.Strings(w.Message), width, "")))
		}
	}
	if r.Disabled {
		lines = append(lines, "", m.styles.Warn.Render(ansi.Wrap("not offered: "+redact.Strings(r.Reason), width, "")))
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
