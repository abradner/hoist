package matrix

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
)

// maxCellWidth caps an env column so one long tag list cannot push the others off screen.
const maxCellWidth = 44

// DriftFunc reports what one env's cluster is running — the raw pod observations, keyed by
// canonical image repo, every distinct running reference kept (see Running) — asked for
// per env at boot so the matrix can say "drifted" where the manifest and the cluster
// disagree. It is not the planning resolver, which picks one digest per repo and would
// collapse a partial rollout (#122). nil means no cluster is configured and no cell is
// ever claimed drifted (nor not drifted). cmd/hoist builds it over pkg/k8s (AGENTS.md
// §4.8: this package takes a function value, never an adaptor).
type DriftFunc func(ctx context.Context, env string) (map[string][]image.Ref, error)

// chooserKind names what the dialog under m.chooser is choosing between.
type chooserKind int

const (
	chooserImage  chooserKind = iota // t on a cell with several first-party images
	chooserOpenPR                    // o with several in flight that have PRs
)

// DriftMsg is one env's answer. gen is the refresh generation it belongs to, so a slow
// answer from before a refresh (F5) or a previous instance of this screen is dropped rather
// than painting stale drift over a newer answer.
type DriftMsg struct {
	gen     uint64
	env     string
	running map[string][]image.Ref
	err     error
}

// RefreshRepoFunc re-reads the repo this matrix shows — cmd/hoist's own fetch-then-discover
// against a cached origin/<base> view (#PR7), never the operator's own working tree
// (AGENTS.md §4.6). Mirrors DriftFunc's own shape: a plain function type this screen holds
// and calls (§4.8 — a screen may call a function type it's handed, just never import the
// git/network machinery behind it itself), built once by cmd/hoist and installed with
// WithRefreshRepo. The returned rev is origin/<base>'s tip at the moment this view was built
// (svc.RefreshRepo's own RepoView.SHA, #PR8) — "" when the fetch fell back to the clone, since
// there is then nothing freshly read to name.
type RefreshRepoFunc func(ctx context.Context) (*gitops.Repo, string, error)

// RepoRefreshedMsg carries RefreshRepoFunc's answer back to Update. Gen ties it to the
// askRepoRefresh call that issued it, the same way DriftMsg's own gen does for the cluster
// fan-out.
type RepoRefreshedMsg struct {
	Gen  uint64
	Repo *gitops.Repo
	Rev  string
	Err  error
}

// nextGen numbers refresh generations across every Model this process builds, so two
// matrices (an earlier one popped away) can never confuse each other's answers.
var nextGen atomic.Uint64

// nextRepoGen numbers repo-refresh generations the same way nextGen numbers drift ones.
var nextRepoGen atomic.Uint64

// Model is the matrix screen. It is a value: Update, SetSize and SetStyles return the
// updated model.
type Model struct {
	repo              *gitops.Repo
	promotable        []string
	envs              config.EnvsConfig
	drift             DriftFunc
	refreshRepo       RefreshRepoFunc
	base, kubeContext string
	matrix            Table
	styles            ui.Styles
	width, height     int
	notice            string
	hint              string

	// row/col is the cell cursor (T3-04, UX-M9): row indexes matrix.Rows, col indexes
	// matrix.Envs. Left/Right move col; Up/Down move row. offset is the grid's own vertical
	// scroll position (the first visible row), kept so a row far down the table is scrolled
	// into view rather than clipped.
	row, col, offset int
	// focus says whether the cursor keys act on the grid or the in-flight pane (tab, T3-04).
	focus Focus
	// paneCursor is which in-flight row is selected while focus is on the pane.
	paneCursor int

	// menu holds the action menu's own state (enter on the grid, T3-04): open, its items and
	// its own cursor, and which cell it was opened for.
	menuOpen            bool
	menuItems           []MenuItem
	menuCursor          int
	menuFamily, menuEnv string

	// confirmAbandon is the huh.Confirm behind shift+x on the pane; confirmAbandonID the
	// promotion it would abandon.
	confirmAbandon      *huh.Confirm
	confirmAbandonID    string
	confirmAbandonBuild flight.Summary

	running  Running
	pending  map[string]bool
	driftErr map[string]string
	gen      uint64

	driftTimeout time.Duration

	refreshingRepo bool
	refreshAgain   bool
	repoGen        uint64
	refreshErr     string

	chooser       *huh.Select[string]
	chooserTarget string
	chooserKind   chooserKind

	inflight    []flight.Summary
	inflightErr string
	now         func() time.Time
}

