package session

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
)

// Driver is what Controller drives one entry's promotion through — internal/service.Drive's own
// consumer-side interface, narrowed to exactly what a poll needs.
// *service.Driver, returned by Backend.StartPromotion/Resume as a service.Drive, satisfies it
// directly (a superset interface always does); a test fakes it without building a real
// service.Driver.
type Driver interface {
	Step(ctx context.Context) (service.Tick, error)
	State() engine.PromotionState
	OverrideCINone()
}

// Backend is the subset of internal/app.Service (and so of internal/service.Service) Controller
// actually calls — narrow and consumer-side for the same reason app.Service already is: a test
// fakes it with a plain struct, never a real Service backed by git/forge/Argo/the cluster.
type Backend interface {
	StartPromotion(ctx context.Context, req service.StartRequest, h service.Hooks) (service.Drive, error)
	Resume(ctx context.Context, id string, o service.ResumeOpts) (service.Drive, error)
	Abandon(ctx context.Context, id string) ([]string, error)
	List(ctx context.Context, o service.ListOpts) ([]service.Listed, error)
}

// BuildID names one Start/Resume attempt Controller is tracking, stable for that attempt's whole
// life (through Building, Stepping, Waiting, Stopped, Abandoning) even before its real promotion
// id is known — StartPromotion's own preflight (the claim, the in-flight scan, the initial save)
// has to finish before engine.DeriveID's answer comes back at all. Comparable and cheap to carry
// in a message, the way flight.Model's own gen already is.
type BuildID uint64

// Phase is one entry's own coarse status, independent of the step-by-step engine.StepStatus rows
// Snapshot.Statuses carries.
type Phase int

const (
	// Building is the preflight window before a real PromotionState exists — StartPromotion or
	// Resume is still running.
	Building Phase = iota
	// Stepping is a Driver.Step call actually in flight.
	Stepping
	// Waiting is between polls — the last Step reported Tick.Waiting and the next pollMsg has
	// not fired yet.
	Waiting
	// Stopped is a Blocked step or a terminal (non-retryable) error — R (Poke) can re-arm it.
	Stopped
	// Done means the promotion finished; the entry is removed the same Update call this phase
	// would otherwise be observed in (see Update's own stepMsg case).
	Done
	// Abandoning is between an Abandon call and Backend.Abandon actually running — waiting for
	// a busy Step to notice its context was cancelled and return.
	Abandoning
)

// String names a Phase for logging/rendering — never used to decide anything here.
func (p Phase) String() string {
	switch p {
	case Building:
		return "building"
	case Stepping:
		return "stepping"
	case Waiting:
		return "waiting"
	case Stopped:
		return "stopped"
	case Done:
		return "done"
	case Abandoning:
		return "abandoning"
	default:
		return "unknown"
	}
}

// LogLine is one progress line Controller has seen for a build, oldest first — the Snapshot's
// own equivalent of flight.Model's buildLog, now owned here so a mirroring screen (the flight screen)
// never has to keep its own copy in sync with a channel it does not own.
type LogLine struct {
	At   time.Time
	Text string
}

