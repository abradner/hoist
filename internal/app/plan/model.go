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
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
)

// Mode is the write path this plan would use.
const (
	ModePR     = "pr"
	ModeDirect = "direct"
)

// Func builds one gitops.Plan through internal/service's own Plan — the identical builder
// `hoist plan`/`hoist promote`/`hoist deploy` use, so a plan built for this screen and one built
// for the CLI's dry run can never silently diverge again (AGENTS.md §4's "Divergences", item
// 10). cmd/hoist supplies it as svc.Plan, wrapping whichever cluster and registry adaptors the
// selected repo's config calls for — so this package never opens a cluster or registry
// connection itself (AGENTS.md §4.3) and never imports cmd (AGENTS.md §4.8). It always talks to
// a cluster/registry when resolution is configured, so model.go calls it only from inside a
// tea.Cmd, never from Update directly. req.Overrides are the operator's per-repo digest
// overrides (the o dialog, #102 — the TUI's `--digest`): Plan hands them to pkg/resolve exactly
// as the CLI does, so each one wins outright and is reported as [override] with the same
// alternatives and warnings.
type Func func(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error)

// ValidateOverride is what the o dialog applies to the text on enter: image.ParseOverride —
// the one predicate `--digest` applies too (AGENTS.md §8, layered checks: the CLI and the
// TUI must refuse the same inputs with the same words) — plus the dialog's own rule that the
// override is for the row it was opened on, since the dialog pre-fills that repo and a plan
// built with an override for some other repo would not be the one the operator was looking
// at. The returned Ref is what the resolver is handed.
func ValidateOverride(repo, text string) (image.Ref, error) {
	ref, err := image.ParseOverride(text)
	if err != nil {
		return image.Ref{}, err
	}
	if ref.Repo != repo {
		return image.Ref{}, fmt.Errorf("this dialog overrides %s; got %s", repo, ref.Repo)
	}
	return ref, nil
}

// state is which part of the screen is showing.
type state int

const (
	// stateSelectEnv prompts for the missing env with a huh.Select — the target when p opened
	// this screen with a source but no configured pair for it (rare, since p now always
	// carries a target), or the SOURCE when p opened it with a target but no unambiguous
	// reverse pair (the screen's own "promote into <target> from…", the normal ambiguous case).
	// selectingSource says which.
	stateSelectEnv state = iota
	stateLoading         // resolving + building the plan (spinner)
	stateReady           // rows + diff shown
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
	Outcome service.Resolution
	Mode    string
	// Ticked is the repo set the operator selected in the multiSelect, unmodified — the
	// same set recomputeDiff already filters Plan.Edits by.
	Ticked         []string
	Source, Target string
	// View is the service.RepoView the underlying planFn call actually planned against
	// (service.PlannedChange.View, carried through loadedMsg) — the root's StartPromotion call
	// passes this as StartRequest.View so the freshness check re-checks the SAME view this plan
	// was built from, rather than whatever the service's current view has since become (an F5
	// refresh between loading this screen and pressing Enter must not silently launder a plan
	// built from a now-stale view).
	View service.RepoView
}

// loadedMsg is delivered once the async discovery+resolution+BuildPlan cmd finishes. Returned
// wrapped as scope.Result[loadedMsg] (loadCmd), so a load an earlier plan screen instance kicked
// off — still running when the operator backed out and opened a new one — cannot land on rows
// this instance never asked for (audit FB-H3; the scope.Foreign guard at the top of Update drops
// it before this type is ever switched on).
type loadedMsg struct {
	plan    gitops.Plan
	outcome service.Resolution
	view    service.RepoView
	// err is fatal for this screen (rendered, never panics): either resolveFn failed —
	// which AGENTS.md §4.10 states is a whole-operation failure whenever resolution was
	// attempted at all, the same asymmetry cmd/hoist's runPlan enforces for the CLI — or
	// BuildPlan itself failed. The screen must not have a looser gate on that rule than
	// the command line does; it never plans from unverified manifest values when the
	// cluster it was told to consult could not be reached.
	err error
}

// historyMsg is one repo's delta (what the target declares → what the source resolved to).
// Returned wrapped as scope.Result[historyMsg] (historyCmds), so a delta requested by a plan
// screen since popped and reopened, or superseded by an override rebuild, is dropped by the
// scope.Foreign guard at the top of Update rather than landing on rows it was never asked about.
type historyMsg struct {
	repo  string
	delta migrate.Delta
	err   error
}