// KeyScreen implements the root's keyed interface (T3-04): the matrix now has a stated row in
// internal/ui/keys' registry, so the root's own "?" help overlay and "l" activity-log handling
// apply to it exactly as they already do to watch/restart/config/activity (T3-03).
func (m Model) KeyScreen() keys.Screen { return keys.ScrMatrix }

// OpenPlanMsg is emitted when the operator asks to plan a promotion into Target (p, or the
// action menu's own "promote into" item). Source is the one reverse pair (envs.pairs) found
// for Target when exactly one exists; empty when there is none or several, in which case the
// plan screen itself asks "promote into Target from…" (T3-04: promote now always goes by the
// TARGET the cursor is on, never a forced prompt for an arbitrary target — the retired P key's
// own job).
type OpenPlanMsg struct {
	Source, Target string
}

// OpenTagsMsg is emitted when the operator asks to pick a new tag for the current cell (t,
// "deploy a tag" — the tag picker, internal/app/tags). ImageRepo is the one first-party image
// repo the family runs in CurrentEnv, or the one the operator chose when there were several.
type OpenTagsMsg struct {
	ImageRepo, Target string
}

// OpenConfigMsg is emitted when the operator asks to read the effective config (c, #104):
// the TUI's `hoist config show`. It carries nothing — the root holds the text and the path.
type OpenConfigMsg struct{}

// OpenRestartMsg is emitted when the operator asks to restart the family under the cursor in
// CurrentEnv (shift+r).
type OpenRestartMsg struct {
	Family, Target string
}

// OpenWatchMsg is emitted when the operator asks to watch the family under the cursor
// converge in CurrentEnv (w).
type OpenWatchMsg struct {
	Family, Target string
}

// New builds the screen for a discovered repo. promotable lists the first-party image repo
// prefixes (see Compute). envs is the repo's policy. drift is how the cluster is asked what
// each env runs; nil never asks. Columns come in pipeline order (envs.pairs — T3-04) rather
// than alphabetically, and the cell cursor starts on the first non-production column so a
// fresh session never opens with the cursor already pointed at a write that asks for
// approval. The model has no size until SetSize is called.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, drift DriftFunc) Model {
	m := Model{
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		drift:      drift,
		matrix:     Order(Compute(repo, promotable, nil), envs),
		running:    Running{},
		pending:    map[string]bool{},
		driftErr:   map[string]string{},
		now:        time.Now,
		gen:        nextGen.Add(1),
	}
	m.col = firstNonProductionColumn(m.matrix, envs)
	m = m.WithDrift(drift)
	return m.SetStyles(ui.NewStyles(true))
}

// firstNonProductionColumn is the column New starts the cursor on: the first env (in the
// already-pipeline-ordered list) that envs does not list as production, or 0 when every
// column is production (or there are none) — never leaving the cursor un-clamped.
func firstNonProductionColumn(t Table, envs config.EnvsConfig) int {
	for i, e := range t.Envs {
		if !envs.IsProduction(e) {
			return i
		}
	}
	return 0
}

// Init asks the cluster what every env is running, one command per env, when a DriftFunc
// was supplied.
func (m Model) Init() tea.Cmd { return m.askCluster() }

// refresh starts a new drift generation (F5): every env pending again, every earlier answer
// kept on screen until its replacement lands, so a refresh never blanks the table.
func (m Model) refresh() (Model, tea.Cmd) {
	if m.drift == nil || len(m.matrix.Envs) == 0 {
		return m, nil
	}
	m.gen = nextGen.Add(1)
	m.pending = map[string]bool{}
	for _, env := range m.matrix.Envs {
		m.pending[env] = true
	}
	return m.layout(), m.askCluster()
}