// Config is Controller's own tuning knobs. New applies usable defaults to every zero field
// (normalize), so a caller only sets what it actually wants to change — and every test sets Now
// and After so nothing here ever sleeps or races real wall-clock time.
type Config struct {
	// Deadline bounds one build+drive's whole ctx, shared between the StartPromotion/Resume call
	// and every Step call that follows it (one ctx per drive — the time the build
	// itself took counts against the same budget the drive polls against, never a fresh window
	// per Step call, mirroring flight.Model's own deadlineAt one layer up).
	Deadline time.Duration
	// ListEvery is the in-flight listing's own poll cadence (mirrors app.go's old inFlightTick).
	ListEvery time.Duration
	// MinTick floors a Step's own requested wait (service.Tick.Wait) — the same floor
	// flight.Model.minTick already applies, one layer down.
	MinTick time.Duration
	// AbandonWait is the interval between abandon-wait rechecks (mirrors app.go's own
	// abandonWaitInterval).
	AbandonWait time.Duration
	// AbandonTimeout bounds the whole abandon wait for a busy Step to notice its ctx was
	// cancelled (mirrors app.go's own abandonWaitMaxAttempts, expressed as a duration since
	// Config carries durations, not attempt counts) — it does NOT bound Backend.Abandon itself;
	// see AbandonCallTimeout for that. A short value here just means the wait gives up sooner
	// and calls Backend.Abandon anyway (re-observation makes that safe — see Abandon's own doc
	// comment), it never shortens the call itself.
	AbandonTimeout time.Duration
	// AbandonCallTimeout bounds one Backend.Abandon call — a real re-observation (ObserveAll:
	// git ls-remote plus forge PR/check lookups) followed by ClosePR and a DeleteRemoteBranch
	// push, none of which is instant. This is deliberately generous and independent of
	// AbandonTimeout/AbandonWait, which only bound the short busy-Step wait that precedes the
	// call: conflating the two would give a real abandon a 1s deadline and routinely fail
	// it with "context deadline exceeded", sometimes after ClosePR had already run but before
	// DeleteRemoteBranch had a chance to.
	AbandonCallTimeout time.Duration
	// ListTimeout bounds one Backend.List call. Kept equal to internal/app/scope.List (60s, P3
	// #14) — that constant's own doc comment is what actually documents the value;
	// this default exists so a caller that builds a Config directly (every test) still gets it.
	ListTimeout time.Duration
	// Now is the clock. Defaults to time.Now; every test injects a fixed one.
	Now func() time.Time
	// After schedules a message after d — tea.Tick by default. Tests inject a version that
	// fires on request (or immediately) so nothing here ever actually sleeps.
	After func(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd
}

func (c Config) normalize() Config {
	if c.Deadline <= 0 {
		c.Deadline = 4 * time.Hour
	}
	if c.ListEvery <= 0 {
		c.ListEvery = 30 * time.Second
	}
	if c.MinTick <= 0 {
		c.MinTick = 2 * time.Second
	}
	if c.AbandonWait <= 0 {
		c.AbandonWait = 100 * time.Millisecond
	}
	if c.AbandonTimeout <= 0 {
		c.AbandonTimeout = time.Second
	}
	if c.AbandonCallTimeout <= 0 {
		c.AbandonCallTimeout = 60 * time.Second
	}
	if c.ListTimeout <= 0 {
		c.ListTimeout = 60 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = tea.Tick
	}
	return c
}

// ChangeKind is what kind of thing happened to one entry (or to the listing) during an Update
// call — prefixed Change- to avoid colliding with Phase's own bare names (Done in particular)
// in this same package.
type ChangeKind int

// The ChangeKind values a Change can carry — see Change's own doc comment for what each means
// to a caller.
const (
	ChangeBuilt ChangeKind = iota
	ChangeBuildFailed
	ChangeStepped
	ChangeLanded
	ChangeDone
	ChangeBlocked
	ChangeFailed
	ChangeAbandoned
	ChangeAbandonFailed
	ChangeListed
	ChangeRefused
	// ChangeProgress: a preflight line or a live History report landed on the entry; the screen
	// showing it should re-mirror the snapshot, and nothing else about the entry changed.
	ChangeProgress
)

// Change is one thing Update decided happened, for the caller (the wiring PR's own root Update)
// to react to — re-key a building screen to its real id, mirror a snapshot onto the flight
// screen showing it, refresh the matrix's in-flight pane, add an activity-log entry. Zero or
// more are returned from a single Update call; most produce exactly one.
type Change struct {
	Kind  ChangeKind
	Build BuildID
	ID    string
	Err   error
	List  []service.Listed
	Lines []string
	// Snap is the entry's own last snapshot, populated only for a Change whose entry Update
	// removes from Controller in the very same call (ChangeDone, ChangeAbandoned) — every other
	// kind leaves the entry live, so a caller can always get a fresher one straight from
	// Controller.Snapshot/BuildSnapshot instead. Without this, a mirroring screen (the flight
	// screen) would have nothing left to mirror the instant a promotion
	// finishes: BuildSnapshot(build) already returns false by the time the Change reaches it,
	// since withoutEntry has already run, and the screen would be frozen one step short of the
	// true final state forever.
	Snap Snapshot
}

// Errors Start/Resume/Poke/OverrideCINone can refuse with — never panics, mirroring every other
// nil-safe convention in internal/app (a caller degrades to a notice).
var (
	// ErrNoBackend is returned when Controller was built with a nil Backend — the same
	// "feature not wired" convention app.Model.svc == nil already uses.
	ErrNoBackend = errors.New("session: no backend configured")
	// ErrNotFound is returned when an operation names a promotion id Controller is not
	// tracking.
	ErrNotFound = errors.New("session: no such promotion")
	// ErrBusy is returned when Poke/OverrideCINone is asked for while a Step is already in
	// flight for that entry, or while it is Abandoning.
	ErrBusy = errors.New("session: this promotion is busy")
	// ErrTargetBusy is returned by Start when another entry is already tracked for the same
	// target env — AGENTS.md invariant 5, enforced here at the same layer service.claimTarget
	// enforces it against the forge/state file.
	ErrTargetBusy = errors.New("session: a promotion is already running for this target env")
	// ErrNoDriver is returned by Poke/OverrideCINone when the tracked entry has no live Driver
	// to step — a Resume whose Backend.Resume call itself failed (Building never reached
	// onBuilt's success path) and was then abandoned, or any other entry that reached Stopped
	// without ever getting a driver. Poke/OverrideCINone would otherwise call Step on a nil
	// interface value and panic (onAbandoned's error path could otherwise leave exactly
	// such an entry behind, driver-less and Poke-able). Enforced twice, deliberately (AGENTS.md
	// §8's layered-checks rule): onAbandoned also refuses to leave a driver-less entry behind at
	// all (see its own doc comment) — deleting either check would make a new state possible
	// (Poke reaching a nil driver), so both are real enforcement, not one enforcement plus
	// politeness.
	ErrNoDriver = errors.New("session: this promotion has no driver to step")
)

// entry is one tracked build/promotion. Never exported: Snapshot is the read-only view a caller
// gets instead.
type entry struct {
	build          BuildID
	id             string
	phase          Phase
	source, target string
	direct         bool

	state    engine.PromotionState
	statuses []engine.StepStatus
	done     bool
	blocked  *engine.BlockedError
	err      error
	retry    bool

	busy bool
	// gen is this entry's own generation, bumped every time it is re-armed (Poke, an override,
	// a fresh ctx) — any stepMsg/pollMsg/builtMsg/abandonWaitMsg carrying an older gen is
	// dropped by Update, the same shape flight.Model.gen already used one layer up.
	gen uint64

	ctx        context.Context
	cancel     context.CancelFunc
	deadlineAt time.Time
	nextPoll   time.Time

	driver Driver
	// landed is set the first time state.LandedSHA() != "" is observed — ChangeLanded is
	// emitted exactly once per entry, on the transition, never again for the same entry even
	// though LandedSHA() stays non-empty on every later poll too.
	landed bool

	log        []LogLine
	progressCh chan progressItem

	abandoning bool
	// abandonIssued is set the moment Backend.Abandon is actually dispatched for this entry —
	// from Abandon's own immediate (!busy) path, from onAbandonWait's timeout branch, or from
	// onBuilt's own abandoning branch below — and is the one thing every call site checks before
	// dispatching a second one. Abandon can be asked for while a Step (Abandon) or a build
	// (Resume/Start) is still in flight; either one can independently decide "time to actually
	// call Abandon now", and gen alone cannot arbitrate between them: a gen bump only takes
	// effect once its own case handler returns, which is too late for a case that already
	// dispatched abandonCmd before the other one's message was even delivered.
	abandonIssued bool
}

// Controller is the value type that owns every drive this session package tracks — see doc.go's
// own "Design" section: every state change happens inside Update or one of Controller's own
// methods, each of which returns a new Controller. Its two maps (entries, byID) are always
// replaced with a maps.Clone'd copy before either is written to, so an older Controller value —
// a stale screen copy, a test's "before" snapshot — never observes a write a newer one made
// (TestControllerIsCopyOnWrite pins this).
type Controller struct {
	backend Backend
	cfg     Config

	entries map[BuildID]entry
	byID    map[string]BuildID

	nextBuild uint64
	// listGen is Controller's own listing generation — Relist bumps it, and a listMsg/
	// listTickMsg carrying an older one is dropped (TestListGenDropsOlderListing).
	listGen uint64
}

// New builds a Controller over backend, with cfg's zero fields defaulted (normalize). backend
// nil is a valid, if useless, Controller: every method that would call it instead returns
// ErrNoBackend, mirroring app.Model's own nil-svc convention.
func New(backend Backend, cfg Config) Controller {
	return Controller{
		backend: backend,
		cfg:     cfg.normalize(),
		entries: map[BuildID]entry{},
		byID:    map[string]BuildID{},
	}
}

// withEntry returns a Controller whose entries (and, when e.id is set, byID) map is a fresh
// clone carrying e — the one place a mutating method actually writes an entry, so the
// copy-on-write discipline lives in exactly one function.
func (c Controller) withEntry(e entry) Controller {
	c.entries = maps.Clone(c.entries)
	c.entries[e.build] = e
	if e.id != "" {
		c.byID = maps.Clone(c.byID)
		c.byID[e.id] = e.build
	}
	return c
}

// withoutEntry returns a Controller with build removed from both maps. It also cancels build's
// own ctx — the one place a finished/failed/abandoned entry's listener goroutine (listenCmd) and
// its context.WithDeadline timer are actually stopped, rather than left running (and blocked on
// a channel nobody will ever read again) until the entry's own 4h deadline. Cancelling here, rather than at each caller
// (onStep's Done case, onBuilt's build-failed case), means no call site can forget and leak the
// goroutine listenCmd's own doc comment says is stopped. Safe to call on an entry whose ctx was already cancelled (CancelBuild/Stop do
// so themselves first): context.CancelFunc is idempotent.
func (c Controller) withoutEntry(build BuildID) Controller {
	e, ok := c.entries[build]
	if ok && e.cancel != nil {
		e.cancel()
	}
	c.entries = maps.Clone(c.entries)
	delete(c.entries, build)
	// byID[e.id] is only ever deleted when it still points at THIS build — a Resume racing a
	// Start for the same promotion id can leave byID[id] pointing at the SURVIVING entry by the
	// time the loser is removed here (onBuilt's own same-target dedup refuses
	// the second builtMsg to land, but if the FIRST one to land already mapped byID[id] to its
	// own build, removing the second, refused entry unconditionally would delete that live mapping
	// out from under the entry that actually won). Deleting unconditionally would make the surviving
	// entry unreachable by Snapshot/Resume/Poke/Abandon while BuildSnapshot still shows it alive —
	// exactly the state TestProbeByIDClobber pins.
	if ok && e.id != "" && c.byID[e.id] == build {
		c.byID = maps.Clone(c.byID)
		delete(c.byID, e.id)
	}
	return c
}

// runningForTarget reports the entry, if any, already tracked for target — Start's own
// same-target refusal (AGENTS.md invariant 5, one layer up from service.claimTarget).
func (c Controller) runningForTarget(target string) (BuildID, bool) {
	return c.runningForTargetExcept(target, 0)
}

// runningForTargetExcept is runningForTarget's own except-self variant — onBuilt's duplicate-
// driver check (below) needs to ask "is there ANOTHER entry already tracking this target",
// which plain runningForTarget cannot answer for a Start-originated entry: Start sets e.target at
// entry creation, before Backend.StartPromotion/onBuilt ever runs, so by the time onBuilt fires
// the entry's own (unexcluded) copy in c.entries already matches target and would always find
// itself.
func (c Controller) runningForTargetExcept(target string, except BuildID) (BuildID, bool) {
	for build, e := range c.entries {
		if build != except && e.target == target {
			return build, true
		}
	}
	return 0, false
}

// Init starts Controller's own background loop: an immediate listing and the recurring
// listTickMsg chain at Config.ListEvery. Returns nil when backend is nil — nothing to list.
func (c Controller) Init() tea.Cmd {
	if c.backend == nil {
		return nil
	}
	gen := c.listGen
	return tea.Batch(c.listCmd(gen), c.cfg.After(c.cfg.ListEvery, func(time.Time) tea.Msg { return listTickMsg{gen: gen} }))
}

func (c Controller) listCmd(gen uint64) tea.Cmd {
	backend, timeout := c.backend, c.cfg.ListTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		list, err := backend.List(ctx, service.ListOpts{})
		return listMsg{gen: gen, list: list, err: err}
	}
}

