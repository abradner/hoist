package app

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
)

// Model is the root tea.Model: a stack of screens, the window size, and the theme, plus
// what a screen needs to open the plan screen (internal/app/plan) or the tag picker
// (internal/app/tags) without app.New having to be called again — the repo, the promotable
// prefixes, the envs config (pairs, production), the digest-resolution adaptor (nil in
// "digest sources: none" mode) and the tag-picker's own registry/forge adaptor (nil runs the
// picker with no data source at all, reported as its own error state rather than a panic).
type Model struct {
	stack         []Screen
	styles        ui.Styles
	width, height int

	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	resolveFn  plan.ResolveFunc
	tagsFn     tags.BuildFunc

	// notice is a transient, root-level message shown below the top screen — currently only
	// used for flight.OpenPRMsg/AbortMsg (see their cases in Update): neither has a real
	// handler wired in yet (cmd/hoist's own URL-opener and abort mechanism are documented
	// follow-up work, per PR #39's own report), so this is the "don't silently drop it"
	// feedback until that wiring lands (PR #39 review finding #1). Cleared on the next
	// keypress, mirroring every screen's own per-keypress notice convention (matrix.Model,
	// plan.Model, flight.Model all clear theirs the same way).
	notice string
}

// New returns the root model with the matrix screen on the stack. promotable lists the
// image repo prefixes that count as first-party (the same list hoist plan --promotable
// takes). envs is the selected repo's envs config (production, pairs), zero-valued when
// there is none. resolveFn is what the plan screen calls to resolve digests; nil runs it in
// "digest sources: none" mode throughout. tagsFn is what the tag-picker screen calls to list
// and fetch registry/forge data for one image repo; nil opens the picker with no data source
// (it reports the resulting error itself, same as a resolveFn failure does for plan). The
// theme starts dark and is replaced when the terminal reports its background.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, resolveFn plan.ResolveFunc, tagsFn tags.BuildFunc) Model {
	m := Model{
		styles:     ui.NewStyles(true),
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		resolveFn:  resolveFn,
		tagsFn:     tagsFn,
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
		m.notice = ""
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
	case plan.StartMsg:
		// internal/app has no repoFullName (RepoConfig.GitHub), CI/approval policy,
		// CloneDir/WorktreeDir/Base, or concrete git.Git/forge.Forge adaptor to build a
		// real engine.PromotionState or flight.DriveFunc from here — cmd/hoist owns wiring
		// that in (AGENTS.md §4.8's "cmd/hoist owns the adapter" rule; see
		// internal/engine/identity.go's DeriveID, template.go's RenderPRBody/
		// RenderCommitMessage, and cmd/hoist/promote.go for what building the real thing
		// actually takes). This pushes the flight screen with only what StartMsg carries
		// and a nil DriveFunc, so it renders read-only (every step "not yet reached", no
		// ticking) until that wiring lands — proving the navigation shape without
		// fabricating an ID or branch name this package cannot derive correctly.
		fs := flightScreen{flight.New(engine.PromotionState{
			SourceEnv: msg.Source,
			TargetEnv: msg.Target,
		}, flight.PollDurations{}, nil)}
		m = m.push(fs)
		return m, fs.Init()
	case flight.BackMsg:
		return m.pop(), nil
	case flight.OpenPRMsg:
		// cmd/hoist has not wired a real "open this URL in the operator's browser"
		// mechanism into internal/app yet (documented follow-up work, per PR #39's own
		// report) — until it does, silently dropping this message would make the o key
		// look like it did nothing. Surface the URL instead (PR #39 review finding #1).
		m.notice = fmt.Sprintf("open PR not wired yet: %s", msg.URL)
		return m, nil
	case flight.AbortMsg:
		// Same principle as OpenPRMsg above: no real abort mechanism (closing the PR,
		// deleting the branch) is wired in yet, so surface a clear notice rather than
		// silently eating the x keypress (PR #39 review finding #1). flight.Model itself
		// now refuses to emit this message at all for an empty/read-only promotion (finding
		// #2), so msg.ID here is always a real, non-empty id.
		m.notice = fmt.Sprintf("abort not wired yet for promotion %s", msg.ID)
		return m, nil
	case matrix.OpenTagsMsg:
		var mapped bool
		var listFn tags.ListFunc
		var metaFn tags.MetaFunc
		if m.tagsFn != nil {
			mapped, listFn, metaFn = m.tagsFn(msg.ImageRepo)
		}
		production := plan.IsProduction(msg.Target, m.envs)
		stagingEnv, stagingTag, hasMismatch := tags.StagingMismatch(m.repo, msg.ImageRepo, msg.Target, m.envs)
		ts := tagsScreen{tags.New(msg.ImageRepo, msg.Target, mapped, production, stagingEnv, stagingTag, hasMismatch, listFn, metaFn)}
		m = m.push(ts)
		return m, ts.Init()
	case tags.BackMsg:
		return m.pop(), nil
	case tags.SelectedMsg, tags.DirectRequestedMsg:
		// This milestone's picker stops at reporting the operator's choice (SelectedMsg/
		// DirectRequestedMsg's own doc comments): no screen in this codebase drives a write
		// yet (hoist promote is CLI-only). Pop back to the matrix; a future "deploy new
		// image" milestone is what turns this into an actual promotion.
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

// View renders the top screen in the alternate screen buffer, with the root's own transient
// notice (see Model.notice) appended below it when one is set.
func (m Model) View() tea.View {
	content := ""
	if n := len(m.stack); n > 0 {
		content = m.stack[n-1].View()
	}
	if m.notice != "" {
		content += "\n" + m.styles.Notice.Render(m.notice)
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