// askCluster issues one command per env for the current generation.
func (m Model) askCluster() tea.Cmd {
	if m.drift == nil || len(m.matrix.Envs) == 0 {
		return nil
	}
	gen, drift := m.gen, m.drift
	timeout := m.driftTimeout
	if timeout <= 0 {
		timeout = scope.Drift
	}
	cmds := make([]tea.Cmd, 0, len(m.matrix.Envs))
	for _, env := range m.matrix.Envs {
		env := env
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := scope.Timeout(timeout)
			defer cancel()
			running, err := drift(ctx, env)
			if err != nil && errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("did not answer in %s", timeout)
			}
			return DriftMsg{gen: gen, env: env, running: running, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// askRepoRefresh re-reads the repo (#PR7's F5, alongside askCluster's own cluster fan-out).
func (m Model) askRepoRefresh() (Model, tea.Cmd) {
	if m.refreshRepo == nil {
		return m, nil
	}
	if m.refreshingRepo {
		m.refreshAgain = true
		return m, nil
	}
	m.refreshingRepo = true
	m.repoGen = nextRepoGen.Add(1)
	gen, refresh := m.repoGen, m.refreshRepo
	return m, func() tea.Msg {
		ctx, cancel := scope.Timeout(scope.RefreshRepo)
		defer cancel()
		repo, rev, err := refresh(ctx)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("did not answer in %s", scope.RefreshRepo)
		}
		return RepoRefreshedMsg{Gen: gen, Repo: repo, Rev: rev, Err: err}
	}
}

// RequestRefresh is Train 2 design PR 4's completion-triggered refresh: exactly what F5 already
// does, exported so the root can call it when a drive lands or finishes.
func (m Model) RequestRefresh() (Model, tea.Cmd) {
	nm, driftCmd := m.refresh()
	nm, repoCmd := nm.askRepoRefresh()
	return nm, tea.Batch(driftCmd, repoCmd)
}

// Update handles the screen's keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case DriftMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		delete(m.pending, msg.env)
		if msg.err != nil {
			m.driftErr[msg.env] = redact.Strings(msg.err.Error())
			return m.layout(), nil
		}
		delete(m.driftErr, msg.env)
		m.running[msg.env] = msg.running
		m.matrix = Order(Compute(m.repo, m.promotable, m.running), m.envs)
		return m.layout(), nil
	case RepoRefreshedMsg:
		if msg.Gen != m.repoGen {
			return m, nil
		}
		m.refreshingRepo = false
		again := m.refreshAgain
		m.refreshAgain = false
		if msg.Err != nil {
			m.refreshErr = redact.Strings(msg.Err.Error())
			nm := m.layout()
			if again {
				return nm.askRepoRefresh()
			}
			return nm, nil
		}
		nm := m.WithRepo(msg.Repo)
		if msg.Rev != "" {
			nm.notice = fmt.Sprintf("origin/%s re-read · %s", nm.baseName(), shortSHA(msg.Rev))
		}
		if again {
			return nm.askRepoRefresh()
		}
		return nm, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.confirmAbandon != nil {
		return m.updateConfirmAbandon(msg)
	}
	if m.chooser != nil {
		return m.updateChooser(msg)
	}
	if m.menuOpen {
		return m.updateMenu(msg)
	}
	m.notice = ""
	switch {
	case keys.Refresh.Matches(msg):
		return m.RequestRefresh()
	case keys.Left.Matches(msg):
		if m.focus == FocusGrid && m.col > 0 {
			m.col--
		}
		return m.layout(), nil
	case keys.Right.Matches(msg):
		if m.focus == FocusGrid && m.col < len(m.matrix.Envs)-1 {
			m.col++
		}
		return m.layout(), nil
	case keys.Up.Matches(msg):
		return m.moveCursor(-1), nil
	case keys.Down.Matches(msg):
		return m.moveCursor(1), nil
	case keys.PgUp.Matches(msg):
		return m.moveCursor(-max(m.gridHeight(), 1)), nil
	case keys.PgDn.Matches(msg):
		return m.moveCursor(max(m.gridHeight(), 1)), nil
	case keys.Home.Matches(msg):
		// P2-8 (T3 review): the design's own home/end row was missing here. moveCursor's own
		// clamp does the actual jump-to-first/last work, the same trick fitWidths' clamp and
		// the plan screen's own Home/End case rely on: a delta at least as large in magnitude
		// as the list always lands at the end it points to, on either cursor (row or pane).
		return m.moveCursor(-max(len(m.matrix.Rows), len(m.inflight))), nil
	case keys.End.Matches(msg):
		return m.moveCursor(max(len(m.matrix.Rows), len(m.inflight))), nil
	case keys.Tab.Matches(msg):
		return m.toggleFocus(), nil
	case keys.Config.Matches(msg):
		return m, func() tea.Msg { return OpenConfigMsg{} }
	case keys.Promote.Matches(msg):
		return m.doPromote()
	case keys.Tag.Matches(msg):
		return m.doTag()
	case keys.Watch.Matches(msg):
		return m.doWatch()
	case keys.Restart.Matches(msg):
		return m.doRestart()
	case keys.Abandon.Matches(msg):
		return m.doAbandonFromPane()
	case keys.Open.Matches(msg):
		return m.doOpenPR()
	case keys.Enter.Matches(msg):
		if m.focus == FocusPane {
			return m.doResumeFromPane()
		}
		return m.openMenu()
	}
	return m, nil
}

