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

// resumedStateDrive is fakeDrive with State() overridden to return a fixed state immediately —
// exactly what a real resumed Driver already carries the moment Backend.Resume hands it back
// (unlike fakeDrive's own State(), which only reports something once a scripted Step has actually
// run) — TestOnBuiltSetsSourceTargetDirectForResumedEntry needs onBuilt's own toBuiltMsg (which
// reads d.State() before any Step call) to see SourceEnv/TargetEnv/Direct right away.
type resumedStateDrive struct {
	fakeDrive
	state engine.PromotionState
}

func (d *resumedStateDrive) State() engine.PromotionState { return d.state }

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
	// which would otherwise mask a missing gen check (both currently refuse a busy entry). Poke's
	// own cmd is now a batch (stepCmd + a fresh listenCmd, since a re-arm needs its own progress
	// listener too), so this goes through started/runFirst exactly like Start's and Resume's own
	// batched cmd, rather than hop (which would hand harnessUpdate a raw tea.BatchMsg — not a
	// session.Event — and silently do nothing).
	h, _ = started(h, freshStepCmd)

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

// TestAbandonCallGetsItsOwnGenerousTimeout: Backend.Abandon does real work (ObserveAll,
// ClosePR, DeleteRemoteBranch) that can legitimately take longer than the short busy-Step wait
// (AbandonWait/AbandonTimeout) but well within AbandonCallTimeout. abandonCmd's own ctx must be
// bound by AbandonCallTimeout, never by AbandonTimeout — conflating the two (found in review of
// ceaccb2) meant a real abandon regularly failed with "context deadline exceeded". The fake here
// sleeps on the real wall clock (abandonCmd's ctx comes from context.Background(), unaffected by
// the test's fixed/injected clock) for longer than AbandonTimeout but shorter than
// AbandonCallTimeout, then reports whether its ctx had already expired.
func TestAbandonCallGetsItsOwnGenerousTimeout(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Waiting: true, Wait: time.Second},
	}}
	var sawExpired bool
	backend := &fakeBackend{
		startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
			return drive, nil
		},
		abandonFn: func(ctx context.Context, _ string) ([]string, error) {
			time.Sleep(20 * time.Millisecond)
			sawExpired = ctx.Err() != nil
			return nil, ctx.Err()
		},
	}
	cfg := testConfig(now)
	cfg.AbandonWait = time.Millisecond
	cfg.AbandonTimeout = 2 * time.Millisecond       // the short busy-step wait bound
	cfg.AbandonCallTimeout = 200 * time.Millisecond // the call's own, generous bound
	c := New(backend, cfg)

	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd)
	h, _ = hop(h, stepCmd1) // resolve the first Step so the entry goes idle (busy=false) before Abandon

	c2, abandonCmd := h.c.Abandon("promo-1")
	h.c = c2
	h.changes = nil
	if abandonCmd == nil {
		t.Fatal("Abandon on an idle entry produced no command")
	}
	h = h.drain(abandonCmd)

	if len(h.changes) != 1 || h.changes[0].Kind != ChangeAbandoned {
		t.Fatalf("changes = %+v, want exactly one ChangeAbandoned", h.changes)
	}
	if sawExpired {
		t.Fatal("Backend.Abandon's ctx had already expired after 20ms — abandonCmd is using the short busy-wait timeout instead of AbandonCallTimeout")
	}
}

