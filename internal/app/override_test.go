package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
)

// recordingDrive is a session.Driver that remembers its own state each time Step is called — the
// only channel through which an override can reach CIGreenStep's own contract in this fake. It
// answers the way the real engine would under ci.none: prompt: Blocked on the ci.none reason
// until the state carries CINoneOverride, satisfied after. state is the Driver's own state,
// exactly as a real service.Driver holds one internally rather than taking it as a Step
// parameter (see session.Driver's own doc comment) — the test seeds it once, at construction,
// with the real promotion id: session.Controller.onBuilt reads e.id from THIS state (d.State()),
// never from whatever engine.PromotionState the fake Start closure separately returns, so a test
// that leaves state's ID empty would attach the flight screen (and every session.Controller
// lookup keyed by id) to "" instead of the real promotion.
type recordingDrive struct {
	mu      sync.Mutex
	seen    []engine.PromotionState
	blocked string
	state   engine.PromotionState
}

func (r *recordingDrive) Step(context.Context) (service.Tick, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, r.state)
	if r.state.CINoneOverride {
		obs := engine.Observation{Satisfied: true, Detail: "overridden"}
		return service.Tick{State: r.state, Statuses: []engine.StepStatus{{Step: engine.StepCIGreen, Observation: obs}}}, nil
	}
	obs := engine.Observation{Blocked: r.blocked}
	// Tick.Blocked (not just the status's own Observation.Blocked string) is what
	// session.Controller.onStep reads to decide Phase == Stopped (internal/app/session/
	// controller.go's own onStep) — the real service.Driver.Step always populates both
	// together; this fake must too, or the controller never stops polling even though the
	// rendered rows already show the block.
	return service.Tick{
		State:    r.state,
		Statuses: []engine.StepStatus{{Step: engine.StepCIGreen, Observation: obs}},
		Blocked:  &engine.BlockedError{Step: engine.StepCIGreen, Reason: r.blocked},
	}, nil
}

func (r *recordingDrive) State() engine.PromotionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *recordingDrive) OverrideCINone() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.CINoneOverride = true
}

func (r *recordingDrive) last() (engine.PromotionState, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return engine.PromotionState{}, 0
	}
	return r.seen[len(r.seen)-1], len(r.seen)
}

// ciNoneBlocked is CIGreenStep's own ci.none=prompt reason, produced by the step rather than
// copied, so the test fails if the wording drifts from what the flight screen recognises.
func ciNoneBlocked(t *testing.T, id string) string {
	t.Helper()
	st := &engine.PromotionState{ID: id, CINone: "prompt", PR: &forge.PR{Number: 7, CreatedAt: time.Now().Add(-time.Hour)}, PushedSHA: "abc"}
	obs, err := engine.CIGreenStep{Forge: &forge.Fake{}}.Observe(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.IsCINonePromptBlock(obs.Blocked) {
		t.Fatalf("fixture precondition: expected the ci.none=prompt block, got %+v", obs)
	}
	return obs.Blocked
}

// blockedOnCINoneAtRoot drives a real StartMsg through session.Controller (attach, then one real
// Step call against drv) so the root's flight screen reaches the state the first poll leaves it
// in — here, stopped on the ci.none block — the way a running program would, rather than poking
// internal fields.
func blockedOnCINoneAtRoot(t *testing.T, drv *recordingDrive) tea.Model {
	t.Helper()
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return drv.state, drv, nil
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd)
	mm = stepOnce(t, mm, stepCmd)
	return tea.Model(mm)
}

// TestFlightOverrideCINoneMsgRedrivesThatPromotion: the root answers OverrideCINoneMsg by asking
// session.Controller.OverrideCINone for this promotion — the next Step call carries
// CINoneOverride — and refuses (with a notice, no drive) a message naming any other promotion.
func TestFlightOverrideCINoneMsgRedrivesThatPromotion(t *testing.T) {
	drv := &recordingDrive{blocked: ciNoneBlocked(t, "abcd1234"), state: engine.PromotionState{ID: "abcd1234"}}
	root := blockedOnCINoneAtRoot(t, drv)
	// The first poll blocked on the ci.none reason; the override's re-drive is the next call.
	if seen, n := drv.last(); n != 1 || seen.CINoneOverride {
		t.Fatalf("setup: %d drive calls before the override (override=%v), want one without it", n, seen.CINoneOverride)
	}
	if !strings.Contains(plain(root), "c treat as green") {
		t.Fatalf("setup: the flight screen should offer c:\n%s", plain(root))
	}

	m, cmd := root.Update(flight.OverrideCINoneMsg{ID: "other-promotion"})
	if cmd != nil {
		t.Error("a message naming a promotion that is not tracked must not drive anything")
	}
	if got := latestActivityText(m.(Model)); !strings.Contains(got, "other-promotion") {
		t.Errorf("the latest activity entry should name the ignored promotion, got %q", got)
	}
	if _, n := drv.last(); n != 1 {
		t.Fatalf("%d drive calls after an ignored override, want the setup's one", n)
	}

	m, cmd = root.Update(flight.OverrideCINoneMsg{ID: "abcd1234"})
	if cmd == nil {
		t.Fatal("the override produced no re-drive command")
	}
	stepMsg := firstStepOfPokeBatch(t, cmd)()
	m, _ = m.Update(stepMsg)
	seen, n := drv.last()
	if n != 2 {
		t.Fatalf("drive calls = %d, want the setup's one plus exactly one re-drive", n)
	}
	if !seen.CINoneOverride {
		t.Error("the re-drive's state did not carry CINoneOverride — the engine step would never see it")
	}
	if seen.ID != "abcd1234" {
		t.Errorf("re-drove %q, want the named promotion", seen.ID)
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Errorf("the flight screen should stay on top: stack has %d screens, want 2", n)
	}
}