// moveCursor moves the row cursor (grid focus) or the pane cursor (pane focus) by delta,
// clamped to the list it belongs to.
func (m Model) moveCursor(delta int) Model {
	if m.focus == FocusPane {
		n := len(m.inflight)
		if n == 0 {
			return m
		}
		m.paneCursor = clamp(m.paneCursor+delta, n-1)
		return m
	}
	n := len(m.matrix.Rows)
	if n == 0 {
		return m
	}
	m.row = clamp(m.row+delta, n-1)
	return m.layout()
}

// clamp bounds v to [0, hi] — every caller in this file clamps a cursor or offset against a
// count-derived upper bound, never an arbitrary lower one.
func clamp(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

// toggleFocus moves the cursor keys between the grid and the in-flight pane (tab, T3-04). A
// tab with nothing in flight does nothing — there is no pane to focus.
func (m Model) toggleFocus() Model {
	if len(m.inflight) == 0 {
		return m
	}
	if m.focus == FocusGrid {
		m.focus = FocusPane
		m.paneCursor = clamp(m.paneCursor, len(m.inflight)-1)
	} else {
		m.focus = FocusGrid
	}
	return m
}

// doPromote is p: promote into the cursor column. Exactly one reverse pair uses it directly;
// otherwise the plan screen itself asks which source (Source left empty).
func (m Model) doPromote() (Model, tea.Cmd) {
	target := m.CurrentEnv()
	if target == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	srcs := m.envs.SourcesOf(target)
	source := ""
	if len(srcs) == 1 {
		source = srcs[0]
	}
	return m, func() tea.Msg { return OpenPlanMsg{Source: source, Target: target} }
}

func (m Model) doRestart() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	family := m.CurrentFamily()
	if family == "" {
		m.notice = "no family under the cursor"
		return m, nil
	}
	return m, func() tea.Msg { return OpenRestartMsg{Family: family, Target: env} }
}

func (m Model) doWatch() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	family := m.CurrentFamily()
	if family == "" {
		m.notice = "no family under the cursor"
		return m, nil
	}
	return m, func() tea.Msg { return OpenWatchMsg{Family: family, Target: env} }
}

func (m Model) doOpenPR() (Model, tea.Cmd) {
	withPR := m.withPR()
	switch len(withPR) {
	case 0:
		m.notice = "nothing in flight has a PR to open"
		return m, nil
	case 1:
		url := withPR[0].PR.URL
		return m, func() tea.Msg { return flight.OpenPRMsg{URL: url} }
	default:
		return m.openInFlightChooser(chooserOpenPR, withPR)
	}
}

// doResumeFromPane is enter while focus is on the in-flight pane: resume/re-attach to the
// promotion under the pane's own cursor.
func (m Model) doResumeFromPane() (Model, tea.Cmd) {
	if m.paneCursor < 0 || m.paneCursor >= len(m.inflight) {
		return m, nil
	}
	s := m.inflight[m.paneCursor]
	return m, func() tea.Msg { return resumeMsgFor(s) }
}