// TestAbandonDuringResumeBuildSucceedsCallsAbandonOnce: Abandon fired while a resumed entry's
// Resume call is still in flight (Building) must, once that call returns successfully, still
// call Backend.Abandon exactly once and never issue a Step — proceeding to a fresh stepCmd (the
// pre-fix behaviour) would silently re-arm past the cancel the operator just asked for (found in
// review of ceaccb2, e.abandoning was written but never read).
func TestAbandonDuringResumeBuildSucceedsCallsAbandonOnce(t *testing.T) {
	now := fixedClock(time.Now())
	var stepCalls []string
	drive := &fakeDrive{id: "promo-1", calls: &stepCalls}
	abandoned := 0
	backend := &fakeBackend{
		resumeFn: func(context.Context, string, service.ResumeOpts) (service.Drive, error) {
			return drive, nil
		},
		abandonFn: func(context.Context, string) ([]string, error) {
			abandoned++
			return []string{"closed PR #1"}, nil
		},
	}
	c := New(backend, testConfig(now))

	c, _, resumeCmd, err := c.Resume("promo-1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := harness{c: c}

	// Abandon fires while the Resume call is still in flight (Building, busy) — it must take the
	// busy-wait path, not call Backend.Abandon yet.
	c2, abandonWaitCmd := h.c.Abandon("promo-1")
	h.c = c2
	if abandonWaitCmd == nil {
		t.Fatal("Abandon on a busy (Building) entry produced no wait command")
	}
	if abandoned != 0 {
		t.Fatal("Backend.Abandon called before the busy Resume even returned")
	}

	// The Resume call itself now returns successfully.
	h, followUp := started(h, resumeCmd)
	if followUp == nil {
		t.Fatal("onBuilt-while-abandoning produced no command — Backend.Abandon should have been called via abandonCmd")
	}
	if abandoned != 0 {
		t.Fatalf("Backend.Abandon called before its own returned command ran: %d", abandoned)
	}
	// Draining followUp must be the abandonCmd itself — resolving it must not produce a Step
	// call and must remove the entry (a successful abandon).
	h = h.drain(followUp)
	if len(stepCalls) != 0 {
		t.Fatalf("Step calls after abandon-during-build: %v, want none", stepCalls)
	}
	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after abandon-during-build completed")
	}

	// The stale abandonWaitMsg chain scheduled by the original Abandon call (old gen) must be
	// inert now — it must not call Backend.Abandon a second time.
	h, next := hop(h, abandonWaitCmd)
	if abandoned != 1 {
		t.Fatalf("Backend.Abandon called %d times total — the stale abandonWaitMsg chain called it again", abandoned)
	}
	if next != nil {
		t.Fatal("the stale (superseded) abandonWaitMsg chain produced a follow-up command")
	}
}

// TestAbandonDuringResumeBuildErrorStillCallsAbandon: when the resumed entry's own Resume call
// comes back with an error (service.Resume checks ctx.Err() eagerly once Abandon cancelled its
// ctx), Backend.Abandon must still run — dropping it because the build "failed" left the branch
// and PR the operator asked to abandon untouched (found in review of ceaccb2).
func TestAbandonDuringResumeBuildErrorStillCallsAbandon(t *testing.T) {
	now := fixedClock(time.Now())
	abandoned := 0
	backend := &fakeBackend{
		resumeFn: func(context.Context, string, service.ResumeOpts) (service.Drive, error) {
			return nil, context.Canceled
		},
		abandonFn: func(context.Context, string) ([]string, error) {
			abandoned++
			return nil, nil
		},
	}
	c := New(backend, testConfig(now))

	c, _, resumeCmd, err := c.Resume("promo-1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := harness{c: c}

	c2, abandonWaitCmd := h.c.Abandon("promo-1")
	h.c = c2
	if abandonWaitCmd == nil {
		t.Fatal("Abandon on a busy entry produced no wait command")
	}

	h, followUp := started(h, resumeCmd) // the Resume call returns an error
	for _, ch := range h.changes {
		if ch.Kind == ChangeBuildFailed {
			t.Fatal("a build error while abandoning was reported as ChangeBuildFailed instead of proceeding to Backend.Abandon")
		}
	}
	if followUp == nil {
		t.Fatal("onBuilt-while-abandoning (build error) produced no command")
	}
	h = h.drain(followUp)
	if abandoned != 1 {
		t.Fatalf("Backend.Abandon called %d times after a failed build-while-abandoning, want 1", abandoned)
	}
	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after abandon-during-failed-build completed")
	}
}

