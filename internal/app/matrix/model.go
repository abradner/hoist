package matrix

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// maxCellWidth caps an env column so one long tag list cannot push the others off screen.
const maxCellWidth = 44

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

	// row/col is the cell cursor (UX-M9): row indexes matrix.Rows, col indexes
	// matrix.Envs. Left/Right move col; Up/Down move row. offset is the grid's own vertical
	// scroll position (the first visible row), kept so a row far down the table is scrolled
	// into view rather than clipped.
	row, col, offset int
	// focus says whether the cursor keys act on the grid or the in-flight pane (tab).
	focus Focus
	// paneCursor is which in-flight row is selected while focus is on the pane.
	paneCursor int

	// menu holds the action menu's own state (enter on the grid): open, its items and
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
	driftID  scope.ID

	driftTimeout time.Duration

	refreshingRepo bool
	refreshAgain   bool
	repoID         scope.ID
	refreshErr     string

	chooser       *huh.Select[string]
	chooserTarget string
	chooserKind   chooserKind

	inflight    []flight.Summary
	inflightErr string
	now         func() time.Time
}

// KeyScreen implements the root's keyed interface: the matrix now has a stated row in
// internal/ui/keys' registry, so the root's own "?" help overlay and "l" activity-log handling
// apply to it exactly as they already do to watch/restart/config/activity.
func (m Model) KeyScreen() keys.Screen { return keys.ScrMatrix }

// OpenPlanMsg is emitted when the operator asks to plan a promotion into Target (p, or the
// action menu's own "promote into" item). Source is the one reverse pair (envs.pairs) found
// for Target when exactly one exists; empty when there is none or several, in which case the
// plan screen itself asks "promote into Target from…" (promote now always goes by the
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
// each env runs; nil never asks. Columns come in pipeline order (envs.pairs) rather
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
		driftID:    scope.New(),
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

// Update handles the screen's keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case DriftMsg:
		if scope.Foreign(m.driftID, msg) {
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
		if scope.Foreign(m.repoID, msg) {
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
			nm.notice = fmt.Sprintf("origin/%s re-read · %s", nm.baseName(), ui.ShortSHA(msg.Rev))
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
		// the design's own home/end row was missing here. moveCursor's own
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

// toggleFocus moves the cursor keys between the grid and the in-flight pane (tab). A
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

// CapturesText reports whether some dialog is open that takes letters as text — the image
// chooser's "/" filter, or its own confirm dialogs — and the root's q-to-quit must not fire
// over it. The action menu counts: it runs an item on a letter, so the root's q, l and ? must
// reach the menu (which ignores them) rather than quit, push the log or open help over it
// (UX-H13).
func (m Model) CapturesText() bool {
	return m.chooser != nil || m.confirmAbandon != nil || m.menuOpen
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

// Repo returns the *gitops.Repo the table is currently computed from.
func (m Model) Repo() *gitops.Repo { return m.repo }

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

func displayRoot(root string) string {
	if root == "" {
		return "."
	}
	if filepath.IsAbs(root) || root == ".." || strings.HasPrefix(root, ".."+string(filepath.Separator)) {
		return filepath.Base(root)
	}
	return filepath.ToSlash(root)
}
