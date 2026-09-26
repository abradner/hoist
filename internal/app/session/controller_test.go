package session

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui/uitest"
)

// --- fakes -------------------------------------------------------------

// fakeDrive is a scripted service.Drive: each Step call returns the next (Tick, error) pair
// from steps/errs, in order, and records "step" into calls — the ordered call log
// TestAbandonWaitsForBusyStepThenAbandonsOnce and friends assert against. Once steps is
// exhausted it keeps returning Waiting forever (never Done, never erroring) rather than
// panicking, so a test can "freeze" a drive mid-flight without having to script every poll it
// might make.
type fakeDrive struct {
	id    string
	steps []service.Tick
	errs  []error
	idx   int
	calls *[]string
}

func (d *fakeDrive) ID() string { return d.id }

func (d *fakeDrive) State() engine.PromotionState {
	if d.idx > 0 && d.idx-1 < len(d.steps) {
		return d.steps[d.idx-1].State
	}
	return engine.PromotionState{ID: d.id}
}

func (d *fakeDrive) OverrideCINone() {}

func (d *fakeDrive) Run(context.Context, service.RunHooks) error { return nil }

func (d *fakeDrive) Step(context.Context) (service.Tick, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, "step")
	}
	if d.idx >= len(d.steps) {
		return service.Tick{State: d.State(), Waiting: true, Wait: time.Second}, nil
	}
	t := d.steps[d.idx]
	var err error
	if d.idx < len(d.errs) {
		err = d.errs[d.idx]
	}
	d.idx++
	return t, err
}

// fakeBackend records every call it makes ("start", "resume", "abandon", "list"), in order, and
// answers through whichever func field the test supplied.
type fakeBackend struct {
	startFn   func(ctx context.Context, req service.StartRequest, h service.Hooks) (service.Drive, error)
	resumeFn  func(ctx context.Context, id string, o service.ResumeOpts) (service.Drive, error)
	abandonFn func(ctx context.Context, id string) ([]string, error)
	listFn    func(ctx context.Context, o service.ListOpts) ([]service.Listed, error)
	calls     []string
}

func (f *fakeBackend) StartPromotion(ctx context.Context, req service.StartRequest, h service.Hooks) (service.Drive, error) {
	f.calls = append(f.calls, "start")
	return f.startFn(ctx, req, h)
}

func (f *fakeBackend) Resume(ctx context.Context, id string, o service.ResumeOpts) (service.Drive, error) {
	f.calls = append(f.calls, "resume")
	return f.resumeFn(ctx, id, o)
}

func (f *fakeBackend) Abandon(ctx context.Context, id string) ([]string, error) {
	f.calls = append(f.calls, "abandon")
	return f.abandonFn(ctx, id)
}

func (f *fakeBackend) List(ctx context.Context, o service.ListOpts) ([]service.Listed, error) {
	f.calls = append(f.calls, "list")
	if f.listFn != nil {
		return f.listFn(ctx, o)
	}
	return nil, nil
}

// --- test harness --------------------------------------------------------

// harness drives a Controller, collecting every Change Update ever hands back — the small
// adapter the PR brief asks tests to route through, since Controller.Update's own three-value
// return doesn't match uitest.UpdateFunc's two-value shape directly.
type harness struct {
	c       Controller
	changes []Change
}

func harnessUpdate(h harness, msg tea.Msg) (harness, tea.Cmd) {
	ev, ok := msg.(Event)
	if !ok {
		return h, nil
	}
	c, cmd, chs := h.c.Update(ev)
	h.c = c
	h.changes = append(h.changes, chs...)
	return h, cmd
}

// runFirst executes cmd once and, if it produced a tea.BatchMsg, returns only its FIRST
// element's own message — never invoking the rest. Start and Resume each batch their real
// build/resume call together with this package's own progress-line listener
// (listenCmd, always the second element), which blocks on an unbuffered read on purpose (it is
// meant to sit in its own goroutine until a line arrives or its ctx is done) — a test must never
// call it synchronously with nothing queued, or it hangs forever. runFirst is the one place that
// distinction is made, so every other helper below can drive the rest of a chain (which never
// batches a blocking listener) through uitest.Drain without knowing about this at all.
func runFirst(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if bm, ok := msg.(tea.BatchMsg); ok {
		if len(bm) == 0 {
			return nil
		}
		return bm[0]()
	}
	return msg
}