// RefreshMsg is r/F5/ctrl+r: rebuild the plan at fresh origin. The screen has no way to
// fetch origin itself (AGENTS.md §4.3 — a screen never opens a git/cluster/registry connection;
// service.Plan itself never fetches either, it only reads whatever repo the service currently
// holds), so this asks the root to run the matrix's own completion-triggered refresh
// (requestMatrixRefresh, the same fetch F5 already runs there) and rebuild this screen's plan
// once matrix.RepoRefreshedMsg lands with the new *gitops.Repo — internal/app/app.go's own new
// case, mirroring deploy.RefreshMsg exactly (the screen's own shared "r" path).
type RefreshMsg struct{}

// Model is the plan screen. It is a value: Update, SetSize and SetStyles return the
// updated model, matching internal/app/matrix's convention.
type Model struct {
	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	planFn     Func
	histFn     history.Funcs
	now        func() time.Time
	// scope is this instance's owned context plus its ID: minted here and again on every
	// override rebuild (Open), so a loadedMsg/historyMsg from a superseded load — a different
	// screen instance, or this same instance's own earlier request — is Foreign and dropped
	// (AGENTS.md §4.8), and Close (called by the root's pop/truncate once this screen is
	// actually removed from the stack) cancels any request still outstanding at that moment.
	scope scope.Scope

	source, target string

	state state
	err   error

	envSelect *huh.Select[string]
	// selectingSource is true when envSelect is prompting for the SOURCE (the screen's own
	// "promote into <target> from…") rather than the target — the field the huh binding
	// writes into, and what updateSelectEnv resyncs from GetValue and moves on from, differ
	// accordingly.
	selectingSource bool

	spinner spinner.Model
	status  string

	plan    gitops.Plan
	outcome service.Resolution
	// view is the service.RepoView the most recent loadCmd actually planned against (from
	// loadedMsg.view) — carried into StartMsg.View so the root's StartPromotion call checks
	// freshness against the view THIS plan was built from.
	view   service.RepoView
	rows   []Row
	prefix string // the image-repo prefix every row shares, shown once in the header
	deltas map[string]history.State

	multiSelect *huh.MultiSelect[string]
	ticked      []string // bound to multiSelect's accessor
	// keepTicked is set by an override rebuild so onLoaded carries the operator's ticked
	// set and cursor through it instead of ticking every row again as a first load does.
	keepTicked bool

	viewport viewport.Model
	diff     string
	showYAML bool

	confirming    bool
	confirmDirect *huh.Confirm
	// confirmValue is huh's write target only; the answer is read from the widget
	// (confirmAgreed) — see internal/app/tags for why the field itself never moves.
	confirmValue bool

	// overrides are the digests the operator has pinned by hand through the o dialog
	// (#102), keyed by image repo; every rebuild of the plan hands them to resolveFn and
	// applies them over its answer exactly as `hoist plan --digest` does.
	overrides map[string]image.Ref
	// overriding is true while the o dialog is up; overrideRepo is the row it was opened
	// on, and overrideErr the last refusal it showed (redacted), "" when none.
	overriding    bool
	overrideRepo  string
	overrideErr   string
	overrideInput *huh.Input
	// overrideValue is huh's write target only, like confirmValue; the text is read back
	// through GetValue (§9 entry 6).
	overrideValue string

	focus  focusPane
	mode   string
	notice string

	// starting is set the instant Enter emits StartMsg and never cleared by this screen on its
	// own (#PR8, FB-L4): a second Enter before the root has reacted at all would otherwise emit
	// a second StartMsg, and while session.Controller's own same-target refusal is the actual
	// enforcement against a second drive ever starting (AGENTS.md §8's deletion test — deleting
	// this field would only bring back a confusing "already in flight" notice flash, never a
	// second real start), there is no reason to manufacture that refusal on purpose every time
	// an operator's Enter key repeats. ResetStarting is the root's own undo for the one case
	// this screen cannot see for itself: a build that failed outright, popping the building
	// screen back to this same instance with nothing else changed (popBuildFailed, app.go) —
	// without it, Enter would stay silently dead on a confirm screen the operator is looking
	// straight at.
	starting bool

	styles        ui.Styles
	width, height int
	leftWidth     int
}

