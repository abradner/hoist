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
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// PollDurations is the plain-value slice of internal/config.PollConfig this screen actually
// needs, in place of importing internal/config itself. AGENTS.md §4.8: a screen never imports
// config/registry policy, only the plain values or function types cmd/hoist (the one place
// allowed to know both sides) translates for it. Zero values are valid — New/pollInterval
// already fall back to a fixed default for anything left unset.
type PollDurations struct {
	CI, Approval, Argo, Rollout, Deadline time.Duration
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
	Open, Reobserve, Abort, Log, Back, Override key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Open:      key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		Reobserve: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "re-observe")),
		Abort:     key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "abort")),
		Log:       key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "log")),
		Back:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Override:  key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "treat no checks as green")),
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

// OverrideCINoneMsg is the operator's confirmed answer to the one Blocked reason that has an
// in-band override: CIGreenStep's "no checks reported after the grace period; ci.none=prompt"
// (engine.IsCINonePromptBlock). It asks whatever composes screens to re-drive promotion ID
// with engine.PromotionState.CINoneOverride set — the TUI's `hoist resume <id>
// --override-ci-none` (#103). Emitted only after `c` and a huh.Confirm answered yes, never on
// the keypress alone, and only for the promotion this screen is showing: the override is a
// per-promotion, one-shot operator instruction, never a launch default and never config
// (AGENTS.md §4.5 — a default may not weaken a gate). The root answers it by calling
// ApplyCINoneOverride on the flight screen whose state carries ID.
type OverrideCINoneMsg struct{ ID string }

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
	// ctx and cancel are this screen's one shared drive context, built once in New (nil when
	// driveFn is nil — a read-only screen never calls driveCmd at all) and reused by every
	// driveCmd call this instance ever makes, automatic ticks and manual R retries alike — see
	// driveCmd's own doc comment for why one shared context, not a fresh one per call. cancel is
	// also this screen's external interrupt handle: app.go calls Cancel (below) before popping
	// this screen for AbortMsg or BackMsg, so a driveCmd already in flight is stopped rather than
	// left to keep running — see Cancel's own doc comment.
	ctx    context.Context
	cancel context.CancelFunc
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

	// building is true from NewBuilding until AdoptBuilt lands this screen's first real
	// PromotionState — see NewBuilding's own doc comment for why this screen exists at all
	// before one does. While true, the existing rendering already does most of the work
	// unmodified: m.rows (derived from m.order, which NewBuilding sets from state.Direct the
	// same way a real promotion's would be) render every step pending, exactly the "screenful
	// of not-yet-reached dots" New's own doc comment already describes for the gap before a
	// real promotion's first poll lands — building is only checked where that reuse isn't
	// enough: the spinner's tick chain (Update's spinner.TickMsg case), and actionSection,
	// which has nothing else to say yet.
	building bool
	// buildLog accumulates progress lines received over progressCh since the last real state
	// landed — before a PromotionState exists at all (preflight: claim, in-flight check,
	// fetch, plan, state save, from cmd/hoist's own StartPromotionFunc implementation), and
	// again during drive, between driveResultMsg arrivals (defect B/C: engine.Drive's own
	// save/onWaiting hooks report through the very same callback, so a long single Act — a
	// push, a commit sitting on signing approval — shows up here before the whole Drive call
	// that contains it ever returns). Cleared by onDriveResult the instant a real state
	// lands — from there m.state.History is the authoritative record of everything buildLog
	// was covering for, and repeating those lines would duplicate them. AdoptBuilt
	// deliberately does NOT clear this (its own doc comment): the preflight lines it was
	// showing stay visible until the first drive result actually supersedes them, and the
	// listener that feeds it keeps running past AdoptBuilt for exactly that reason. logView
	// renders state.History first, buildLog after — oldest to newest. Each entry keeps its
	// own arrival time separate from its text (buildLogLine, below) rather than one
	// pre-formatted string: logView needs the "<timestamp>  <text>" shape, but
	// actionSection's own live-status label needs the bare text alone — the timestamp
	// belongs on a log line, not folded into a one-line "still working" indicator next to a
	// spinner.
	buildLog []buildLogLine
	// progressCh is drained one line at a time by listenCmd, which re-issues itself after
	// every receive — a raw channel read inside Update would block the whole program, so this
	// is the standard bubbletea "listen on a channel" shape. nil once the channel's owner
	// (app.go) closes it, or for a screen built with New, which never has one.
	progressCh <-chan string

	styles        ui.Styles
	keys          keyMap
	width, height int
	// now is the clock the header's elapsed and deadline are worded against; a test pins it.
	now func() time.Time
	// log is the History scrollback when l toggles it on.
	log viewport.Model

	// confirming is true while the `c` gesture's dialog is up; confirmOverride is the widget.
	// confirmValue is huh's write target only and is never read to decide anything — Value
	// binds a pointer into the copy of this value-typed model that built the widget, so the
	// answer is read back through the widget's GetValue (confirmAgreed), the shape the tag
	// picker's D gesture settled on after shipping broken twice (AGENTS.md §9 entry 6).
	confirming      bool
	confirmOverride *huh.Confirm
	confirmValue    bool
}