// started runs Start (or Resume)'s own returned cmd through runFirst and feeds the resulting
// builtMsg through Update once — the one hop every test needs before it can drive the resulting
// stepCmd chain (which never batches the listener) through ordinary means.
func started(h harness, cmd tea.Cmd) (harness, tea.Cmd) {
	return harnessUpdate(h, runFirst(cmd))
}

// hop executes cmd exactly once and feeds its result through Update exactly once — no
// recursion. Used wherever a test needs to freeze a drive mid-flight (a busy Step that hasn't
// "returned" yet, an abandon wait mid-countdown) rather than run it to whatever its script
// implies next, which uitest.Drain's own unconditional recursion cannot do.
func hop(h harness, cmd tea.Cmd) (harness, tea.Cmd) {
	if cmd == nil {
		return h, nil
	}
	return harnessUpdate(h, cmd())
}

// drain fully resolves cmd via uitest.Drain — safe for any chain that does not itself contain
// Start/Resume's own batched listener (every cmd this package returns AFTER the initial
// builtMsg is exactly that: single, non-blocking funcs) and that is known to terminate (a
// scripted Done, or a bounded number of scripted ticks).
func (h harness) drain(cmd tea.Cmd) harness {
	return uitest.Drain(h, cmd, harnessUpdate)
}

func testConfig(now func() time.Time) Config {
	return Config{
		Deadline:       time.Hour,
		ListEvery:      time.Minute,
		MinTick:        time.Millisecond, // low floor: tests assert real Tick.Wait values, not the floor
		AbandonWait:    time.Millisecond,
		AbandonTimeout: 3 * time.Millisecond,
		ListTimeout:    time.Second,
		Now:            now,
		After: func(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
			return func() tea.Msg { return f(now()) }
		},
	}
}

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// --- tests ---------------------------------------------------------------

// TestStepSchedulesPollAtTickWait: once a Step returns Waiting, the next Step is scheduled at
// exactly Tick.Wait — not sooner, not the MinTick floor overriding a real, larger value.
func TestStepSchedulesPollAtTickWait(t *testing.T) {
	now := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var calls []string
	drive := &fakeDrive{id: "promo-1", calls: &calls, steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Waiting: true, Wait: 30 * time.Second},
		{State: engine.PromotionState{ID: "promo-1"}, Done: true},
	}}
	var durations []time.Duration
	cfg := testConfig(now)
	cfg.After = func(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
		durations = append(durations, d)
		return func() tea.Msg { return f(now()) }
	}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, cfg)

	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	h = h.drain(next) // finite: Waiting -> scheduled poll -> Done

	if len(durations) != 1 || durations[0] != 30*time.Second {
		t.Fatalf("scheduled durations = %v, want exactly [30s]", durations)
	}
	if got := len(calls); got != 2 {
		t.Fatalf("Step calls = %d, want 2 (the first poll, then the one it scheduled)", got)
	}
	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after Done")
	}
}