// TestBlockedDuringAbandonKeepsAbandoningPhase: a Blocked tick reported for a Step that Abandon
// is waiting on must not overwrite phase Abandoning with Stopped — doing so let Poke re-arm
// (bumping gen), which drops the abandonWaitMsg already scheduled and the abandon silently
// disappears (found in review of ceaccb2).
func TestBlockedDuringAbandonKeepsAbandoningPhase(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Blocked: &engine.BlockedError{Reason: "conflict"}},
	}}
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
	c := New(backend, testConfig(now))

	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd) // onBuilt's own first Step call, not yet resolved — entry busy

	c2, abandonWaitCmd := h.c.Abandon("promo-1")
	h.c = c2

	h, followUp := hop(h, stepCmd1) // the busy step returns Blocked
	if followUp != nil {
		t.Fatal("a Blocked tick observed while abandoning scheduled a follow-up (must wait for abandonWaitMsg instead)")
	}
	snap, ok := h.c.Snapshot("promo-1")
	if !ok {
		t.Fatal("promo-1 no longer tracked after a Blocked tick while abandoning")
	}
	if snap.Phase != Abandoning {
		t.Fatalf("phase after a Blocked tick while abandoning = %v, want Abandoning", snap.Phase)
	}
	if _, _, err := h.c.Poke("promo-1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Poke while Abandoning after a Blocked tick = %v, want ErrBusy", err)
	}

	h = h.drain(abandonWaitCmd) // busy has cleared — proceeds to Backend.Abandon
	if abandoned != 1 {
		t.Fatalf("Backend.Abandon called %d times, want 1", abandoned)
	}
	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after abandon completed")
	}
}

// TestWithoutEntryCancelsCtx: a finished (Done) or failed-to-build entry's own ctx must be
// cancelled the moment it is dropped, so its listenCmd goroutine (blocked on progressCh) and its
// context.WithDeadline timer both stop immediately instead of leaking until the entry's own 4h
// deadline — the leak listenCmd's own doc comment claims is fixed (found in review of ceaccb2:
// two removal sites, onStep's Done case and onBuilt's build-failed case, never actually called
// cancel).
func TestWithoutEntryCancelsCtx(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Done: true},
	}}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))

	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	entryCtx := h.c.entries[build].ctx
	if entryCtx.Err() != nil {
		t.Fatal("entry ctx already cancelled before the drive even finished")
	}

	h, next := started(h, cmd)
	h = h.drain(next) // resolves the scripted Done tick

	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after Done")
	}
	select {
	case <-entryCtx.Done():
	default:
		t.Fatal("entry ctx was not cancelled when the finished entry was removed — its listenCmd goroutine and deadline timer leak")
	}
	if entryCtx.Err() != context.Canceled {
		t.Fatalf("entry ctx.Err() = %v, want context.Canceled", entryCtx.Err())
	}
}

// TestWithoutEntryCancelsCtxOnBuildFailure is TestWithoutEntryCancelsCtx's build-failure twin:
// onBuilt's own ChangeBuildFailed removal path must cancel just as reliably.
func TestWithoutEntryCancelsCtxOnBuildFailure(t *testing.T) {
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
	entryCtx := h.c.entries[build].ctx

	h, _ = started(h, cmd) // the build fails; onBuilt removes the entry via withoutEntry

	if entryCtx.Err() != context.Canceled {
		t.Fatalf("entry ctx.Err() after a failed build = %v, want context.Canceled", entryCtx.Err())
	}
}

// TestPokeRearmsAFreshListener: after Poke re-arms an entry onto a new ctx/gen, a progress line
// the re-armed Step reports must still reach onProgress — Poke's own returned cmd used to be the
// bare stepCmd, with nothing left listening on progressCh for the entry's new generation (the old
// listenCmd stops the instant rearm cancels the old ctx, per its own doc comment); the flight log
// froze for the rest of the drive (found in review of ceaccb2).
func TestPokeRearmsAFreshListener(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1"} // always Waiting — never Done
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))

	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd)
	h, _ = hop(h, stepCmd1) // settle the first Step so the entry is idle before Poke

	c2, pokeCmd, pokeErr := h.c.Poke("promo-1")
	h.c = c2
	if pokeErr != nil {
		t.Fatalf("Poke: %v", pokeErr)
	}
	batch, ok := pokeCmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 || batch[0] == nil || batch[1] == nil {
		t.Fatalf("Poke's own cmd = %#v, want tea.BatchMsg(stepCmd, listenCmd)", pokeCmd)
	}

	// Push a progress line onto the (re-armed) entry's own channel, as a real driver's Hooks.Progress
	// would, then run the batch's second element (the fresh listener) and feed its result through
	// Update — this is exactly what a live progress line needs to actually surface.
	ch := h.c.entries[build].progressCh
	ch <- "restarted step"
	h, _ = harnessUpdate(h, batch[1]())

	snap, ok := h.c.Snapshot("promo-1")
	if !ok {
		t.Fatal("promo-1 no longer tracked")
	}
	if len(snap.Log) != 1 || snap.Log[0].Text != "restarted step" {
		t.Fatalf("Log after Poke's re-armed listener = %+v, want exactly one line \"restarted step\"", snap.Log)
	}
}