// WithNow fixes the clock (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// New builds the flight screen for a promotion already at least identified (state.ID,
// SourceEnv, TargetEnv — whatever the caller already has, typically fresh off the plan
// screen's "start" flow or engine.LoadState on hoist resume). driveFn is nil in a read-only
// context with nothing to drive: the screen still renders state and never ticks or
// schedules a poll, and R shows a notice instead of calling nil.
func New(state engine.PromotionState, poll PollDurations, driveFn DriveFunc) Model {
	m := Model{
		state:   state,
		order:   OrderFor(state),
		poll:    poll,
		driveFn: driveFn,
		spinner: spinner.New(spinner.WithSpinner(spinner.Line)),
		keys:    defaultKeyMap(),
		gen:     nextGen.Add(1),
		now:     time.Now,
		log:     viewport.New(),
		styles:  ui.NewStyles(true),
		// Visible by default (no stated convention before now — §4.8 proposal, this PR):
		// "what is hoist actually doing" is the operator's question every time a promotion
		// runs, not a fact to go looking for behind a key. layout's own sizing already
		// treats the log as the first thing to shrink on a short terminal (it is handed
		// whatever's left after the fixed sections, clamped to a 3-line minimum, never the
		// other way around), so this does not reopen the §4.8/#164 "blocked reason always
		// visible" guarantee — l still hides it for an operator who wants the room back.
		showLog: true,
	}
	if poll.Deadline > 0 {
		// One absolute deadline for this screen's whole drive, from the moment it starts —
		// see driveCmd's own doc comment for why deriving a fresh timeout per poll instead
		// would let the total wait outlive poll.Deadline indefinitely.
		m.deadlineAt = time.Now().Add(poll.Deadline)
	}
	if driveFn != nil {
		// Built once, here, and reused by every driveCmd call this instance ever makes — see
		// Model.ctx's own doc comment. A read-only screen (driveFn nil) never calls driveCmd
		// and so never needs a cancelable context at all.
		ctx := context.Background()
		if m.deadlineAt.IsZero() {
			m.ctx, m.cancel = context.WithCancel(ctx)
		} else {
			m.ctx, m.cancel = context.WithDeadline(ctx, m.deadlineAt)
		}
	}
	m.rows = DeriveRows(m.order, false, nil) // every step "not yet reached" until the first poll lands
	if driveFn != nil {
		m.busy = true
	}
	return m
}

// ID is the promotion this screen shows — what the root matches an OverrideCINoneMsg against.
func (m Model) ID() string { return m.state.ID }

// NewBuilding starts the flight screen before a PromotionState exists at all — the preflight
// phase between the operator confirming a plan and cmd/hoist's own StartPromotionFunc
// actually producing one (claim, in-flight check, fetch, plan rebuild, initial state save;
// direct mode's fresh-base check too). Without this, app.go had nothing to push until that
// whole call returned, so pressing enter looked like a dead key for however long the
// preflight took — the confirm screen re-rendered unchanged, and a second enter (the natural
// response to a key that looks dead) cancelled the first attempt and started it over, since
// the confirm screen stayed on top and kept receiving keys. Pushing this screen on the
// keypress instead means something visibly changes at once, the confirm screen is no longer
// on top to receive that second enter, and the same screen instance carries through into the
// real drive once AdoptBuilt lands it — no flicker, no second screen.
//
// source/target/direct are already known from the confirmed plan (gitops.Plan, translated by
// app.go — this package still never imports it, AGENTS.md §4.8) and are enough to render a
// sensible header and pick the right step order (OrderFor only needs Direct) before anything
// else exists; every other field of state stays zero-valued, which New already renders
// correctly with a nil driveFn — this constructor is built on top of New for exactly that
// reuse, not a parallel rendering path. progressCh is drained by listenCmd; its lines are
// shown via buildLog until AdoptBuilt clears it.
func NewBuilding(source, target string, direct bool, poll PollDurations, progressCh <-chan string) Model {
	m := New(engine.PromotionState{SourceEnv: source, TargetEnv: target, Direct: direct}, poll, nil)
	m.building = true
	m.busy = true
	m.progressCh = progressCh
	return m
}

// Building reports whether this screen is still in its preflight phase — app.go's
// promotionBuiltMsg handler uses it to decide whether to adopt this screen (AdoptBuilt) or
// fall back to pushing a fresh one (the matrix.ResumeMsg path, which has no building screen
// pre-pushed to adopt into); its flight.BackMsg handler uses it to know whether backing out
// here must also cancel an outstanding build (this screen's own m.cancel is nil throughout
// building — driveFn is nil until AdoptBuilt — so Cancel alone cannot reach it).
func (m Model) Building() bool { return m.building }

