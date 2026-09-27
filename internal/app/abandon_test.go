package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
)

// idleDriver answers Step once with a long wait and never blocks — the fixture every abandon
// test below attaches through the real Start flow with, so the entry settles at Busy=false (a
// Step call is not outstanding) before AbandonMsg is sent, matching the "operator watching an
// idle promotion presses X" shape these tests actually mean to cover. See idleAttached.
type idleDriver struct{ id string }

func (d idleDriver) Step(context.Context) (service.Tick, error) {
	return service.Tick{State: engine.PromotionState{ID: d.id}, Wait: time.Hour}, nil
}
func (d idleDriver) State() engine.PromotionState { return engine.PromotionState{ID: d.id} }
func (d idleDriver) OverrideCINone()              {}

// idleAttached runs a real StartMsg through session.Controller (attach, then one real Step call)
// so the returned Model's flight screen is attached to id and settled at Busy=false.
func idleAttached(t *testing.T, svc *fakeService, id string) Model {
	t.Helper()
	svc.startFn = func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: id}, idleDriver{id: id}, nil
	}
	m := sizedWithService(t, svc, Promotion{}).(Model)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd)
	mm = stepOnce(t, mm, stepCmd)
	return mm
}

// TestFlightAbandonMsgReturnsToMatrixAndCallsAbandonFn: the flight screen's own X gesture
// already confirmed the operator wants this (flight.AbandonMsg's own doc comment) — the root
// pops back to the matrix immediately and session.Controller.Abandon fires the real AbandonFn as
// a command; once that resolves, the notice reports the outcome.
func TestFlightAbandonMsgReturnsToMatrixAndCallsAbandonFn(t *testing.T) {
	var gotID string
	svc := &fakeService{AbandonFn: func(_ context.Context, id string) error {
		gotID = id
		return nil
	}}
	mm := idleAttached(t, svc, "abcd1234")

	tm, cmd := tea.Model(mm).Update(flight.AbandonMsg{ID: "abcd1234"})
	root := tm.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("AbandonMsg should return to the matrix immediately: stack has %d screens, want 1", n)
	}
	if cmd == nil {
		t.Fatal("AbandonMsg produced no command — the real AbandonFn was never called")
	}
	tm2, _ := root.Update(cmd())
	root = tm2.(Model)
	if gotID != "abcd1234" {
		t.Errorf("AbandonFn called with id %q, want abcd1234", gotID)
	}
	if got := latestActivityText(root); !strings.Contains(got, "abandoned abcd1234") {
		t.Errorf("latest activity entry = %q, want it to report the successful abandon", got)
	}
}

// TestFlightAbandonMsgFailureShowsNotice: AbandonFn's own refusal (e.g. cmd/hoist's real
// implementation refusing an already-landed promotion) surfaces as a notice, never a panic or a
// silently-dropped error.
func TestFlightAbandonMsgFailureShowsNotice(t *testing.T) {
	svc := &fakeService{AbandonFn: func(context.Context, string) error {
		return errors.New("abcd1234 has already landed; abandoning is not a rollback")
	}}
	mm := idleAttached(t, svc, "abcd1234")

	tm, cmd := tea.Model(mm).Update(flight.AbandonMsg{ID: "abcd1234"})
	root := tm.(Model)
	if cmd == nil {
		t.Fatal("AbandonMsg produced no command")
	}
	tm2, _ := root.Update(cmd())
	root = tm2.(Model)
	if got := latestActivityText(root); !strings.Contains(got, "abandon abcd1234 failed") || !strings.Contains(got, "not a rollback") {
		t.Errorf("latest activity entry = %q, want it to report the real failure", got)
	}
}

// TestFlightAbandonMsgWithoutHandlerShowsNotice: with nothing tracked at all (no backend wired,
// sized(t)'s own default), AbandonMsg still pops back to the matrix (the operator already
// confirmed through the X gesture) but session.Controller.Abandon on an id it never tracked is a
// silent no-op — there is nothing here to abandon, so nothing is asked of a backend that was
// never wired in, mirroring every other unwired-adaptor convention in this package.
func TestFlightAbandonMsgWithoutHandlerShowsNotice(t *testing.T) {
	root := sized(t).(Model)
	tm, cmd := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = tm.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("AbandonMsg should still return to the matrix: stack has %d screens, want 1", n)
	}
	if cmd != nil {
		t.Error("AbandonMsg produced a command with nothing tracked")
	}
}