// TestOnBuiltSetsSourceTargetDirectForResumedEntry: Resume, unlike Start, never has
// source/target/direct to seed an entry with up front (the caller only ever names an id) — before
// this fix the entry stayed blank for the whole Building window and forever after, so the flight
// header/pane line read blank and a resumed drive's target could not be seen by
// runningForTarget/ErrTargetBusy at all (AGENTS.md invariant 5's own claim didn't hold — found in
// review of ceaccb2). onBuilt must fill all three in from msg.state.
func TestOnBuiltSetsSourceTargetDirectForResumedEntry(t *testing.T) {
	now := fixedClock(time.Now())
	state := engine.PromotionState{ID: "promo-1", SourceEnv: "staging", TargetEnv: "prod", Direct: true}
	drive := &resumedStateDrive{fakeDrive: fakeDrive{id: "promo-1"}, state: state}
	backend := &fakeBackend{resumeFn: func(context.Context, string, service.ResumeOpts) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))

	c, _, resumeCmd, err := c.Resume("promo-1")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := harness{c: c}
	snapBeforeBuilt, _ := h.c.Snapshot("promo-1")
	if snapBeforeBuilt.Source != "" || snapBeforeBuilt.Target != "" {
		t.Fatalf("setup: entry already carries source/target before onBuilt: %+v", snapBeforeBuilt)
	}

	h, _ = started(h, resumeCmd)

	snap, ok := h.c.Snapshot("promo-1")
	if !ok {
		t.Fatal("promo-1 not tracked after onBuilt")
	}
	if snap.Source != "staging" || snap.Target != "prod" || !snap.Direct {
		t.Fatalf("snapshot after onBuilt = %+v, want Source=staging Target=prod Direct=true", snap)
	}

	// The invariant this was actually for: a resumed drive's target must now be visible to
	// Start's own same-target refusal.
	if _, _, _, err := h.c.Start(service.StartRequest{}, "staging", "prod"); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("Start against a resumed drive's target = %v, want ErrTargetBusy", err)
	}
}

// TestResumeDuringStartBuildOfSameTargetIsDeduped closes the duplicate-driver window found in
// review of ceaccb2: between StartPromotion's own first preflight save and this process's
// builtMsg arriving, a fresh listing can already show the real id beside the still id-less
// Building entry Start created. Resume(id) at that point gets past the byID dedup entirely
// (Resume sets e.id from the id it was GIVEN — that entry has no way to know it collides with
// Start's own still-id-less entry until its own onBuilt reveals the target) and would otherwise
// end up running a second live Driver against the exact same target. onBuilt must refuse the
// second one exactly like Start's own same-target rule, once the target is known.
func TestResumeDuringStartBuildOfSameTargetIsDeduped(t *testing.T) {
	now := fixedClock(time.Now())
	startDrive := &fakeDrive{id: "promo-1"} // always Waiting
	resumeDrive := &resumedStateDrive{
		fakeDrive: fakeDrive{id: "promo-1"},
		state:     engine.PromotionState{ID: "promo-1", SourceEnv: "staging", TargetEnv: "prod"},
	}
	backend := &fakeBackend{
		startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
			return startDrive, nil
		},
		resumeFn: func(context.Context, string, service.ResumeOpts) (service.Drive, error) {
			return resumeDrive, nil
		},
	}
	c := New(backend, testConfig(now))

	// Start's own Building entry registers "prod" as its target immediately, before
	// StartPromotion's own background call ever runs — this is the entry a same-target Resume
	// races against.
	c, startBuild, startCmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}

	// A listing (not modelled here directly) already shows "promo-1" — the operator resumes it
	// before Start's own builtMsg has landed.
	c2, resumeBuild, resumeCmd, err := h.c.Resume("promo-1")
	h.c = c2
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumeBuild == startBuild {
		t.Fatal("setup: Resume returned the Start entry's own BuildID — byID already deduped it, nothing left to test")
	}

	// The resumed call's own build finishes first — this is exactly where the collision must be
	// caught.
	h, follow := started(h, resumeCmd)
	if follow != nil {
		t.Fatal("a deduped duplicate build produced a follow-up Step command")
	}
	if len(h.changes) != 1 || h.changes[0].Kind != ChangeBuildFailed {
		t.Fatalf("changes = %+v, want exactly one ChangeBuildFailed", h.changes)
	}
	if !errors.Is(h.changes[0].Err, ErrTargetBusy) {
		t.Fatalf("ChangeBuildFailed.Err = %v, want ErrTargetBusy", h.changes[0].Err)
	}
	if _, ok := h.c.BuildSnapshot(resumeBuild); ok {
		t.Fatal("the duplicate resumed build is still tracked")
	}
	// The original Start entry must be entirely unaffected.
	if snap, ok := h.c.BuildSnapshot(startBuild); !ok || snap.Target != "prod" {
		t.Fatalf("the original Start entry was disturbed: %+v (ok=%v)", snap, ok)
	}

	// Cleanly finish the original — proves this is not a leftover held-open target, just the
	// duplicate.
	h, _ = harnessUpdate(h, runFirst(startCmd))
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