// Relist issues a fresh listing right away, superseding any listTickMsg chain still in flight
// from before (its gen no longer matches, so Update drops it once it fires).
func (c Controller) Relist() (Controller, tea.Cmd) {
	c.listGen++
	if c.backend == nil {
		return c, nil
	}
	return c, c.listCmd(c.listGen)
}

func listenCmd(ctx context.Context, build BuildID, gen uint64, ch chan progressItem) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case item, ok := <-ch:
			return progressMsg{build: build, gen: gen, line: item.line, hist: item.hist, ok: ok}
		case <-ctx.Done():
			// This is the fix for the goroutine leak app.go's own buildCmd doc comment
			// describes: once this entry's ctx is done (deadline, cancel, or a fresh one from a
			// re-arm), the listener stops re-issuing itself instead of blocking on a channel
			// forever with nobody left to read it.
			return nil
		}
	}
}

func newCtx(now func() time.Time, deadline time.Duration) (context.Context, context.CancelFunc, time.Time) {
	deadlineAt := now().Add(deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadlineAt)
	return ctx, cancel, deadlineAt
}

// startHooks builds the service.Hooks a build/resume/drive shares. Progress reports one short
// text line per preflight stage and OnWaiting the signing wait, both through the entry's one
// channel. OnHistory sends each newly saved History entry down the same channel as a typed item
// (never as text), so a step's outcome reaches the screen as it is saved, in order with the
// text lines. Every send is non-blocking: a full channel drops the item, which costs nothing
// lasting — the state the next Step tick carries holds every entry.
func startHooks(ch chan progressItem) service.Hooks {
	send := func(it progressItem) {
		select {
		case ch <- it:
		default:
		}
	}
	progress := func(line string) { send(progressItem{line: line}) }
	return service.Hooks{
		Progress:  progress,
		OnWaiting: func() { progress("waiting for signing approval") },
		OnHistory: func(e engine.HistoryEntry, s engine.PromotionState) {
			send(progressItem{hist: &historyItem{entry: e, state: s}})
		},
	}
}