// TestFlightAbandonMsgWaitsForBusyStepBeforeAbandoning is the round-2 review finding against PR
// #182, re-proved against session.Controller.Abandon (Train 2 design): a plain context cancel is
// not a join, so Abandon must wait for a busy Step to actually notice and return before ever
// calling the real AbandonFn — otherwise the delete-the-state-file (or close-the-PR) write could
// race a canceled Step's own last save. The fixture's driver blocks forever, so the wait must
// eventually give up (session.Config's own AbandonTimeout) and proceed anyway — the real
// AbandonFn re-observes before touching anything, which is what keeps that safe.
func TestFlightAbandonMsgWaitsForBusyStepBeforeAbandoning(t *testing.T) {
	gotErr := make(chan error, 1)
	abandoned := make(chan string, 1)
	hung := funcDriver{
		StepFunc: func(ctx context.Context) (service.Tick, error) {
			<-ctx.Done()
			gotErr <- ctx.Err()
			return service.Tick{}, ctx.Err()
		},
		// See app_test.go's own comment on this same shape: stateDrive.State() passes straight
		// through to this, and session.Controller.onBuilt reads the promotion's real id from
		// exactly that — leaving it nil would attach the entry to id "" instead of "abcd1234".
		StateFunc: func() engine.PromotionState { return engine.PromotionState{ID: "abcd1234"} },
	}
	svc := &fakeService{
		startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			return engine.PromotionState{ID: "abcd1234"}, hung, nil
		},
		AbandonFn: func(_ context.Context, id string) error { abandoned <- id; return nil },
	}
	m := sizedWithService(t, svc, Promotion{}).(Model)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd) // Busy=true: onBuilt's own first Step is outstanding
	go stepCmd()

	root, waitCmd := tea.Model(mm).Update(flight.AbandonMsg{ID: "abcd1234"})
	if n := len(root.(Model).stack); n != 1 {
		t.Fatalf("AbandonMsg pops to the matrix immediately regardless of Busy: stack has %d, want 1", n)
	}
	if waitCmd == nil {
		t.Fatal("AbandonMsg produced no wait command")
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Abandon did not cancel the busy Step within 2s")
	}

	// Drive the wait loop until it gives up and proceeds (session.Config's own default
	// AbandonTimeout/AbandonWait bound how many iterations this takes).
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case id := <-abandoned:
			if id != "abcd1234" {
				t.Fatalf("AbandonFn called with %q, want abcd1234", id)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("AbandonFn was never called within 2s of the busy Step being cancelled")
		}
		root, waitCmd = root.Update(waitCmd())
		if waitCmd == nil {
			t.Fatal("the abandon wait/dispatch chain produced no further command before AbandonFn was called")
		}
	}
}

// TestAbandonDuringBusyStepCannotBeOutlived drives the X gesture through real keypresses
// (X, y, enter) rather than injecting flight.AbandonMsg directly (every other abandon test above
// does that), then proves the entry cannot be "outlived" by a race: R (session.ReobserveMsg),
// sent while the wait is still watching a busy Step, is refused outright (ErrBusy, from
// session.Controller.Poke's own Abandoning guard) with a notice naming the promotion and no new
// Step call — the attacker this guards against is a re-observe racing the abandon's own wait, once
// re-armed a Step could be outstanding again the moment Backend.Abandon also runs. Only after the
// busy Step actually returns does the wait proceed, and Backend.Abandon is called exactly once.
func TestAbandonDuringBusyStepCannotBeOutlived(t *testing.T) {
	release := make(chan struct{})
	var stepCalls int
	abandoned := make(chan string, 1)
	hung := funcDriver{
		StepFunc: func(ctx context.Context) (service.Tick, error) {
			stepCalls++
			select {
			case <-ctx.Done():
				return service.Tick{}, ctx.Err()
			case <-release:
				return service.Tick{State: engine.PromotionState{ID: "abcd1234"}, Done: true}, nil
			}
		},
		// See app_test.go's own comment on this same shape: stateDrive.State() passes straight
		// through to this, and session.Controller.onBuilt reads the promotion's real id from
		// exactly that.
		StateFunc: func() engine.PromotionState { return engine.PromotionState{ID: "abcd1234"} },
	}
	svc := &fakeService{
		startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			return engine.PromotionState{ID: "abcd1234"}, hung, nil
		},
		AbandonFn: func(_ context.Context, id string) error { abandoned <- id; return nil },
	}
	m := sizedWithService(t, svc, Promotion{}).(Model)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd) // Busy=true: onBuilt's own first Step is outstanding
	go stepCmd()

	// X, y, enter: the flight screen's own keypress-then-confirm gesture, driven through real
	// keys rather than the AbandonMsg every other test in this file injects directly.
	var top tea.Model = mm
	top, _ = press(t, top, tea.KeyPressMsg{Code: 'X', Text: "X"})
	if !strings.Contains(plain(top), "Abandon promotion") {
		t.Fatalf("setup: X did not open the abandon confirm dialog:\n%s", plain(top))
	}
	top, _ = press(t, top, tea.KeyPressMsg{Code: 'y', Text: "y"})
	top, cmd2 := press(t, top, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd2 == nil {
		t.Fatal("enter on the abandon confirm produced no command")
	}
	top, waitCmd := top.Update(cmd2()) // AbandonMsg reaches the root's own handler
	root := top.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("Abandon should return to the matrix immediately: stack has %d screens, want 1", n)
	}
	if waitCmd == nil {
		t.Fatal("Abandon produced no wait command while the Step is busy")
	}

	// R, sent directly (the flight screen is already gone — the root's own case answers it
	// regardless): refused while Abandoning, no new Step, and the operator sees why.
	before := stepCalls
	top2, cmd3 := root.Update(flight.ReobserveMsg{ID: "abcd1234"})
	root2 := top2.(Model)
	if cmd3 != nil {
		t.Error("R produced a command while the promotion is abandoning")
	}
	if got := latestActivityText(root2); !strings.Contains(got, "abcd1234") {
		t.Errorf("latest activity entry = %q, want it to name the refused promotion", got)
	}
	if stepCalls != before {
		t.Errorf("Step called %d additional time(s) after the refused R, want 0", stepCalls-before)
	}

	// Release the busy Step: the wait loop notices it returned and proceeds to the real
	// AbandonFn exactly once.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case id := <-abandoned:
			if id != "abcd1234" {
				t.Fatalf("AbandonFn called with %q, want abcd1234", id)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("AbandonFn was never called within 2s of the busy Step being released")
		}
		var tm tea.Model
		tm, waitCmd = root2.Update(waitCmd())
		root2 = tm.(Model)
		if waitCmd == nil {
			t.Fatal("the abandon wait/dispatch chain produced no further command before AbandonFn was called")
		}
	}
}
