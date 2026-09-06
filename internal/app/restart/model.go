// Package restart is the matrix's R key: the screen that shows what a restart would roll, takes
// the confirmation, and follows the rollout.
//
// It is deliberately not the deploy confirm screen with a different noun. That screen's whole
// content is a diff, and a restart has none — nothing in the manifest changes. What an operator
// needs to see here instead is live: which Deployments, how many replicas each, what it last
// restarted at, and every reason this particular restart will not be seamless.
package restart

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
)

// ReadFunc reads what a restart of these Deployments would touch. Supplied by the root rather
// than reached for here: this package never opens a cluster connection of its own (AGENTS.md
// §4.8).
type ReadFunc func(ctx context.Context, env string, names []string) (restart.Plan, error)

// DoFunc performs the restart and returns the names it got through, in order, so a failure can
// say what had already rolled. It takes no env: restart.Plan already carries one, and two
// sources for the same fact is how they come to disagree.
type DoFunc func(ctx context.Context, p restart.Plan, at time.Time) ([]string, error)

// ObserveFunc reads how far the rollout has got.
type ObserveFunc func(ctx context.Context, env string, names []string, at time.Time) ([]restart.Progress, error)

// Funcs is the whole of this screen's access to the world.
type Funcs struct {
	Read    ReadFunc
	Do      DoFunc
	Observe ObserveFunc
	// Interval is how often the rollout is re-read once it is running.
	Interval time.Duration
}

type state int

const (
	stateReading state = iota
	stateConfirm
	stateRolling
	stateDone
	stateFailed
)

// BackMsg asks whatever composes screens to pop this one.
type BackMsg struct{}

type planMsg struct {
	plan restart.Plan
	err  error
}
type startedMsg struct {
	at   time.Time
	done []string
	err  error
}
type progressMsg struct {
	progress []restart.Progress
	err      error
}
type tickMsg struct{}

// Model is the screen.
type Model struct {
	styles ui.Styles
	funcs  Funcs

	env        string
	family     string
	names      []string
	production bool

	state  state
	plan   restart.Plan
	at     time.Time
	notice string
	// done is which Deployments have finished rolling, kept on the model rather than anywhere
	// package-level: two of these screens can exist at once (two envs), and shared mutable
	// state between them would be both a race and a lie.
	done map[string]bool

	// confirm is the production gate: §4.5 cannot apply to something that commits nothing, so
	// what stands in its place is a second, deliberate gesture. Non-production envs do not get
	// one — the target list and its warnings are already on screen, and enter is the answer.
	confirm  *huh.Confirm
	confirmV bool

	body          viewport.Model
	width, height int
}

// New builds the screen for one family in one env. names are the Deployments the repo says that
// family declares; nothing is read from the cluster until Init runs.
func New(env, family string, names []string, production bool, funcs Funcs, styles ui.Styles) Model {
	return Model{
		styles: styles, funcs: funcs,
		env: env, family: family, names: names, production: production,
		state: stateReading,
		body:  viewport.New(),
	}
}

// Init reads the cluster.
func (m Model) Init() tea.Cmd {
	read, env, names := m.funcs.Read, m.env, m.names
	if read == nil {
		return func() tea.Msg { return planMsg{err: fmt.Errorf("restarting is not wired up")} }
	}
	return func() tea.Msg {
		p, err := read(context.Background(), env, names)
		return planMsg{plan: p, err: err}
	}
}

// Update implements the screen contract.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case planMsg:
		if msg.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(msg.err.Error())
			return m, nil
		}
		m.plan = msg.plan
		m.state = stateConfirm
		if len(m.plan.Targets) == 0 {
			m.state, m.notice = stateFailed, fmt.Sprintf("none of %s's Deployments exist in the cluster", m.env)
		}
		return m.render(), nil
	case startedMsg:
		m.at = msg.at
		if msg.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(msg.err.Error())
			if len(msg.done) > 0 {
				m.notice += fmt.Sprintf(" — already restarted: %s", strings.Join(msg.done, ", "))
			}
			return m.render(), nil
		}
		m.state = stateRolling
		return m.render(), m.tick()
	case progressMsg:
		if msg.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(msg.err.Error())
			return m.render(), nil
		}
		return m.onProgress(msg.progress)
	case tickMsg:
		return m, m.observe()
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return m, cmd
}

