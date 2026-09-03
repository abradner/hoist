package flight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
)

// PollDurations is the plain-value slice of internal/config.PollConfig this screen actually
// needs, in place of importing internal/config itself. AGENTS.md §4.8: a screen never imports
// config/registry policy, only the plain values or function types cmd/hoist (the one place
// allowed to know both sides) translates for it. Zero values are valid — New/pollInterval
// already fall back to a fixed default for anything left unset.
type PollDurations struct {
	CI, Approval, Deadline time.Duration
}

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

// driveResultMsg is delivered once a driveCmd finishes. gen is stamped with the issuing
// Model's own generation (see nextGen and Model.gen) so onDriveResult can tell a result
// belonging to THIS model instance apart from a stale one left over from another — see
// onDriveResult's own doc comment for why that distinction matters.
type driveResultMsg struct {
	gen      uint64
	state    engine.PromotionState
	done     bool
	statuses []engine.StepStatus
	err      error
}

// nextGen hands out a unique generation number to every flight.Model constructed by New,
// process-wide — see Model.gen's own doc comment for what it guards against. A plain atomic
// counter is enough: it only ever needs to distinguish Model instances within one running
// process (TUI state is never persisted or shared across processes, AGENTS.md §4.1), never to
// be stable, meaningful, or unique across a restart.
var nextGen atomic.Uint64

// Model is the flight screen. It is a value: Update, SetSize and SetStyles return the
// updated model, matching internal/app/plan and internal/app/matrix's convention.
type Model struct {
	state engine.PromotionState
	order []engine.StepName
	rows  []Row
	done  bool
	// stopped is true once a driveFn call returned a non-retryable error (see retryableErr):
	// scheduleTick is not called again automatically, though R still lets the operator retry
	// by hand (mirroring hoist resume's own "re-run to retry" convention for a terminal
	// failure — see onDriveResult's own doc comment for why this must not be done regardless
	// of which step or error shape failed).
	stopped bool

	driveFn DriveFunc
	poll    PollDurations
	// deadlineAt is the one absolute instant poll.Deadline names for this flight screen's
	// entire drive, computed once here rather than re-derived per poll — see driveCmd's own
	// doc comment for why a fresh per-call timeout would let the wait outlive the deadline
	// entirely. Zero when poll.Deadline <= 0 ("no bound at all", the existing convention).
	deadlineAt time.Time
	// busy is true while a driveCmd is in flight, so a tick landing mid-call and a manual R
	// press can't both fire a second, overlapping DriveFunc call.
	busy bool
	// gen is this Model instance's own generation, stamped into every driveResultMsg its own
	// driveCmd calls produce (see nextGen and onDriveResult) — the guard against a driveCmd
	// issued by a DIFFERENT flight.Model instance (one the operator has since aborted, popped
	// off the stack) still landing here and being silently adopted as this instance's own
	// result once it eventually completes.
	gen uint64

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
func New(state engine.PromotionState, poll PollDurations, driveFn DriveFunc) Model {
	m := Model{
		state:   state,
		order:   StepOrder,
		poll:    poll,
		driveFn: driveFn,
		spinner: spinner.New(spinner.WithSpinner(spinner.Line)),
		keys:    defaultKeyMap(),
		gen:     nextGen.Add(1),
	}
	if poll.Deadline > 0 {
		// One absolute deadline for this screen's whole drive, from the moment it starts —
		// see driveCmd's own doc comment for why deriving a fresh timeout per poll instead
		// would let the total wait outlive poll.Deadline indefinitely.
		m.deadlineAt = time.Now().Add(poll.Deadline)
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
//
// The call is bounded by m.deadlineAt, not left on context.Background(): without this, a
// single hung network call (a stalled TCP connection to GitHub/Argo with no OS-level timeout)
// would block this goroutine — and therefore this screen's ability to ever show progress or
// let the user act — forever, with no way to cancel. m.deadlineAt is one absolute instant
// computed once in New (poll.Deadline is generous, default 4h, the same value cmd/hoist's own
// driveToCompletion bounds an entire promotion's wait by) — every poll's context shares that
// same instant, bounded by the time remaining until it, rather than each call deriving its own
// fresh poll.Deadline-length timeout from "now". A fresh-per-call timeout would let a promotion
// stuck re-polling CI/approval (each individual wait returns well within the deadline, then
// schedules another call with a brand new full-length timeout) outlive the configured deadline
// indefinitely — cmd/hoist/drive.go's own driveToCompletion enforces exactly one deadline for
// its whole wait (wrapped around ctx once, by its caller, before the retry loop starts), and
// this screen must not silently offer a looser guarantee than the CLI's own (Codex review, PR
// #50).
func (m Model) driveCmd() tea.Cmd {
	driveFn, state, deadlineAt := m.driveFn, m.state, m.deadlineAt
	gen := m.gen
	if driveFn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx := context.Background()
		if !deadlineAt.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, deadlineAt)
			defer cancel()
		}
		next, done, statuses, err := driveFn(ctx, state)
		return driveResultMsg{gen: gen, state: next, done: done, statuses: statuses, err: err}
	}
}

