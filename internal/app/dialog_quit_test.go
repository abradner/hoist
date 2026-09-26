package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
)

// quits reports whether cmd yields tea.QuitMsg — the root's own answer to q.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// pressRoot types one key through the root's Update, the way the runtime would.
func pressRoot(m tea.Model, k string) (tea.Model, tea.Cmd) {
	return m.Update(uitest.Key(k))
}

// TestQuitKeyWhileFlightOverrideDialogIsOpenDoesNotQuit is the composition the flight
// screen's own CapturesText test cannot prove: with the flight screen on top, blocked on
// ci.none and its c dialog up, q through the ROOT is handed to the dialog rather than
// treated as the global quit — an operator mid-decision cannot quit the program by
// mistake. Positive control: the same q, with no dialog up, quits.
func TestQuitKeyWhileFlightOverrideDialogIsOpenDoesNotQuit(t *testing.T) {
	drv := &recordingDrive{blocked: ciNoneBlocked(t, "abcd1234"), state: engine.PromotionState{ID: "abcd1234"}}
	tm := blockedOnCINoneAtRoot(t, drv)
	if !strings.Contains(plain(tm), "c treat as green") {
		t.Fatalf("setup: the flight screen should offer c:\n%s", plain(tm))
	}
	if _, cmd := pressRoot(tm, "q"); !quits(cmd) {
		t.Fatal("positive control: q with no dialog open must quit")
	}

	tm, _ = pressRoot(tm, "c")
	if !strings.Contains(plain(tm), "treat no checks as green") {
		t.Fatalf("c did not open the flight dialog:\n%s", plain(tm))
	}
	tm, cmd := pressRoot(tm, "q")
	if quits(cmd) {
		t.Fatal("q quit the program while the flight screen's c dialog was open")
	}
	if !tm.(Model).capturesText() {
		t.Error("the flight dialog closed on q")
	}
	if got := plain(tm); !strings.Contains(got, "treat no checks as green") {
		t.Errorf("the dialog should still be drawn after q:\n%s", got)
	}
}

// TestQuitKeyWhilePlanOverrideDialogIsOpenDoesNotQuit: the same composition for the plan
// screen's o dialog, a text input that takes q as a character. Positive control: q on the
// ready plan screen with no dialog up quits.
func TestQuitKeyWhilePlanOverrideDialogIsOpenDoesNotQuit(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	planFn := plan.Func(func(_ context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		pl, err := gitops.BuildPlanWith(req.Repo, req.Source, req.Target, []string{"ghcr.io/"}, req.Overrides, nil)
		if err != nil {
			return service.PlannedChange{}, err
		}
		return service.PlannedChange{Plan: pl, Repo: req.Repo}, nil
	})
	pm := plan.New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, planFn, history.Funcs{})
	pm = uitest.Drain(pm, pm.Init(), plan.Model.Update)
	root := sized(t).(Model).push(planScreen{pm})
	var tm tea.Model = root
	if _, cmd := pressRoot(tm, "q"); !quits(cmd) {
		t.Fatal("positive control: q on the plan screen with no dialog open must quit")
	}

	tm, _ = pressRoot(tm, "o")
	if !strings.Contains(plain(tm), "override the digest for") {
		t.Fatalf("o did not open the plan override dialog:\n%s", plain(tm))
	}
	tm, cmd := pressRoot(tm, "q")
	if quits(cmd) {
		t.Fatal("q quit the program while the plan screen's o dialog was open")
	}
	if !tm.(Model).capturesText() {
		t.Error("the plan override dialog closed on q")
	}
	if got := plain(tm); !strings.Contains(got, "override the digest for") {
		t.Errorf("the dialog should still be drawn after q:\n%s", got)
	}
}

// TestQuitWithRunningDriveAsksFirst is Train 2 design PR 3's own quit gate: q with at least one
// drive running here (session.Controller.AnyRunning) must ask before quitting rather than stop
// every one of them silently — the same keypress-then-huh.Confirm shape every other destructive
// gesture in this package uses. n keeps everything running (and the dialog closes without
// touching the session at all); y calls StopAll and actually quits, cancelling the drive's own
// ctx. Control: q with nothing running quits immediately, no dialog — the pre-existing
// TestQuitKeys already covers that shape on a boot-time root with no backend at all; this control
// re-proves it on a root that HAS a backend wired but nothing in flight, so the two cases can't be
// confused.
func TestQuitWithRunningDriveAsksFirst(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := funcDriver{
		StepFunc: func(ctx context.Context) (service.Tick, error) {
			<-ctx.Done()
			gotErr <- ctx.Err()
			return service.Tick{}, ctx.Err()
		},
		StateFunc: func() engine.PromotionState { return engine.PromotionState{ID: "abcd1234"} },
	}
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: "abcd1234"}, hung, nil
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd) // Busy=true: onBuilt's own first Step is outstanding
	go stepCmd()
	if !mm.sess.AnyRunning() {
		t.Fatal("setup: the attached entry must count as running")
	}

	var top tea.Model = mm
	top, cmd2 := pressRoot(top, "q")
	if quits(cmd2) {
		t.Fatal("q quit immediately with a drive running — it must ask first")
	}
	if !strings.Contains(plain(top), "Quit hoist?") {
		t.Fatalf("q did not open the quit confirm dialog:\n%s", plain(top))
	}

	// n, enter: decline. Nothing stops, nothing quits.
	top, _ = pressRoot(top, "n")
	top, cmd3 := pressRoot(top, "enter")
	if quits(cmd3) {
		t.Fatal("declining the quit confirm still quit")
	}
	if !top.(Model).sess.Running("abcd1234") {
		t.Fatal("declining the quit confirm must leave the drive running")
	}
	select {
	case err := <-gotErr:
		t.Fatalf("declining the quit confirm cancelled the drive's ctx: %v", err)
	case <-time.After(100 * time.Millisecond):
		// No cancellation observed — correct.
	}

	// q, y, enter: confirm. Every drive stops (StopAll) and the program actually quits.
	top, cmd4 := pressRoot(top, "q")
	if quits(cmd4) {
		t.Fatal("re-opening the quit confirm quit immediately")
	}
	top, _ = pressRoot(top, "y")
	top, cmd5 := pressRoot(top, "enter")
	if !quits(cmd5) {
		t.Fatal("confirming the quit confirm did not quit")
	}
	if top.(Model).sess.Running("abcd1234") {
		t.Error("confirming quit must drop the entry from session.Controller (StopAll)")
	}
	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driver's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirming quit did not cancel the running drive's ctx within 2s")
	}
}

// TestQuitWithNoRunningDriveQuitsImmediately is TestQuitWithRunningDriveAsksFirst's own control,
// on a root that HAS a backend wired (unlike TestQuitKeys' boot-time root) but nothing in flight:
// q must still quit immediately, with no dialog, so AnyRunning (not merely "a backend exists") is
// what gates the confirm.
func TestQuitWithNoRunningDriveQuitsImmediately(t *testing.T) {
	m := sizedWithPromotion(t, testPromo{})
	if m.(Model).sess.AnyRunning() {
		t.Fatal("setup: nothing should be running yet")
	}
	_, cmd := pressRoot(m, "q")
	if !quits(cmd) {
		t.Fatal("q with nothing running must quit immediately")
	}
}
