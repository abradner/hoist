package app

import (
	"context"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
)

// This file is the test-only seam onto app.Service (internal/app/service.go), built for the PR F
// refactor that replaced Model's separate func-typed fields (a start-a-promotion func, its plain
// Direct/Confirmed options struct, an in-flight list/resume pair, and an abandon func) with the
// one Service interface. Every fixture in this package's other _test.go files used to build a
// plain func value for exactly one of those old shapes; fakeService keeps that exact shape (one
// func field per operation, nil left unset so an unintended call panics rather than silently
// doing nothing) while satisfying app.Service's fuller, service.Drive-returning signatures
// underneath.

// startOpts is the test-only stand-in for the old (now-deleted) start-a-promotion options
// struct — the plain Direct/Confirmed pair every fake Start closure in this package's tests
// asserts against. Production code carries the identical pair inside service.Mode now; this type
// exists purely so those existing closures need no signature change beyond their enclosing
// struct's name.
type startOpts struct {
	Direct    bool
	Confirmed bool
}

// startFn is the old (now-deleted) start-a-promotion function's shape.
type startFn func(ctx context.Context, p gitops.Plan, opts startOpts, progress func(string)) (engine.PromotionState, flight.Driver, error)

// resumeFn is the old in-flight pane's Resume function shape.
type resumeFn func(ctx context.Context, id string) (engine.PromotionState, flight.Driver, error)

// fakeInFlight is the test-only stand-in for the old (now-deleted) in-flight List/Resume pair,
// bundled the same way the old Model's fluent in-flight setter took them. svcWithInFlight below
// is what replaces that setter itself: there is no post-construction fluent setter any more, so a
// test builds the *fakeService up front and hands it to New's own svc parameter.
type fakeInFlight struct {
	List   func(context.Context) ([]service.Listed, error)
	Resume resumeFn
}

// svcWithInFlight adapts fakeInFlight's own List/Resume into a *fakeService, for the tests that
// used to wire the in-flight pane's list/resume funcs directly onto the root — that fluent setter
// no longer exists on Model; List/Resume now reach the root only through New's svc parameter.
func svcWithInFlight(in fakeInFlight) *fakeService {
	return &fakeService{ListFn: in.List, ResumeFn: in.Resume}
}

// testPromo is app.Promotion (app.go) plus the deleted Start field, kept as a separate type so
// the many existing `Promotion{Start: ...}` fixtures across this package's tests need only a
// type rename (Promotion -> testPromo) at their declaration, not a restructure. sizedWithPromotion
// (app_test.go) is what splits this back into a *fakeService (for Start) and a plain
// app.Promotion (for Poll/OpenURL/OpenPRMode) at New(...) call time.
type testPromo struct {
	Start      startFn
	Poll       flight.PollDurations
	OpenURL    func(url string) error
	OpenPRMode string
}

// stateDrive adapts a (state, flight.Driver) pair — everything a fake Start/Resume closure
// returns, exactly like the old start-a-promotion/in-flight-resume function signatures — into
// the fuller service.Drive interface app.Model now requires (ID/Run on top of flight.Driver's own
// Step/State/OverrideCINone). inner is nil whenever the fake's own closure deliberately returned
// a nil driveFn (several fixtures do, to exercise app.go's own nil-driveFn guard,
// TestPromotionBuiltMsgNilDriveFnShowsNotice) — every method below falls back to the fixed state
// captured at construction in that case, mirroring app_test.go's own pre-existing fixedDriver.
type stateDrive struct {
	state engine.PromotionState
	inner flight.Driver
}

func (d *stateDrive) ID() string { return d.state.ID }

func (d *stateDrive) State() engine.PromotionState {
	if d.inner != nil {
		return d.inner.State()
	}
	return d.state
}

func (d *stateDrive) Step(ctx context.Context) (service.Tick, error) {
	if d.inner != nil {
		return d.inner.Step(ctx)
	}
	return service.Tick{State: d.state, Done: true}, nil
}

func (d *stateDrive) OverrideCINone() {
	if d.inner != nil {
		d.inner.OverrideCINone()
	}
}