// Update handles the screen's own keys, the spinner, and the drive/tick loop.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case driveResultMsg:
		return m.onDriveResult(msg)
	case tickMsg:
		if m.busy || m.done || m.stopped || m.driveFn == nil {
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

// onDriveResult processes a driveCmd's result — but only if it actually belongs to this Model
// instance. The root's message dispatch (internal/app/app.go's Update, the "forward everything
// else to the top screen" default case) delivers a message to whichever screen is currently on
// top by its concrete Go type alone; it has no notion of which screen instance actually issued
// the tea.Cmd that produced it. A driveCmd already in flight for a flight.Model the operator has
// since aborted (popped off the stack, see AbortMsg's own handling in app.go) can still complete
// later and deliver one more driveResultMsg — which, without the msg.gen check below, a
// different flight.Model now on top (driving a different promotion) would silently adopt as its
// own: the wrong state and step statuses, while continuing to poll and save under THIS model's
// own driveFn/state-file closure (PR #50 review finding #4). msg.gen (stamped by driveCmd from
// m.gen at the point New constructed this instance) is the guard: a result whose gen doesn't
// match is dropped outright, leaving every field — including busy — untouched, since it says
// nothing about whether THIS instance's own driveCmd is still in flight.
func (m Model) onDriveResult(msg driveResultMsg) (Model, tea.Cmd) {
	if msg.gen != m.gen {
		return m, nil
	}
	m.busy = false
	// msg.state is adopted unconditionally, before msg.err is ever classified below — PR #50
	// round-4 review finding #7 (Codex). driveFn (cmd/hoist/wiring.go) always returns however
	// far one drive iteration actually got, even when it ends in error: engine.Drive can
	// create the branch, commit, push and open a PR — each a real, already-persisted change —
	// before a later step's Act then fails (an auto-approved promotion whose merge or branch
	// cleanup errors on its very first iteration, say). Before this fix, the error branch below
	// returned without ever touching m.state, so the screen kept showing whatever it was
	// constructed with (typically empty: no History, no PR) — o reported no PR to open despite
	// one having actually been created, and R re-drove from that same stale copy instead of the
	// real, further-along state driveFn had just handed back and persisted. The success path
	// already did this unconditionally for itself; this just moves it earlier so the error path
	// gets it too, matching how far the underlying promotion has actually progressed regardless
	// of whether this particular poll ended cleanly.
	m.state = msg.state
	// m.done/m.rows are derived unconditionally too, before msg.err is classified — the same
	// reasoning as m.state just above, extended: cmd/hoist/wiring.go's DriveFunc always calls
	// engine.Status after engine.Drive regardless of whether Drive itself errored, so
	// msg.statuses reflects the real, current step-by-step standing even on a failed poll.
	// Before this fix, a failing poll left m.rows showing whatever the PREVIOUS successful
	// poll (or the screen's own construction) had rendered — e.g. a PR already opened before a
	// later step's Act failed would still show "PR: not yet opened" (PR #50 review, round 5).
	m.done = msg.done
	m.rows = DeriveRows(m.order, m.done, msg.statuses)
	if msg.err != nil {
		m.errNotice = redact.Strings(msg.err.Error())
		if !retryableErr(msg.err) {
			// A terminal failure — cmd/hoist/drive.go's own driveToCompletion only retries a
			// *engine.StepError on StepCIGreen/StepApproved (Known bug classes: a transient
			// 404/permissions hiccup on Checks/Comments); every other shape — a rejected push,
			// a failed signing commit, ctx.DeadlineExceeded/Canceled included — is terminal
			// there and returned immediately, never retried. Before this fix, onDriveResult
			// scheduled another poll for literally any non-nil err, so this screen would
			// silently repeat a terminal Act failure every ~2s until poll.Deadline elapsed
			// instead of stopping and surfacing it as a real failure (Codex review, PR #50).
			// R still lets the operator retry by hand (handleKey's own Reobserve case only
			// gates on busy/done, not stopped) — mirroring hoist resume's "re-run to retry"
			// convention for a promotion a killed process left mid-flight.
			m.stopped = true
			return m, nil
		}
		// A retryable error (the CIGreen/Approved transient-hiccup case) must clear m.stopped,
		// not merely leave scheduleTick to fire: if this poll came from a manual R retry after
		// an EARLIER, unrelated terminal stop (R bypasses the stopped gate — see its own
		// comment above), m.stopped was still true from that prior stop, and the automatic
		// tick this call schedules would immediately be suppressed by the same m.stopped gate
		// (line ~229's busy||done||stopped||driveFn==nil check) the moment it fires — silently
		// breaking automatic re-polling from here on, even though this particular error is
		// exactly the transient kind that's supposed to keep retrying on its own (Copilot
		// review, PR #50 round 5).
		m.stopped = false
		return m, m.scheduleTick()
	}
	m.errNotice = ""
	m.stopped = false
	if m.done {
		return m, nil
	}
	if _, blocked := BlockedStep(m.rows); blocked {
		// Blocked is terminal until an operator resolves the underlying conflict out-of-band
		// (a same-name branch already on origin with different content, a CI check that
		// reported failed rather than pending, a rejected approval) — engine.BlockedError's
		// own doc comment: "retrying will not help". cmd/hoist/wiring.go's DriveFunc
		// deliberately never surfaces this as msg.err (Blocked is read from the statuses
		// engine.Status produces, the same way Waiting already is — see DriveFunc's own
		// comment), so msg.err == nil here and the terminal branch above never runs for it.
		// Without this check, this screen would otherwise silently repeat the identical
		// blocked observation and state save every ~2s until poll.Deadline elapsed, exactly
		// the "stuck polling a promotion nothing will unstick" failure the msg.err-driven
		// terminal check above already exists to prevent for a StepError — Blocked just
		// never goes through that path (Codex review, PR #50 round 4). R still lets the
		// operator retry by hand once the conflict is resolved, same as the msg.err terminal
		// case above.
		m.stopped = true
		return m, nil
	}
	return m, m.scheduleTick()
}

// retryableErr mirrors cmd/hoist/drive.go's driveToCompletion classification exactly: only a
// *engine.StepError on StepCIGreen or StepApproved is worth retrying automatically (see
// retryableStep below); every other error shape — including one that doesn't even parse as
// *engine.StepError, such as a bare ctx.DeadlineExceeded/Canceled surfacing straight from
// engine.Drive's own ctx.Err() check — is terminal from this screen's point of view too.
func retryableErr(err error) bool {
	var stepErr *engine.StepError
	return errors.As(err, &stepErr) && retryableStep(stepErr.Step)
}

// retryableStep mirrors cmd/hoist/drive.go's own retryableStep exactly. It is duplicated
// rather than imported for the same reason pollInterval below already is (cmd/hoist is package
// main and cannot be imported from here): CIGreen and Approved are the only two steps whose
// Observe calls out to a forge endpoint that can transiently 404/scope-error without the
// underlying condition (CI status, an approval) actually being answerable yet; every other
// step's error is terminal. A reviewer changing cmd/hoist/drive.go's own retryableStep should
// double-check this copy stays in step with it, exactly as pollInterval's own doc comment
// already asks for that function.
func retryableStep(step engine.StepName) bool {
	return step == engine.StepCIGreen || step == engine.StepApproved
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
	if m.stopped {
		// A Blocked step (BlockedStep's own doc comment) stops polling via m.stopped too, but
		// never sets m.errNotice — its own reason is already shown as the blocked row's
		// Detail, in the step list above, not as a separate notice below. "see error below"
		// would send the operator looking for text that was never written, so this checks
		// for that case specifically rather than assuming every stop came with one.
		if _, blocked := BlockedStep(m.rows); blocked {
			return "blocked: resolve the conflict, then press R to re-observe"
		}
		return "stopped: see error below"
	}
	return ""
}

func (m Model) hint() string {
	return "o open PR · R re-observe · x abort · l log · esc back"
}

// pollInterval mirrors cmd/hoist/drive.go's own pollInterval exactly. It is duplicated
// rather than imported because cmd/hoist is package main and cannot be imported from here;
// the PR report flags this duplication for a reviewer to double-check against
// cmd/hoist/drive.go if that function's own switch ever changes. CI and Approval read the
// PollDurations the caller translated from config.PollConfig at the cmd/hoist boundary, so
// this never hand-copies cmd/hoist's magic numbers itself — only the 2s fallback for every
// other step is a literal, identical to cmd/hoist's own (there is nothing to tune there:
// every other step only ever waits on the interactive signing prompt or a single
// merge/branch-delete retry).
func pollInterval(poll PollDurations, phase engine.StepName) time.Duration {
	const fallback = 2 * time.Second
	switch phase {
	case engine.StepCIGreen:
		if poll.CI <= 0 {
			return fallback
		}
		return poll.CI
	case engine.StepApproved:
		if poll.Approval <= 0 {
			return fallback
		}
		return poll.Approval
	default:
		return fallback
	}
}
