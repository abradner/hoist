package matrix

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
)

// maxCellWidth caps an env column so one long tag list cannot push the others off screen.
const maxCellWidth = 44

// DriftFunc reports what one env's cluster is running, keyed by image repo — the pod
// digests hoist already resolves for a plan, asked for per env at boot so the matrix can say
// "drifted" where the manifest and the cluster disagree. nil means no cluster is configured
// and no cell is ever claimed drifted (nor not drifted). The root builds it from the same
// resolve function the plan screen uses (AGENTS.md §4.8: this package takes a function
// value, never an adaptor).
type DriftFunc func(ctx context.Context, env string) (map[string]image.Ref, error)

// driftMsg is one env's answer. gen is the refresh generation it belongs to, so a slow
// answer from before a refresh (F5) or a previous instance of this screen is dropped rather
// than painting stale drift over a newer answer.
type driftMsg struct {
	gen     uint64
	env     string
	running map[string]image.Ref
	err     error
}

// nextGen numbers refresh generations across every Model this process builds, so two
// matrices (an earlier one popped away) can never confuse each other's answers.
var nextGen atomic.Uint64

// Model is the matrix screen. It is a value: Update, SetSize and SetStyles return the
// updated model.
type Model struct {
	repo          *gitops.Repo
	promotable    []string
	envs          config.EnvsConfig
	drift         DriftFunc
	matrix        Table
	tbl           table.Model
	styles        ui.Styles
	keys          keyMap
	help          help.Model
	width, height int
	showHelp      bool
	notice        string
	// col is the focused env column: CurrentEnv's index into matrix.Envs. Left/Right move
	// it; it is what p, P, d and R all act on, so the header marks it and the footer names it.
	col int

	// running is what each env's cluster answered; pending the envs still being asked;
	// driftErr the envs whose cluster could not be asked, with the reason. gen is the
	// refresh generation the outstanding requests belong to.
	running  Running
	pending  map[string]bool
	driftErr map[string]string
	gen      uint64

	// chooser is open when d found several first-party images in the cell and the operator
	// has to say which one to deploy (#85: "d picks the first sorted image, silently").
	chooser       *huh.Select[string]
	chooserTarget string
	chooserResume bool // the chooser picks a promotion to resume, not an image

	// inflight is what the root listed as promoting right now (SetInFlight), drawn as the
	// pane under the table (inflight.go); inflightErr when the listing itself failed. now
	// ages them; a test pins it.
	inflight    []flight.Summary
	inflightErr string
	now         func() time.Time
}

type keyMap struct {
	Up, Down, Left, Right, Promote, PromoteAs, DeployNew, Restart, Resume, OpenPR, Refresh, Help, Quit key.Binding
}

// ShortHelp is the hint set shown in the footer: the writes first, since they are what an
// operator is looking for the key of.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Promote, k.DeployNew, k.Resume, k.Help, k.Quit}
}