// Run loops Step to completion — no test in this package drives Run's own retry/heartbeat timing
// through this fake (that machinery is covered directly against the real service.Driver in
// internal/service/driver_test.go); this exists only so *stateDrive satisfies service.Drive.
func (d *stateDrive) Run(ctx context.Context, h service.RunHooks) error {
	for {
		tick, err := d.Step(ctx)
		if err != nil {
			return err
		}
		if tick.Done {
			return nil
		}
		if tick.Blocked != nil {
			return tick.Blocked
		}
		if h.OnTick != nil {
			h.OnTick(tick)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// fakeService is the test double for app.Service — one func field per method. A nil field means
// the test never intended that method to be called; the method panics loudly rather than
// returning a silent zero value, so a test that accidentally exercises an unwired path fails
// with a clear diagnostic instead of a confusing downstream assertion failure.
type fakeService struct {
	startFn   startFn
	ListFn    func(context.Context) ([]service.Listed, error)
	ResumeFn  resumeFn
	AbandonFn func(context.Context, string) error
	PlanFn    func(context.Context, service.PlanRequest) (service.PlannedChange, error)
	RefreshFn func(context.Context) (service.RepoView, error)
	RepoFn    func() service.RepoView
	// onStart, when set, is called with the FULL service.StartRequest StartPromotion received,
	// before startFn's own narrower (Plan, startOpts, progress) shape strips everything else
	// out — startFn's shape predates StartRequest.View (fakeservice_test.go's own doc comment:
	// it exists purely so old fixtures need no signature change) and has no way to observe it.
	// Added for TestPlanStartMsgCarriesItsOwnPlannedView/TestDeployStartMsgCarriesItsOwnPlannedView
	// (app_test.go, t1-review.md P2-a): the root's job is to pass StartRequest.View as the
	// view THIS plan/deploy was actually built against, and startFn alone cannot see whether it
	// did.
	onStart func(service.StartRequest)
}

func (f *fakeService) Plan(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error) {
	if f.PlanFn == nil {
		panic("fakeService: Plan called but PlanFn not set — this package's tests drive planning through the separate plan.Func parameter, never svc.Plan")
	}
	return f.PlanFn(ctx, req)
}

func (f *fakeService) StartPromotion(ctx context.Context, req service.StartRequest, h service.Hooks) (service.Drive, error) {
	if f.onStart != nil {
		f.onStart(req)
	}
	if f.startFn == nil {
		panic("fakeService: StartPromotion called but startFn not set")
	}
	opts := startOpts{Direct: req.Mode.Direct, Confirmed: req.Mode.Confirmed}
	state, driver, err := f.startFn(ctx, req.Plan, opts, h.Progress)
	if err != nil {
		return nil, err
	}
	return &stateDrive{state: state, inner: driver}, nil
}

// List defaults to an empty, error-free listing when ListFn is unset — unlike this file's other
// methods, a non-nil svc's List is reached incidentally by app.go's own relist plumbing
// (doAbandon, matrix.ResumeMsg's own path, every pop back to the matrix) even in tests whose
// whole point is a different method entirely (Abandon, Resume) — panicking here would make every
// one of those tests responsible for wiring a listing it never asked to exercise.
func (f *fakeService) List(ctx context.Context, _ service.ListOpts) ([]service.Listed, error) {
	if f.ListFn == nil {
		return nil, nil
	}
	return f.ListFn(ctx)
}

func (f *fakeService) Resume(ctx context.Context, id string, _ service.ResumeOpts) (service.Drive, error) {
	if f.ResumeFn == nil {
		panic("fakeService: Resume called but ResumeFn not set")
	}
	state, driver, err := f.ResumeFn(ctx, id)
	if err != nil {
		return nil, err
	}
	return &stateDrive{state: state, inner: driver}, nil
}

func (f *fakeService) Abandon(ctx context.Context, id string) ([]string, error) {
	if f.AbandonFn == nil {
		panic("fakeService: Abandon called but AbandonFn not set")
	}
	if err := f.AbandonFn(ctx, id); err != nil {
		return nil, err
	}
	return nil, nil
}

func (f *fakeService) RefreshRepo(ctx context.Context) (service.RepoView, error) {
	if f.RefreshFn == nil {
		panic("fakeService: RefreshRepo called but RefreshFn not set")
	}
	return f.RefreshFn(ctx)
}

func (f *fakeService) Repo() service.RepoView {
	if f.RepoFn == nil {
		panic("fakeService: Repo called but RepoFn not set")
	}
	return f.RepoFn()
}
