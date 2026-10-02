package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/abradner/hoist/internal/engine"
)

// Tick is one poll iteration's result — the CLI's own retry/heartbeat loop and the flight
// screen's tick loop both drive off exactly this shape, so neither reimplements the other's
// retry/wait decision (AGENTS.md §9 entry 11's own lesson: a decision made twice, even
// correctly today, drifts). State is a value copy of the promotion's state as it stood the
// instant this Step call returned — safe for a caller to hold or render without racing a later
// Step call on the same Drive. Done and Statuses mirror engine.DriveStatus's own return shape
// exactly; Waiting and Blocked are the same distinction engine.Observation makes, surfaced here
// because Step's own err is never ErrWaiting or a *BlockedError — see Driver.Step's own doc
// comment for why. Retry and Wait are meaningful together: Retry is engine.Retryable(err) —
// true only when Step actually returned a genuine error and the failing step is one of
// engine.RetryableStep's five — and Wait is engine.PollInterval for whichever step is active
// (the failing step, when there was one, else the promotion's own current phase), always
// populated regardless of err so a caller scheduling the NEXT poll never has to re-derive it
// from statuses itself.
type Tick struct {
	State    engine.PromotionState
	Done     bool
	Statuses []engine.StepStatus
	Waiting  bool
	Blocked  *engine.BlockedError
	Retry    bool
	Wait     time.Duration
}

// Drive is what a caller — cmd/hoist's own commands and internal/app/flight's screen alike —
// actually needs from a promotion driver: one poll (Step), the CLI's own run-to-completion loop
// (Run), the state it is driving (State, ID), and the one operator override a promotion can pick
// up mid-flight (OverrideCINone). *Driver satisfies it; a test fakes it directly.
type Drive interface {
	ID() string
	State() engine.PromotionState
	Step(ctx context.Context) (Tick, error)
	OverrideCINone()
	Run(ctx context.Context, h RunHooks) error
}

// Driver drives one promotion's steps to completion, one engine.DriveStatus walk per Step call
// (see DriveStatus's own doc comment for the double-observation problem this replaces: a caller
// that used to run engine.Drive and then engine.Status every poll — cmd/hoist's driveToCompletion
// and internal/app/flight's own DriveFunc both did — now makes one real walk against the world
// per poll, whether that poll is a CLI retry, a flight screen tick, or the operator's own R/c
// gesture). mu serializes every method that touches state: Step, Run (which only ever calls
// Step) and OverrideCINone, so a screen's manual retry can never race an automatic tick's Step
// call over the same *engine.PromotionState — the same one-in-flight-per-instance guarantee
// flight.Model's own busy flag gives at the UI layer, held here too so a caller with no busy
// flag of its own (session.Controller, which holds a Drive across screens) still gets it for
// free.
type Driver struct {
	mu    sync.Mutex
	steps []engine.Step
	state *engine.PromotionState
	save  func(*engine.PromotionState) error
	poll  engine.PollIntervals
}

// NewDriver builds a Driver over steps, saving through save after every step DriveStatus
// actually acts on or stops at (exactly as engine.Drive itself already does — save is called
// from inside DriveStatus, never by Driver directly). state is driven in place: every Step call
// mutates *state and returns a snapshot of it, so a caller that also holds state directly (a
// resumed CLI command's own *engine.PromotionState) sees it advance the same way engine.Drive
// always mutated its own s argument in place.
//
// Exported and taking steps/state/save/poll positionally rather than a settled Deps/Settings
// shape because internal/service.StartPromotion and Resume are what build a Driver in
// production, and are expected to unexport this constructor behind their own request/response
// types once every caller goes through them.
func NewDriver(steps []engine.Step, state *engine.PromotionState, save func(*engine.PromotionState) error, poll engine.PollIntervals) *Driver {
	return &Driver{steps: steps, state: state, save: save, poll: poll}
}

// ID is the promotion this Driver drives.
func (d *Driver) ID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.ID
}

// State returns a value copy of the promotion's current state, safe to hold or render without
// racing a Step call in flight.
func (d *Driver) State() engine.PromotionState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.state
}

// OverrideCINone sets CINoneOverride on this Driver's own state — the TUI's `c` gesture and the
// CLI's `--override-ci-none` both land here. It takes effect on the NEXT Step call, whose own
// CIGreenStep.Observe reads it and whose own save persists it; OverrideCINone itself never
// saves, exactly as flight.Model.ApplyCINoneOverride never saved on its own before this change —
// only a Step (via engine.DriveStatus's own per-step save) does.
func (d *Driver) OverrideCINone() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state.CINoneOverride = true
}