// Start begins a fresh StartPromotion for req, tracked under a new BuildID — refused outright
// (ErrTargetBusy) when another entry already targets the same env (AGENTS.md invariant 5,
// TestSecondStartSameTargetRefused), and (ErrNoBackend) when this Controller has none.
func (c Controller) Start(req service.StartRequest, source, target string) (Controller, BuildID, tea.Cmd, error) {
	if _, ok := c.runningForTarget(target); ok {
		return c, 0, nil, ErrTargetBusy
	}
	if c.backend == nil {
		return c, 0, nil, ErrNoBackend
	}
	c.nextBuild++
	build := BuildID(c.nextBuild)
	ctx, cancel, deadlineAt := newCtx(c.cfg.Now, c.cfg.Deadline)
	ch := make(chan progressItem, 64)
	e := entry{
		build: build, phase: Building, busy: true, gen: 1,
		source: source, target: target, direct: req.Mode.Direct,
		ctx: ctx, cancel: cancel, deadlineAt: deadlineAt,
		progressCh: ch,
	}
	c = c.withEntry(e)
	backend := c.backend
	buildCmd := func() tea.Msg {
		d, err := backend.StartPromotion(ctx, req, startHooks(ch))
		return toBuiltMsg(build, e.gen, d, err)
	}
	return c, build, tea.Batch(buildCmd, listenCmd(ctx, build, e.gen, ch)), nil
}

// Resume re-attaches to (or starts resuming) promotion id. If id is already tracked — running
// or itself mid-resume — this starts nothing and returns that entry's own BuildID
// (TestResumeOfRunningIDStartsNothing: Backend.Resume must be called zero times).
func (c Controller) Resume(id string) (Controller, BuildID, tea.Cmd, error) {
	if build, ok := c.byID[id]; ok {
		return c, build, nil, nil
	}
	if c.backend == nil {
		return c, 0, nil, ErrNoBackend
	}
	c.nextBuild++
	build := BuildID(c.nextBuild)
	ctx, cancel, deadlineAt := newCtx(c.cfg.Now, c.cfg.Deadline)
	ch := make(chan progressItem, 64)
	e := entry{
		build: build, id: id, phase: Building, busy: true, gen: 1,
		ctx: ctx, cancel: cancel, deadlineAt: deadlineAt,
		progressCh: ch,
	}
	c = c.withEntry(e)
	backend := c.backend
	resumeCmd := func() tea.Msg {
		// Hooks{Progress, OnWaiting} fixes FB-M2: a resumed drive now reports
		// live log lines exactly as a freshly started one already did (service.Resume's own doc
		// comment on ResumeOpts.Hooks).
		d, err := backend.Resume(ctx, id, service.ResumeOpts{Hooks: startHooks(ch)})
		return toBuiltMsg(build, e.gen, d, err)
	}
	return c, build, tea.Batch(resumeCmd, listenCmd(ctx, build, e.gen, ch)), nil
}

func toBuiltMsg(build BuildID, gen uint64, d service.Drive, err error) builtMsg {
	if err == nil && d == nil {
		// A Backend that returns (nil, nil) has no promotion to attach to and no error to show —
		// production StartPromotion/Resume never do this (§4.1's own re-observation always leaves
		// one or the other), but a test double or a future Backend implementation that does must
		// not reach d.State() below: that's a nil interface method call, which panics rather than
		// degrading to a notice like every other unwired/misbehaving-adaptor path in this package
		// (guarded here once rather than in onBuilt a second time).
		err = errors.New("session: backend returned neither a drive nor an error")
	}
	var state engine.PromotionState
	var driver Driver
	if err == nil {
		state, driver = d.State(), d
	}
	return builtMsg{build: build, gen: gen, state: state, drive: driver, err: err}
}

// Running reports whether id names an entry Controller is currently tracking, in any phase —
// running, waiting, stopped, or itself mid-build/resume.
func (c Controller) Running(id string) bool {
	_, ok := c.byID[id]
	return ok
}

func stepCmd(ctx context.Context, build BuildID, gen uint64, driver Driver, overrideFirst bool) tea.Cmd {
	return func() tea.Msg {
		if overrideFirst {
			driver.OverrideCINone()
		}
		tick, err := driver.Step(ctx)
		return stepMsg{build: build, gen: gen, tick: tick, err: err}
	}
}

// rearm bumps e's generation, cancels its old ctx and builds a fresh one from now — the shared
// half of Poke and OverrideCINone: both need "a new window, and any msg still addressed to the
// old generation is now stale."
func (c Controller) rearm(e entry) entry {
	if e.cancel != nil {
		e.cancel()
	}
	e.gen++
	e.ctx, e.cancel, e.deadlineAt = newCtx(c.cfg.Now, c.cfg.Deadline)
	e.busy = true
	e.phase = Stepping
	return e
}

// Poke re-arms and steps promotion id at once — the operator's R. Refused (ErrBusy) while a
// Step is already outstanding or while abandoning; refused (ErrNotFound) for an untracked id.
func (c Controller) Poke(id string) (Controller, tea.Cmd, error) {
	build, ok := c.byID[id]
	if !ok {
		return c, nil, ErrNotFound
	}
	e := c.entries[build]
	if e.busy || e.phase == Abandoning {
		return c, nil, ErrBusy
	}
	if e.driver == nil {
		// A driver-less Stopped entry — a Resume whose own Backend.Resume call failed and was
		// then abandoned (see onAbandoned's own doc comment). Stepping a nil Driver panics;
		// refuse instead, the same nil-safe convention every other Controller method uses.
		return c, nil, ErrNoDriver
	}
	e = c.rearm(e)
	c = c.withEntry(e)
	// rearm cancelled the old ctx, which stops the old listenCmd (it returns nil the instant it
	// sees <-ctx.Done(), per its own doc comment) — but nothing else restarts one on the fresh
	// ctx/gen it just built, so any progress line the re-armed Step reports would sit unread in
	// progressCh until the whole drive finished, freezing the flight log. Batch a fresh listener alongside the Step so progress keeps flowing.
	return c, tea.Batch(stepCmd(e.ctx, e.build, e.gen, e.driver, false), listenCmd(e.ctx, e.build, e.gen, e.progressCh)), nil
}

