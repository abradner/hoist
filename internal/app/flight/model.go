package flight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
)

// DriveFunc advances a promotion by one poll iteration: it runs engine.Drive once (Drive
// itself calls Act on whichever steps are not yet satisfied, in order, then returns at the
// first step that is Waiting, Blocked, or erroring — see engine.Drive's own doc comment)
// and re-derives every step's own standing with engine.Status, so the screen can render the
// full step list rather than only wherever Drive stopped. err is non-nil only for a genuine
// plumbing failure (Known bug classes: a 404/permissions hiccup on Checks or Comments,
// mirroring cmd/hoist/drive.go's driveToCompletion) — Waiting, Blocked and "not yet acted
// on" are never errors, they are read from statuses instead.
//
// cmd/hoist supplies the concrete function, closing over the real git.Git/forge.Forge
// adaptors and whatever state-save path the CLI's own promote/resume commands already use —
// the same shape plan.ResolveFunc uses to keep the plan screen ignorant of cluster/registry
// adaptors (AGENTS.md §4.8's "cmd/hoist owns the adapter" rule). This package therefore
// never imports pkg/git, pkg/forge, or a state-persistence path; it takes and returns plain
// engine.PromotionState values (not a pointer) so a tea.Cmd's goroutine never races the
// model's own copy — see driveCmd. done and statuses mirror engine.Status's own return
// shape exactly (a real implementation is expected to call engine.Drive then engine.Status
// in turn) rather than making this package re-derive "is the promotion finished" from the
// statuses slice by, say, checking whether the last entry names StepMerged — engine.Status
// already answers that question and its short-circuit's own reasoning (see its doc
// comment) lives in exactly one place.
type DriveFunc func(ctx context.Context, s engine.PromotionState) (next engine.PromotionState, done bool, statuses []engine.StepStatus, err error)

// keyMap is this screen's own key vocabulary, on top of the root's global quit keys.
type keyMap struct {
	Open, Reobserve, Abort, Log, Back key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Open:      key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		Reobserve: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "re-observe")),
		Abort:     key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "abort")),
		Log:       key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "log")),
		Back:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	}
}

// OpenPRMsg asks whatever composes screens to open s.PR's URL in the operator's browser —
// the actual open mechanism (exec.Command("open", …) or equivalent) is out of scope for
// this screen (AGENTS.md §4.8: a screen requests navigation by emitting its own concrete
// type; mirrors matrix.OpenPlanMsg and plan.BackMsg).
type OpenPRMsg struct{ URL string }

// AbortMsg asks whatever composes screens to abort promotion ID — closing the PR, deleting
// the branch, or whatever "abort" means operationally is out of scope for this screen; it
// only requests it (same convention as OpenPRMsg above).
type AbortMsg struct{ ID string }

// BackMsg pops this screen back to whatever was underneath it (mirrors plan.BackMsg).
type BackMsg struct{}

// tickMsg fires the next poll iteration.
type tickMsg struct{}

// driveResultMsg is delivered once a driveCmd finishes.
type driveResultMsg struct {
	state    engine.PromotionState
	done     bool
	statuses []engine.StepStatus
	err      error
}

// Model is the flight screen. It is a value: Update, SetSize and SetStyles return the
// updated model, matching internal/app/plan and internal/app/matrix's convention.
type Model struct {
	state engine.PromotionState
	order []engine.StepName
	rows  []Row
	done  bool

	driveFn DriveFunc
	poll    config.PollConfig
	// busy is true while a driveCmd is in flight, so a tick landing mid-call and a manual R
	// press can't both fire a second, overlapping DriveFunc call.
	busy bool

	spinner spinner.Model
	showLog bool
	notice  string
	// errNotice is the last DriveFunc plumbing error (redacted), shown until the next
	// successful poll clears it.
	errNotice string

	styles        ui.Styles
	keys          keyMap
	width, height int
}

// New builds the flight screen for a promotion already at least identified (state.ID,
// SourceEnv, TargetEnv — whatever the caller already has, typically fresh off the plan
// screen's "start" flow or engine.LoadState on hoist resume). driveFn is nil in a read-only
// context with nothing to drive: the screen still renders state and never ticks or
// schedules a poll, and R shows a notice instead of calling nil.
func New(state engine.PromotionState, poll config.PollConfig, driveFn DriveFunc) Model {
	m := Model{
		state:   state,
		order:   StepOrder,
		poll:    poll,
		driveFn: driveFn,
		spinner: spinner.New(spinner.WithSpinner(spinner.Line)),
		keys:    defaultKeyMap(),
	}
	m.rows = DeriveRows(m.order, false, nil) // every step "not yet reached" until the first poll lands
	if driveFn != nil {
		m.busy = true
	}
	return m
}