func (m Model) onProgress(progress []restart.Progress) (Model, tea.Cmd) {
	var pending []string
	for _, pr := range progress {
		switch {
		case pr.Superseded:
			m.state = stateFailed
			m.notice = fmt.Sprintf("%s was restarted by something else while this one was in flight; the pods are rolling, but not for this restart", pr.Name)
			return m.render(), nil
		case pr.Blocked != "":
			m.state, m.notice = stateFailed, fmt.Sprintf("%s: %s", pr.Name, pr.Blocked)
			return m.render(), nil
		case !pr.Done:
			pending = append(pending, pr.Name)
		}
	}
	if m.done == nil {
		m.done = map[string]bool{}
	}
	for _, pr := range progress {
		if pr.Done {
			m.done[pr.Name] = true
		}
	}
	if len(pending) == 0 {
		m.state = stateDone
		return m.render(), nil
	}
	return m.render(), m.tick()
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return BackMsg{} }
	case "enter":
		if m.state != stateConfirm {
			return m, nil
		}
		if m.production {
			// The one extra gesture production gets. Nothing is committed, so there is no PR to
			// review and no approval comment to wait for; what stands in its place is having to
			// say yes on purpose.
			m.confirmV = false
			m.confirm = huh.NewConfirm().
				Title(fmt.Sprintf("Restart %d Deployment(s) in %s? It is a production env.", len(m.plan.Targets), m.env))
			// huh.NewConfirm leaves keymap zero-valued, and a zero key.Binding matches nothing:
			// a Confirm used standalone rather than inside a huh.Form ignores every keypress.
			m.confirm.WithKeyMap(huh.NewDefaultKeyMap())
			m.confirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
			if m.width > 0 {
				m.confirm.WithWidth(m.width)
			}
			return m, tea.Batch(m.confirm.Init(), m.confirm.Focus())
		}
		return m.start()
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return m, cmd
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		m.confirm = nil
		return m.render(), nil
	}
	f, cmd := m.confirm.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirm = c
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" {
		// Read from the widget, not the bool it was pointed at: Value takes the address of a
		// field in whichever Model copy built it, and every Update since has returned a new one.
		agreed, _ := m.confirm.GetValue().(bool)
		m.confirm = nil
		if !agreed {
			m.notice = "not restarted"
			return m.render(), nil
		}
		return m.start()
	}
	return m, cmd
}

func (m Model) start() (Model, tea.Cmd) {
	do, pl := m.funcs.Do, m.plan
	if do == nil {
		m.state, m.notice = stateFailed, "restarting is not wired up"
		return m.render(), nil
	}
	at := time.Now().UTC()
	m.state = stateRolling
	m.notice = ""
	return m.render(), func() tea.Msg {
		done, err := do(context.Background(), pl, at)
		return startedMsg{at: at, done: done, err: err}
	}
}

func (m Model) tick() tea.Cmd {
	d := m.funcs.Interval
	if d <= 0 {
		d = 3 * time.Second
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) observe() tea.Cmd {
	obs, env, at := m.funcs.Observe, m.env, m.at
	names := m.pending()
	if obs == nil || len(names) == 0 {
		return nil
	}
	return func() tea.Msg {
		pr, err := obs(context.Background(), env, names, at)
		return progressMsg{progress: pr, err: err}
	}
}

func (m Model) pending() []string {
	var out []string
	for _, t := range m.plan.Targets {
		if !m.done[t.Name] {
			out = append(out, t.Name)
		}
	}
	return out
}

// render refreshes the body for the current state. Called wherever the state changes, so View
// itself stays a pure read.
func (m Model) render() Model {
	var b strings.Builder
	switch m.state {
	case stateReading:
		b.WriteString("reading " + m.env + "…\n")
	default:
		fmt.Fprintf(&b, "%d Deployment(s) in %s\n\n", len(m.plan.Targets), m.env)
		for _, st := range m.plan.Targets {
			mark := " "
			switch {
			case m.done[st.Name]:
				mark = "✔"
			case m.state == stateRolling:
				mark = "…"
			}
			was := st.RestartedAt
			if was == "" {
				was = "never restarted this way"
			}
			fmt.Fprintf(&b, " %s %s  (%d replica(s), %s, last restart: %s)\n", mark, st.Name, st.Replicas, st.Strategy, was)
			for _, c := range st.GracefulRestartConcerns() {
				fmt.Fprintf(&b, "     warning: %s\n", c)
			}
		}
		for _, name := range m.plan.Absent {
			fmt.Fprintf(&b, "   %s\n     warning: declared in the repo but not in the cluster — not restarted\n", name)
		}
	}
	m.body.SetContent(b.String())
	return m
}

// View renders the screen. Every rendered string passes through redact.Strings at this one
// boundary, the same convention the other screens use.
func (m Model) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hoist restart: %s / %s%s\n", m.env, m.family, productionLabel(m.production))
	if m.confirm != nil {
		b.WriteString(m.confirm.View())
		return redact.Strings(b.String())
	}
	b.WriteString(m.body.View())
	if m.notice != "" {
		fmt.Fprintf(&b, "\n%s", m.styles.Notice.Render(m.notice))
	}
	b.WriteString("\n" + m.styles.Hint.Render(m.hint()))
	return redact.Strings(b.String())
}

func (m Model) hint() string {
	switch m.state {
	case stateReading:
		return "reading… · esc back"
	case stateConfirm:
		return "enter restart · esc back"
	case stateRolling:
		return "rolling… · esc back"
	case stateDone:
		return "all rolled · esc back"
	default:
		return "esc back"
	}
}

func productionLabel(production bool) string {
	if production {
		return "   · production"
	}
	return ""
}

// SetSize implements the screen contract.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	m.body.SetWidth(width)
	if h := height - 4; h > 0 {
		m.body.SetHeight(h)
	}
	if m.confirm != nil {
		m.confirm.WithWidth(width)
	}
	return m
}

// SetStyles implements the screen contract.
func (m Model) SetStyles(s ui.Styles) Model { m.styles = s; return m }

// CapturesText reports whether a keypress belongs to this screen's own input — true only while
// the production confirmation is open.
func (m Model) CapturesText() bool { return m.confirm != nil }