// OverrideCINone re-arms and steps promotion id with Driver.OverrideCINone called first — the
// operator's `c` gesture answer (OverrideCINoneMsg, one layer up). Refused (ErrBusy) while
// already busy or abandoning, exactly like Poke.
func (c Controller) OverrideCINone(id string) (Controller, tea.Cmd, error) {
	build, ok := c.byID[id]
	if !ok {
		return c, nil, ErrNotFound
	}
	e := c.entries[build]
	if e.busy || e.phase == Abandoning {
		return c, nil, ErrBusy
	}
	if e.driver == nil {
		// Same driver-less-Stopped-entry guard as Poke's own — see its comment and ErrNoDriver's.
		return c, nil, ErrNoDriver
	}
	e = c.rearm(e)
	c = c.withEntry(e)
	// Same reasoning as Poke's own comment: the re-arm needs a fresh listener on its new
	// ctx/gen, or progress freezes for the rest of the drive.
	return c, tea.Batch(stepCmd(e.ctx, e.build, e.gen, e.driver, true), listenCmd(e.ctx, e.build, e.gen, e.progressCh)), nil
}

func maxAbandonAttempts(cfg Config) int {
	if cfg.AbandonWait <= 0 {
		return 1
	}
	n := int(cfg.AbandonTimeout / cfg.AbandonWait)
	if n < 1 {
		n = 1
	}
	return n
}

// Abandon cancels id's ctx and, once its current Step (if any) actually notices and returns,
// calls Backend.Abandon — a plain context cancel is not a join, so a Step already in flight is
// waited for rather than raced (TestAbandonWaitsForBusyStepThenAbandonsOnce): nothing here calls
// Step again once Abandon has been asked for (the phase==Abandoning guard on Poke/pollMsg/
// stepMsg's own re-arm path), and Backend.Abandon itself is called at most once
// (TestAbandonWaitsForBusyStepThenAbandonsOnce's own ordered call log). The wait gives up after
// Config.AbandonTimeout and proceeds anyway (TestAbandonTimeoutProceeds) — Backend.Abandon's own
// re-observation is what keeps that safe even if the wait gave up too early (see
// internal/service.Abandon's own doc comment). The call itself is bounded separately, and far
// more generously, by Config.AbandonCallTimeout.
func (c Controller) Abandon(id string) (Controller, tea.Cmd) {
	build, ok := c.byID[id]
	if !ok {
		return c, nil
	}
	e := c.entries[build]
	if e.phase == Abandoning || e.abandoning {
		// Already on its way out — a second X (or a re-observe racing the same key) must not
		// schedule another abandonWaitMsg chain or a second Backend.Abandon call while the first
		// is still outstanding. The flight screen's own guard (handleKey's Abandon case) refuses
		// this before it ever reaches here, but this is the authoritative check (AGENTS.md §8,
		// the deletion test): deleting the screen's guard should only make the notice ruder, never
		// make a second abandon possible.
		return c, nil
	}
	if e.cancel != nil {
		e.cancel()
	}
	e.phase = Abandoning
	e.abandoning = true
	if e.busy {
		c = c.withEntry(e)
		gen := e.gen
		return c, c.cfg.After(c.cfg.AbandonWait, func(time.Time) tea.Msg {
			return abandonWaitMsg{build: e.build, gen: gen, attempt: 0}
		})
	}
	e.abandonIssued = true
	c = c.withEntry(e)
	return c, c.abandonCmd(e.build, e.gen, id)
}

func (c Controller) abandonCmd(build BuildID, gen uint64, id string) tea.Cmd {
	backend, timeout := c.backend, c.cfg.AbandonCallTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		lines, err := backend.Abandon(ctx, id)
		return abandonedMsg{build: build, gen: gen, id: id, lines: lines, err: err}
	}
}

// Stop drops id from Controller without calling Backend.Abandon — the operator backing out of
// watching it (esc), leaving the real branch/PR/state file exactly as they are. Its ctx is
// cancelled first, so a Step in flight for it does not keep running unwatched.
func (c Controller) Stop(id string) Controller {
	build, ok := c.byID[id]
	if !ok {
		return c
	}
	return c.CancelBuild(build)
}

// StopAll cancels and drops every tracked entry — the operator's q-with-drives-running confirm,
// answered yes.
func (c Controller) StopAll() Controller {
	for _, e := range c.entries {
		if e.cancel != nil {
			e.cancel()
		}
	}
	c.entries = map[BuildID]entry{}
	c.byID = map[string]BuildID{}
	return c
}

// CancelBuild drops one build by id without calling Backend.Abandon (Stop's own BuildID-keyed
// twin — used while a build has no promotion id yet to key Stop by).
func (c Controller) CancelBuild(build BuildID) Controller {
	e, ok := c.entries[build]
	if !ok {
		return c
	}
	if e.cancel != nil {
		e.cancel()
	}
	return c.withoutEntry(build)
}

// Snapshot is the read-only view of one tracked entry a caller (a mirrored flight screen, the
// matrix's in-flight pane, a test) actually gets — never the entry itself, and never a live
// reference into anything Controller still owns: every slice is copied.
type Snapshot struct {
	Build          BuildID
	ID             string
	Phase          Phase
	Source, Target string
	Direct         bool

	State    engine.PromotionState
	Statuses []engine.StepStatus
	Done     bool
	Blocked  *engine.BlockedError
	Err      error
	Retry    bool
	Busy     bool

	NextPoll, DeadlineAt time.Time
	Log                  []LogLine
}

func snapshotOf(e entry) Snapshot {
	return Snapshot{
		Build: e.build, ID: e.id, Phase: e.phase,
		Source: e.source, Target: e.target, Direct: e.direct,
		State:    e.state,
		Statuses: append([]engine.StepStatus(nil), e.statuses...),
		Done:     e.done, Blocked: e.blocked, Err: e.err, Retry: e.retry, Busy: e.busy,
		NextPoll: e.nextPoll, DeadlineAt: e.deadlineAt,
		Log: append([]LogLine(nil), e.log...),
	}
}

