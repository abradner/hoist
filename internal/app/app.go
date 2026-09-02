package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
)

// Model is the root tea.Model: a stack of screens, the window size, and the theme, plus
// what a screen needs to open the plan screen (internal/app/plan) without app.New having
// to be called again — the repo, the promotable prefixes, the envs config (pairs,
// production) and the digest-resolution adaptor (nil in "digest sources: none" mode).
type Model struct {
	stack         []Screen
	styles        ui.Styles
	width, height int

	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	resolveFn  plan.ResolveFunc
}

// New returns the root model with the matrix screen on the stack. promotable lists the
// image repo prefixes that count as first-party (the same list hoist plan --promotable
// takes). envs is the selected repo's envs config (production, pairs), zero-valued when
// there is none. resolveFn is what the plan screen calls to resolve digests; nil runs it in
// "digest sources: none" mode throughout. The theme starts dark and is replaced when the
// terminal reports its background.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, resolveFn plan.ResolveFunc) Model {
	m := Model{
		styles:     ui.NewStyles(true),
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		resolveFn:  resolveFn,
	}
	return m.push(matrixScreen{matrix.New(repo, promotable)})
}

// Init asks the terminal for its background colour so the palette can follow it, and starts
// whatever the top (only, at boot) screen's own Init needs.
func (m Model) Init() tea.Cmd {
	var screenCmd tea.Cmd
	if len(m.stack) > 0 {
		screenCmd = m.stack[len(m.stack)-1].Init()
	}
	return tea.Batch(tea.RequestBackgroundColor, screenCmd)
}

// Update handles window size, theme and the global keys, and forwards everything else to
// the top screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m.each(func(s Screen) Screen { return s.SetSize(msg.Width, msg.Height) }), nil
	case tea.BackgroundColorMsg:
		m.styles = ui.NewStyles(msg.IsDark())
		return m.each(func(s Screen) Screen { return s.SetStyles(m.styles) }), nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	case matrix.OpenPlanMsg:
		target := ""
		if m.envs.Pairs != nil {
			target = m.envs.Pairs[msg.Source]
		}
		ps := planScreen{plan.New(m.repo, m.promotable, m.envs, msg.Source, target, msg.Force, m.resolveFn)}
		m = m.push(ps)
		return m, ps.Init()
	case plan.BackMsg:
		return m.pop(), nil
	}
	if len(m.stack) == 0 {
		return m, nil
	}
	top := len(m.stack) - 1
	s, cmd := m.stack[top].Update(msg)
	stack := append([]Screen(nil), m.stack...)
	stack[top] = s
	m.stack = stack
	return m, cmd
}

// View renders the top screen in the alternate screen buffer.
func (m Model) View() tea.View {
	content := ""
	if n := len(m.stack); n > 0 {
		content = m.stack[n-1].View()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// push adds a screen on top, sized and themed like the rest. The slice is copied so the
// returned model shares nothing with the receiver.
func (m Model) push(s Screen) Model {
	s = s.SetStyles(m.styles)
	if m.width > 0 && m.height > 0 {
		s = s.SetSize(m.width, m.height)
	}
	m.stack = append(append([]Screen(nil), m.stack...), s)
	return m
}

// pop drops the top screen, when there is more than one — the matrix, at the bottom, is
// never popped. doc.go's "the stack has push only today" note is what this ends: a screen
// asks to be popped with a message the root recognizes by concrete type (matrix.OpenPlanMsg
// pushes, plan.BackMsg pops), never by calling back into app itself.
func (m Model) pop() Model {
	if len(m.stack) <= 1 {
		return m
	}
	m.stack = append([]Screen(nil), m.stack[:len(m.stack)-1]...)
	return m
}

// each applies f to every screen, copying the stack.
func (m Model) each(f func(Screen) Screen) Model {
	stack := make([]Screen, len(m.stack))
	for i, s := range m.stack {
		stack[i] = f(s)
	}
	m.stack = stack
	return m
}