// Step runs one poll iteration: a single engine.DriveStatus walk, and — only when that walk
// returns a genuine failure, never for Waiting or Blocked — a second engine.Status walk to
// recompute the full step list, matching exactly what cmd/hoist/wiring.go's old driveFuncFor did
// unconditionally (call engine.Drive, then always call engine.Status for the rows) on ITS OWN
// error path: a caller rendering rows after a failure sees precisely what it saw before this
// change. On every other outcome — done, waiting, blocked — DriveStatus's own statuses are the
// only walk; the second walk this replaces was pure waste there (AGENTS.md §9 entry 11).
//
// The returned error is never ErrWaiting and never a *BlockedError — those are reported through
// Tick.Waiting and Tick.Blocked instead, exactly as flight.DriveFunc's own doc comment already
// described for the old two-call shape. A non-nil error here is always a genuine plumbing or
// terminal failure, classified by Tick.Retry (engine.Retryable) for whatever the caller's own
// retry loop needs.
func (d *Driver) Step(ctx context.Context) (Tick, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	done, statuses, driveErr := engine.DriveStatus(ctx, d.steps, d.state, d.save)

	var blocked *engine.BlockedError
	waiting := errors.Is(driveErr, engine.ErrWaiting)
	isBlocked := errors.As(driveErr, &blocked)

	var outErr error
	if driveErr != nil && !waiting && !isBlocked {
		// A genuine failure: recompute the full list via engine.Status, exactly like the old
		// driveFuncFor's unconditional second call — the one walk DriveStatus already did on
		// this same *d.state stops at the failing step, same as engine.Drive always did, so
		// this reproduces driveFuncFor's error-path rows byte-for-byte rather than changing
		// what a caller sees on failure.
		done, statuses, _ = engine.Status(ctx, d.steps, d.state)
		outErr = driveErr
	}

	phase := d.state.Phase
	var stepErr *engine.StepError
	if errors.As(outErr, &stepErr) {
		phase = stepErr.Step
	}

	return Tick{
		State:    *d.state,
		Done:     done,
		Statuses: statuses,
		Waiting:  waiting,
		Blocked:  blocked,
		Retry:    engine.Retryable(outErr),
		Wait:     engine.PollInterval(d.poll, phase),
	}, outErr
}

// RunHooks are the CLI's own reporting hooks for Run — all nil-safe. OnTick fires once per poll
// that comes back Waiting (mirroring cmd/hoist/drive.go's old waitingReporter.report call on the
// same condition); OnRetry fires once per poll that came back a genuine, retryable error;
// OnHeartbeat fires during the sleep between polls, at most every Heartbeat, but only while the
// most recent poll was Waiting (a retryable-error sleep prints nothing further today, matching
// driveToCompletion exactly). Heartbeat <= 0 means "no heartbeat slicing" — the whole wait is one
// sleep.
type RunHooks struct {
	OnTick      func(Tick)
	OnRetry     func(error)
	Heartbeat   time.Duration
	OnHeartbeat func(Tick)
}

// Run drives this promotion to completion, calling Step repeatedly and sleeping Tick.Wait
// between calls (in Heartbeat-sized slices, so a long wait can still report it is alive) until
// Step reports Done, a *engine.BlockedError, a terminal (non-Retry) error, or ctx is done. This
// is cmd/hoist/drive.go's old driveToCompletion, unchanged in outward behavior: the actual
// waiting still lives here, in the caller's own loop, never inside a Step's Act (AGENTS.md
// invariant 4) — Run just calls Driver.Step instead of engine.Drive+engine.Status directly, so
// there is still exactly one walk against the world per poll.
func (d *Driver) Run(ctx context.Context, h RunHooks) error {
	for {
		tick, err := d.Step(ctx)
		switch {
		case err == nil && tick.Done:
			return nil
		case tick.Blocked != nil:
			return tick.Blocked
		case tick.Waiting:
			if h.OnTick != nil {
				h.OnTick(tick)
			}
		case err != nil:
			if !tick.Retry {
				return err
			}
			if h.OnRetry != nil {
				h.OnRetry(err)
			}
		default:
			// engine.DriveStatus never actually returns this shape (a nil error means fully
			// done, per engine.Drive's own contract) — stay defensive rather than loop forever
			// silently if that ever stops being true.
			return nil
		}
		remaining := tick.Wait
		for remaining > 0 {
			nap := remaining
			if h.Heartbeat > 0 && h.Heartbeat < nap {
				nap = h.Heartbeat
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nap):
			}
			remaining -= nap
			if tick.Waiting && h.OnHeartbeat != nil {
				h.OnHeartbeat(tick)
			}
		}
	}
}