// FullHelp is what ? expands to; one group, rendered on a single line.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.Left, k.Right, k.Promote, k.PromoteAs, k.DeployNew, k.Restart, k.Resume, k.OpenPR, k.Refresh, k.Help, k.Quit}}
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Left:      key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "env")),
		Right:     key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "env")),
		Promote:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "promote")),
		PromoteAs: key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "promote to…")),
		DeployNew: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "deploy")),
		// Capital R, deliberately. A restart rolls every pod of a family, and the lower-case
		// keys on this screen all mean "open a screen to look at something". The screen this
		// opens is still a confirmation, so R asks rather than does — but it asks for a write,
		// and the shift key is a cheap way to keep it out of reach of a mistyped r.
		Restart: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart")),
		// r and enter both open the in-flight promotion on the flight screen: r is the verb
		// (`hoist resume`), enter is "details" for an operator reading the pane.
		Resume:  key.NewBinding(key.WithKeys("r", "enter"), key.WithHelp("r", "resume")),
		OpenPR:  key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		Refresh: key.NewBinding(key.WithKeys("f5", "ctrl+r"), key.WithHelp("F5", "re-read the cluster")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// OpenPlanMsg is emitted when the operator asks to plan a promotion from CurrentEnv: p asks
// for the configured pair (envs.pairs[Source] — the root looks it up), P (Force) always
// prompts for the target instead. The root recognizes this by concrete type in its own
// Update switch (see internal/app/screen.go).
type OpenPlanMsg struct {
	Source string
	Force  bool
}

// OpenTagsMsg is emitted when the operator asks to pick a new tag for the current cell (d,
// "deploy image" — the tag picker, internal/app/tags). ImageRepo is the one first-party image
// repo the family runs in CurrentEnv, or the one the operator chose when there were several.
type OpenTagsMsg struct {
	ImageRepo, Target string
}

// OpenRestartMsg is emitted when the operator asks to restart the family under the cursor in
// CurrentEnv (R). It names a family rather than an image because a restart changes no image:
// what it rolls is every Deployment that family declares, which is the unit an Argo Application
// already covers and therefore the unit the rollout is watched at.
type OpenRestartMsg struct {
	Family, Target string
}

// New builds the screen for a discovered repo. promotable lists the first-party image repo
// prefixes (see Compute). envs is the repo's policy (which envs are production — marked in
// the header and named in the footer, since this is the one screen where an operator
// chooses an env and until M10 the one place that fact was absent, #86). drift is how the
// cluster is asked what each env runs; nil never asks. The model has no size until SetSize
// is called.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, drift DriftFunc) Model {
	m := Model{
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		drift:      drift,
		matrix:     Compute(repo, promotable, nil),
		keys:       defaultKeyMap(),
		help:       help.New(),
		running:    Running{},
		pending:    map[string]bool{},
		driftErr:   map[string]string{},
		now:        time.Now,
		// The first generation is minted here, not in Init: Init has a value receiver and
		// returns only a command, so a generation minted there would never reach the model
		// the root keeps, and every answer would be dropped as stale.
		gen: nextGen.Add(1),
	}
	if drift != nil {
		for _, env := range m.matrix.Envs {
			m.pending[env] = true
		}
	}
	// The table's own up/down bindings are replaced so the screen owns the key vocabulary.
	km := table.DefaultKeyMap()
	km.LineUp, km.LineDown = m.keys.Up, m.keys.Down
	cols := m.columns()
	m.tbl = table.New(
		table.WithColumns(cols),
		table.WithRows(m.rows(cols)),
		table.WithKeyMap(km),
		table.WithFocused(true),
	)
	return m.SetStyles(ui.NewStyles(true))
}