// Snapshot returns promotion id's current Snapshot, or false when it is not tracked.
func (c Controller) Snapshot(id string) (Snapshot, bool) {
	build, ok := c.byID[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(c.entries[build]), true
}

// BuildSnapshot is Snapshot's BuildID-keyed twin — the id-less window before a build's real
// promotion id is known.
func (c Controller) BuildSnapshot(build BuildID) (Snapshot, bool) {
	e, ok := c.entries[build]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(e), true
}

// Live returns every entry Controller currently tracks, in no particular order — the matrix's
// in-flight pane's own source, merged with a fresh listing (Relist) by whatever wires this in.
func (c Controller) Live() []Snapshot {
	out := make([]Snapshot, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, snapshotOf(e))
	}
	return out
}

// AnyRunning reports whether at least one tracked entry is actually in progress — Building,
// Stepping, or Waiting, or an Abandoning entry whose Backend.Abandon call has actually been
// dispatched (abandonIssued) — the root's own q-with-drives-running gate. A
// Stopped entry (a Blocked step or a terminal, non-retryable error — R/Poke re-arms it) or one
// still Abandoning but not yet abandonIssued (waiting on a busy Step to notice its ctx was
// cancelled — nothing has actually been dispatched to the forge/cluster yet) does not, by itself,
// need a confirm before the program quits: StopAll's own ctx cancel is all there would be to
// interrupt. An abandonIssued entry is different — a real Backend.Abandon call (re-observe, close
// the PR, delete the branch) is in flight, and quitting out from under it is exactly the "kills a
// running service.Abandon mid-way" case a confirm exists for (so only an Abandoning entry whose
// abandonIssued is false is excluded).
func (c Controller) AnyRunning() bool {
	for _, e := range c.entries {
		switch e.phase {
		case Building, Stepping, Waiting:
			return true
		case Abandoning:
			if e.abandonIssued {
				return true
			}
		}
	}
	return false
}

func (c Controller) capWait(e entry, wait time.Duration) time.Duration {
	if wait < c.cfg.MinTick {
		wait = c.cfg.MinTick
	}
	if !e.deadlineAt.IsZero() {
		if left := e.deadlineAt.Sub(c.cfg.Now()); left < wait {
			if left < 0 {
				left = 0
			}
			wait = left
		}
	}
	return wait
}

// Update is Controller's own message handler: every background command this package issues
// eventually delivers one Event here, and every state change Controller ever makes happens
// inside this method or the request methods above (Start, Resume, Poke, OverrideCINone, Abandon,
// Stop, StopAll, CancelBuild, Relist) — never anywhere else. Returns the changes a caller should
// react to (re-key a building screen, mirror a snapshot, refresh the matrix, log an activity
// entry) — most events produce exactly one, some (a stepMsg that both lands and finishes) two.
func (c Controller) Update(ev Event) (Controller, tea.Cmd, []Change) {
	switch msg := ev.(type) {
	case builtMsg:
		return c.onBuilt(msg)
	case progressMsg:
		return c.onProgress(msg)
	case stepMsg:
		return c.onStep(msg)
	case pollMsg:
		return c.onPoll(msg)
	case abandonWaitMsg:
		return c.onAbandonWait(msg)
	case abandonedMsg:
		return c.onAbandoned(msg)
	case listMsg:
		return c.onList(msg)
	case listTickMsg:
		return c.onListTick(msg)
	}
	return c, nil, nil
}

func (c Controller) onBuilt(msg builtMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen {
		return c, nil, nil
	}
	if e.abandoning {
		// Abandon was requested while this entry's Resume call was still in flight — Abandon can
		// only ever target an id already tracked in byID, and Resume (unlike Start) sets e.id
		// before the call even starts, so e.id is always known here. The build's own outcome no
		// longer matters: proceeding to a fresh Step (on success) would silently re-arm past the
		// cancel, and reporting a build failure (on the ctx this same Abandon call cancelled)
		// would drop Backend.Abandon entirely. Either way the
		// operator asked to abandon, so that is what happens now.
		e.busy = false
		if msg.err == nil {
			e.state = msg.state
			e.driver = msg.drive
			// Same fill as the ordinary (non-abandoning) success path below: a Resume never has
			// source/target/direct to seed the entry with up front, so without this an abandon
			// fired during a resumed entry's Build window would show a blank header for the brief
			// window before the abandon actually completes (the ordinary path's own fill is not
			// reached here, since this branch returns before it).
			if e.source == "" && e.target == "" {
				e.source = msg.state.SourceEnv
				e.target = msg.state.TargetEnv
				e.direct = msg.state.Direct
			}
		}
		if e.abandonIssued {
			// Abandon's own wait chain (Abandon's immediate path, or onAbandonWait's timeout
			// branch) already dispatched Backend.Abandon for this entry before this builtMsg
			// arrived, capturing its own (build, gen) at that dispatch time — deliberately NOT
			// bumped here, since that call's own abandonedMsg still has to match e.gen when it
			// comes back (bumping gen unconditionally would orphan that in-flight call's
			// result instead of preventing a second one — the entry would never get removed). Record the build's own outcome and stop: the
			// already-dispatched abandonCmd will resolve this entry on its own.
			c = c.withEntry(e)
			return c, nil, nil
		}
		// Nothing has dispatched Backend.Abandon for this entry yet — this onBuilt call is the
		// first thing to decide "call it now". Bump gen so a same-gen abandonWaitMsg still
		// in-flight from Abandon's own busy path (below) is dropped by onAbandonWait's own gen
		// check instead of racing this dispatch (this bump alone does not prevent a double call;
		// abandonIssued above is what does, when the wait chain gets there first instead).
		e.gen++
		e.abandonIssued = true
		c = c.withEntry(e)
		return c, c.abandonCmd(e.build, e.gen, e.id), nil
	}
	if msg.err != nil {
		c = c.withoutEntry(msg.build)
		return c, nil, []Change{{Kind: ChangeBuildFailed, Build: msg.build, Err: msg.err}}
	}
	e.state = msg.state
	e.id = msg.state.ID
	// Resume (unlike Start) never has source/target/direct to seed the entry with up front — the
	// caller only ever names an id — so until they are filled a resumed entry's Snapshot reads blank
	// for all three: the flight header and pane line go blank during Build, and runningForTarget
	// cannot see it at all. Fill them from msg.state, which service.StartPromotion/Resume both
	// populate from the very first preflight save (internal/service/start.go's own
	// SourceEnv/TargetEnv, set before a Driver is ever handed back) — but only when Start hasn't
	// already set them from its own, equally authoritative req/target/source parameters: a
	// resumed entry is exactly the one whose e.source/e.target are still blank here.
	if e.source == "" && e.target == "" {
		e.source = msg.state.SourceEnv
		e.target = msg.state.TargetEnv
		e.direct = msg.state.Direct
	}
	// Duplicate-driver window: between StartPromotion's own first
	// preflight save and this process's builtMsg arriving, a fresh listing can already show the
	// real id beside the still id-less Building entry Start created — Resume(id) then gets past
	// the byID dedup entirely (Resume sets e.id at entry creation, from the id it was GIVEN, but
	// nothing here learns the OTHER entry's target until its own onBuilt runs — see
	// runningForTargetExcept's own doc comment) and this second onBuilt would otherwise adopt a
	// second live Driver for the exact same target. Once msg.state names the real target, a
	// same-target collision with a build that already exists independently of this one is exactly
	// that race resolving in the operator's disfavour — refuse it exactly like Start's own
	// same-target rule (AGENTS.md invariant 5), rather than silently running two drivers against
	// one promotion.
	if _, ok := c.runningForTargetExcept(e.target, e.build); ok {
		c = c.withoutEntry(e.build)
		return c, nil, []Change{{Kind: ChangeBuildFailed, Build: msg.build, ID: e.id, Err: ErrTargetBusy}}
	}
	e.driver = msg.drive
	e.phase = Stepping
	e.busy = true
	c = c.withEntry(e)
	changes := []Change{{Kind: ChangeBuilt, Build: e.build, ID: e.id}}
	return c, stepCmd(e.ctx, e.build, e.gen, e.driver, false), changes
}