// TestAnyRunningReflectsPhase: AnyRunning is true for Building/Stepping/Waiting and false once
// an entry settles at Stopped (blocked) or is removed outright (Done) — the root's own
// q-with-drives-running gate (Train 2 design PR 3) must not keep asking once there is nothing
// left that a quit would actually interrupt.
func TestAnyRunningReflectsPhase(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Blocked: &engine.BlockedError{Reason: "conflict"}},
	}}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !c.AnyRunning() {
		t.Fatal("Building must count as running")
	}
	h := harness{c: c}
	h, next := started(h, cmd) // onBuilt's own first Step call is issued...
	if !h.c.AnyRunning() {
		t.Fatal("Stepping must count as running")
	}
	h, _ = harnessUpdate(h, next()) // ...and lands the scripted Blocked tick.
	if h.c.AnyRunning() {
		t.Fatal("a Stopped (blocked) entry must not count as running — R can re-arm it later, but nothing is happening to it right now")
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

// TestBackendReturningNilDriveAndNilErrIsBuildFailed is PR 2's own documented gap (its final
// report flagged toBuiltMsg's own contract as untested): a Backend returning (nil, nil) — no
// drive, no error — must not reach d.State() below, a nil-interface method call that panics
// rather than degrading like every other misbehaving-adaptor path in this package. This proves
// toBuiltMsg turns that shape into a clear ChangeBuildFailed instead.
func TestBackendReturningNilDriveAndNilErrIsBuildFailed(t *testing.T) {
	now := fixedClock(time.Now())
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return nil, nil
	}}
	c := New(backend, testConfig(now))
	c, build, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked on a nil drive/nil error backend answer: %v", r)
			}
		}()
		h, _ = started(h, cmd)
	}()
	if _, ok := h.c.BuildSnapshot(build); ok {
		t.Fatal("a build failed this way is still tracked")
	}
	if len(h.changes) != 1 || h.changes[0].Kind != ChangeBuildFailed {
		t.Fatalf("changes = %+v, want exactly one ChangeBuildFailed", h.changes)
	}
	if h.changes[0].Err == nil {
		t.Error("ChangeBuildFailed carries no error to show the operator")
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

// --- ported flight-level tests (P2 #6) -----------------------------------
//
// These port the coverage TestDriveResultBlockedStopsPolling, TestDriveErrorOnNonRetryableStepStopsPolling,
// TestDriveErrorShowsNoticeAndKeepsPolling, TestRetryableErrorAfterPriorStop…, TestCapToDeadline,
// TestMinTickIsAUIFloorNotPolicy, TestReobserveAfterTheDeadlineGetsAFreshWindow and
// TestApplyCINoneOverrideRedrivesWithTheFlagSet used to provide one layer up, in
// internal/app/flight, before Train 2 moved drive-owning down into this package (ceaccb2,
// 16fea38) and left no controller-level replacement (found in review of ceaccb2).

// TestBlockedTickSchedulesNoPoll: a Blocked tick must not schedule a follow-up poll — Poke (R) is
// the only thing that re-arms a blocked entry.
func TestBlockedTickSchedulesNoPoll(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: engine.PromotionState{ID: "promo-1"}, Blocked: &engine.BlockedError{Reason: "conflict"}},
	}}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	h, follow := harnessUpdate(h, next())
	if follow != nil {
		t.Fatal("a Blocked tick scheduled a follow-up poll")
	}
	snap, ok := h.c.Snapshot("promo-1")
	if !ok || snap.Phase != Stopped {
		t.Fatalf("snapshot after Blocked = %+v, want Phase=Stopped", snap)
	}
}