// AdoptBuilt transitions this screen from preflight into a real, driving promotion once
// cmd/hoist's StartPromotionFunc call actually returns one — NewBuilding's counterpart.
// poll/deadlineAt are left exactly as NewBuilding already set them (computed the moment the
// operator confirmed, not recomputed here): that is what makes the whole build-and-drive
// share one budget, the same guarantee a single ctx.WithTimeout already gives the CLI path —
// recomputing a fresh window at this point would let the time the build itself took go
// uncounted against it.
//
// building clears (the view stops rendering the preflight spinner/label), but progressCh and
// buildLog do NOT — cmd/hoist's DriveFunc (driveFuncFor) closes over the exact same progress
// callback this screen's preflight lines arrived through, and reuses it for engine.Drive's own
// per-step save/onWaiting hooks (defect B/C): a long single Act — a git push, a commit sitting
// on signing approval — still needs somewhere live to show up before the whole Drive call that
// contains it returns and replaces m.state wholesale. Nil-ing progressCh here (an earlier
// version of this method did) stopped listenCmd's own re-issue chain the moment building went
// false, which silently dropped every drive-phase progress line from that point on — the
// channel stayed open (nothing here ever closes it) but nothing was left reading from it, so
// each send hit the select's own default case and vanished. buildLog itself is cleared by
// onDriveResult instead, the moment a real state lands and m.state.History becomes the
// authoritative record of everything buildLog was covering for — never here, before the first
// one has landed at all.
func (m Model) AdoptBuilt(state engine.PromotionState, driveFn DriveFunc) (Model, tea.Cmd) {
	m.building = false
	m.state = state
	m.order = OrderFor(state)
	m.rows = DeriveRows(m.order, false, nil)
	m.driveFn = driveFn
	if driveFn == nil {
		m.busy = false
		return m, m.listenCmd()
	}
	ctx := context.Background()
	if m.deadlineAt.IsZero() {
		m.ctx, m.cancel = context.WithCancel(ctx)
	} else {
		m.ctx, m.cancel = context.WithDeadline(ctx, m.deadlineAt)
	}
	m.busy = true
	// listenCmd appended, index 2: driveCmd stays at index 1, matching this batch's shape
	// before this field was added — a test or caller that already knows "index 1 is the
	// drive call" (the shape flight.Model's own doc comments have described since New) does
	// not need to change alongside this.
	return m, tea.Batch(m.spinner.Tick, m.driveCmd(), m.listenCmd())
}

// progressMsg carries one line off progressCh, or reports it closed (ok=false) — a distinct,
// explicit case in Update rather than folding "closed" into "nothing more to do", so the
// listen loop stops cleanly instead of spinning on a channel that will only ever return the
// zero value from here on.
type progressMsg struct {
	line string
	ok   bool
}

// buildLogLine is one entry in Model.buildLog: at is stamped once, at arrival (the
// progressMsg handler in Update), never recomputed at render time — logView runs on every
// layout, including a spinner tick, and formatting m.now() there would print a fresh
// timestamp on every frame for a line that already happened.
type buildLogLine struct {
	at   time.Time
	text string
}

// listenCmd reads one value off m.progressCh and returns it as a progressMsg; Update
// re-issues this after every receive, for as long as the channel stays open — the standard
// bubbletea shape for draining a channel without blocking Update itself (a raw <-ch inside
// Update would stall the whole program until a line arrived). Returns nil once progressCh is
// nil, whether because this screen was built with New (no build phase at all) or because a
// prior progressMsg already observed the channel closed and cleared it — bubbletea treats a
// nil tea.Cmd as a no-op, so re-issuing this at the end of that branch is safe.
func (m Model) listenCmd() tea.Cmd {
	ch := m.progressCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		line, ok := <-ch
		return progressMsg{line: line, ok: ok}
	}
}

// Cancel interrupts this screen's shared drive context immediately, rather than waiting for
// m.deadlineAt or for driveFn to notice at its own next Observe/Act that nobody is watching
// anymore. app.go calls this on the current flight screen before popping it for AbortMsg or
// BackMsg (see their own doc comments in app.go): without it, a driveCmd already in flight kept
// running to completion after the operator had already stopped watching it — free to keep
// committing, pushing, opening a PR, or merging, and a later reconfirmation of the same
// deterministic promotion id could then start a second driver racing the first, since the
// original's claim was already released (Copilot review, PR #50 round 11). A nil cancel — a
// read-only screen, driveFn nil, New never builds one — makes this a no-op.
func (m Model) Cancel() {
	if m.cancel != nil {
		m.cancel()
	}
}

// Init starts the spinner and the first poll, but only when there is something to drive —
// a read-only screen (driveFn nil) has nothing to animate or observe, so Init returns nil
// rather than starting a spinner tick chain that would otherwise run forever with nothing
// ever rendering it (PR #39 review finding #5). The first poll runs immediately rather than
// waiting a full pollInterval, so the screen shows real status as soon as it opens instead
// of a screenful of "not yet reached" dots.
//
// A building screen (NewBuilding, driveFn still nil by construction) is the one other case
// that starts the spinner: there is nothing to drive yet, but there is something to animate
// and something to listen for — listenCmd, draining progressCh as preflight lines arrive.
func (m Model) Init() tea.Cmd {
	if m.building {
		return tea.Batch(m.spinner.Tick, m.listenCmd())
	}
	if m.driveFn == nil {
		return nil
	}
	return tea.Batch(m.spinner.Tick, m.driveCmd())
}