// doAbandonFromPane is shift+x on the pane: abandon the promotion under the pane's cursor,
// behind a huh.Confirm — an abandon is destructive (it closes the PR and deletes the branch),
// so it never fires on the bare keypress.
func (m Model) doAbandonFromPane() (Model, tea.Cmd) {
	if m.focus != FocusPane || m.paneCursor < 0 || m.paneCursor >= len(m.inflight) {
		return m, nil
	}
	s := m.inflight[m.paneCursor]
	confirm := huh.NewConfirm().
		Title(fmt.Sprintf("abandon %s (%s → %s)?", paneID(s), s.Source, s.Target)).
		Affirmative("abandon").Negative("cancel")
	// A standalone huh field ships a zero keymap and ignores every key (AGENTS.md §9 entry 6).
	confirm.WithKeyMap(keys.HuhKeyMap())
	confirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmAbandon = confirm
	m.confirmAbandonID = s.ID
	m.confirmAbandonBuild = s
	return m, tea.Batch(confirm.Init(), confirm.Focus())
}

func (m Model) updateConfirmAbandon(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	_, cmd := m.confirmAbandon.Update(msg)
	if msg.String() == "esc" {
		m.confirmAbandon = nil
		return m, nil
	}
	if msg.String() != "enter" {
		return m, cmd
	}
	yes, _ := m.confirmAbandon.GetValue().(bool) // GetValue, never a captured field (§9 entry 6)
	s := m.confirmAbandonBuild
	m.confirmAbandon = nil
	if !yes {
		return m, nil
	}
	return m, func() tea.Msg { return flight.AbandonMsg{ID: s.ID} }
}

// doTag is t: deploy a new tag into the cursor cell. Mirrors the retired d gesture exactly.
func (m Model) doTag() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	repos := m.currentImageRepos(env)
	switch len(repos) {
	case 0:
		m.notice = "no first-party image in this cell"
		return m, nil
	case 1:
		repo := repos[0]
		return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: repo, Target: env} }
	default:
		return m.openChooser(env, repos)
	}
}

// openMenu opens the action menu for the cell under the cursor (enter, T3-04).
func (m Model) openMenu() (Model, tea.Cmd) {
	fam, env := m.CurrentFamily(), m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	m.menuItems = MenuFor(m.matrix, fam, env, m.envs, m.inflight)
	m.menuCursor = 0
	m.menuFamily, m.menuEnv = fam, env
	m.menuOpen = true
	return m, nil
}

func (m Model) updateMenu(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case keys.Esc.Matches(msg):
		m.menuOpen = false
		return m, nil
	case keys.Up.Matches(msg):
		m.menuCursor = clamp(m.menuCursor-1, max(len(m.menuItems)-1, 0))
		return m, nil
	case keys.Down.Matches(msg):
		m.menuCursor = clamp(m.menuCursor+1, max(len(m.menuItems)-1, 0))
		return m, nil
	case keys.Enter.Matches(msg):
		if m.menuCursor < 0 || m.menuCursor >= len(m.menuItems) {
			m.menuOpen = false
			return m, nil
		}
		return m.runMenuItem(m.menuItems[m.menuCursor])
	}
	// A letter that matches one of the listed items runs it directly, without moving the
	// cursor there first (v2·02: "the letter runs it directly").
	for _, item := range m.menuItems {
		if item.B.Name != "" && item.B.Matches(msg) {
			return m.runMenuItem(item)
		}
	}
	return m, nil
}

func (m Model) runMenuItem(item MenuItem) (Model, tea.Cmd) {
	m.menuOpen = false
	if !item.Enabled {
		return m, nil
	}
	if openMsg, ok := item.Msg.(openTagsMenuMsg); ok {
		repos := m.currentImageReposFor(openMsg.Family, openMsg.Env)
		switch len(repos) {
		case 0:
			m.notice = "no first-party image in this cell"
			return m, nil
		case 1:
			repo := repos[0]
			return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: repo, Target: openMsg.Env} }
		default:
			return m.openChooser(openMsg.Env, repos)
		}
	}
	msg := item.Msg
	return m, func() tea.Msg { return msg }
}