// ResetStarting clears the one-shot Enter guard starting sets — see its own doc comment. The
// root calls this after popping a failed build back onto this same screen instance.
func (m Model) ResetStarting() Model {
	m.starting = false
	return m
}

// newViewport is the impact pane's own scrolling body, bound to keys.ViewportKeyMap (P2-6 in
// the T3 review) rather than left on viewport.New()'s bubbles-library default — which binds
// space/f/b to page and bare "d"/"u" to half-page, none of it shown anywhere and "d" already
// meaning "toggle yaml diff" on this screen. SoftWrap is on (T3 followup, group 2): the yaml
// diff's whole point is the digest, and a hard-truncated line hides it past the pane's right
// edge — wrapping trades a taller line for a visible one.
func newViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = keys.ViewportKeyMap()
	v.SoftWrap = true
	return v
}

// New builds the plan screen, which retires the old "P forces a prompt" gesture: p on the
// matrix always names a Target (the cursor's column), and Source is either the one
// unambiguous reverse pair or "" — in which case this screen prompts "promote into <target>
// from…" itself, rather than the matrix ever forcing a prompt for an arbitrary target. A
// caller with a source but no target (kept for a caller that still has one, e.g. a future
// non-matrix producer) still gets the older "promote <source> to…" prompt instead. planFn is
// nil in tests that never load a plan through the real service (a fixture builds gitops.Plan
// directly via loadedMsg instead). hist is the commit-history bundle (M10); a zero value
// degrades every repo to a named gap.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, source, target string, planFn Func, hist history.Funcs) Model {
	m := Model{
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		planFn:     planFn,
		histFn:     hist,
		now:        time.Now,
		scope:      scope.Open(),
		source:     source,
		target:     target,
		mode:       ModePR,
		spinner:    spinner.New(spinner.WithSpinner(spinner.Line)),
		viewport:   newViewport(),
		deltas:     map[string]history.State{},
		overrides:  map[string]image.Ref{},
		styles:     ui.NewStyles(true),
	}
	switch {
	case target != "" && source == "":
		m.state = stateSelectEnv
		m.selectingSource = true
		m.buildSourceSelect()
	case target == "":
		m.state = stateSelectEnv
		m.buildEnvSelect()
	default:
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", source)
	}
	return m
}

// Close cancels this screen's outstanding history and load requests. Called by the root's own
// pop/truncate the moment this screen is actually removed from the stack (AGENTS.md §4.8) —
// never by this package itself any more: Enter hands off to the building screen without
// removing this one (m.start's own design, Train 2 design PR 2), and cancelling here on Enter
// used to strand every not-yet-loaded delta if that build then failed and the building screen
// popped back to a plan screen whose history could never load again (FB-M3).
func (m Model) Close() { m.scope.Close() }

// WithNow fixes the clock relative dates are worded against (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// Reload is RefreshMsg's own rebuild: the root calls this once matrix.RepoRefreshedMsg
// lands with a fresh *gitops.Repo, from origin, so this screen's plan is rebuilt against exactly
// what F5 just fetched rather than the boot-time snapshot it was opened with. A no-op outside
// stateReady: a load already in flight (stateLoading) or a still-open env prompt has nothing yet
// to rebuild FROM. keepTicked/a fresh scope ID mirror the override rebuild's own shape
// (updateOverride) — the operator's ticked set survives, intersected against whatever the fresh
// plan's rows turn out to be, and a load already outstanding for the stale repo is dropped by
// the scope.Foreign guard rather than landing on rows this reload never asked for.
func (m Model) Reload(repo *gitops.Repo) (Model, tea.Cmd) {
	if m.state != stateReady || repo == nil {
		return m, nil
	}
	m.repo = repo
	m.keepTicked = true
	m.scope.ID = scope.New()
	m.state = stateLoading
	m.status = fmt.Sprintf("resolving digests from %s pods…", m.source)
	return m, tea.Batch(m.spinner.Tick, m.loadCmd())
}