// driveCmd runs one DriveFunc call off the Update call stack (AGENTS.md §4.3: it talks to
// git/the forge). state is captured by value at call time, so a concurrent Update never
// races the copy this goroutine reads.
//
// The call is bounded by m.ctx, never context.Background(): without this, a single hung
// network call (a stalled TCP connection to GitHub/Argo with no OS-level timeout) would block
// this goroutine — and therefore this screen's ability to ever show progress or let the user
// act — forever, with no way to cancel. m.ctx is built once in New from m.deadlineAt (poll.Deadline
// is generous, default 4h, the same value cmd/hoist's own driveToCompletion bounds an entire
// promotion's wait by) and reused by every call this instance ever makes — automatic ticks and
// manual R retries alike — rather than each deriving its own fresh poll.Deadline-length timeout
// from "now". A fresh-per-call timeout would let a promotion stuck re-polling CI/approval (each
// individual wait returns well within the deadline, then schedules another call with a brand new
// full-length timeout) outlive the configured deadline indefinitely — cmd/hoist/drive.go's own
// driveToCompletion enforces exactly one deadline for its whole wait (wrapped around ctx once, by
// its caller, before the retry loop starts), and this screen must not silently offer a looser
// guarantee than the CLI's own (Codex review, PR #50). Reusing one context rather than deriving a
// fresh one per call is also what makes Cancel (above) actually able to interrupt a call already
// in flight, not just whichever one happens to be constructed next.
func (m Model) driveCmd() tea.Cmd {
	driveFn, state, ctx := m.driveFn, m.state, m.ctx
	gen := m.gen
	if driveFn == nil {
		return nil
	}
	return func() tea.Msg {
		next, done, statuses, err := driveFn(ctx, state)
		return driveResultMsg{gen: gen, state: next, done: done, statuses: statuses, err: err}
	}
}

// Update handles the screen's own keys, the spinner, and the drive/tick loop.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case driveResultMsg:
		return m.onDriveResult(msg)
	case progressMsg:
		if !msg.ok {
			// The channel closed — app.go's build goroutine returned (successfully or not)
			// and drained nothing more into it. Stop listening; AdoptBuilt (success) or the
			// root popping this screen (failure) is what happens next, neither of which this
			// screen drives itself.
			m.progressCh = nil
			return m, nil
		}
		m.buildLog = append(m.buildLog, buildLogLine{at: m.now(), text: msg.line})
		return m, m.listenCmd()
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
		// building is the one case busy alone doesn't already cover: NewBuilding sets busy
		// true with driveFn still nil (nothing to drive yet), so the plain guard below would
		// stop the spinner on its very first tick — building keeps it alive until AdoptBuilt.
		if !m.building && (!m.busy || m.done || m.driveFn == nil) {
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
	// buildLog is cleared HERE, not in AdoptBuilt (see that method's own doc comment): a real
	// state has just landed, so m.state.History is now the authoritative record of everything
	// buildLog was covering for since the last one. Unconditional, error included, for the same
	// reason m.state itself is adopted unconditionally just above — engine.Drive still appends
	// to History (and this screen's own progress callback still reports it) right up to the
	// step whose Act actually failed.
	m.buildLog = nil
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
		var stepErr *engine.StepError
		if errors.As(msg.err, &stepErr) {
			return m, m.scheduleTickAfter(stepErr.Step)
		}
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
	if !errors.As(err, &stepErr) || !retryableStep(stepErr.Step) {
		return false
	}
	// The same sentinel exclusion cmd/hoist/drive.go applies, and for the same reason: a
	// missing Application or Deployment is a structural condition no amount of waiting
	// resolves. Observe reports it as Blocked, but Act has no way to produce a BlockedError of
	// its own, so an Act that races a deletion after a successful Observe surfaces the sentinel
	// as an ordinary error — which, once the cluster steps became retryable, this screen would
	// have retried until the deadline instead of stopping (Copilot, PR #72).
	return !isNotFoundErr(stepErr.Err)
}

// isNotFoundErr mirrors cmd/hoist/drive.go's own, duplicated for the same reason retryableStep
// is: cmd/hoist is package main and cannot be imported from here.
func isNotFoundErr(err error) bool {
	return errors.Is(err, argo.ErrNotFound) || errors.Is(err, rollout.ErrNotFound)
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
	switch step {
	case engine.StepCIGreen, engine.StepApproved:
		// The two forge-polling steps: Checks and Comments can transiently 404 or
		// scope-error without the underlying condition being answerable yet.
		return true
	case engine.StepArgoRefreshed, engine.StepArgoSynced, engine.StepRolledOut:
		// The three cluster-polling steps, for exactly the same reason: a Kubernetes Get can
		// fail transiently (an API server restart, a dropped connection) while the sync or
		// rollout it is asking about is still perfectly well under way. Before the flight
		// screen drove these, such an error could only reach the CLI's own loop, which
		// retries; reaching this classifier instead used to stop the flight dead on a hiccup
		// (issue #64). A genuinely missing Application is a Blocked observation, not an
		// error, so it is unaffected by this.
		return true
	default:
		return false
	}
}