// TestStaleStepMsgAfterPokeIsDropped: the attacker is a pollMsg scheduled before Poke re-armed
// the entry, delivered after — it must be dropped (no Step call, no follow-up cmd), since Poke
// bumped the entry's own generation and started a fresh Step of its own.
func TestStaleStepMsgAfterPokeIsDropped(t *testing.T) {
	now := fixedClock(time.Now())
	var calls []string
	drive := &fakeDrive{id: "promo-1", calls: &calls} // always Waiting — never Done
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))

	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd)   // onBuilt's own first Step call (gen 1), not yet run
	h, stalePoll := hop(h, stepCmd1) // Step #1 (gen 1) returns Waiting, schedules a poll — captured, not run; entry is idle again (busy=false)
	if stalePoll == nil {
		t.Fatal("no poll scheduled after a Waiting step")
	}

	c2, freshStepCmd, pokeErr := h.c.Poke("promo-1")
	h.c = c2
	if pokeErr != nil {
		t.Fatalf("Poke: %v", pokeErr)
	}
	// Resolve Poke's own fresh Step too, so the entry is idle again (busy=false, gen 2) by the
	// time the stale poll arrives below — isolating the gen check from the separate busy guard,
	// which would otherwise mask a missing gen check (both currently refuse a busy entry).
	h, _ = hop(h, freshStepCmd)

	before := len(calls)
	h, next := hop(h, stalePoll) // the attacker: gen-1's own scheduled poll, delivered after Poke re-armed to gen 2
	if next != nil {
		t.Fatal("a stale pollMsg produced a follow-up command")
	}
	if got := len(calls); got != before {
		t.Fatalf("stale pollMsg triggered a Step call: %d -> %d, want no change", before, got)
	}
}

// TestAbandonWaitsForBusyStepThenAbandonsOnce: Abandon while a Step is in flight must wait for
// it to actually return before ever calling Backend.Abandon — and while waiting, Poke is
// refused. The ordered call log proves no Step happens once Abandon has taken over, and
// Backend.Abandon runs exactly once.
func TestAbandonWaitsForBusyStepThenAbandonsOnce(t *testing.T) {
	now := fixedClock(time.Now())
	var calls []string
	drive := &fakeDrive{id: "promo-1", calls: &calls} // always Waiting
	abandoned := 0
	backend := &fakeBackend{
		startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
			return drive, nil
		},
		abandonFn: func(context.Context, string) ([]string, error) {
			abandoned++
			return []string{"closed PR #1"}, nil
		},
	}
	c := New(backend, testConfig(now))

	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd) // entry is now busy: onBuilt's own Step call hasn't "returned" yet

	c2, abandonWaitCmd := h.c.Abandon("promo-1")
	h.c = c2
	if abandonWaitCmd == nil {
		t.Fatal("Abandon while busy produced no wait command")
	}

	// Attacker: Poke while the abandon wait is watching a busy step.
	if _, _, err := h.c.Poke("promo-1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Poke during abandon wait = %v, want ErrBusy", err)
	}

	stepsBefore := len(calls)
	h, followUp := hop(h, stepCmd1) // the busy step finally "returns"
	if followUp != nil {
		t.Fatal("a step result observed while abandoning scheduled another poll")
	}
	if got := len(calls) - stepsBefore; got != 1 {
		t.Fatalf("Step calls from the in-flight step returning = %d, want 1 (its own result only)", got)
	}

	h, abandonRunCmd := hop(h, abandonWaitCmd) // busy has cleared — proceeds straight to Abandon
	h = h.drain(abandonRunCmd)

	if abandoned != 1 {
		t.Fatalf("Backend.Abandon called %d times, want 1", abandoned)
	}
	if got := len(calls); got != stepsBefore+1 {
		t.Fatalf("a Step call happened after Backend.Abandon started: total=%d, want %d", got, stepsBefore+1)
	}
	if _, ok := h.c.entries[build]; ok {
		t.Fatal("entry still tracked after a successful abandon")
	}
}