// Init asks the cluster what every env is running, one command per env, when a DriftFunc
// was supplied. The table is already drawn from the manifests; each answer refines its
// column when it lands.
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
	cmds := make([]tea.Cmd, 0, len(m.matrix.Envs))
	for _, env := range m.matrix.Envs {
		env := env
		cmds = append(cmds, func() tea.Msg {
			running, err := drift(context.Background(), env)
			return driftMsg{gen: gen, env: env, running: running, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// Update handles the screen's keys and forwards the rest to the table. Quit is the root's.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case driftMsg:
		if msg.gen != m.gen {
			return m, nil // an earlier generation's answer; see driftMsg
		}
		delete(m.pending, msg.env)
		if msg.err != nil {
			// Adaptor errors are scrubbed at the adaptor, but everything printed passes
			// through redact before the terminal regardless — the plan screen's rule, applied
			// to the one other place a cluster error is rendered.
			m.driftErr[msg.env] = redact.Strings(msg.err.Error())
			return m.layout(), nil
		}
		delete(m.driftErr, msg.env)
		m.running[msg.env] = msg.running
		m.matrix = Compute(m.repo, m.promotable, m.running)
		return m.layout(), nil
	case tea.KeyPressMsg:
		if m.chooser != nil {
			return m.updateChooser(msg)
		}
		m.notice = ""
		switch {
		case key.Matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp
			return m.layout(), nil
		case key.Matches(msg, m.keys.Refresh):
			return m.refresh()
		case key.Matches(msg, m.keys.Left):
			if m.col > 0 {
				m.col--
			}
			// Through layout, not a bare return: the marker lives in the column titles, so the
			// header has to be rebuilt for it to move with the cursor.
			return m.layout(), nil
		case key.Matches(msg, m.keys.Right):
			if m.col < len(m.matrix.Envs)-1 {
				m.col++
			}
			return m.layout(), nil
		case key.Matches(msg, m.keys.Promote):
			source := m.CurrentEnv()
			if source == "" {
				m.notice = "no environments discovered"
				return m, nil
			}
			return m, func() tea.Msg { return OpenPlanMsg{Source: source} }
		case key.Matches(msg, m.keys.PromoteAs):
			source := m.CurrentEnv()
			if source == "" {
				m.notice = "no environments discovered"
				return m, nil
			}
			return m, func() tea.Msg { return OpenPlanMsg{Source: source, Force: true} }
		case key.Matches(msg, m.keys.Restart):
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
		case key.Matches(msg, m.keys.Resume):
			switch len(m.inflight) {
			case 0:
				if msg.String() == "r" {
					m.notice = "nothing in flight to resume"
				}
				return m, nil
			case 1:
				id := m.inflight[0].ID
				return m, func() tea.Msg { return ResumeMsg{ID: id} }
			default:
				return m.openResumeChooser()
			}
		case key.Matches(msg, m.keys.OpenPR):
			for _, s := range m.inflight {
				if s.PR != nil && s.PR.URL != "" {
					url := s.PR.URL
					return m, func() tea.Msg { return flight.OpenPRMsg{URL: url} }
				}
			}
			m.notice = "nothing in flight has a PR to open"
			return m, nil
		case key.Matches(msg, m.keys.DeployNew):
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
	}
	var cmd tea.Cmd
	m.tbl, cmd = m.tbl.Update(msg)
	return m, cmd
}

// openChooser asks which of several first-party images d meant, as a dialog over the matrix.
func (m Model) openChooser(env string, repos []string) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(repos))
	for _, r := range repos {
		opts = append(opts, huh.NewOption(r, r))
	}
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("deploy which image in %s?", env)).Options(opts...)
	// A standalone huh field ships a zero keymap and ignores every key (AGENTS.md §4.8).
	sel.WithKeyMap(huh.NewDefaultKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = env
	return m, tea.Batch(sel.Init(), sel.Focus())
}

// openResumeChooser asks which of several in-flight promotions to open.
func (m Model) openResumeChooser() (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(m.inflight))
	for _, s := range m.inflight {
		opts = append(opts, huh.NewOption(s.ID+"  "+pair(s)+"  "+s.Verdict(), s.ID))
	}
	sel := huh.NewSelect[string]().Title("resume which promotion?").Options(opts...)
	sel.WithKeyMap(huh.NewDefaultKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = ""
	m.chooserResume = true
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func (m Model) updateChooser(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.chooser = nil
		return m, nil
	case "enter":
		if m.chooser.GetFiltering() {
			break // enter ends the filter first; a second enter chooses
		}
		choice, _ := m.chooser.GetValue().(string) // GetValue, never a captured field
		target, resume := m.chooserTarget, m.chooserResume
		m.chooser, m.chooserResume = nil, false
		if choice == "" {
			return m, nil
		}
		if resume {
			return m, func() tea.Msg { return ResumeMsg{ID: choice} }
		}
		return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: choice, Target: target} }
	}
	_, cmd := m.chooser.Update(msg)
	return m, cmd
}

// CapturesText reports whether the image chooser is open: its "/" filter takes letters, and
// the root's q-to-quit must not fire over it.
func (m Model) CapturesText() bool { return m.chooser != nil }

// currentImageRepos lists the first-party image repos the focused row's family runs in env.
func (m Model) currentImageRepos(env string) []string {
	row := m.tbl.Cursor()
	if row < 0 || row >= len(m.matrix.Rows) {
		return nil
	}
	e, ok := m.repo.Envs[env]
	if !ok {
		return nil
	}
	return FirstPartyRepos(e.Families[m.matrix.Rows[row].Family], m.promotable)
}

// CurrentEnv is the env the column cursor is on, "" when the repo has none.
func (m Model) CurrentEnv() string {
	if len(m.matrix.Envs) == 0 {
		return ""
	}
	col := m.col
	if col < 0 || col >= len(m.matrix.Envs) {
		col = 0
	}
	return m.matrix.Envs[col]
}

// CurrentFamily is the family the row cursor is on, "" when the matrix has no rows.
func (m Model) CurrentFamily() string {
	row := m.tbl.Cursor()
	if row < 0 || row >= len(m.matrix.Rows) {
		return ""
	}
	return m.matrix.Rows[row].Family
}

// IsProduction reports whether env is listed in envs.production.
func (m Model) IsProduction(env string) bool { return m.envs.IsProduction(env) }

// View is the frame: the table, a notes section when there is something to say about the
// cursor's column (drift, a cluster that could not be asked, the help line), and the footer.
// With the chooser open the frame is drawn under the dialog.
func (m Model) View() string {
	// Laid out here, on this copy, so the table's height always reflects the notes section
	// as it is now — a notice set or cleared since the last SetSize would otherwise leave the
	// box a few rows short or push the notes off the bottom.
	m = m.layout()
	frame := ui.Frame{Title: m.title(), Sections: []string{m.tbl.View()}, Footer: m.statusBar()}
	if notes := m.notes(); notes != "" {
		frame.Sections = append(frame.Sections, notes)
	}
	frame.Panes = []string{m.inflightPane(m.paneBudget())}
	view := frame.Render(m.styles, m.width, m.height)
	if m.chooser != nil {
		title := "deploy"
		if m.chooserResume {
			title = "resume"
		}
		return ui.Dialog(m.styles, view, title, m.chooser.View(), m.width, m.height)
	}
	return view
}