// renewDeadline rebuilds the drive context for another poll.Deadline from now (or an
// uncancelled one when no deadline is configured), cancelling the exhausted one.
func (m Model) renewDeadline() Model {
	if m.cancel != nil {
		m.cancel()
	}
	if m.poll.Deadline > 0 {
		m.deadlineAt = m.now().Add(m.poll.Deadline)
		m.ctx, m.cancel = context.WithDeadline(context.Background(), m.deadlineAt)
	} else {
		m.deadlineAt = time.Time{}
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}
	return m
}

// scheduleTick waits pollInterval's answer for whichever step is currently active before
// firing the next poll (AGENTS.md invariant 4: the actual waiting lives in the caller's own
// loop, never inside a Step's Act — this is that loop's TUI-driven twin).
func (m Model) scheduleTick() tea.Cmd {
	return tea.Tick(m.tickDelay(""), func(time.Time) tea.Msg { return tickMsg{} })
}

// scheduleTickAfter is scheduleTick for a retry after a Status error: Status returns only the
// rows before the step whose Observe failed, so ActiveStep finds nothing and the interval
// would fall back to the last step's — 2s against a 30s approval poll (#61). The failing
// step is known from the error itself, and names the cadence.
func (m Model) scheduleTickAfter(failed engine.StepName) tea.Cmd {
	return tea.Tick(m.tickDelay(failed), func(time.Time) tea.Msg { return tickMsg{} })
}

// tickDelay is the wait before the next poll: the configured interval for the active step
// (or for failed, when given), capped at what is left of the deadline (#59) — a 1s deadline
// with a 20s CI interval used to report 19s late. Never below zero; a passed deadline polls
// at once so the deadline error surfaces instead of a sleep hiding it.
func (m Model) tickDelay(failed engine.StepName) time.Duration {
	phase := failed
	if phase == "" {
		if active, ok := ActiveStep(m.rows); ok {
			phase = active
		} else if len(m.order) > 0 {
			phase = m.order[len(m.order)-1]
		}
	}
	d := pollInterval(m.poll, phase)
	if !m.deadlineAt.IsZero() {
		if left := m.deadlineAt.Sub(m.now()); left < d {
			d = max(left, 0)
		}
	}
	return d
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	if m.confirming {
		// Esc leaves the dialog without answering it (the tag picker's round-3 finding:
		// huh's own Update swallows Esc, trapping the operator); everything else is the
		// widget's, until enter reads its answer.
		if key.Matches(msg, m.keys.Back) {
			m.confirming = false
			return m, nil
		}
		return m.updateConfirm(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Override):
		if !m.offersCINoneOverride() {
			// Any other block (a failed check, ci.none=block, a branch conflict) has no
			// in-band override; the key does nothing rather than open a dialog whose yes
			// would change nothing (AGENTS.md §2 principle 1: an offer that cannot deliver
			// is a bug).
			return m, nil
		}
		return m.openConfirm()
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
		if m.ctx != nil && m.ctx.Err() != nil {
			// The shared drive context was built once from the screen's deadline; once that
			// has passed, a retry on it fails before it starts. R is the operator asking for
			// another go, so it gets another window of poll.Deadline — the decision #57
			// asked for, taken this way because "R does nothing" was the other option.
			m = m.renewDeadline()
			m.notice = "deadline had passed — a fresh window for this retry"
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
	if m.showLog {
		// The log is a viewport: unmatched keys (↑/↓, PageUp/PageDown, g/G) scroll it. Without
		// this, l showed the first page of a long history and nothing moved it. Laid out on
		// this copy first — View lays out its own copy, so the retained viewport would
		// otherwise be the zero-sized one New built (Copilot, #124).
		m = m.layout()
		var cmd tea.Cmd
		m.log, cmd = m.log.Update(msg)
		return m, cmd
	}
	return m, nil
}

// offersCINoneOverride is true exactly when `c` has something to do: the drive stopped on a
// Blocked CI step whose reason is the ci.none=prompt one (engine.IsCINonePromptBlock) — the
// single Blocked reason with an override. A failed or skipped check, ci.none=block, or any
// other step's block never qualifies, so the offer, the hint and the key all agree.
func (m Model) offersCINoneOverride() bool {
	if m.done || !m.stopped {
		return false
	}
	for _, r := range m.rows {
		if r.Glyph == GlyphBlocked {
			return r.Step == engine.StepCIGreen && engine.IsCINonePromptBlock(r.Detail)
		}
	}
	return false
}

// openConfirm raises the `c` gesture's dialog — the tag picker's D shape: keypress, then a
// huh.Confirm, and only a yes emits anything.
func (m Model) openConfirm() (Model, tea.Cmd) {
	m.confirming = true
	m.confirmValue = false
	title := fmt.Sprintf("Treat this PR's missing checks as green and let %s merge on approval alone? ci.none is prompt; this applies to promotion %s only.", m.state.TargetEnv, m.state.ID)
	m.confirmOverride = huh.NewConfirm().Title(title).Value(&m.confirmValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6).
	m.confirmOverride.WithKeyMap(huh.NewDefaultKeyMap())
	m.confirmOverride.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmOverride.WithWidth(m.dialogWidth())
	return m, tea.Batch(m.confirmOverride.Init(), m.confirmOverride.Focus())
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirming = false
		if !m.confirmAgreed() {
			return m, nil
		}
		id := m.state.ID
		return m, func() tea.Msg { return OverrideCINoneMsg{ID: id} }
	}
	f, cmd := m.confirmOverride.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmOverride = c
	}
	return m, cmd
}

