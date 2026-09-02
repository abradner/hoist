package matrix

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
)

// maxCellWidth caps an env column so one long tag list cannot push the others off screen.
const maxCellWidth = 40

// Model is the matrix screen. It is a value: Update, SetSize and SetStyles return the
// updated model.
type Model struct {
	repo          *gitops.Repo
	matrix        Table
	tbl           table.Model
	styles        ui.Styles
	keys          keyMap
	help          help.Model
	width, height int
	showHelp      bool
	notice        string
	// col is the focused env column: CurrentEnv's index into matrix.Envs. It has no
	// visual marker yet (a follow-up, not this screen's shipped concern) but is real state:
	// Left/Right move it, and it is what OpenPlanMsg names as Source.
	col int
}

type keyMap struct {
	Up, Down, Left, Right, Promote, PromoteAs, Help, Quit key.Binding
}

// ShortHelp is the hint set shown in the status bar.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Help, k.Quit}
}

// FullHelp is what ? expands to; one group, rendered on a single line.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.Left, k.Right, k.Promote, k.PromoteAs, k.Help, k.Quit}}
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Left:      key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "source env")),
		Right:     key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "source env")),
		Promote:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "plan promotion")),
		PromoteAs: key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "plan promotion to…")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// OpenPlanMsg is emitted when the operator asks to plan a promotion from CurrentEnv: p asks
// for the configured pair (envs.pairs[Source] — the root looks it up, since this package
// never imports internal/config, matching AGENTS.md §4.8's "screens never import app" the
// other way round too), P (Force) always prompts for the target instead. The root
// recognizes this by concrete type in its own Update switch (see internal/app/screen.go).
type OpenPlanMsg struct {
	Source string
	Force  bool
}

// New builds the screen for a discovered repo. promotable lists the first-party image repo
// prefixes (see Compute). The model has no size until SetSize is called.
func New(repo *gitops.Repo, promotable []string) Model {
	m := Model{
		repo:   repo,
		matrix: Compute(repo, promotable),
		keys:   defaultKeyMap(),
		help:   help.New(),
	}
	// The table's own up/down bindings are replaced so the screen owns the key vocabulary.
	km := table.DefaultKeyMap()
	km.LineUp, km.LineDown = m.keys.Up, m.keys.Down
	m.tbl = table.New(
		table.WithColumns(columns(m.matrix)),
		table.WithRows(rows(m.matrix)),
		table.WithKeyMap(km),
		table.WithFocused(true),
	)
	return m.SetStyles(ui.NewStyles(true))
}

// Init has nothing to start; the root requests the background colour.
func (m Model) Init() tea.Cmd { return nil }

// Update handles the screen's keys and forwards the rest to the table. Quit is the root's.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyPressMsg); ok {
		m.notice = ""
		switch {
		case key.Matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp
			return m.layout(), nil
		case key.Matches(msg, m.keys.Left):
			if m.col > 0 {
				m.col--
			}
			return m, nil
		case key.Matches(msg, m.keys.Right):
			if m.col < len(m.matrix.Envs)-1 {
				m.col++
			}
			return m, nil
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
		}
	}
	var cmd tea.Cmd
	m.tbl, cmd = m.tbl.Update(msg)
	return m, cmd
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

// View is the table, the help line when toggled, and the status bar.
func (m Model) View() string {
	parts := []string{m.tbl.View()}
	if m.showHelp {
		parts = append(parts, m.styles.Help.Render(m.help.ShortHelpView(m.keys.FullHelp()[0])))
	}
	parts = append(parts, m.statusBar())
	return strings.Join(parts, "\n")
}

// SetSize fits the table to a width × height terminal, keeping one line for the status bar
// and one for the help line when it is shown.
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

func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	reserved := 1 // status bar
	if m.showHelp {
		reserved++
	}
	m.tbl.SetColumns(fit(columns(m.matrix), m.width))
	m.tbl.SetWidth(m.width)
	m.tbl.SetHeight(max(m.height-reserved, 1))
	m.help.SetWidth(m.width)
	return m
}

// minCellWidth is the narrowest an env column shrinks to before the table simply overflows.
const minCellWidth = 8

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
	var left string
	if m.notice != "" {
		left = m.styles.Notice.Render(m.notice)
	} else {
		left = m.styles.Status.Render(fmt.Sprintf("%s  envs %d · families %d · unmanaged %d",
			displayRoot(m.repo.Root), len(m.matrix.Envs), len(m.matrix.Rows), len(m.repo.Unmanaged)))
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

func columns(t Table) []table.Column {
	cols := []table.Column{{Title: "family", Width: len("family")}}
	for _, r := range t.Rows {
		cols[0].Width = max(cols[0].Width, ansi.StringWidth(r.Family))
	}
	for i, e := range t.Envs {
		w := ansi.StringWidth(e)
		for _, r := range t.Rows {
			w = max(w, ansi.StringWidth(r.Cells[i].String()))
		}
		cols = append(cols, table.Column{Title: e, Width: min(w, maxCellWidth)})
	}
	return cols
}

func rows(t Table) []table.Row {
	out := make([]table.Row, 0, len(t.Rows))
	for _, r := range t.Rows {
		row := table.Row{r.Family}
		for _, c := range r.Cells {
			row = append(row, c.String())
		}
		out = append(out, row)
	}
	return out
}