func (c Controller) onProgress(msg progressMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen {
		return c, nil, nil
	}
	if !msg.ok {
		// The channel closed — nothing more will ever arrive on it. Stop listening (a nil ch
		// makes any later listenCmd call a no-op), matching flight.Model's own progressMsg
		// handling for this case.
		e.progressCh = nil
		c = c.withEntry(e)
		return c, nil, nil
	}
	if msg.hist != nil {
		e = applyHistory(e, *msg.hist)
	} else {
		e.log = append(append([]LogLine(nil), e.log...), LogLine{At: c.cfg.Now(), Text: msg.line})
	}
	c = c.withEntry(e)
	return c, listenCmd(e.ctx, e.build, e.gen, e.progressCh), []Change{{Kind: ChangeProgress, Build: e.build, ID: e.id}}
}

// applyHistory folds one live OnHistory report into e's snapshot: the state as saved with the
// entry replaces e.state — but only when it carries MORE History than e.state already does, so
// a report that arrives after the Step tick it belongs to (the channel and the tick are
// separate commands, in no fixed order) can never roll the snapshot back — and the step's row
// is updated from the entry. The text of the entry is NOT added to e.log: the flight screen
// renders History itself, and the tick that ends the walk replaces e.state wholesale, so each
// entry is on screen exactly once whichever way the two arrive.
func applyHistory(e entry, h historyItem) entry {
	if len(h.state.History) <= len(e.state.History) {
		return e
	}
	e.state = h.state
	e.statuses = upsertStatus(e.statuses, statusOf(h.entry))
	return e
}

// statusOf is the row-level meaning of one History entry: acted and already-satisfied entries
// mark their step done, a waiting or blocked entry marks it the step the promotion is stopped
// at. act failed has no row meaning of its own (the tick carries the error), so it reports the
// step as not yet satisfied.
func statusOf(h engine.HistoryEntry) engine.StepStatus {
	st := engine.StepStatus{Step: h.Step}
	switch {
	case h.Detail == "acted":
		st.Satisfied, st.Detail = true, "acted"
	case strings.HasPrefix(h.Detail, "already satisfied: "):
		st.Satisfied, st.Detail = true, strings.TrimPrefix(h.Detail, "already satisfied: ")
	case strings.HasPrefix(h.Detail, "waiting: "):
		st.Waiting, st.Detail = true, strings.TrimPrefix(h.Detail, "waiting: ")
	case strings.HasPrefix(h.Detail, "blocked: "):
		st.Blocked = strings.TrimPrefix(h.Detail, "blocked: ")
	default:
		st.Detail = h.Detail
	}
	return st
}

func upsertStatus(in []engine.StepStatus, st engine.StepStatus) []engine.StepStatus {
	out := append([]engine.StepStatus(nil), in...)
	for i := range out {
		if out[i].Step == st.Step {
			out[i] = st
			return out
		}
	}
	return append(out, st)
}