// confirmAgreed reads the operator's answer from the widget, never from confirmValue — see
// the field's own comment.
func (m Model) confirmAgreed() bool {
	if m.confirmOverride == nil {
		return false
	}
	v, _ := m.confirmOverride.GetValue().(bool)
	return v
}

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

// ApplyCINoneOverride is what the root calls in answer to OverrideCINoneMsg: it sets
// CINoneOverride on this screen's own copy of the state — the state every driveCmd hands to
// DriveFunc, so the next engine.Drive's CIGreenStep.Observe reads it (and the drive's own
// save persists it, exactly as `hoist resume --override-ci-none` does) — and re-drives at
// once, the way R does. Only this screen's promotion is touched: no other state, file or
// screen sees the flag. A read-only screen (driveFn nil) can record the wish but not act on
// it, and says so. A busy or finished screen refuses before it records: the flag is only
// ever acted on by the re-drive this method schedules, so recording it on a screen that
// schedules none would carry an override no engine step ever reads — and the next R would
// then apply it silently, without the c gesture that is meant to be the operator's decision.
func (m Model) ApplyCINoneOverride() (Model, tea.Cmd) {
	if m.driveFn == nil {
		m.state.CINoneOverride = true
		m.notice = "override recorded, but nothing is driving this promotion here (read-only) — run `hoist resume " + m.state.ID + " --override-ci-none`"
		return m, nil
	}
	if m.busy || m.done {
		m.notice = "override not applied: this promotion is still being driven"
		if m.done {
			m.notice = "override not applied: this promotion is finished"
		}
		return m, nil
	}
	m.state.CINoneOverride = true
	m.stopped = false
	if m.ctx != nil && m.ctx.Err() != nil {
		m = m.renewDeadline()
	}
	m.notice = "treating no checks as green for this promotion — re-observing"
	m.busy = true
	return m, tea.Batch(m.driveCmd(), m.spinner.Tick)
}

// SetSize records the terminal size. The log is a viewport sized to what the frame leaves
// (layout) and scrolls with the unmatched keys handleKey forwards; the step list has no
// scrolling and degrades to the one-line strip on a short terminal (stepsSection).
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	if m.confirmOverride != nil {
		m.confirmOverride.WithWidth(m.dialogWidth())
	}
	return m.layout()
}

// layout sizes the log viewport to what the frame leaves after the fixed sections.
func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	fixed := lipgloss.Height(m.headerSection()) + lipgloss.Height(m.stepsSection())
	sections := 2
	if a := m.actionSection(); a != "" {
		fixed += lipgloss.Height(a)
		sections++
	}
	if n := m.notes(); n != "" {
		fixed += lipgloss.Height(n)
		sections++
	}
	if m.showLog {
		sections++
		fixed++ // the "history" label above the log
	}
	m.log.SetWidth(m.width - 2)
	m.log.SetHeight(max(ui.BodyHeight(m.height, sections)-fixed, 3))
	m.log.SetContent(m.logView())
	return m
}

// CapturesText reports whether the c gesture's dialog is up: while it is, the root must
// hand every key to this screen rather than treat q as quit, or an operator deciding
// whether to treat no checks as green can quit the program mid-decision (Arc 2 review,
// the same gap the tag picker's D dialog closes through tags.Model.CapturesText).
func (m Model) CapturesText() bool { return m.confirming }

// SetStyles applies the palette (and re-themes the `c` dialog when one is up).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	if m.confirmOverride != nil {
		m.confirmOverride.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	return m
}