func (m *Model) buildEnvSelect() {
	candidates := TargetsFor(m.repo, m.source)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote %s to…", m.source)).Value(&m.target)
	// Wires Down/Up/"/" filtering the same way a huh.Form/Group would, without adopting either
	// (AGENTS.md §4.7 — this is component wiring, not layout). See CapturesText's own doc
	// comment for why this call has to happen here rather than being left to a Form/Group.
	sel.WithKeyMap(keys.HuhKeyMap())
	if len(candidates) > 0 {
		opts := make([]huh.Option[string], 0, len(candidates))
		for _, e := range candidates {
			opts = append(opts, huh.NewOption(e, e))
		}
		sel = sel.Options(opts...)
	} else {
		// (UX-M18's own convention, matrix's emptyView): name what to check, not just
		// what failed — hoist discovers envs from Argo CD Application wrappers under the apps
		// root, so a repo with only one is either genuinely single-env or looking in the wrong
		// place.
		m.err = fmt.Errorf("no other env to promote %s to — check repos[].envs.pairs and apps_root in the config, or --apps-root/--repo if this looked in the wrong place", m.source)
	}
	m.envSelect = sel
}

// buildSourceSelect is buildEnvSelect's mirror for the screen's own "promote into <target>
// from…": every other discovered env is a candidate source, bound to m.source instead of
// m.target.
func (m *Model) buildSourceSelect() {
	candidates := SourcesFor(m.repo, m.target)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote into %s from…", m.target)).Value(&m.source)
	sel.WithKeyMap(keys.HuhKeyMap())
	if len(candidates) > 0 {
		opts := make([]huh.Option[string], 0, len(candidates))
		for _, e := range candidates {
			opts = append(opts, huh.NewOption(e, e))
		}
		sel = sel.Options(opts...)
	} else {
		m.err = fmt.Errorf("no other env to promote into %s from — check repos[].envs.pairs and apps_root in the config, or --apps-root/--repo if this looked in the wrong place", m.target)
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
// set (bound through Value) and the cursor — called at load and again when the pane's width
// changes, since the labels are truncated to it and huh would otherwise wrap them.
//
// The cursor has to be put back by hand (#121): huh v2.0.3's MultiSelect exposes the cursor
// only through Hovered(), and re-setting Options on the existing widget is no better than a
// rebuild — Options calls selectOptions, which parks the cursor on the first ticked option.
// So the hovered index is read before the rebuild and walked back to with Up/Down presses
// through the widget's own Update (GotoTop is disabled in huh's default keymap, so there is
// no shorter route) — the same keys an operator would press, never a private field.
func (m *Model) rebuildMultiSelect() {
	hovered := m.hoveredIndex()
	sel := Selectable(m.rows)
	labels := Labels(sel, m.prefix, m.labelWidth())
	opts := make([]huh.Option[string], 0, len(sel))
	ticked := map[string]bool{}
	for _, t := range m.ticked {
		ticked[t] = true
	}
	for i, r := range sel {
		opts = append(opts, huh.NewOption(labels[i], r.Repo).Selected(ticked[r.Repo]))
	}
	ms := huh.NewMultiSelect[string]().Value(&m.ticked)
	// Same wiring as buildEnvSelect's own WithKeyMap call — see CapturesText's doc comment.
	// This is what makes Down/space("x" retired)/"/" actually reach the field's Update.
	ms.WithKeyMap(keys.HuhKeyMap())
	if len(opts) > 0 {
		ms = ms.Options(opts...)
	}
	ms.Focus()
	m.multiSelect = ms
	if hovered >= 0 {
		m.moveCursorTo(hovered)
	}
}

// intersect keeps the ticked repos that are still selectable rows, in row order.
func intersect(ticked []string, rows []Row) []string {
	set := map[string]bool{}
	for _, t := range ticked {
		set[t] = true
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if set[r.Repo] {
			out = append(out, r.Repo)
		}
	}
	return out
}

// hoveredIndex is the position of the hovered row among the selectable rows, -1 when there
// is no field or no row under the cursor.
func (m Model) hoveredIndex() int {
	if m.multiSelect == nil {
		return -1
	}
	repo, ok := m.multiSelect.Hovered()
	if !ok {
		return -1
	}
	for i, r := range Selectable(m.rows) {
		if r.Repo == repo {
			return i
		}
	}
	return -1
}

// moveCursorTo walks the field's cursor to index i with Up/Down presses; bounded by the row
// count so a stale index can never loop.
func (m *Model) moveCursorTo(i int) {
	n := len(Selectable(m.rows))
	for step := 0; step < n; step++ {
		at := m.hoveredIndex()
		if at < 0 || at == i {
			return
		}
		code := tea.KeyDown
		if at > i {
			code = tea.KeyUp
		}
		m.multiSelect.Update(tea.KeyPressMsg{Code: code})
	}
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

// loadCmd runs Plan (internal/service) off the Update call stack (AGENTS.md §4.3: resolution
// opens a cluster/registry connection) — the screen no longer builds the gitops.Plan itself
// (service:Plan, PR B): planFn is svc.Plan, so this screen's plan and the CLI's dry run can
// never silently diverge again.
func (m Model) loadCmd() tea.Cmd {
	repo, source, target, planFn := m.repo, m.source, m.target, m.planFn
	overrides := make(map[string]image.Ref, len(m.overrides))
	for k, v := range m.overrides {
		overrides[k] = v
	}
	sc := m.scope
	return scope.DoCtx(sc, scope.Resolve, func(ctx context.Context) loadedMsg {
		pc, err := planFn(ctx, service.PlanRequest{Repo: repo, Source: source, Target: target, Overrides: overrides})
		if err != nil {
			// A Func error means digest resolution was attempted and failed outright —
			// the cluster was unreachable, or the resolution configuration itself was
			// invalid. It is never a per-repo registry miss (pkg/resolve handles that as an
			// unresolved Resolution, not an error return), so there is nothing safe left to
			// plan from: fail the screen exactly as cmd/hoist's plan command fails the whole
			// run, rather than building a selectable plan from manifest values nobody has
			// confirmed against the running environment.
			return loadedMsg{err: fmt.Errorf("digest resolution: %w", err)}
		}
		var outcome service.Resolution
		if pc.Resolution != nil {
			outcome = *pc.Resolution
		}
		return loadedMsg{plan: pc.Plan, outcome: outcome, view: pc.View}
	})
}

// historyCmds asks for every selectable row's delta at once (the adaptor caches, and the
// confirm screen wants all of them for its totals), or nil when history is not wired.
func (m Model) historyCmds() tea.Cmd {
	if m.histFn.Delta == nil {
		return nil
	}
	delta, sc := m.histFn.Delta, m.scope
	cmds := make([]tea.Cmd, 0, len(m.rows))
	for _, r := range m.rows {
		if r.Disabled {
			continue
		}
		from, to, repo := r.Old, r.New, r.Repo
		m.deltas[repo] = history.State{}
		cmds = append(cmds, scope.DoCtx(sc, scope.History, func(ctx context.Context) historyMsg {
			d, err := delta(ctx, from, to)
			return historyMsg{repo: repo, delta: d, err: err}
		}))
	}
	return tea.Batch(cmds...)
}

// Update handles the screen's own keys, the loading messages, and forwards everything else
// to whichever huh field or bubbles component owns the current state.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if scope.Foreign(m.scope.ID, msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case scope.Result[loadedMsg]:
		return m.onLoaded(msg.V)
	case scope.Result[historyMsg]:
		hm := msg.V
		m.deltas[hm.repo] = history.State{Loaded: true, Delta: hm.delta, Err: hm.err}
		return m.refreshRight(), nil
	case spinner.TickMsg:
		if m.state == stateLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyPressMsg:
		if keys.Esc.Matches(msg) && !m.confirming && !m.overriding && !m.filtering() {
			// No cancel here any more (FB-M3): the root's own pop, triggered by this BackMsg,
			// calls Close on this screen the moment it is actually removed from the stack —
			// see Close's own doc comment. P1-2: while a huh field's own filter is open, esc
			// belongs to the widget (it closes the filter, not the screen) — see filtering's
			// own doc comment.
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

func (m Model) onLoaded(msg loadedMsg) (Model, tea.Cmd) {
	keep := m.keepTicked
	m.keepTicked = false
	m.state = stateReady
	m.status = ""
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.plan = msg.plan
	m.outcome = msg.outcome
	m.view = msg.view
	m.rows = DeriveRows(m.plan, m.outcome.Res)
	m.prefix = CommonPrefix(m.rows)
	if keep {
		// An override rebuild keeps what the operator unticked: rebuilding from scratch
		// would tick every row again, and Enter would promote the repos they had excluded.
		// The set is intersected with the rebuilt rows (a repo may have stopped being
		// selectable), and rebuildMultiSelect puts the cursor back by repo (#121).
		m.ticked = intersect(m.ticked, Selectable(m.rows))
		m.rebuildMultiSelect()
	} else {
		m.buildMultiSelect()
	}
	m = m.recomputeDiff()
	m = m.layout()
	return m.refreshRight(), m.historyCmds()
}

// filtering reports whether the currently-active huh field has its own filter box open (the
// operator typed "/"), for the field whichever state the screen is in owns. While it is true,
// every key — esc included — belongs to the widget, never to this screen's own key handling
// (P1-2 in the T3 review): a probe found "/" then "r" firing plan.RefreshMsg, "/" then enter
// firing plan.StartMsg (starting the promotion), and esc popping the whole screen instead of
// just closing the filter.
func (m Model) filtering() bool {
	switch m.state {
	case stateSelectEnv:
		return m.envSelect != nil && m.envSelect.GetFiltering()
	case stateReady:
		return m.multiSelect != nil && m.multiSelect.GetFiltering()
	default:
		return false
	}
}

func (m Model) updateSelectEnv(msg tea.Msg) (Model, tea.Cmd) {
	// P1-2: while the select's own filter is open (the operator typed "/"), every key —
	// including enter — belongs to the widget's filter box, never to this screen's own
	// enter-confirms-the-choice handling below. Without this check, enter while filtering
	// could confirm the still-highlighted pre-filter option instead of accepting the filter
	// text, and esc would fall through to huh's own Update, which on a Select swallows esc
	// silently rather than closing the screen — so forwarding it here is also what keeps a
	// dialog-level esc from being mistaken for "no-op" instead of "closes the filter".
	if m.envSelect.GetFiltering() {
		_, cmd := m.envSelect.Update(msg)
		if v, ok := m.envSelect.GetValue().(string); ok {
			if m.selectingSource {
				m.source = v
			} else {
				m.target = v
			}
		}
		return m, cmd
	}
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		if m.selectingSource {
			if m.source == "" {
				return m, nil
			}
		} else if m.target == "" {
			return m, nil
		}
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", m.source)
		return m, m.Init()
	}
	_, cmd := m.envSelect.Update(msg)
	// Re-read the field this select is bound to from the widget itself rather than trusting
	// buildEnvSelect's/buildSourceSelect's own Value(&m.target)/Value(&m.source) binding to
	// have kept it current: this Model is a value passed by copy through every Update in the
	// chain, so the pointer that binding captured addresses a Model snapshot that stopped
	// being "the" model the instant New returned. Without this resync, Down could move the
	// highlighted option while the field silently stayed pinned to whichever option
	// construction time happened to default to.
	if v, ok := m.envSelect.GetValue().(string); ok {
		if m.selectingSource {
			m.source = v
		} else {
			m.target = v
		}
	}
	return m, cmd
}

func (m Model) updateReady(msg tea.Msg) (Model, tea.Cmd) {
	if m.confirming {
		return m.updateConfirm(msg)
	}
	if m.overriding {
		return m.updateOverride(msg)
	}
	kmsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	// P1-2: while the multi-select's own filter is open, every key belongs to it — r, d, e,
	// shift+d and enter must never reach this screen's own switch below (a probe found "/"
	// then "r" firing plan.RefreshMsg and "/" then enter firing plan.StartMsg, which starts
	// the promotion while the operator was still typing a filter query).
	if m.multiSelect != nil && m.multiSelect.GetFiltering() {
		return m.updateMultiSelect(msg)
	}
	m.notice = ""
	// shift+d toggles direct mode — checked ahead of the switch below since
	// keys.Direct.Matches is the stateless write-binding test (rule 5's shift-vs-caps-lock
	// distinction), not a string a switch case could match directly (mirrors deploy.Model.onKey's
	// own gesture).
	if keys.Direct.Matches(kmsg) {
		return m.toggleDirect()
	}
	switch {
	case keys.Edit.Matches(kmsg):
		r, ok := m.hoveredRow()
		if !ok || m.err != nil {
			m.notice = "no repo under the cursor to override"
			return m, nil
		}
		m.overriding = true
		m.buildOverride(r.Repo)
		return m, tea.Batch(m.overrideInput.Init(), m.overrideInput.Focus())
	case keys.Enter.Matches(kmsg):
		if m.starting {
			// #PR8/FB-L4: a repeated Enter before the root has reacted at all — see starting's
			// own doc comment.
			return m, nil
		}
		if len(m.ticked) == 0 {
			m.notice = "nothing ticked to promote"
			return m, nil
		}
		m.starting = true
		ticked := append([]string(nil), m.ticked...)
		plan, outcome, mode, source, target, view := m.plan, m.outcome, m.mode, m.source, m.target, m.view
		// No cancel here (FB-M3): this screen stays under the building screen Enter pushes
		// (m.start's own design), and if that build fails the building screen pops back to
		// THIS screen — whose history must still be able to load, not be permanently stuck
		// from a cancel that fired the instant Enter was pressed.
		return m, func() tea.Msg {
			return StartMsg{Plan: plan, Outcome: outcome, Mode: mode, Ticked: ticked, Source: source, Target: target, View: view}
		}
	case keys.Refresh.Matches(kmsg):
		// r/F5/ctrl+r rebuilds the plan at fresh origin — see RefreshMsg's own doc
		// comment for why this only asks the root rather than fetching origin itself.
		return m, func() tea.Msg { return RefreshMsg{} }
	case keys.Diff.Matches(kmsg):
		m.showYAML = !m.showYAML
		m = m.refreshRight()
		m.viewport.GotoTop() // two unrelated documents; a scroll offset from one hides the other's head
		return m, nil
	// home/end on the impact pane (focusRight) — the multi-select's own
	// left-pane home/end already work through huh's own GotoTop/GotoBottom (keys.HuhKeyMap
	// binds them to "home"/"end"), reached by falling out of this switch to
	// updateMultiSelect's own forwarding below; the viewport gets no such keymap field
	// (bubbles' viewport.KeyMap has none), so it needs its own case here.
	case keys.Home.Matches(kmsg) && m.focus == focusRight:
		m.viewport.GotoTop()
		return m, nil
	case keys.End.Matches(kmsg) && m.focus == focusRight:
		m.viewport.GotoBottom()
		return m, nil
	case keys.Tab.Matches(kmsg):
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
	return m.updateMultiSelect(msg)
}

// updateMultiSelect forwards msg to the multi-select field unconditionally and resyncs
// m.ticked/the diff from it — the tail of updateReady's own handling, factored out so P1-2's
// filtering short-circuit above can reach it directly without falling through the switch that
// owns r/d/e/enter/tab when the filter isn't open.
func (m Model) updateMultiSelect(msg tea.Msg) (Model, tea.Cmd) {
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

// toggleDirect is shift+d (was m): behind a confirmation only when turning direct mode
// ON — turning it back off is silent, mirroring deploy.Model's own toggleDirect and the
// approved keymap's own "confirm when turning on" wording (the retired m gesture had confirmed
// both directions, which the keymap never asked for). Never offered at all for a production
// target: hidden from the footer/help (hints' own registry row) and, pressed anyway, a silent
// no-op here too — politeness only (rule 5's note); the real, unbypassable enforcement is
// engine.DirectCommitGateStep, which refuses a production env regardless of what this screen
// believed.
func (m Model) toggleDirect() (Model, tea.Cmd) {
	if m.envs.IsProduction(m.target) {
		return m, nil
	}
	if m.mode == ModeDirect {
		m.mode = ModePR
		return m, nil
	}
	m.confirming = true
	m.buildConfirm()
	return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
}

// buildConfirm builds the direct-mode dialog, only ever reached (via toggleDirect above) while
// turning direct mode on, so the verb is fixed. WithKeyMap is not decoration: huh.NewConfirm
// leaves its keymap zero-valued, and a zero key.Binding matches nothing, so a standalone
// Confirm ignored y, n and the arrows — this screen's m gesture could not be completed by a
// real operator until M10, and its test had set the bound bool directly (#85's named trap;
// internal/app/tags and internal/app/deploy had already been fixed the same way).
func (m *Model) buildConfirm() {
	m.confirmValue = false
	m.confirmDirect = huh.NewConfirm().
		Title("Switch to direct mode (commit straight to the default branch, no PR)?").
		Value(&m.confirmValue)
	m.confirmDirect.WithKeyMap(keys.HuhKeyMap())
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
				m.mode = ModeDirect
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

// buildOverride builds the o dialog for repo: a huh.Input pre-filled with "<repo>=" so the
// operator types only the reference, in the exact form `--digest` takes. WithKeyMap for the
// same reason buildConfirm gives (§9 entry 6); the answer is read back with GetValue.
func (m *Model) buildOverride(repo string) {
	m.overrideRepo = repo
	m.overrideErr = ""
	m.overrideValue = repo + "="
	if ov, ok := m.overrides[repo]; ok {
		m.overrideValue = repo + "=" + ov.String()
	}
	m.overrideInput = huh.NewInput().
		Title("override the digest for " + repo).
		Description("repo=repo:tag@sha256:<digest> — wins over every digest source, as --digest does").
		Value(&m.overrideValue)
	m.overrideInput.WithKeyMap(keys.HuhKeyMap())
	m.overrideInput.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.overrideInput.WithWidth(m.dialogWidth())
}

// updateOverride drives the o dialog: esc closes it with nothing changed; enter validates
// the text through ValidateOverride and, when it holds, records the override and rebuilds
// the plan through resolveFn with it in place — the same load path the screen opened with —
// or, when it does not, shows the refusal in the dialog and keeps it open. Enter is handled
// here rather than left to huh's own Submit: a standalone Input's Submit only emits
// NextField, which nothing on this screen listens for.
func (m Model) updateOverride(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		m.overrideErr = ""
		switch kmsg.String() {
		case "esc":
			m.overriding = false
			return m, nil
		case "enter":
			text, _ := m.overrideInput.GetValue().(string)
			ref, err := ValidateOverride(m.overrideRepo, text)
			if err != nil {
				// Redacted here as well as at View's boundary: the operator may paste a
				// credential-bearing string by accident, and the refusal echoes what was typed.
				m.overrideErr = redact.Strings(err.Error())
				return m, nil
			}
			m.overriding = false
			m.overrides[ref.Repo] = ref
			m.keepTicked = true
			// Only the ID changes: a load/delta still outstanding for the pre-override plan
			// must not land on the new rows, but this is one screen instance reloading itself
			// in place, not a new one — its ctx (and so its Close-on-pop lifetime, owned by
			// the root, AGENTS.md §4.8) stays exactly what it was for this screen's whole life.
			m.scope.ID = scope.New()
			m.state = stateLoading
			m.status = fmt.Sprintf("re-resolving digests from %s pods with the override…", m.source)
			m.showYAML = false
			return m, tea.Batch(m.spinner.Tick, m.loadCmd())
		}
	}
	f, cmd := m.overrideInput.Update(msg)
	if in, ok := f.(*huh.Input); ok {
		m.overrideInput = in
	}
	return m, cmd
}

// overrideBody is the dialog's content: the input, and under it the last refusal.
func (m Model) overrideBody() string {
	body := m.overrideInput.View()
	if m.overrideErr != "" {
		body += "\n" + m.styles.Bad.Render(ansi.Wordwrap(m.overrideErr, m.dialogWidth(), ""))
	}
	return body
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
		// P3 (T3 review): the shift+d direct-mode confirm is a huh.Confirm, not a text field,
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
		m.confirmDirect.WithWidth(m.dialogWidth())
	}
	if m.overrideInput != nil {
		m.overrideInput.WithWidth(m.dialogWidth())
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
	parts := []string{m.styles.Title.Render(plural(repos, "repo") + " ticked")}
	if commits > 0 || (loading == 0 && missing == 0 && repos > 0) {
		parts = append(parts, m.styles.Title.Render(plural(commits, "commit")))
	}
	if migrations > 0 {
		word := plural(migrations, "migration")
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
		parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("%s, %s", plural(refs, "image reference"), plural(files, "file"))))
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
	lines = append(lines, m.styles.Dim.Render(ansi.Wrap(fmt.Sprintf("%s · %s · digest from %s", plural(r.Count, "image reference"), plural(r.Files, "file"), r.Source), width, "")))
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
			lines = append(lines, "", m.styles.Warn.Render(plural(len(st.Delta.Migrations), "migration")+qualifier+" run on this promotion:"))
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