// TestAbandonTimeoutProceeds: a busy step that never notices cancellation must not wedge
// Abandon forever — once Config.AbandonTimeout elapses, Backend.Abandon runs anyway.
func TestAbandonTimeoutProceeds(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1"} // always Waiting; this test never resolves its Step call
	abandoned := 0
	backend := &fakeBackend{
		startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
			return drive, nil
		},
		abandonFn: func(context.Context, string) ([]string, error) {
			abandoned++
			return nil, nil
		},
	}
	cfg := testConfig(now)
	cfg.AbandonWait = time.Millisecond
	cfg.AbandonTimeout = 3 * time.Millisecond // -> maxAbandonAttempts == 3
	c := New(backend, cfg)

	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, _ = started(h, cmd) // entry is busy; its Step call is never resolved in this test

	c2, waitCmd := h.c.Abandon("promo-1")
	h.c = c2
	for i := 0; i < maxAbandonAttempts(cfg); i++ {
		var next tea.Cmd
		h, next = hop(h, waitCmd)
		if next == nil {
			t.Fatalf("abandon wait chain stopped early at attempt %d", i)
		}
		waitCmd = next
	}
	h = h.drain(waitCmd) // the final hop: the wait gave up and this is the actual Abandon call
	if abandoned != 1 {
		t.Fatalf("Backend.Abandon called %d times, want exactly 1 once the timeout elapsed", abandoned)
	}
	if _, ok := h.c.entries[build]; ok {
		t.Fatal("entry still tracked after timeout-then-abandon")
	}
}

// TestSecondStartSameTargetRefused pins AGENTS.md invariant 5 at this layer: Controller itself
// refuses a second Start for a target env it is already tracking, before ever calling Backend.
func TestSecondStartSameTargetRefused(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return &fakeDrive{id: "promo-1"}, nil
	}}
	c := New(backend, testConfig(now))

	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	h := harness{c: c}
	h, _ = started(h, cmd) // actually invoke Backend.StartPromotion once, for the call-count check below
	_, _, _, err = h.c.Start(service.StartRequest{}, "staging", "prod")
	if !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("second Start for the same target = %v, want ErrTargetBusy", err)
	}
	startCalls := 0
	for _, call := range backend.calls {
		if call == "start" {
			startCalls++
		}
	}
	if startCalls != 1 {
		t.Fatalf("Backend.StartPromotion called %d times, want 1 (the refused one never reaches it)", startCalls)
	}
}

// TestResumeOfRunningIDStartsNothing: resuming an id already tracked (running or itself
// mid-resume) must call Backend.Resume zero times and just hand back the existing BuildID.
func TestResumeOfRunningIDStartsNothing(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{
		startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
			return &fakeDrive{id: "promo-1"}, nil
		},
		resumeFn: func(context.Context, string, service.ResumeOpts) (service.Drive, error) {
			t.Fatal("Backend.Resume should not be called for an id already tracked")
			return nil, nil
		},
	}
	c := New(backend, testConfig(now))
	c, firstBuild, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, _ = started(h, cmd)

	_, build, resumeCmd, err := h.c.Resume("promo-1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumeCmd != nil {
		t.Fatal("Resume of an already-tracked id returned a command (it should start nothing)")
	}
	if build != firstBuild {
		t.Fatalf("Resume returned BuildID %d, want the existing entry's %d", build, firstBuild)
	}
	for _, call := range backend.calls {
		if call == "resume" {
			t.Fatal("Backend.Resume was called")
		}
	}
}

// TestListGenDropsOlderListing: a listing whose generation predates the most recent Relist is
// dropped; the current generation's own listing still lands (positive control).
func TestListGenDropsOlderListing(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{}
	c := New(backend, testConfig(now))

	c, _ = c.Relist()
	staleGen := c.listGen
	c, _ = c.Relist() // supersedes staleGen
	currentGen := c.listGen

	h := harness{c: c}
	h = h.drain(func() tea.Msg {
		return listMsg{gen: staleGen, list: []service.Listed{{State: engine.PromotionState{ID: "stale"}}}}
	})
	if len(h.changes) != 0 {
		t.Fatalf("a stale listMsg produced a Change: %+v, want none", h.changes)
	}

	h = h.drain(func() tea.Msg {
		return listMsg{gen: currentGen, list: []service.Listed{{State: engine.PromotionState{ID: "fresh"}}}}
	})
	if len(h.changes) != 1 || h.changes[0].Kind != ChangeListed {
		t.Fatalf("the current generation's own listing did not land: changes=%+v", h.changes)
	}
}

