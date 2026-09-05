// Package deploy is the confirm screen for writing one named image into one env — the
// "image bump" half of hoist's problem statement, reached with d on the matrix and a tag
// chosen in internal/app/tags.
//
// It exists as its own screen rather than as a mode of internal/app/plan because the two
// confirm different things. The plan screen confirms a set: several repos, each tickable,
// derived from a source env. A deploy confirms one image the operator named outright, so
// there is nothing to tick and no source to describe — and the questions it should answer
// ("what is in this build", "what is running now") are not the plan screen's.
//
// What it does share is the rule that no write happens without the operator seeing the
// bytes: it renders the same unified diff, through plan.RenderDiff, for the same reason.
package deploy

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
)

// BackMsg asks whatever composes screens to pop this one.
type BackMsg struct{}

// StartMsg is the operator confirming the deploy. Mode mirrors plan.StartMsg's: ModePR opens
// a pull request, ModeDirect commits straight to the base branch. Confirmed is true only when
// ModeDirect was reached through this screen's own huh.Confirm — engine.DirectCommitGateStep
// reads it as the record of that gesture (internal/engine/direct.go).
type StartMsg struct {
	Plan      gitops.Plan
	Mode      string
	Confirmed bool
	Target    string
	Image     string
}

// Mode values, mirroring internal/app/plan's own so the root can treat both screens' StartMsgs
// the same way.
const (
	ModePR     = "PR"
	ModeDirect = "direct"
)

// Model is the deploy confirm screen.
type Model struct {
	styles        ui.Styles
	width, height int

	pl         gitops.Plan
	root       string
	target     string
	image      string
	production bool

	diff     viewport.Model
	diffErr  error
	mode     string
	confirm  *huh.Confirm
	confirmV bool
	notice   string
	ticked   map[string]bool
}

// New builds the screen for an already-constructed deploy plan. root is the repo checkout the
// diff is read from; envs decides whether the target is production, which forces PR mode.
func New(pl gitops.Plan, root, image string, envs config.EnvsConfig, styles ui.Styles) Model {
	ticked := map[string]bool{}
	for _, e := range pl.Edits {
		ticked[e.Ref.Repo] = true
	}
	m := Model{
		styles:     styles,
		pl:         pl,
		root:       root,
		target:     pl.TargetEnv,
		image:      image,
		production: plan.IsProduction(pl.TargetEnv, envs),
		mode:       ModePR,
		ticked:     ticked,
		diff:       viewport.New(),
	}
	body, err := plan.RenderDiff(root, pl.Edits, ticked)
	if err != nil {
		m.diffErr = err
	} else {
		m.diff.SetContent(body)
	}
	return m
}

// WithDirectMode opens the screen already in direct mode, for the picker's own D path: that
// gesture (keypress + huh.Confirm, internal/app/tags) has already been completed, and asking
// for it twice would be ceremony rather than safety. Production is the exception — §4.5 gives
// it no direct path at all, so the request is dropped and the screen says why.
func (m Model) WithDirectMode() Model {
	if m.production {
		m.notice = fmt.Sprintf("%s is a production env — deploys there always open a PR", m.target)
		return m
	}
	m.mode = ModeDirect
	return m
}

// Init implements the screen contract; nothing to load, the plan arrived built.
func (m Model) Init() tea.Cmd { return nil }