// TestNonRetryableErrorSchedulesNoPoll: an error with Tick.Retry=false must stop polling, just
// like Blocked.
func TestNonRetryableErrorSchedulesNoPoll(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1",
		steps: []service.Tick{{State: engine.PromotionState{ID: "promo-1"}, Retry: false}},
		errs:  []error{errors.New("fatal")},
	}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	h, follow := harnessUpdate(h, next())
	if follow != nil {
		t.Fatal("a non-retryable error scheduled a follow-up poll")
	}
	snap, ok := h.c.Snapshot("promo-1")
	if !ok || snap.Phase != Stopped {
		t.Fatalf("snapshot after a non-retryable error = %+v, want Phase=Stopped", snap)
	}
	if snap.Err == nil {
		t.Fatal("snapshot lost the non-retryable error")
	}
}

// TestRetryableErrorKeepsPolling: an error with Tick.Retry=true must still schedule the next
// poll, unlike a non-retryable one — a transient failure (a flaky forge read) must not wedge the
// drive the way a real block does.
func TestRetryableErrorKeepsPolling(t *testing.T) {
	now := fixedClock(time.Now())
	drive := &fakeDrive{id: "promo-1",
		steps: []service.Tick{
			{State: engine.PromotionState{ID: "promo-1"}, Retry: true, Wait: 5 * time.Second},
			{State: engine.PromotionState{ID: "promo-1"}, Done: true},
		},
		errs: []error{errors.New("transient")},
	}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, next := started(h, cmd)
	h, follow := harnessUpdate(h, next())
	if follow == nil {
		t.Fatal("a retryable error produced no follow-up poll")
	}
	snap, ok := h.c.Snapshot("promo-1")
	if !ok {
		t.Fatal("promo-1 not tracked after a retryable error")
	}
	if snap.Phase == Stopped {
		t.Fatal("a retryable error left the entry Stopped — it must keep polling")
	}
	if snap.Err == nil {
		t.Fatal("snapshot lost the retryable error (still worth showing while it keeps polling)")
	}
	h = h.drain(follow) // resolves to the scripted Done
	if _, ok := h.c.Snapshot("promo-1"); ok {
		t.Fatal("promo-1 still tracked after Done")
	}
}

// TestCapWaitRespectsMinTickFloor: a Step's own requested wait, however small (even zero or
// negative), is floored to Config.MinTick — the same floor flight.Model.minTick used to apply one
// layer up.
func TestCapWaitRespectsMinTickFloor(t *testing.T) {
	now := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg := testConfig(now)
	cfg.MinTick = 2 * time.Second
	cfg.Deadline = time.Hour
	c := New(&fakeBackend{}, cfg)
	e := entry{deadlineAt: now().Add(cfg.Deadline)}
	if got := c.capWait(e, time.Millisecond); got != cfg.MinTick {
		t.Fatalf("capWait(1ms) = %v, want the MinTick floor %v", got, cfg.MinTick)
	}
	if got := c.capWait(e, -time.Second); got != cfg.MinTick {
		t.Fatalf("capWait(-1s) = %v, want the MinTick floor %v", got, cfg.MinTick)
	}
}

// TestCapWaitRespectsDeadlineCap: a Step's own requested wait, however large, is capped to what
// is actually left of the entry's own deadline — never scheduling a poll the deadline will have
// already passed by.
func TestCapWaitRespectsDeadlineCap(t *testing.T) {
	now := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg := testConfig(now)
	cfg.MinTick = time.Millisecond
	c := New(&fakeBackend{}, cfg)
	e := entry{deadlineAt: now().Add(10 * time.Second)}
	if got := c.capWait(e, time.Hour); got != 10*time.Second {
		t.Fatalf("capWait(1h) with 10s left on the deadline = %v, want 10s", got)
	}
	// Past the deadline entirely: never a negative wait.
	e2 := entry{deadlineAt: now().Add(-time.Second)}
	if got := c.capWait(e2, time.Hour); got != 0 {
		t.Fatalf("capWait past the deadline = %v, want 0 (never negative)", got)
	}
}