// Init starts the spinner and the first poll, but only when there is something to drive —
// a read-only screen (driveFn nil) has nothing to animate or observe, so Init returns nil
// rather than starting a spinner tick chain that would otherwise run forever with nothing
// ever rendering it (PR #39 review finding #5). The first poll runs immediately rather than
// waiting a full pollInterval, so the screen shows real status as soon as it opens instead
// of a screenful of "not yet reached" dots.
func (m Model) Init() tea.Cmd {
	if m.driveFn == nil {
		return nil
	}
	return tea.Batch(m.spinner.Tick, m.driveCmd())
}

// driveCmd runs one DriveFunc call off the Update call stack (AGENTS.md §4.3: it talks to
// git/the forge). state is captured by value at call time, so a concurrent Update never
// races the copy this goroutine reads.
func (m Model) driveCmd() tea.Cmd {
	driveFn, state := m.driveFn, m.state
	if driveFn == nil {
		return nil
	}
	return func() tea.Msg {
		next, done, statuses, err := driveFn(context.Background(), state)
		return driveResultMsg{state: next, done: done, statuses: statuses, err: err}
	}
}

// Update handles the screen's own keys, the spinner, and the drive/tick loop.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case driveResultMsg:
		return m.onDriveResult(msg)
	case tickMsg:
		if m.busy || m.done || m.driveFn == nil {
			return m, nil
		}
		m.busy = true
		return m, tea.Batch(m.driveCmd(), m.spinner.Tick)
	case spinner.TickMsg:
		// Only keep the spinner's own tick chain alive while it is actually animating
		// something: busy (a driveCmd is in flight, or Init just kicked one off) and not
		// done and not read-only. Rescheduling unconditionally here ran a permanent,
		// invisible tick loop for as long as the screen stayed open, done or read-only
		// included (PR #39 review finding #5) — busy already implies driveFn != nil and
		// !done (see onDriveResult and the tickMsg/Reobserve guards above), but the extra
		// checks are cheap and keep this case as defensive as the tickMsg case it mirrors.
		if !m.busy || m.done || m.driveFn == nil {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) onDriveResult(msg driveResultMsg) (Model, tea.Cmd) {
	m.busy = false
	if msg.err != nil {
		// A plumbing hiccup (Known bug classes: a 404/permissions blip on Checks or
		// Comments — see cmd/hoist/drive.go's retryableStep) must not stop the screen from
		// ever polling again: show it and keep ticking at the same interval.
		m.errNotice = redact.Strings(msg.err.Error())
		return m, m.scheduleTick()
	}
	m.errNotice = ""
	m.state = msg.state
	m.done = msg.done
	m.rows = DeriveRows(m.order, m.done, msg.statuses)
	if m.done {
		return m, nil
	}
	return m, m.scheduleTick()
}

// scheduleTick waits pollInterval's answer for whichever step is currently active before
// firing the next poll (AGENTS.md invariant 4: the actual waiting lives in the caller's own
// loop, never inside a Step's Act — this is that loop's TUI-driven twin).
func (m Model) scheduleTick() tea.Cmd {
	phase, ok := ActiveStep(m.rows)
	if !ok && len(m.order) > 0 {
		phase = m.order[len(m.order)-1]
	}
	d := pollInterval(m.poll, phase)
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	switch {
	case key.Matches(msg, m.keys.Open):
		if url, ok := PRURL(m.state); ok {
			return m, func() tea.Msg { return OpenPRMsg{URL: url} }
		}
		m.notice = "no PR to open yet"
		return m, nil
	case key.Matches(msg, m.keys.Reobserve):
		if m.driveFn == nil {
			m.notice = "nothing to re-observe (read-only)"
			return m, nil
		}
		if m.busy || m.done {
			return m, nil
		}
		m.busy = true
		return m, tea.Batch(m.driveCmd(), m.spinner.Tick)
	case key.Matches(msg, m.keys.Abort):
		// Nothing to abort when there is no real DriveFunc wired (read-only, the shape
		// app.go's plan.StartMsg handler currently pushes) or the promotion has no real ID
		// (the same stub state) — emitting AbortMsg here would hand a future handler
		// nothing it could safely act on (PR #39 review finding #2: abort fired
		// unconditionally, risking an empty-ID abort being mishandled downstream).
		if m.driveFn == nil || m.state.ID == "" {
			m.notice = "nothing to abort — this promotion isn't being driven yet"
			return m, nil
		}
		id := m.state.ID
		return m, func() tea.Msg { return AbortMsg{ID: id} }
	case key.Matches(msg, m.keys.Log):
		m.showLog = !m.showLog
		return m, nil
	case key.Matches(msg, m.keys.Back):
		return m, func() tea.Msg { return BackMsg{} }
	}
	return m, nil
}