func (m Model) title() string {
	return "hoist · matrix · " + displayRoot(m.repo.Root)
}

// notes is the section under the table: the transient notice first (word-wrapped, never
// clipped — #85's "long errors are clipped"), else what the cluster said about the cursor's
// column, then the help line when toggled. Empty when there is nothing to say.
func (m Model) notes() string {
	lines := m.baseNotes()
	if m.paneRows(m.paneBudget()) == 0 {
		if l := m.inflightLine(); l != "" {
			lines = append(lines, ansi.Truncate(l, max(m.width-2, 1), "…"))
		}
	}
	if m.showHelp {
		lines = append(lines, m.styles.Help.Render(m.help.ShortHelpView(m.keys.FullHelp()[0])))
	}
	return strings.Join(lines, "\n")
}

// baseNotes is notes without the in-flight fold and the help line — what the pane budget
// is computed against, so the two cannot ask each other in a loop.
func (m Model) baseNotes() []string {
	inner := max(m.width-2, 1)
	var lines []string
	switch {
	case m.notice != "":
		lines = append(lines, m.styles.Notice.Render(ansi.Wordwrap(m.notice, inner, "")))
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

// paneBudget is how many rows the in-flight pane may take: what is left after the frame's
// chrome, the base notes (plus the help line when shown) and a table tall enough to keep
// its families on screen.
func (m Model) paneBudget() int {
	notes := len(m.baseNotes()) + boolInt(m.showHelp)
	rows := ui.BodyHeight(m.height, 1+boolInt(notes > 0)) - notes
	table := len(m.matrix.Rows) + 1
	return rows - max(table, minTableRows)
}

// driftLines is one sentence per drifted family in env: the cursor's row first, then the
// rest, capped so the table keeps its rows.
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
	cursor := m.tbl.Cursor()
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
		out = append(out, fmt.Sprintf("! %s runs %s in %s; manifest says %s", m.matrix.Rows[i].Family, c.Running, env, c.Text))
	}
	return out
}

// SetSize fits the table to a width × height terminal.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m.layout()
}

// SetStyles applies a palette to the table, the help line and the status bar.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	m.tbl.SetStyles(table.Styles{Header: s.Header, Cell: s.Cell, Selected: s.Selected})
	m.help.Styles = help.DefaultStyles(s.Dark)
	return m
}

// Cursor is the index of the selected family row.
func (m Model) Cursor() int { return m.tbl.Cursor() }

// Matrix is the computed matrix the screen shows.
func (m Model) Matrix() Table { return m.matrix }

// WithNotice sets the notice shown under the table — exported so the root can surface an
// honest message on the matrix after popping back to it from another screen whose own
// message it chose not to act on.
func (m Model) WithNotice(notice string) Model {
	m.notice = notice
	return m
}

func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	sections := 1
	notes := m.notes()
	if notes != "" {
		sections++
	}
	// The table fills what the frame leaves after the notes and the in-flight pane, so the
	// box reaches the footer (or the pane does).
	rows := ui.BodyHeight(m.height, sections) - lipgloss.Height(notes)*boolInt(notes != "") - m.paneRows(m.paneBudget())
	cols := fit(m.columns(), m.width-2)
	m.tbl.SetColumns(cols)
	m.tbl.SetRows(m.rows(cols))
	m.tbl.SetWidth(m.width - 2)
	m.tbl.SetHeight(max(rows, 2))
	m.help.SetWidth(m.width - 2)
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

// minCellWidth is the narrowest an env column shrinks to before the table simply overflows.
const minCellWidth = 10

