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
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/app/writepath"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
)

// Mode is the write path this plan would use.
const (
	ModePR     = writepath.ModePR
	ModeDirect = writepath.ModeDirect
)

// Func builds one gitops.Plan through internal/service's own Plan — the identical builder
// `hoist plan`/`hoist promote`/`hoist deploy` use, so a plan built for this screen and one built
// for the CLI's dry run can never silently diverge again. cmd/hoist supplies it as svc.Plan,
// wrapping whichever cluster and registry adaptors the
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
	// overrideValue is huh's write target only; the text is read back
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

// newViewport is the impact pane's own scrolling body, bound to keys.ViewportKeyMap
// rather than left on viewport.New()'s bubbles-library default — which binds
// space/f/b to page and bare "d"/"u" to half-page, none of it shown anywhere and "d" already
// meaning "toggle yaml diff" on this screen. SoftWrap is on: the yaml
// diff's whole point is the digest, and a hard-truncated line hides it past the pane's right
// edge — wrapping trades a taller line for a visible one.
func newViewport() viewport.Model {
	return keys.NewViewport(true, true)
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
// removing this one (m.start's own design), and cancelling here on Enter
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
// (updateOverride) — the operator's ticked set survives, narrowed to the fresh plan's selectable
// rows, with any row that was not selectable before ticked as a first load would tick it
// (withNewlySelectable, #161: a repo that appears after the refresh arrives ticked), and a load already outstanding for the stale repo is dropped by
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
// (service.Plan): planFn is svc.Plan, so this screen's plan and the CLI's dry run can
// never silently diverge.
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
			// see Close's own doc comment. While a huh field's own filter is open, esc
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
	before := Selectable(m.rows)
	m.plan = msg.plan
	m.outcome = msg.outcome
	m.view = msg.view
	m.rows = DeriveRows(m.plan, m.outcome.Res)
	m.prefix = CommonPrefix(m.rows)
	if keep {
		// An override rebuild keeps what the operator unticked: rebuilding from scratch
		// would tick every row again, and Enter would promote the repos they had excluded.
		// The set is intersected with the rebuilt rows (a repo may have stopped being
		// selectable), a row that was not selectable before and is now (an unresolved digest
		// that resolved this time) is ticked as a first load would tick it, so the promotion
		// does not silently omit it (#161), and rebuildMultiSelect puts the cursor back by
		// repo (#121).
		m.ticked = withNewlySelectable(m.ticked, before, Selectable(m.rows))
		m.rebuildMultiSelect()
	} else {
		m.buildMultiSelect()
	}
	m = m.recomputeDiff()
	m = m.layout()
	return m.refreshRight(), m.historyCmds()
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
	// While the multi-select's own filter is open, every key belongs to it — r, d, e,
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

func (m Model) recomputeDiff() Model {
	ticked := map[string]bool{}
	for _, r := range m.ticked {
		ticked[r] = true
	}
	diff, err := writepath.RenderDiff(m.repo.Root, m.plan.Edits, ticked)
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