// View renders the frame: the header (what and how long), the step list, what it is waiting
// for and what to type, the log when toggled, notices, and the footer. The whole assembled
// string passes through redact.Strings once here at the final boundary, matching
// plan.Model's own belt-and-suspenders convention. This is not defense in depth on top of an
// earlier redaction: engine.Status hands Row.Detail over unredacted (see rows.go's Row.Detail
// comment for why appendHistory's redaction does not apply to this path) — this call is the
// one place that text is actually scrubbed before reaching the terminal.
func (m Model) View() string {
	m = m.layout()
	sections := []string{m.headerSection(), m.stepsSection()}
	if a := m.actionSection(); a != "" {
		sections = append(sections, a)
	}
	// notes (the transient notice, the last plumbing error) comes BEFORE the log, not after —
	// ui.Frame.Render crops from the bottom when a short terminal can't hold everything
	// (#164's own lesson, AGENTS.md §9 entry 10: a notice appended after a full-height frame
	// is a notice nobody reads). layout's own log-height floor of 3 lines is unconditional —
	// it does not shrink to 0 even when there is no room at all — so on a terminal short
	// enough that header+steps+action+notes alone nearly fill it, the log's floor can still
	// push the total past height. Ordering the log last means THAT is what gets cropped, never
	// the notice — the log already accepts being shrunk to its floor; the notice never should
	// be.
	if n := m.notes(); n != "" {
		sections = append(sections, n)
	}
	if m.showLog {
		sections = append(sections, m.styles.Dim.Render("history")+"\n"+m.log.View())
	}
	out := ui.Frame{Title: m.title(), Sections: sections, Footer: ui.StatusBar(m.width, m.styles.Status.Render(m.statusLeft()), m.styles.Hint.Render(m.hint()))}.Render(m.styles, m.width, m.height)
	if m.confirming && m.confirmOverride != nil {
		out = ui.Dialog(m.styles, out, "treat no checks as green", m.confirmOverride.View(), m.width, m.height)
	}
	return redact.Strings(out)
}

func (m Model) title() string {
	if m.state.SourceEnv == "" {
		return "hoist · deploy · in flight"
	}
	return "hoist · promotion · in flight"
}

// headerSection is the id, the envs, how long it has run and how long it has left.
func (m Model) headerSection() string {
	// A deploy has no source env, so the promotion's "A → B" would render with a hole where
	// the source belongs — the state's own empty SourceEnv is what distinguishes them, the
	// same discriminator internal/engine/template.go and cmd/hoist's success line use.
	pair := m.styles.Title.Render("deploy → " + m.state.TargetEnv)
	if m.state.SourceEnv != "" {
		pair = m.styles.Title.Render(m.state.SourceEnv + " → " + m.state.TargetEnv)
	}
	left := m.styles.Accent.Render(m.state.ID) + "   " + pair
	if m.state.Direct {
		left += "   " + m.styles.Warn.Render("direct")
	}
	var right []string
	if start := StartedAt(m.state); !start.IsZero() {
		right = append(right, "started "+ui.Ago(m.now(), start))
	}
	if !m.deadlineAt.IsZero() && !m.done {
		right = append(right, "deadline "+ui.Until(m.now(), m.deadlineAt))
	}
	return ui.StatusBar(max(m.width-2, 1), left, m.styles.Dim.Render(strings.Join(right, " · ")))
}