// SetSize records the terminal size; the step list and log have no internal scrolling
// component (no layout library, AGENTS.md §4.7) so a very long history can overflow a short
// terminal — see the PR report's note on this tradeoff.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m
}

// SetStyles applies the palette; this screen has no huh fields or other themed components
// beyond the shared status bar and notice styles.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	return m
}

// View renders the header (id, envs, stopwatch), the step list, the log when toggled, any
// notice, and the status bar. The whole assembled string passes through redact.Strings once
// here at the final boundary, matching plan.Model's own belt-and-suspenders convention. This
// is not defense in depth on top of an earlier redaction: engine.Status hands Row.Detail
// over unredacted (see rows.go's Row.Detail comment for why appendHistory's redaction does
// not apply to this path) — this call is the one place that text is actually scrubbed before
// reaching the terminal.
func (m Model) View() string {
	parts := []string{m.header(), m.stepList()}
	if m.showLog {
		parts = append(parts, m.logView())
	}
	if m.notice != "" {
		parts = append(parts, m.styles.Notice.Render(m.notice))
	}
	if m.errNotice != "" {
		parts = append(parts, m.styles.Notice.Render(m.errNotice))
	}
	parts = append(parts, ui.StatusBar(m.width, m.statusLeft(), m.hint()))
	return redact.Strings(strings.Join(parts, "\n"))
}

func (m Model) header() string {
	left := fmt.Sprintf("hoist promote: %s -> %s  (%s)", m.state.SourceEnv, m.state.TargetEnv, m.state.ID)
	return ui.StatusBar(m.width, left, m.elapsed())
}

func (m Model) elapsed() string {
	start := StartedAt(m.state)
	if start.IsZero() {
		return ""
	}
	return time.Since(start).Round(time.Second).String()
}

// stepList renders one line per step (glyph + label) with a second, indented detail line
// for whichever row is Active — the "active step detail" the design brief calls for (e.g.
// "CI 2/3 complete", "waiting for `hoist approve …`"). A fully done promotion has no active
// row (DeriveRows never sets Active when done — every row is already Done), but its own
// last row still carries a real Detail (engine.Status's short-circuited final-step
// Observation, e.g. "merged as <sha>; branch deleted"), so that one row's detail is shown
// too even though it is not "active" in the mid-flight sense.
func (m Model) stepList() string {
	var b strings.Builder
	last := len(m.rows) - 1
	for i, r := range m.rows {
		marker := r.Glyph
		if r.Active && m.busy {
			marker = m.spinner.View()
		}
		fmt.Fprintf(&b, "%s %s\n", marker, Label(r.Step))
		if r.Detail != "" && (r.Active || (m.done && i == last)) {
			fmt.Fprintf(&b, "    %s\n", r.Detail)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) logView() string {
	var b strings.Builder
	b.WriteString("\nHistory:\n")
	for _, h := range m.state.History {
		fmt.Fprintf(&b, "  %s  %-12s  %s\n", h.At.Format(time.RFC3339), h.Step, h.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) statusLeft() string {
	if m.done {
		return "promotion complete"
	}
	return ""
}

func (m Model) hint() string {
	return "o open PR · R re-observe · x abort · l log · esc back"
}

// pollInterval mirrors cmd/hoist/drive.go's own pollInterval exactly. It is duplicated
// rather than imported because cmd/hoist is package main and cannot be imported from here;
// the PR report flags this duplication for a reviewer to double-check against
// cmd/hoist/drive.go if that function's own switch ever changes. CI and Approval read
// config.PollConfig, so this never hand-copies cmd/hoist's magic numbers — only the 2s
// fallback for every other step is a literal, identical to cmd/hoist's own (there is
// nothing to tune there: every other step only ever waits on the interactive signing prompt
// or a single merge/branch-delete retry).
func pollInterval(poll config.PollConfig, phase engine.StepName) time.Duration {
	switch phase {
	case engine.StepCIGreen:
		return time.Duration(poll.CI)
	case engine.StepApproved:
		return time.Duration(poll.Approval)
	default:
		return 2 * time.Second
	}
}