// openChooser asks which of several first-party images t meant, as a dialog over the matrix.
func (m Model) openChooser(env string, repos []string) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(repos))
	for _, r := range repos {
		opts = append(opts, huh.NewOption(r, r))
	}
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("deploy which image in %s?", env)).Options(opts...)
	sel.WithKeyMap(keys.HuhKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = env
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func chooserKey(s flight.Summary) string {
	if s.ID != "" {
		return s.ID
	}
	return fmt.Sprintf("build:%d", s.Build)
}

func (m Model) openInFlightChooser(kind chooserKind, from []flight.Summary) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(from))
	for _, s := range from {
		opts = append(opts, huh.NewOption(paneID(s)+"  "+pair(s)+"  "+s.Verdict(), chooserKey(s)))
	}
	title := "resume which promotion?"
	if kind == chooserOpenPR {
		title = "open which promotion's PR?"
	}
	sel := huh.NewSelect[string]().Title(title).Options(opts...)
	sel.WithKeyMap(keys.HuhKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = ""
	m.chooserKind = kind
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func (m Model) withPR() []flight.Summary {
	var out []flight.Summary
	for _, s := range m.inflight {
		if s.PR != nil && s.PR.URL != "" {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) updateChooser(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.chooser, m.chooserKind = nil, chooserImage
		return m, nil
	case "enter":
		if m.chooser.GetFiltering() {
			break
		}
		choice, _ := m.chooser.GetValue().(string)
		target, kind := m.chooserTarget, m.chooserKind
		m.chooser, m.chooserKind = nil, chooserImage
		if choice == "" {
			return m, nil
		}
		if kind == chooserOpenPR {
			for _, s := range m.withPR() {
				if s.ID == choice {
					url := s.PR.URL
					return m, func() tea.Msg { return flight.OpenPRMsg{URL: url} }
				}
			}
			m.notice = "that promotion no longer has a PR to open"
			return m, nil
		}
		return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: choice, Target: target} }
	}
	_, cmd := m.chooser.Update(msg)
	return m, cmd
}

// CapturesText reports whether some dialog is open that takes letters as text — the image
// chooser's "/" filter, or its own confirm dialogs — and the root's q-to-quit must not fire
// over it.
func (m Model) CapturesText() bool {
	return m.chooser != nil || m.confirmAbandon != nil
}

func (m Model) currentImageRepos(env string) []string {
	return m.currentImageReposFor(m.CurrentFamily(), env)
}

func (m Model) currentImageReposFor(fam, env string) []string {
	e, ok := m.repo.Envs[env]
	if !ok {
		return nil
	}
	return FirstPartyRepos(e.Families[fam], m.promotable)
}

// CurrentEnv is the env the column cursor is on, "" when the repo has none.
func (m Model) CurrentEnv() string {
	if len(m.matrix.Envs) == 0 {
		return ""
	}
	col := clamp(m.col, len(m.matrix.Envs)-1)
	return m.matrix.Envs[col]
}

// CurrentFamily is the family the row cursor is on, "" when the matrix has no rows.
func (m Model) CurrentFamily() string {
	if len(m.matrix.Rows) == 0 {
		return ""
	}
	row := clamp(m.row, len(m.matrix.Rows)-1)
	return m.matrix.Rows[row].Family
}

// IsProduction reports whether env is listed in envs.production.
func (m Model) IsProduction(env string) bool { return m.envs.IsProduction(env) }

const (
	minWidth  = 40
	minHeight = 8
)

// View renders the frame: the subheader, the grid, the notes, the in-flight pane (inside the
// frame, T3-05) and the footer, with the menu or a chooser dialog over it when one is open.
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
		// T3-05: the in-flight pane is a Section of this SAME frame now, not a separate
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
// (T3-05, 120-column layout) the env/family counts.
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
// P1-4 (T3 review): total must also count the "│" grid.go's own dataRow/ruleRow join every
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