// stepList renders one line per step (glyph + label) with a second, indented detail line
// for whichever row is Active — e.g. "CI 2/3 complete". A fully done promotion has no active
// row (DeriveRows never sets Active when done — every row is already Done), but its own last
// row still carries a real Detail (engine.Status's short-circuited final-step Observation,
// e.g. "merged as <sha>; branch deleted"), so that one row's detail is shown too.
func (m Model) stepList() string {
	var b strings.Builder
	last := len(m.rows) - 1
	for i, r := range m.rows {
		marker := r.Glyph
		if r.Active && m.busy {
			marker = m.spinner.View()
		}
		line := marker + " " + Label(r.Step)
		switch r.Glyph {
		case GlyphDone:
			line = m.styles.Good.Render(line)
		case GlyphBlocked:
			line = m.styles.Bad.Render(line)
		case GlyphActive, GlyphWaiting:
			line = m.styles.Warn.Render(line)
		default:
			line = m.styles.Dim.Render(line)
		}
		b.WriteString(line + "\n")
		if r.Detail != "" && (r.Active || (m.done && i == last)) {
			b.WriteString("    " + ansi.Wrap(r.Detail, max(m.width-6, 20), "") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepsSection is the step list when the terminal has the rows for it, else the one-line
// strip the matrix's in-flight pane draws — degrade by dropping evidence, never the verdict:
// on a short terminal the notice and the action still fit, and every step is still named.
func (m Model) stepsSection() string {
	list := m.stepList()
	other := lipgloss.Height(m.headerSection()) + lipgloss.Height(m.actionSection()) + lipgloss.Height(m.notes()) + 4
	if lipgloss.Height(list)+other <= m.height {
		return list
	}
	sum := Summary{ID: m.state.ID, PR: m.state.PR, Rows: m.rows, Done: m.done}
	strip := sum.StepStrip()
	if ansi.StringWidth(strip) > m.width-2 {
		// Pack whole steps onto lines no wider than the frame's interior: a fixed break
		// after the merge still overflowed on terminals narrower than the first half.
		var lines []string
		line := ""
		for _, p := range strings.Split(strip, "  ") {
			switch {
			case line == "":
				line = p
			case ansi.StringWidth(line)+2+ansi.StringWidth(p) <= m.width-2:
				line += "  " + p
			default:
				lines = append(lines, line)
				line = p
			}
		}
		strip = strings.Join(append(lines, line), "\n")
	}
	if step, ok := ActiveStep(m.rows); ok {
		for _, r := range m.rows {
			if r.Step == step && r.Detail != "" {
				strip += "\n" + ansi.Truncate("    "+r.Detail, max(m.width-2, 20), "…")
			}
		}
	}
	return strip
}

// actionSection is what the promotion is waiting for and what the operator can do about it
// — the same words the matrix's in-flight pane uses (Summary), so the two never disagree.
// Empty when there is nothing to say beyond the step list itself.
func (m Model) actionSection() string {
	if m.building {
		// The one thing this section says during preflight: it's alive, and — via the same
		// spinner.View() the step list uses for an Active row — what it's doing right now,
		// the last line buildLog received. Before the first line arrives there is nothing
		// truer to say than "starting".
		label := "starting"
		if n := len(m.buildLog); n > 0 {
			label = m.buildLog[n-1].text
		}
		return m.styles.Warn.Render(m.spinner.View() + " " + label)
	}
	sum := Summary{ID: m.state.ID, Source: m.state.SourceEnv, Target: m.state.TargetEnv, Direct: m.state.Direct, PR: m.state.PR, Rows: m.rows, Done: m.done}
	text, command := sum.Action()
	switch {
	case m.done:
		return m.styles.Good.Render("done")
	case command != "":
		return m.styles.Warn.Render(text) + "\n\n    " + m.styles.Accent.Render(command)
	case m.stopped:
		if _, blocked := BlockedStep(m.rows); blocked {
			// The step's own reason first — a CI policy block, a failed check, a missing
			// Application — and the generic advice only where there is none.
			if text != "" {
				out := m.styles.Bad.Render(ansi.Wrap("blocked — "+text, max(m.width-2, 20), ""))
				if m.offersCINoneOverride() {
					// The one block with an in-band answer: the CLI's own re-run command is
					// already in the reason text; this is the TUI's equivalent (#103).
					return out + "\n" + m.styles.Accent.Render("press c to treat no checks as green for this promotion") + "\n" + m.styles.Dim.Render("R to re-observe once resolved")
				}
				return out + "\n" + m.styles.Dim.Render("R to re-observe once resolved")
			}
			return m.styles.Bad.Render("blocked — resolve the conflict, then R to re-observe")
		}
		return m.styles.Bad.Render("stopped — see the error below; R retries")
	}
	return ""
}

// logView renders state.History first, then buildLog — chronological order, oldest to
// newest: state.History is the settled record up to the last driveResultMsg (or, before the
// first one has landed at all, empty); buildLog is whatever has arrived since — preflight
// lines before a PromotionState exists, or a step mid-Act during drive that hasn't reached its
// own appendHistory yet — so it always describes what's NEWER than state.History, never what's
// already in it (onDriveResult's own doc comment: buildLog is cleared the instant a real state
// lands, precisely so the two never describe the same line twice).
func (m Model) logView() string {
	if len(m.buildLog) == 0 && len(m.state.History) == 0 {
		return m.styles.Dim.Render("(no history yet)")
	}
	var b strings.Builder
	for _, h := range m.state.History {
		fmt.Fprintf(&b, "%s  %-12s  %s\n", h.At.Format(time.RFC3339), h.Step, h.Detail)
	}
	for _, line := range m.buildLog {
		fmt.Fprintf(&b, "%s  %s\n", line.at.Format(time.RFC3339), line.text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// notes is the transient notice and the last plumbing error, word-wrapped.
func (m Model) notes() string {
	var lines []string
	if m.notice != "" {
		lines = append(lines, m.styles.Notice.Render(ansi.Wrap(m.notice, max(m.width-2, 20), "")))
	}
	if m.errNotice != "" {
		lines = append(lines, m.styles.Bad.Render(ansi.Wrap(m.errNotice, max(m.width-2, 20), "")))
	}
	return strings.Join(lines, "\n")
}

func (m Model) statusLeft() string {
	if m.done {
		return "promotion complete"
	}
	if m.stopped {
		// A Blocked step (BlockedStep's own doc comment) stops polling via m.stopped too, but
		// never sets m.errNotice — its own reason is already shown as the blocked row's
		// Detail, in the step list above, not as a separate notice below.
		if _, blocked := BlockedStep(m.rows); blocked {
			if m.offersCINoneOverride() {
				return "blocked: no checks reported; c treats them as green"
			}
			return "blocked: resolve the conflict, then press R to re-observe"
		}
		return "stopped: see error below"
	}
	return ""
}

func (m Model) hint() string {
	h := "R re-observe · x abort · l log · esc back"
	if m.offersCINoneOverride() {
		h = "c treat as green · " + h
	}
	if _, ok := PRURL(m.state); ok {
		h = "o open PR · " + h
	}
	return h
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
	case engine.StepArgoRefreshed, engine.StepArgoSynced:
		if poll.Argo <= 0 {
			return fallback
		}
		return poll.Argo
	case engine.StepRolledOut:
		if poll.Rollout <= 0 {
			return fallback
		}
		return poll.Rollout
	default:
		return fallback
	}
}