// TestPokeGivesAFreshDeadlineAt: Poke (rearm) must start a new deadline window from now, not
// extend the stale one — a promotion parked for hours and then poked should get a fresh
// Config.Deadline budget, not immediately read as "about to expire."
func TestPokeGivesAFreshDeadlineAt(t *testing.T) {
	now := fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	drive := &fakeDrive{id: "promo-1"} // always Waiting
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd)
	h, _ = hop(h, stepCmd1) // settle busy=false before Poke

	snapBefore, _ := h.c.Snapshot("promo-1")

	later := now().Add(3 * time.Hour)
	h.c.cfg.Now = fixedClock(later) // simulate real time passing before the operator pokes
	c2, pokeCmd, pokeErr := h.c.Poke("promo-1")
	h.c = c2
	if pokeErr != nil {
		t.Fatalf("Poke: %v", pokeErr)
	}
	if pokeCmd == nil {
		t.Fatal("Poke produced no command")
	}

	snapAfter, ok := h.c.Snapshot("promo-1")
	if !ok {
		t.Fatal("promo-1 not tracked after Poke")
	}
	if !snapAfter.DeadlineAt.After(snapBefore.DeadlineAt) {
		t.Fatalf("DeadlineAt after Poke = %v, want later than the original %v", snapAfter.DeadlineAt, snapBefore.DeadlineAt)
	}
	wantDeadline := later.Add(testConfig(now).Deadline)
	if !snapAfter.DeadlineAt.Equal(wantDeadline) {
		t.Fatalf("DeadlineAt after Poke = %v, want a fresh window from the poke time: %v", snapAfter.DeadlineAt, wantDeadline)
	}
}

// TestOverrideCINoneCallsDriverBeforeStep: OverrideCINone must call Driver.OverrideCINone before
// Driver.Step — the whole point is that the next Step call actually observes the override.
func TestOverrideCINoneCallsDriverBeforeStep(t *testing.T) {
	now := fixedClock(time.Now())
	var order []string
	drive := &orderedOverrideDrive{order: &order}
	backend := &fakeBackend{startFn: func(context.Context, service.StartRequest, service.Hooks) (service.Drive, error) {
		return drive, nil
	}}
	c := New(backend, testConfig(now))
	c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	h := harness{c: c}
	h, stepCmd1 := started(h, cmd)
	h, _ = hop(h, stepCmd1) // settle busy=false before the override
	order = nil             // discard the setup Step call — only the override's own ordering matters

	c2, overrideCmd, overrideErr := h.c.OverrideCINone("promo-1")
	h.c = c2
	if overrideErr != nil {
		t.Fatalf("OverrideCINone: %v", overrideErr)
	}
	batch, ok := overrideCmd().(tea.BatchMsg)
	if !ok || len(batch) < 1 {
		t.Fatalf("OverrideCINone's own cmd = %#v, want tea.BatchMsg(stepCmd, listenCmd)", overrideCmd)
	}
	h, _ = harnessUpdate(h, batch[0]()) // runs the actual (override-first) Step call

	if len(order) != 2 || order[0] != "override" || order[1] != "step" {
		t.Fatalf("call order = %v, want [override step]", order)
	}
}

// orderedOverrideDrive records the order OverrideCINone and Step are called in.
type orderedOverrideDrive struct {
	order *[]string
}

func (d *orderedOverrideDrive) ID() string { return "promo-1" }

func (d *orderedOverrideDrive) OverrideCINone() { *d.order = append(*d.order, "override") }

func (d *orderedOverrideDrive) State() engine.PromotionState {
	return engine.PromotionState{ID: "promo-1"}
}

func (d *orderedOverrideDrive) Step(context.Context) (service.Tick, error) {
	*d.order = append(*d.order, "step")
	return service.Tick{State: engine.PromotionState{ID: "promo-1"}, Waiting: true, Wait: time.Second}, nil
}

func (d *orderedOverrideDrive) Run(context.Context, service.RunHooks) error { return nil }