// TestControllerIsCopyOnWrite: Start on a copy of a Controller must never mutate the original's
// own maps — the discipline every mutating method relies on (maps.Clone before every write).
func TestControllerIsCopyOnWrite(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return &fakeDrive{id: "promo-1"}, nil
	}}
	original := New(backend, testConfig(now))
	before := len(original.entries)

	updated, _, _, err := original.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := len(original.entries); got != before {
		t.Fatalf("original.entries mutated by a copy's Start: len=%d, want %d", got, before)
	}
	if len(original.byID) != 0 {
		t.Fatal("original.byID mutated by a copy's Start")
	}
	if len(updated.entries) == before {
		t.Fatal("the new Controller never actually recorded the new entry")
	}
}

// TestLandedEmittedOnce: ChangeLanded fires exactly once, on the transition to a non-empty
// LandedSHA — never again on a later poll that still reports the same landed sha.
func TestLandedEmittedOnce(t *testing.T) {
	now := fixedClock(time.Now())
	landed := engine.PromotionState{ID: "promo-1", PushedSHA: "abc123", Direct: true}
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: landed, Waiting: true, Wait: time.Second},
		{State: landed, Waiting: true, Wait: time.Second},
		{State: landed, Done: true},
	}}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{Mode: service.Mode{Direct: true}}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	h = h.drain(next) // finite: Waiting, Waiting, Done

	landedCount := 0
	for _, ch := range h.changes {
		if ch.Kind == ChangeLanded {
			landedCount++
		}
	}
	if landedCount != 1 {
		t.Fatalf("ChangeLanded emitted %d times, want exactly 1 (changes: %+v)", landedCount, h.changes)
	}
}

// TestBuildFailedRemovesEntry: a failed Start (a real in-flight conflict, a missing github
// config) removes the entry and reports ChangeBuildFailed — nothing left to Poke or Abandon.
func TestBuildFailedRemovesEntry(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return nil, errors.New("boom")
	}}
	c := New(backend, testConfig(now))
	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	if next != nil {
		t.Fatal("a failed build produced a follow-up command")
	}
	if _, ok := h.c.BuildSnapshot(build); ok {
		t.Fatal("a failed build is still tracked")
	}
	if len(h.changes) != 1 || h.changes[0].Kind != ChangeBuildFailed {
		t.Fatalf("changes = %+v, want exactly one ChangeBuildFailed", h.changes)
	}
}

// TestProgressListenerStopsOnCancel proves the fix for app.go's own documented goroutine leak:
// once an entry's ctx is done, listenCmd's own select returns nil rather than blocking on the
// channel forever with nobody left to drain it.
func TestProgressListenerStopsOnCancel(t *testing.T) {
	ch := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := listenCmd(ctx, 1, 1, ch)
	if msg := cmd(); msg != nil {
		t.Fatalf("listenCmd after ctx cancel = %#v, want nil (no more listening, no leak)", msg)
	}
}

// TestProgressLineDeliveredAndReissued: a line sent on an entry's channel while it is still
// building arrives as a log entry, and the listener re-issues itself for the next one.
func TestProgressLineDeliveredAndReissued(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{startFn: func(_ context.Context, _ service.StartRequest, h service.Hooks) (service.Drive, error) {
		h.Progress("checking your checkout against origin/main")
		return &fakeDrive{id: "promo-1"}, nil
	}}
	c := New(backend, testConfig(now))
	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	// runFirst only ever runs the build's own cmd — call the listener directly ourselves, once,
	// now that a line is actually queued (so it returns immediately rather than blocking).
	buildMsg := runFirst(cmd)
	h, _ = harnessUpdate(h, buildMsg)

	e := h.c.entries[build]
	line := listenCmd(e.ctx, e.build, e.gen, e.progressCh)()
	h, next := harnessUpdate(h, line)
	if next == nil {
		t.Fatal("a delivered progress line produced no follow-up listen command")
	}
	snap, _ := h.c.BuildSnapshot(build)
	if len(snap.Log) != 1 || snap.Log[0].Text != "checking your checkout against origin/main" {
		t.Fatalf("Log = %+v, want the one delivered line", snap.Log)
	}
}