func (c Controller) onStep(msg stepMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen {
		return c, nil, nil
	}
	e.busy = false
	e.state = msg.tick.State
	e.statuses = append([]engine.StepStatus(nil), msg.tick.Statuses...)
	e.done = msg.tick.Done
	e.blocked = msg.tick.Blocked
	e.retry = msg.tick.Retry
	e.err = msg.err

	var changes []Change
	if !e.landed && e.state.LandedSHA() != "" {
		e.landed = true
		changes = append(changes, Change{Kind: ChangeLanded, Build: e.build, ID: e.id})
	}

	abandoning := e.phase == Abandoning

	switch {
	case e.done:
		// Snap carries the final tick's own state/statuses (already applied to e above) out
		// past withoutEntry, which is about to make BuildSnapshot/Snapshot forget them —
		// see Change.Snap's own doc comment. Done wins even while abandoning: the real
		// promotion finished before the cancel was noticed, so there is nothing left to
		// abandon.
		changes = append(changes, Change{Kind: ChangeDone, Build: e.build, ID: e.id, Snap: snapshotOf(e)})
		c = c.withoutEntry(e.build)
		return c, nil, changes
	case abandoning:
		// This is exactly the busy step Abandon's own wait is watching for — whatever it
		// reports (blocked, a retryable/non-retryable error, or an ordinary tick) must not
		// overwrite Abandoning with Stopped: that would let Poke re-arm (bumping gen), which
		// drops the abandonWaitMsg already scheduled and the abandon silently disappears
		// (the guard exists so an Abandoning entry is never overwritten). The entry just goes idle (busy already false above); the pending
		// abandonWaitMsg, next time it fires, proceeds to Backend.Abandon regardless of what
		// this tick said.
		changes = append(changes, Change{Kind: ChangeStepped, Build: e.build, ID: e.id})
		c = c.withEntry(e)
		return c, nil, changes
	case e.blocked != nil:
		e.phase = Stopped
		changes = append(changes, Change{Kind: ChangeBlocked, Build: e.build, ID: e.id, Err: e.blocked})
		c = c.withEntry(e)
		return c, nil, changes
	case msg.err != nil && !msg.tick.Retry:
		e.phase = Stopped
		changes = append(changes, Change{Kind: ChangeFailed, Build: e.build, ID: e.id, Err: msg.err})
		c = c.withEntry(e)
		return c, nil, changes
	}

	changes = append(changes, Change{Kind: ChangeStepped, Build: e.build, ID: e.id})
	if msg.tick.Waiting {
		e.phase = Waiting
	} else {
		e.phase = Stepping
	}
	wait := c.capWait(e, msg.tick.Wait)
	e.nextPoll = c.cfg.Now().Add(wait)
	c = c.withEntry(e)
	build, gen := e.build, e.gen
	return c, c.cfg.After(wait, func(time.Time) tea.Msg { return pollMsg{build: build, gen: gen} }), changes
}

func (c Controller) onPoll(msg pollMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen || e.busy || e.phase == Abandoning {
		return c, nil, nil
	}
	e.busy = true
	e.phase = Stepping
	c = c.withEntry(e)
	return c, stepCmd(e.ctx, e.build, e.gen, e.driver, false), nil
}

func (c Controller) onAbandonWait(msg abandonWaitMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen {
		return c, nil, nil
	}
	if e.busy && msg.attempt+1 < maxAbandonAttempts(c.cfg) {
		build, gen, attempt := e.build, e.gen, msg.attempt+1
		return c, c.cfg.After(c.cfg.AbandonWait, func(time.Time) tea.Msg {
			return abandonWaitMsg{build: build, gen: gen, attempt: attempt}
		}), nil
	}
	// The wait gave up (or the entry was already idle) — this is the wait chain's own dispatch of
	// Backend.Abandon. Mark it issued before onBuilt's own abandoning branch (below) can find out:
	// a slow Resume/Start build landing at the same gen after this point must not dispatch a
	// second Backend.Abandon call of its own (see abandonIssued's own doc
	// comment on why gen alone cannot arbitrate this).
	e.abandonIssued = true
	c = c.withEntry(e)
	return c, c.abandonCmd(e.build, e.gen, e.id), nil
}

func (c Controller) onAbandoned(msg abandonedMsg) (Controller, tea.Cmd, []Change) {
	e, ok := c.entries[msg.build]
	if !ok || msg.gen != e.gen {
		return c, nil, nil
	}
	if msg.err != nil {
		if e.driver == nil {
			// A failed abandon of a driver-less entry — either a never-built drive (e.g. Resume's
			// own Backend.Resume call returned an error and was then abandoned before any driver
			// ever existed) or one whose build is simply still in flight (Abandon's own busy-wait
			// or onAbandonWait's timeout branch can dispatch Backend.Abandon before onBuilt has
			// ever landed a driver for it) — must not leave a Stopped, driver-less entry behind:
			// Poke/OverrideCINone's own ErrNoDriver guard refuses it, but that guard is defence in
			// depth — the actual enforcement is here, not leaving a state Poke would ever have to
			// refuse in the first place. There is nothing left to retry (no driver to step, no
			// PR/branch this attempt ever created), so this entry is simply gone; the operator
			// sees the failure and can Resume/Start fresh.
			e.phase = Stopped
			snap := snapshotOf(e)
			c = c.withoutEntry(e.build)
			return c, nil, []Change{{Kind: ChangeAbandonFailed, Build: e.build, ID: e.id, Err: msg.err, Snap: snap}}
		}
		// A pollMsg already scheduled (Waiting) or the busy Step this abandon was itself waiting
		// on (Stepping-busy) can still be in flight/queued at this point — neither has any gen
		// bump to drop it, since this whole abandon attempt never re-armed one. Left alone, the
		// pending pollMsg would pass onPoll's busy/Abandoning guard (this entry is neither, once
		// set to Stopped) and dispatch a fresh Step on the ctx Abandon already cancelled, or the
		// in-flight Step's own stepMsg would land in onStep and — since e.phase is no longer
		// Abandoning by the time it arrives — fall through to the ordinary "schedule the next
		// poll" path instead of being recognised as the abandon's own busy step, silently
		// reviving an entry the operator was told had failed to abandon. Bumping
		// gen here drops both: any pollMsg/stepMsg still addressed to the old generation is stale.
		e.gen++
		e.busy = false
		e.phase = Stopped
		e.abandoning = false
		e.abandonIssued = false
		c = c.withEntry(e)
		return c, nil, []Change{{Kind: ChangeAbandonFailed, Build: e.build, ID: e.id, Err: msg.err}}
	}
	e.phase = Stopped
	snap := snapshotOf(e)
	c = c.withoutEntry(e.build)
	return c, nil, []Change{{Kind: ChangeAbandoned, Build: msg.build, ID: msg.id, Lines: msg.lines, Snap: snap}}
}

func (c Controller) onList(msg listMsg) (Controller, tea.Cmd, []Change) {
	if msg.gen != c.listGen {
		return c, nil, nil
	}
	if msg.err != nil {
		return c, nil, []Change{{Kind: ChangeRefused, Err: msg.err}}
	}
	return c, nil, []Change{{Kind: ChangeListed, List: msg.list}}
}

func (c Controller) onListTick(msg listTickMsg) (Controller, tea.Cmd, []Change) {
	if msg.gen != c.listGen {
		return c, nil, nil
	}
	gen := c.listGen
	return c, tea.Batch(c.listCmd(gen), c.cfg.After(c.cfg.ListEvery, func(time.Time) tea.Msg { return listTickMsg{gen: gen} })), nil
}