// fit shrinks the widest env columns, one cell at a time, until the table fits width. The
// family column keeps its natural width; a cell narrower than its text is clipped with an
// ellipsis by the table. cellPad is the horizontal padding ui.Styles.Cell adds per column.
func fit(cols []table.Column, width int) []table.Column {
	const cellPad = 2
	total := func() int {
		n := 0
		for _, c := range cols {
			n += c.Width + cellPad
		}
		return n
	}
	for total() > width {
		widest := -1
		for i := 1; i < len(cols); i++ {
			if cols[i].Width > minCellWidth && (widest < 0 || cols[i].Width > cols[widest].Width) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		cols[widest].Width--
	}
	return cols
}

func (m Model) statusBar() string {
	env := m.CurrentEnv()
	envWord := orNoEnv(env)
	if env != "" && m.IsProduction(env) {
		envWord = m.styles.Production.Render(env + " (production)")
	}
	// The env alone on the left: it governs every write gesture, and at 80 columns the
	// family and unmanaged counts were what the hints truncated away first (the header row
	// shows the families anyway; unmanaged directories are in the notes when they matter).
	left := m.styles.Status.Render("env " + envWord)
	// The unmanaged count only when the bar has room for it whole: a truncated "2 unmana…"
	// says less than nothing.
	if n := len(m.repo.Unmanaged); n > 0 && m.width >= 100 {
		left += m.styles.Dim.Render(fmt.Sprintf(" · %d unmanaged", n))
	}
	right := m.styles.Hint.Render(m.help.ShortHelpView(m.keys.ShortHelp()))
	return ui.StatusBar(m.width, left, right)
}

// displayRoot never shows a full path: a plain relative root is shown as given, anything
// absolute or climbing out of the working directory is reduced to its base name.
func displayRoot(root string) string {
	if root == "" {
		return "."
	}
	if filepath.IsAbs(root) || root == ".." || strings.HasPrefix(root, ".."+string(filepath.Separator)) {
		return filepath.Base(root)
	}
	return filepath.ToSlash(root)
}

// selectedMarker prefixes the env column the cursor is on. The table highlights the selected
// ROW on its own, but nothing marked the selected COLUMN — and the column is what p, P, d and R
// all act on, so an operator could not see which env they were about to write to.
const selectedMarker = "▸ "

// productionMarker follows a production env's name in the header (#86): the one screen
// where an env is chosen, and until M10 the one place that fact was absent.
const productionMarker = " ⚠"

// columns builds the header: FAMILY, then one column per env, the cursor's marked and every
// production env flagged. Widths come from the widest tag-plus-state in each column.
func (m Model) columns() []table.Column {
	t := m.matrix
	cols := []table.Column{{Title: "FAMILY", Width: len("FAMILY")}}
	for _, r := range t.Rows {
		cols[0].Width = max(cols[0].Width, ansi.StringWidth(r.Family))
	}
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
		cols = append(cols, table.Column{Title: title, Width: min(w, maxCellWidth)})
	}
	return cols
}

// cellWidths is the widest text and the widest state word in column i, so rows can align
// the state to the right edge of every cell in the column.
func (m Model) cellWidths(i int) (text, state int) {
	for _, r := range m.matrix.Rows {
		c := r.Cells[i]
		text = max(text, ansi.StringWidth(c.Text))
		state = max(state, ansi.StringWidth(m.stateWord(c, m.matrix.Envs[i])))
	}
	return text, state
}

// stateWord is the cell's state as shown: "resolving…" while the cluster is being asked
// and the cell could still turn out drifted, else the cell's own word.
func (m Model) stateWord(c Cell, env string) string {
	if !c.Present {
		return ""
	}
	if m.pending[env] && (c.State == StatePinned || c.State == StateUnpinned) {
		return "resolving…"
	}
	return string(c.State)
}

// rows renders every cell to exactly its column's width, the state word flush right and
// the tag on the left, truncated with "…" when the column is too narrow for both — the
// word is the state; the tag is what a narrow terminal loses first. cols must be the fitted
// columns the table is showing, so the two agree.
func (m Model) rows(cols []table.Column) []table.Row {
	t := m.matrix
	out := make([]table.Row, 0, len(t.Rows))
	for _, r := range t.Rows {
		row := table.Row{r.Family}
		for i, c := range r.Cells {
			if !c.Present {
				row = append(row, "")
				continue
			}
			width := cols[i+1].Width
			word := m.stateWord(c, t.Envs[i])
			if word == "" {
				row = append(row, ansi.Truncate(c.Text, width, "…"))
				continue
			}
			room := width - ansi.StringWidth(word) - 2
			if room < 1 {
				row = append(row, ansi.Truncate(word, width, "…"))
				continue
			}
			text := ansi.Truncate(c.Text, room, "…")
			row = append(row, fmt.Sprintf("%-*s  %s", room, text, word))
		}
		out = append(out, row)
	}
	return out
}

// orNoEnv renders an empty selection readably rather than as a gap.
func orNoEnv(env string) string {
	if env == "" {
		return "(none)"
	}
	return env
}