// hasPane reports whether the in-flight pane occupies a Section at all (T3-05: it is one of
// Frame's own Sections now, not a separate Pane appended below the box — see View), so its own
// inter-section rule has to be counted alongside the subheader's and the notes' whenever one is
// present. It says nothing about whether the pane ultimately finds room to render non-empty at
// the current height — paneBudget/inflightPane decide that — so a height too tight for even the
// compact form is, at worst, one row off here; the same approximation the pre-T3-05 code lived
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
	rows -= lipgloss.Height(notes) * boolInt(notes != "")
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// notes is the section under the grid: the transient notice or hint first, else what the
// cluster said about the cursor's column, then the in-flight fold when the pane itself has no
// room (paneBudget's own decision). The bubbles help.Model line this used to carry is retired
// in T3-04 — the root's own overlay (internal/ui/keys.HelpView) replaces it entirely.
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

// SetSize fits the grid to a width × height terminal.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m.layout()
}

// SetStyles applies a palette.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	return m
}

// Cursor is the index of the selected family row.
func (m Model) Cursor() int { return m.row }

// Matrix is the computed matrix the screen shows.
func (m Model) Matrix() Table { return m.matrix }

// TakeRefreshError returns and clears a failed F5's own reason, so the root can log it to the
// activity log instead of leaving it as the matrix's own clear-on-next-key notice — an async
// refresh's failure can easily land after the operator has already pressed another key.
func (m Model) TakeRefreshError() (string, Model) {
	err := m.refreshErr
	m.refreshErr = ""
	return err, m
}

// WithNotice sets the notice shown under the table.
func (m Model) WithNotice(notice string) Model {
	m.notice = notice
	return m
}

// WithDrift replaces the cluster-asking function.
func (m Model) WithDrift(drift DriftFunc) Model {
	m.drift = drift
	m.pending = map[string]bool{}
	if drift == nil {
		return m
	}
	for _, env := range m.matrix.Envs {
		if _, answered := m.running[env]; answered {
			continue
		}
		if _, failed := m.driftErr[env]; failed {
			continue
		}
		m.pending[env] = true
	}
	return m
}

// WithRepo replaces the *gitops.Repo the table is computed from.
func (m Model) WithRepo(repo *gitops.Repo) Model {
	if repo == nil {
		return m
	}
	m.repo = repo
	m.matrix = Order(Compute(m.repo, m.promotable, m.running), m.envs)
	m.row = clamp(m.row, max(len(m.matrix.Rows)-1, 0))
	m.col = clamp(m.col, max(len(m.matrix.Envs)-1, 0))
	return m.layout()
}

// Repo returns the *gitops.Repo the table is currently computed from.
func (m Model) Repo() *gitops.Repo { return m.repo }

// WithRefreshRepo installs the function F5 calls to re-read the repo.
func (m Model) WithRefreshRepo(refresh RefreshRepoFunc) Model {
	m.refreshRepo = refresh
	return m
}

// WithRun names the base branch and kube context this session runs against (#105).
func (m Model) WithRun(base, kubeContext string) Model {
	m.base, m.kubeContext = base, kubeContext
	return m
}

func (m Model) baseName() string {
	if m.base == "" {
		return "main"
	}
	return m.base
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func displayRoot(root string) string {
	if root == "" {
		return "."
	}
	if filepath.IsAbs(root) || root == ".." || strings.HasPrefix(root, ".."+string(filepath.Separator)) {
		return filepath.Base(root)
	}
	return filepath.ToSlash(root)
}

const selectedMarker = "▸ "
const productionMarker = " ⚠"

// statusBar is the matrix's own footer, through keys.Footer (T3-03/04): the env under the
// cursor on the left, the writes and verbs an operator is actually looking for the key of on
// the right, in priority order so a narrow terminal drops the least useful first. The cursor's
// own production marker (T3 followup, group 3) is productionMarker — the same "⚠" the header
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
	// no-op otherwise, T3-04) — advertising it with nothing in flight is a hint for a key that
	// does nothing (P2-13, T3 review).
	if len(m.inflight) > 0 {
		hints = append(hints, keys.Hint{B: keys.Tab, Long: "tab in flight", Short: "tab in flight", Pri: 7})
	}
	hints = append(hints, keys.Hint{B: keys.Quit, Long: "q quit", Short: "q quit", Pri: 0})
	// help true: the root's own overlay (T3-03/04) — Footer appends "? help"/"? more" itself.
	return keys.Footer(m.styles, m.width, status, hints, true)
}

func orNoEnv(env string) string {
	if env == "" {
		return "(none)"
	}
	return env
}