// Update implements the screen contract.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		return m.onKey(k)
	}
	var cmd tea.Cmd
	m.diff, cmd = m.diff.Update(msg)
	return m, cmd
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	switch msg.String() {
	case "esc":
		return m, func() tea.Msg { return BackMsg{} }
	case "enter":
		if m.diffErr != nil {
			// The screen's entire promise is that the bytes are visible before anything is
			// written. When RenderDiff failed there are no bytes on screen, so enter would
			// confirm a write on the operator's behalf against something they were never
			// shown — the one thing this screen exists to prevent (Copilot, PR #72). Esc back
			// and fix the cause; there is no way to force past it, deliberately.
			m.notice = "cannot confirm a deploy whose diff could not be rendered — esc back and retry"
			return m, nil
		}
		return m, m.start(m.mode, false)
	case "m":
		if m.production {
			// §4.5: production always goes through a PR. Refusing with the reason beats a
			// key that silently does nothing.
			m.notice = fmt.Sprintf("%s is a production env — deploys there always open a PR", m.target)
			return m, nil
		}
		if m.mode == ModeDirect {
			m.mode = ModePR
			return m, nil
		}
		m.confirmV = false
		m.confirm = huh.NewConfirm().
			Title(fmt.Sprintf("Commit %s straight to %s with no PR?", m.image, m.target)).
			Value(&m.confirmV)
		m.confirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
		if m.width > 0 {
			m.confirm.WithWidth(m.width)
		}
		return m, tea.Batch(m.confirm.Init(), m.confirm.Focus())
	}
	var cmd tea.Cmd
	m.diff, cmd = m.diff.Update(msg)
	return m, cmd
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		m.confirm = nil
		return m, nil
	}
	_, cmd := m.confirm.Update(msg)
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" {
		agreed := m.confirmV
		m.confirm = nil
		if agreed {
			m.mode = ModeDirect
		}
		return m, nil
	}
	return m, cmd
}

// start emits the confirmation. confirmed is true only on the direct path that came through
// the huh.Confirm above.
func (m Model) start(mode string, _ bool) tea.Cmd {
	pl, target, image := m.pl, m.target, m.image
	confirmed := mode == ModeDirect
	return func() tea.Msg {
		return StartMsg{Plan: pl, Mode: mode, Confirmed: confirmed, Target: target, Image: image}
	}
}

// SetSize implements the screen contract.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	m.diff.SetWidth(width)
	if h := height - 5; h > 0 {
		m.diff.SetHeight(h)
	}
	if m.confirm != nil {
		m.confirm.WithWidth(width)
	}
	return m
}

// SetStyles implements the screen contract.
func (m Model) SetStyles(s ui.Styles) Model { m.styles = s; return m }

// CapturesText reports whether a keypress belongs to this screen's own text input — true only
// while the direct-mode confirmation is open.
func (m Model) CapturesText() bool { return m.confirm != nil }

// View implements the screen contract.
func (m Model) View() string {
	var b strings.Builder
	mode := m.mode
	if m.production {
		mode += " · production"
	}
	fmt.Fprintf(&b, "hoist deploy: %s -> %s   mode: %s\n", m.image, m.target, mode)
	fmt.Fprintf(&b, "%s\n\n", scale(m.pl))
	if m.confirm != nil {
		b.WriteString(m.confirm.View())
		return b.String()
	}
	if m.diffErr != nil {
		fmt.Fprintf(&b, "could not render the diff: %v\n", m.diffErr)
	} else {
		b.WriteString(m.diff.View())
	}
	// The plan's own warnings, above the hint and above the fold — this is the last screen
	// before a write, and a warning the CLI's dry run and the PR body both carry (see
	// internal/app/plan.WarnDeployIntoProduction) has no business being invisible on the one
	// surface where the operator is about to press enter. Informational, never blocking
	// (AGENTS.md §4.5): enter still works.
	for _, w := range m.pl.Warnings {
		fmt.Fprintf(&b, "\n%s", m.styles.Notice.Render("warning: "+w.Message))
	}
	if m.notice != "" {
		fmt.Fprintf(&b, "\n%s", m.styles.Notice.Render(m.notice))
	}
	b.WriteString("\n" + m.styles.Hint.Render("enter deploy · m mode · esc back"))
	return b.String()
}

// scale is the one-line summary of what will be written — the sentence an operator would say
// out loud before pressing enter.
func scale(pl gitops.Plan) string {
	files := map[string]bool{}
	n := 0
	for _, e := range pl.Edits {
		if e.NoOp() {
			continue
		}
		n++
		files[e.File] = true
	}
	return fmt.Sprintf("%s · %s", plural(n, "occurrence"), plural(len(files), "file"))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
