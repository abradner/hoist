package app

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

const (
	fixtureRoot = "../../testdata/repo"
	width       = 80
	height      = 24
)

// sized returns the root model after Init and the first WindowSizeMsg, as a running
// program would deliver them — no terminal involved. Promotion is the zero value: Start and
// OpenURL both nil, matching a caller that hasn't wired cmd/hoist's real adaptors in yet (see
// TestStartMsgWithNoStartPromotionShowsNotice and TestFlightOpenPRMsgShowsNotice below).
func sized(t *testing.T) tea.Model {
	t.Helper()
	return sizedWithPromotion(t, Promotion{})
}

// sizedWithPromotion is sized's general form, for tests that need a fake Start/OpenURL wired
// in without cmd/hoist's own pkg/git/pkg/forge adaptors (this package must never import
// those — AGENTS.md §4.8).
func sizedWithPromotion(t *testing.T, promo Promotion) tea.Model {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, promo, nil, apprestart.Funcs{})
	_ = m.Init()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return tm
}

// extractBuildCmd unwraps the tea.Batch(fs.Init(), buildCmd) shape plan.StartMsg/deploy.StartMsg
// now return (the preflight phase, #PR2 — the building flight screen is pushed on the
// keypress itself, not once the build finishes) and returns buildCmd itself, uncalled — never
// fs.Init(), which starts flight.Model's own progressCh listener (listenCmd) and would block
// forever receiving from a test channel nothing sends on or closes. buildCmd is always the
// batch's last element by construction (app.go's plan.StartMsg/deploy.StartMsg build it that
// way); a test that changes that order needs to update this comment along with it, per
// AGENTS.md §10 meta-rule 2. Returned uncalled so a caller that needs to run it in its own
// goroutine (a test proving cancellation actually reaches the hung call) can.
func extractBuildCmd(t *testing.T, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("command yields %T, want tea.BatchMsg(fs.Init(), buildCmd)", msg)
	}
	last := batch[len(batch)-1]
	if last == nil {
		t.Fatal("batch's last element (expected buildCmd) is nil")
	}
	return last
}

// buildResultFrom is extractBuildCmd plus running it, for the common case of a test that just
// wants the eventual promotionBuiltMsg.
func buildResultFrom(t *testing.T, cmd tea.Cmd) promotionBuiltMsg {
	t.Helper()
	pbm, ok := extractBuildCmd(t, cmd)().(promotionBuiltMsg)
	if !ok {
		t.Fatal("buildCmd did not yield a promotionBuiltMsg")
	}
	return pbm
}

func press(t *testing.T, m tea.Model, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(k)
}

// runBatch invokes cmd the way the real bubbletea runtime would, one level deep: calling a
// tea.Cmd directly (a plain func call, no runtime involved) only ever runs the func itself —
// for a tea.Batch this returns a tea.BatchMsg (a slice of the cmds it wraps) without ever
// invoking any of them, since unwrapping and dispatching a batch is normally the runtime's job.
// flight.Model.Init returns tea.Batch(spinner.Tick, driveCmd()), so calling it directly never
// actually starts the driveCmd goroutine a test needs running. This runs cmd, and if the result
// is a BatchMsg, runs every sub-cmd in its own goroutine too — enough to exercise a real
// in-flight driveCmd without pulling in the whole runtime.
func runBatch(cmd tea.Cmd) {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				go sub()
			}
		}
	}
}

func plain(m tea.Model) string { return ansi.Strip(m.View().Content) }

// pressD presses d on the matrix and, when the cell has several first-party images (the
// fixture's first family, counta, has two), accepts the chooser's first option with enter —
// the same image the pre-M10 "first sorted repo" rule picked silently. Returns the command
// the matrix emitted (matrix.OpenTagsMsg's cmd).
func pressD(t *testing.T, m tea.Model) (tea.Model, tea.Cmd) {
	t.Helper()
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd != nil {
		if _, ok := cmd().(matrix.OpenTagsMsg); ok {
			return m, cmd
		}
	}
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("d, then enter on the chooser, produced no command")
	}
	return m, cmd
}

func TestViewSnapshot(t *testing.T) {
	m := sized(t)
	got := plain(m)
	// "env app-production" rather than the whole status line: a real env name is long, and at
	// this fixture's 80 columns the line truncates its tail. What matters is that the SELECTED
	// env is named at all — it governs every write gesture on this screen and used to appear
	// nowhere — and it is placed first for exactly that reason.
	for _, want := range []string{"FAMILY", "APP-PRODUCTION", "APP-STAGING", "v202602201200", "2 images", "external", "env app-production", "? help"} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	if strings.Contains(got, string(filepath.Separator)+"testdata") || strings.Contains(got, "..") {
		t.Error("view shows a path, not a base name")
	}
	uitest.Golden(t, "matrix-root", m.View().Content, width, height)
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		_, cmd := press(t, sized(t), k)
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: command yields %T, want tea.QuitMsg", k, cmd())
		}
	}
}

func TestMovementKeys(t *testing.T) {
	m := sized(t)
	cursor := func() int { return m.(Model).stack[0].(matrixScreen).Cursor() }
	if cursor() != 0 {
		t.Fatalf("initial cursor %d", cursor())
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if cursor() != 2 {
		t.Errorf("after j, down: cursor %d, want 2", cursor())
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if cursor() != 0 {
		t.Errorf("after k, up, up: cursor %d, want 0 (clamped)", cursor())
	}
}

func TestHelpToggleKeepsHeight(t *testing.T) {
	m := sized(t)
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	v := plain(m)
	if n := len(strings.Split(v, "\n")); n != height {
		t.Errorf("with help: %d lines, want %d", n, height)
	}
	if !strings.Contains(v, "promote to…") {
		t.Errorf("help line missing:\n%s", v)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if v := plain(m); strings.Contains(v, "promote to…") {
		t.Error("help line still shown after second ?")
	}
}

// TestPromotePushesPlanScreen is the second half of issue #2: p on the matrix screen opens
// the plan screen (internal/app/plan) rather than M1's placeholder notice. The fixture repo
// has no configured envs.pairs, so the plan screen starts in its env-select state, prompting
// for a target among the repo's other envs.
func TestPromotePushesPlanScreen(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenPlanMsg); !ok {
		t.Fatalf("p's command yields %T, want matrix.OpenPlanMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after p, want 2", n)
	}
	if v := plain(m); !strings.Contains(v, "app-production") || !strings.Contains(v, "app-staging") {
		t.Errorf("plan screen view missing the fixture's env names:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the plan screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// TestStartMsgWithNoStartPromotionShowsNotice: a caller that hasn't wired a real
// StartPromotionFunc in (Promotion{} zero value, sized's own default) must show a clear notice
// on confirm rather than pushing a broken flight screen or panicking on a nil call — the same
// nil-adaptor convention plan.ResolveFunc and flight.OpenPRMsg's OpenURL already use.
func TestStartMsgWithNoStartPromotionShowsNotice(t *testing.T) {
	m := sized(t)
	before := len(m.(Model).stack)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("StartMsg with no startPromotion wired produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed from %d to %d screens; an unwired StartMsg must not push", before, n)
	}
	if v := plain(m); !strings.Contains(v, "not wired up") {
		t.Errorf("view missing the not-wired notice:\n%s", v)
	}
}

// TestStartMsgBuildsFlightScreenOnSuccess: plan.StartMsg dispatches the wired
// StartPromotionFunc off the Update call stack (it can talk to a real git remote/forge, so it
// must not run directly inside Update — mirrors plan.ResolveFunc's own loadCmd), and a
// successful promotionBuiltMsg then pushes the flight screen with the real state and
// DriveFunc it returned — no more nil, no more a bare {SourceEnv, TargetEnv}. The fake driveFn
// is a trivial non-nil stub, never nil: a real StartPromotionFunc success always builds one
// (wiring.go's buildStartPromotion never returns a nil driveFn alongside a nil error), and a
// nil driveFn here would now hit the promotionBuiltMsg nil-driveFn guard (Copilot's PR #50
// finding — see TestPromotionBuiltMsgNilDriveFnShowsNotice) instead of exercising this test's
// actual subject, the successful push.
func TestStartMsgBuildsFlightScreenOnSuccess(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	called := false
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		if p.SourceEnv != "app-staging" || p.TargetEnv != "app-production" {
			t.Errorf("startPromotion called with unexpected plan: %+v", p)
		}
		return wantState, stubDriveFn, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens right after StartMsg, want 2 (the building flight screen pushed on the keypress)", n)
	}
	pbm := buildResultFrom(t, cmd)
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	if pbm.err != nil {
		t.Fatalf("unexpected error from a successful startPromotion: %v", pbm.err)
	}
	m, _ = m.Update(pbm)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after a successful promotionBuiltMsg, want 2 (adopted into the same screen, not pushed again)", n)
	}
	if v := plain(m); !strings.Contains(v, "app-staging → app-production") || !strings.Contains(v, wantState.ID) {
		t.Errorf("flight screen view missing the real state's envs/id:\n%s", v)
	}
}

// TestProgressSurvivesFromPreflightThroughDrive is a regression test for a P1 two independent
// adversarial reviews of this same commit found: the build goroutine plan.StartMsg/
// deploy.StartMsg spawn used to close progressCh the instant startPromotion returned
// (`defer close(progressCh)`) — on the wrong assumption that the channel's job ended with
// preflight. But cmd/hoist's real driveFuncFor reuses the SAME progress callback for
// engine.Drive's own per-step save hook (defect B/C: a long single Act streams into the log as
// it happens, not only once the whole Drive call returns) — so the very first real drive call
// after a successful build sent on an already-closed channel, and a send on a closed channel
// panics unconditionally in Go; select/default only guards a full buffer, never a closed one.
// No other test in this file could have caught it: every other fake Start/driveFn pair here
// (stubDriveFn and friends) never calls progress from the driveFn side at all, so the bug's
// actual trigger — the SAME closure called again, later, from a different goroutine, after the
// build's own goroutine returned — never fired. This one does: the fake DriveFunc below calls
// progress from a REAL DriveFunc, driven through the REAL plan.StartMsg → promotionBuiltMsg →
// AdoptBuilt → driveCmd path, the same sequence a real cmd/hoist wiring drives — proving app.go
// itself never closes the channel out from under a drive that is still going to use it.
func TestProgressSurvivesFromPreflightThroughDrive(t *testing.T) {
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts, progress func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		// Preflight: exactly what buildStartPromotion's own report(...) calls do.
		progress("checking your checkout against origin/main")
		progress("claiming " + p.TargetEnv + " and checking for a conflicting promotion")
		return engine.PromotionState{ID: "abcd1234", SourceEnv: p.SourceEnv, TargetEnv: p.TargetEnv},
			func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
				// Drive: exactly what driveFuncFor's own wrapped save does — call the SAME
				// progress closure the preflight above just used, from a call that only
				// happens after the build goroutine that constructed it has already
				// returned. This is the exact shape that panicked.
				progress("branched: acted")
				s.History = append(s.History, engine.HistoryEntry{Step: engine.StepBranched, Detail: "acted"})
				return s, true, nil, nil
			}, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked reaching drive with a live progress callback: %v", r)
			}
		}()
		m, cmd := m.Update(msg)
		pbm := buildResultFrom(t, cmd)
		m, adoptCmd := m.Update(pbm)
		if adoptCmd == nil {
			t.Fatal("promotionBuiltMsg's adopt path produced no command")
		}
		batch, ok := adoptCmd().(tea.BatchMsg)
		if !ok || len(batch) < 2 {
			t.Fatalf("AdoptBuilt's command = %#v, want a batch including the drive call at index 1", adoptCmd())
		}
		// index 1: driveCmd — the call that used to panic. Never index 2 (listenCmd): that
		// would block forever on this test's own internal, unreferenced channel.
		driveMsg := batch[1]()
		m, _ = m.Update(driveMsg)
		if v := plain(m); !strings.Contains(v, "acted") {
			t.Errorf("flight screen view missing the real drive result:\n%s", v)
		}
	}()
}

// TestPromotionBuiltMsgStampsCurrentBuildGen is the direct, narrow check that a StartMsg's
// own command stamps promotionBuiltMsg with the Model's buildGen at the moment it was issued —
// mirrors TestDriveCmdStampsCurrentGen at the flight layer (internal/app/flight/model_test.go),
// one layer up the stack, guarding the build step instead of the drive step.
func TestPromotionBuiltMsgStampsCurrentBuildGen(t *testing.T) {
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	pbm := buildResultFrom(t, cmd)
	if pbm.gen != m.(Model).buildGen {
		t.Errorf("promotionBuiltMsg.gen = %d, want %d (m.buildGen)", pbm.gen, m.(Model).buildGen)
	}
}

// TestStalePromotionBuiltMsgFromBackedOutPlanIsDropped is PR #50 round-4 review finding #4
// (Codex), updated for the preflight phase (#PR2): StartMsg now pushes a building flight
// screen on the keypress itself, so the plan screen is no longer on top and the operator's
// Esc is real UI now routes to flight.BackMsg, not plan.BackMsg — the building screen's own
// handler (app.go's flight.BackMsg case) is what bumps buildGen and cancels the outstanding
// build, mirroring what plan.BackMsg used to do for the pre-preflight design. Without that
// generation check, the promotionBuiltMsg would still be adopted unconditionally once it
// landed — resurrecting a flight screen (which immediately starts driving: committing,
// pushing, opening a PR) for a plan the operator already backed out of. This proves the stale
// result is dropped: the stack stays on the plan screen it returned to, and nothing new is
// pushed or adopted.
func TestStalePromotionBuiltMsgFromBackedOutPlanIsDropped(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		return wantState, stubDriveFn, nil
	}}
	m := sizedWithPromotion(t, promo)

	// Push the plan screen (mirrors TestPromotePushesPlanScreen).
	m, _ = m.Update(matrix.OpenPlanMsg{Source: "app-staging"})
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after opening the plan screen, want 2", n)
	}

	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	if n := len(m.(Model).stack); n != 3 {
		t.Fatalf("stack has %d screens right after StartMsg, want 3 (matrix, plan, the building flight screen)", n)
	}
	pbm := buildResultFrom(t, cmd) // the request completes...

	// ...but before its result is delivered, the operator backs out of the building flight
	// screen (Esc — flight.Model emits BackMsg for this key regardless of screen state).
	m, _ = m.Update(flight.BackMsg{})
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("flight.BackMsg left stack at %d screens, want 2 (popped back to the plan screen)", n)
	}

	m, cmd = m.Update(pbm)
	if cmd != nil {
		t.Errorf("a stale promotionBuiltMsg produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Errorf("stack changed to %d screens processing a stale promotionBuiltMsg, want unchanged at 2 (no flight screen adopted or pushed for an abandoned plan)", n)
	}
}

// TestStalePromotionBuiltMsgFromSupersededStartMsgIsDropped is PR #50 round-4 review finding
// #4 (Codex), updated for the preflight phase (#PR2). Through real UI interaction this
// scenario can no longer arise the way it originally did — the plan screen is buried under
// the first StartMsg's own building flight screen the instant it fires, so it no longer
// receives keys and cannot itself emit a second StartMsg — but the invariant this test proves
// (buildGen decides which result is adopted, not delivery order) still has to hold for
// whatever DOES call Update with a second StartMsg while a first is outstanding, and
// popIfBuilding's own job (plan.StartMsg's comment) is exactly to stop that second call from
// stacking a second building screen on top of the first rather than replacing it — this test
// is what proves that: only ONE flight screen ever exists at a time across both requests, and
// it always ends up showing the current (second) one's state, never the superseded first.
func TestStalePromotionBuiltMsgFromSupersededStartMsgIsDropped(t *testing.T) {
	first := engine.PromotionState{ID: "first-request", SourceEnv: "app-staging", TargetEnv: "app-production"}
	second := engine.PromotionState{ID: "second-request", SourceEnv: "app-staging", TargetEnv: "app-production"}
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	calls := 0
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		calls++
		if calls == 1 {
			return first, stubDriveFn, nil
		}
		return second, stubDriveFn, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}

	// First confirmation: request issued, not yet resolved. Pushes a building screen.
	m, cmd1 := m.Update(msg)
	if cmd1 == nil {
		t.Fatal("first StartMsg produced no command")
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after the first StartMsg, want 2 (matrix, the building flight screen)", n)
	}
	firstResult := buildResultFrom(t, cmd1)

	// Second confirmation, before the first ever resolved: supersedes the first. popIfBuilding
	// (inside the plan.StartMsg case) removes the first building screen before pushing a
	// second — the stack must stay at 2, never grow to 3, or the first would be leaked,
	// buried and invisible underneath.
	m, cmd2 := m.Update(msg)
	if cmd2 == nil {
		t.Fatal("second StartMsg produced no command")
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after the superseding StartMsg, want 2 (the first building screen replaced, not stacked under a second)", n)
	}
	secondResult := buildResultFrom(t, cmd2)

	// The first (now-stale) result arrives first: must be dropped, not adopted.
	before := len(m.(Model).stack)
	m, cmd := m.Update(firstResult)
	if cmd != nil {
		t.Errorf("the stale first result produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Fatalf("stack changed to %d screens processing the stale first result, want unchanged at %d", n, before)
	}

	// The second (current) result arrives: must be adopted into the same screen instance,
	// never pushed again.
	m, _ = m.Update(secondResult)
	if n := len(m.(Model).stack); n != before {
		t.Fatalf("stack has %d screens after the current second result, want unchanged at %d (adopted in place)", n, before)
	}
	if v := plain(m); !strings.Contains(v, second.ID) {
		t.Errorf("flight screen view missing the second request's own state ID %q:\n%s", second.ID, v)
	}
	if v := plain(m); strings.Contains(v, first.ID) {
		t.Errorf("flight screen view shows the superseded first request's state ID %q:\n%s", first.ID, v)
	}
}

// TestStartMsgFiltersToTickedRepos is PR #50 review finding #2: plan.StartMsg.Ticked is the
// repo subset the operator actually left checked (the same set plan.Model.recomputeDiff
// already filters the confirm screen's own diff by), but msg.Plan carries BuildPlan's full,
// unfiltered edit set. Without filterTicked, startPromotion would be called with every edit
// in the plan — including a repo the operator explicitly unticked and never saw in the
// confirmed diff — and would commit it anyway. This proves only the ticked repo's edit
// reaches startPromotion, never the unticked one.
func TestStartMsgFiltersToTickedRepos(t *testing.T) {
	keep := image.Ref{Repo: "ghcr.io/example/keep", Tag: "v2"}
	drop := image.Ref{Repo: "ghcr.io/example/drop", Tag: "v2"}
	edits := []gitops.Edit{
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: keep.Repo, Tag: "v1"}}, New: keep},
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: drop.Repo, Tag: "v1"}}, New: drop},
	}
	var gotPlan gitops.Plan
	called := false
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		gotPlan = p
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production", Edits: edits},
		Mode:   plan.ModePR,
		Ticked: []string{keep.Repo},
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	buildResultFrom(t, cmd)
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	if len(gotPlan.Edits) != 1 {
		t.Fatalf("startPromotion called with %d edits, want exactly 1 (only the ticked repo): %+v", len(gotPlan.Edits), gotPlan.Edits)
	}
	if got := gotPlan.Edits[0].Ref.Repo; got != keep.Repo {
		t.Errorf("startPromotion's one edit is for repo %q, want the ticked repo %q", got, keep.Repo)
	}
}

// TestStartMsgFiltersWarningsToTickedRepos is PR #50 round-4 review finding #3 (Copilot):
// filterTicked narrowed Plan.Edits to the operator's ticked selection but left Plan.Warnings
// untouched, so engine.RenderPRBody (which renders p.Warnings verbatim) could describe a repo
// the operator explicitly unticked and never saw change in the confirmed diff. This proves a
// warning about the dropped repo never reaches startPromotion, a warning about the kept repo
// does, and a warning tied to no repo at all (zero Occurrences — plan.WarningRepo's own "no
// convention produces this today, but nothing forbids it" case) is kept regardless, since
// there is no ticked/unticked repo to test it against.
func TestStartMsgFiltersWarningsToTickedRepos(t *testing.T) {
	keep := image.Ref{Repo: "ghcr.io/example/keep", Tag: "v2"}
	drop := image.Ref{Repo: "ghcr.io/example/drop", Tag: "v2"}
	edits := []gitops.Edit{
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: keep.Repo, Tag: "v1"}}, New: keep},
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: drop.Repo, Tag: "v1"}}, New: drop},
	}
	warnings := []gitops.Warning{
		{Code: "keep-warning", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: keep.Repo}}}},
		{Code: "drop-warning", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: drop.Repo}}}},
		{Code: "no-repo-warning"},
	}
	var gotPlan gitops.Plan
	called := false
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		gotPlan = p
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production", Edits: edits, Warnings: warnings},
		Mode:   plan.ModePR,
		Ticked: []string{keep.Repo},
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	buildResultFrom(t, cmd)
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	var codes []string
	for _, w := range gotPlan.Warnings {
		codes = append(codes, w.Code)
	}
	if len(gotPlan.Warnings) != 2 {
		t.Fatalf("startPromotion called with %d warnings, want exactly 2 (kept repo + no-repo): %v", len(gotPlan.Warnings), codes)
	}
	for _, want := range []string{"keep-warning", "no-repo-warning"} {
		found := false
		for _, c := range codes {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("warnings %v missing %q", codes, want)
		}
	}
	for _, c := range codes {
		if c == "drop-warning" {
			t.Errorf("warnings %v still carry drop-warning for the unticked repo", codes)
		}
	}
}

// TestStartMsgRefusesDirectModeNotWired is PR #50 review finding #3: direct mode's real
// step-selection machinery (M6/PR #43's engine.DirectCommitStep) is not present on this
// branch — buildStartPromotion (cmd/hoist/wiring.go) always builds engine.AllSteps regardless
// of what the operator chose, and StartPromotionFunc's signature carries no Mode at all. A
// StartMsg confirmed with Mode: plan.ModeDirect must therefore never reach startPromotion —
// silently driving PR mode instead would mean the confirm screen told the operator "commit
// straight to the branch, no PR" and then opened one anyway.
// A direct-mode confirm reaches the start function AS direct. The TUI used to refuse this
// outright, because nothing downstream could honour it: StartPromotionFunc had no way to carry
// the choice, and buildStartPromotion always built the PR step list. Confirming would have told
// the operator "commit straight to the branch, no PR" and then opened a PR — so refusing was
// the honest option at the time. StartOpts carries it now, and AllDirectSteps honours it.
//
// Confirmed rides along with Direct because reaching ModeDirect already required the plan
// screen's own keypress-then-huh.Confirm gesture, which is exactly what
// engine.DirectCommitGateStep asks Confirmed to attest — and the gate re-derives the production
// refusal independently regardless.
func TestStartMsgCarriesDirectModeThrough(t *testing.T) {
	var got StartOpts
	called := false
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		called, got = true, opts
		return engine.PromotionState{}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"},
		Mode:   plan.ModeDirect,
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("direct-mode StartMsg produced no command; the confirm should start a promotion")
	}
	buildResultFrom(t, cmd) // the start call happens off the Update stack
	if !called {
		t.Fatal("startPromotion was never called for a direct-mode confirm")
	}
	if !got.Direct {
		t.Error("StartOpts.Direct = false for a ModeDirect confirm: the promotion would open a PR the operator declined")
	}
	if !got.Confirmed {
		t.Error("StartOpts.Confirmed = false: the gate would refuse a gesture the operator actually completed")
	}
}

// The PR path must not accidentally inherit direct mode.
func TestStartMsgPRModeIsNotDirect(t *testing.T) {
	var got StartOpts
	called := false
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		called, got = true, opts
		return engine.PromotionState{}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	_, cmd := m.Update(plan.StartMsg{
		Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"},
		Mode: plan.ModePR, Source: "app-staging", Target: "app-production",
	})
	if cmd == nil {
		t.Fatal("PR-mode StartMsg produced no command")
	}
	buildResultFrom(t, cmd)
	if !called {
		// Direct/Confirmed both default false, the same values a correct PR-mode call
		// itself passes — without this, the assertion below would pass whether or not
		// startPromotion was ever actually called.
		t.Fatal("startPromotion was never called")
	}
	if got.Direct || got.Confirmed {
		t.Errorf("PR mode leaked direct opts: %+v", got)
	}
}

// TestPromotionBuiltMsgNilDriveFnShowsNotice is Copilot's PR #50 review finding: a
// promotionBuiltMsg with err == nil but driveFn == nil (a contract violation by whatever built
// it) must not silently push a read-only flight screen — that would reintroduce exactly the
// pre-wiring stub behavior this PR exists to remove, with no visible sign anything is wrong.
func TestPromotionBuiltMsgNilDriveFnShowsNotice(t *testing.T) {
	m := sized(t)
	before := len(m.(Model).stack)
	msg := promotionBuiltMsg{state: engine.PromotionState{ID: "abcd1234"}, driveFn: nil, err: nil}
	m, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("nil-driveFn promotionBuiltMsg produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed from %d to %d screens; a nil driveFn must not push the flight screen", before, n)
	}
	if v := plain(m); !strings.Contains(v, "no way to drive it") {
		t.Errorf("view missing the nil-driveFn notice:\n%s", v)
	}
}

// TestStartMsgShowsNoticeOnBuildError: buildPromotionForConfirm's own refusals (a real
// in-flight conflict, missing github config, a claim failure) must surface as a notice on the
// screen that popped up plan.StartMsg (plan, still on top — the flight screen is never
// pushed) rather than crashing.
func TestStartMsgShowsNoticeOnBuildError(t *testing.T) {
	wantErr := errors.New("promotion existing-id targeting app-production is still in flight (at pr-opened: open); run `hoist resume existing-id` instead of starting a second one")
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{}, nil, wantErr
	}}
	m := sizedWithPromotion(t, promo)
	before := len(m.(Model).stack)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	// The building flight screen goes up on the keypress itself (#PR2), before the error is
	// known — popIfBuilding (inside promotionBuiltMsg's own error branch) is what takes the
	// stack back to `before` once the error actually arrives, not the absence of a push.
	if n := len(m.(Model).stack); n != before+1 {
		t.Fatalf("stack has %d screens right after StartMsg, want %d (the building flight screen pushed)", n, before+1)
	}
	m, _ = m.Update(buildResultFrom(t, cmd))
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack ended at %d screens after a construction error, want back to %d (the building screen popped)", n, before)
	}
	if v := plain(m); !strings.Contains(v, "still in flight") {
		t.Errorf("view missing the construction-error notice:\n%s", v)
	}
}

// TestStartMsgBoundedByPollDeadline: mirrors flight.Model's own TestDriveCmdBoundedByPollDeadline
// — a startPromotion call that hangs (blocks on ctx.Done() rather than ever returning) must not
// stall the plan screen forever. m.poll.Deadline bounds the call the same way it bounds
// flight.Model.driveCmd's own DriveFunc call, so this returns with ctx's deadline error instead
// of the goroutine blocking indefinitely.
func TestStartMsgBoundedByPollDeadline(t *testing.T) {
	hung := func(ctx context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		<-ctx.Done()
		return engine.PromotionState{}, nil, ctx.Err()
	}
	promo := Promotion{Start: hung, Poll: flight.PollDurations{Deadline: 20 * time.Millisecond}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	buildCmd := extractBuildCmd(t, cmd)
	done := make(chan tea.Msg, 1)
	go func() { done <- buildCmd() }()
	select {
	case built := <-done:
		pbm, ok := built.(promotionBuiltMsg)
		if !ok {
			t.Fatalf("command yields %T, want promotionBuiltMsg", built)
		}
		if !errors.Is(pbm.err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", pbm.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartMsg's command did not return within 2s of a 20ms poll.Deadline — a hung startPromotion call can still stall the plan screen forever")
	}
}

// TestBackingOutCancelsOutstandingBuild is Copilot's PR #50 final-round finding: buildGen alone
// (the pre-existing guard) only ever stops an abandoned build's eventual RESULT from being
// acted on — it does nothing to the goroutine itself, which used to run to completion
// regardless, bounded only by poll.Deadline (often hours), potentially still claiming and
// persisting a real, orphaned "in-flight" promotion long after the operator backed out and
// moved on to something else. This proves plan.BackMsg actually interrupts the outstanding
// startPromotion call's own context, not just its result.
func TestBackingOutCancelsOutstandingBuild(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, nil, ctx.Err()
	}
	promo := Promotion{Start: hung}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2 (matrix, the building flight screen)", n)
	}
	go extractBuildCmd(t, cmd)()

	// Esc on the building flight screen (real UI routes here now, not plan.BackMsg — see
	// flight.BackMsg's own handler, which is what actually reaches m.buildCancel for a
	// screen still in its preflight phase).
	m, _ = m.Update(flight.BackMsg{})
	if m.(Model).buildCancel != nil {
		t.Error("buildCancel should be cleared after flight.BackMsg cancels it")
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("build's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backing out did not cancel the outstanding build's context within 2s")
	}
}

// TestSupersedingStartMsgCancelsPreviousBuild is TestBackingOutCancelsOutstandingBuild's sibling
// for the OTHER way a build gets abandoned: a second confirmation (a new plan.StartMsg) before
// the first one's result ever arrives. The first call's own context must be cancelled too, not
// merely left to run out its full poll.Deadline unwatched.
func TestSupersedingStartMsgCancelsPreviousBuild(t *testing.T) {
	firstErr := make(chan error, 1)
	callCount := 0
	promo := Promotion{Start: func(ctx context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		callCount++
		if callCount == 1 {
			<-ctx.Done()
			firstErr <- ctx.Err()
			return engine.PromotionState{}, nil, ctx.Err()
		}
		return engine.PromotionState{ID: "second"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd1 := m.Update(msg)
	if cmd1 == nil {
		t.Fatal("first StartMsg produced no command")
	}
	go extractBuildCmd(t, cmd1)()

	_, cmd2 := m.Update(msg)
	if cmd2 == nil {
		t.Fatal("second StartMsg produced no command")
	}
	// The second call's own result isn't this test's concern, only the first's cancellation.

	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("first build's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a superseding StartMsg did not cancel the first build's context within 2s")
	}
}

// TestFlightScreenSharesBuildDeadlineWithDrive is Copilot's PR #50 round-7 finding, reproved
// against the preflight phase's own mechanism (#PR2): flight.New used to be handed the raw
// m.poll.Deadline and start a FRESH poll.Deadline-length window of its own once CONSTRUCTED —
// and construction used to happen only once the build (this StartMsg's own startPromotion
// call) had already spent some of that SAME configured budget. The fix used to be an explicit
// "how much of poll.Deadline is left" recompute at construction time; now there is nothing to
// recompute, because construction itself (flight.NewBuilding) happens BEFORE the build even
// starts — deadlineAt is fixed once, at the keypress, and AdoptBuilt reuses it unchanged. This
// proves the guarantee still holds under the new mechanism: a build that consumes most of a
// tiny deadline still leaves the drive call that follows bounded by only whatever's left, not
// a fresh full window — the drive call (which blocks forever on its own ctx.Done() otherwise)
// must report context.DeadlineExceeded almost immediately.
func TestFlightScreenSharesBuildDeadlineWithDrive(t *testing.T) {
	const total = 200 * time.Millisecond
	const buildSleep = 150 * time.Millisecond // leaves ~50ms of the budget for drive
	hungDrive := func(ctx context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		return s, false, nil, ctx.Err()
	}
	promo := Promotion{
		Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
			time.Sleep(buildSleep)
			return engine.PromotionState{ID: "abcd1234"}, hungDrive, nil
		},
		Poll: flight.PollDurations{Deadline: total},
	}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	// deadlineAt is fixed HERE, by flight.NewBuilding inside this StartMsg handler — before
	// Start's own buildSleep ever runs.
	m, cmd := m.Update(msg)
	buildCmd := extractBuildCmd(t, cmd)
	pbm := buildCmd() // runs Start's own buildSleep synchronously in this goroutine

	m, adoptCmd := m.Update(pbm)
	if adoptCmd == nil {
		t.Fatal("AdoptBuilt (via promotionBuiltMsg's adopt path) produced no command")
	}
	// 3 commands: spinner tick, drive (index 1 — this test's own concern), and listenCmd
	// (AdoptBuilt keeps the preflight progress listener alive into the drive phase, its own
	// doc comment) — never called here, since this test has no reference to the internal
	// channel plan.StartMsg's own handler constructed, and calling it would block forever on
	// an empty, unclosed channel nothing in this test ever sends on or closes.
	batch, ok := adoptCmd().(tea.BatchMsg)
	if !ok || len(batch) != 3 {
		t.Fatalf("AdoptBuilt's command = %#v, want a 3-command batch (spinner tick, drive, listen)", adoptCmd())
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- batch[1]() }()
	select {
	case driveMsg := <-done:
		m, _ = m.Update(driveMsg)
		if v := plain(m); !strings.Contains(v, "deadline exceeded") {
			t.Errorf("view missing a deadline-exceeded notice after the shared budget ran out:\n%s", v)
		}
	case <-time.After(120 * time.Millisecond): // comfortably above the ~50ms shared remainder,
		// comfortably below a fresh, unshared 200ms window measured from roughly this same point
		t.Fatal("flight screen's own drive call did not report the shared deadline in time — poll.Deadline was NOT shared with the build step")
	}
}

// TestStartMsgErrorNoticeIsRedacted: buildPromotionForConfirm's error can embed a git/forge
// transport message carrying a credential (a token in a remote URL, say) — the root notice must
// scrub it the same way plan.Model.View and flight.Model.View already redact their own rendered
// output, rather than leaking it to the terminal unredacted.
func TestStartMsgErrorNoticeIsRedacted(t *testing.T) {
	const secret = "ghp_totallysecrettoken1234567890"
	redact.Register(secret)
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{}, nil, fmt.Errorf("push failed: authentication using %s rejected", secret)
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	m, _ = m.Update(buildResultFrom(t, cmd))
	v := plain(m)
	if strings.Contains(v, secret) {
		t.Errorf("view leaks the registered secret unredacted:\n%s", v)
	}
	if !strings.Contains(v, redact.Redacted) {
		t.Errorf("view missing %q for the redacted notice:\n%s", redact.Redacted, v)
	}
}

// TestFlightOpenPRMsgShowsNotice: PR #39 review finding #1 — the root previously dropped
// flight.OpenPRMsg silently (no case in Update at all), so pressing o did nothing visible
// once cmd/hoist eventually wires a real DriveFunc in. Until a real URL-opener is wired in,
// the root must show a visible notice naming the URL instead of a silent no-op.
func TestFlightOpenPRMsgShowsNotice(t *testing.T) {
	m := sized(t)
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		t.Error("OpenPRMsg produced a command")
	}
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the open-PR notice:\n%s", v)
	}
}

// TestFlightOpenPRMsgCallsOpenURL: once a real OpenURL is wired in (cmd/hoist's own browser
// opener, in real use), OpenPRMsg must actually call it with the PR's URL instead of showing
// the "not wired yet" notice.
func TestFlightOpenPRMsgCallsOpenURL(t *testing.T) {
	var got string
	promo := Promotion{OpenURL: func(url string) error {
		got = url
		return nil
	}}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	if got != "https://example.invalid/pr/1" {
		t.Errorf("OpenURL called with %q, want the PR URL", got)
	}
	if v := plain(m); strings.Contains(v, "not wired yet") {
		t.Errorf("view still shows the not-wired notice once OpenURL is wired:\n%s", v)
	}
}

// TestFlightOpenPRMsgShowsErrorFromOpenURL: a real OpenURL that fails (no browser found, the
// operator's platform has none) must surface the error as a notice rather than swallow it.
func TestFlightOpenPRMsgShowsErrorFromOpenURL(t *testing.T) {
	promo := Promotion{OpenURL: func(_ string) error {
		return errors.New("no such browser")
	}}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	if v := plain(m); !strings.Contains(v, "no such browser") {
		t.Errorf("view missing the OpenURL error notice:\n%s", v)
	}
}

// TestFlightOpenPRMsgDisplayModeNeverCallsOpenURL: preferences.open_pr: display must always
// just show the URL as text — never attempting a launch at all, so a headless/SSH session with
// no OpenURL wired in whatsoever (m.openURL nil) still shows the URL rather than the "not wired
// yet" notice display mode has no use for.
func TestFlightOpenPRMsgDisplayModeNeverCallsOpenURL(t *testing.T) {
	called := false
	promo := Promotion{
		OpenPRMode: "display",
		OpenURL:    func(_ string) error { called = true; return nil },
	}
	m := sizedWithPromotion(t, promo)
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		t.Error("OpenPRMsg produced a command")
	}
	if called {
		t.Error("display mode called OpenURL — it must never attempt a launch")
	}
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL in display mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgDisplayModeWorksWithNilOpenURL is display mode's own headless-session
// case: no browser opener wired in at all (m.openURL nil, e.g. a real SSH session with nothing
// to launch into) must still show the URL, not the generic "not wired yet" notice launch/both
// modes fall back to for that case.
func TestFlightOpenPRMsgDisplayModeWorksWithNilOpenURL(t *testing.T) {
	promo := Promotion{OpenPRMode: "display"}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	v := plain(m)
	if !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL:\n%s", v)
	}
	if strings.Contains(v, "not wired yet") {
		t.Errorf("view shows the launch-mode not-wired notice in display mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgBothModeShowsURLOnSuccess: preferences.open_pr: both must show the URL as
// text even when the launch itself succeeds — the whole point of "both" over plain "launch" is
// a copy/paste fallback that exists unconditionally, not only on failure.
func TestFlightOpenPRMsgBothModeShowsURLOnSuccess(t *testing.T) {
	promo := Promotion{
		OpenPRMode: "both",
		OpenURL:    func(_ string) error { return nil },
	}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL after a successful launch in both mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgBothModeShowsURLAndErrorOnFailure: both mode's failure path must still
// name the URL (so the operator can act on it manually) alongside the launch error, not just
// the bare error a plain "launch" mode shows.
func TestFlightOpenPRMsgBothModeShowsURLAndErrorOnFailure(t *testing.T) {
	promo := Promotion{
		OpenPRMode: "both",
		OpenURL:    func(_ string) error { return errors.New("no such browser") },
	}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	v := plain(m)
	if !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL after a failed launch in both mode:\n%s", v)
	}
	if !strings.Contains(v, "no such browser") {
		t.Errorf("view missing the launch error in both mode:\n%s", v)
	}
}

// TestFlightAbortMsgReturnsToMatrix: AbortMsg's real engine-level semantics (close the PR?
// delete the branch?) are deliberately out of scope for this brief (see app.go's own comment
// on this case) — the one narrow, safe interpretation implemented is a pure navigation
// reset: drop every screen above the matrix, leaving the real branch/PR/state file untouched.
// This pushes matrix -> plan -> flight (three deep) first, specifically to prove AbortMsg
// resets all the way to the matrix rather than popping only the flight screen back to plan.
func TestFlightAbortMsgReturnsToMatrix(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	m, _ = m.Update(cmd())
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("setup: stack has %d screens after opening plan, want 2", n)
	}
	root := m.(Model)
	root = root.push(flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, nil)})
	m = tea.Model(root)
	if n := len(m.(Model).stack); n != 3 {
		t.Fatalf("setup: stack has %d screens after pushing flight, want 3", n)
	}

	m, abortCmd := m.Update(flight.AbortMsg{ID: "abcd1234"})
	if abortCmd != nil {
		t.Error("AbortMsg produced a command")
	}
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("AbortMsg should return all the way to the matrix: stack has %d screens, want 1", n)
	}
	if v := plain(m); strings.Contains(v, "abcd1234") {
		t.Errorf("matrix view should not mention the aborted promotion's id:\n%s", v)
	}
}

// TestFlightAbortMsgCancelsInFlightDriveCmd is Copilot's PR #50 round-11 finding: popping the
// flight screen used to leave any driveCmd already in flight running to completion, free to
// keep committing, pushing, opening a PR, or merging after the operator had walked away — and
// since the claim was already released once the initial state saved, a later reconfirmation of
// the same deterministic promotion id could start a second driver racing the first. This proves
// AbortMsg actually cancels the popped screen's own drive context, not just its message.
func TestFlightAbortMsgCancelsInFlightDriveCmd(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, false, nil, ctx.Err()
	}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, hung)}
	initCmd := fs.Init()
	if initCmd == nil {
		t.Fatal("setup: flight screen's Init produced no command")
	}
	root = root.push(fs)
	go runBatch(initCmd)

	rootTM, _ := root.Update(flight.AbortMsg{ID: "abcd1234"})
	root = rootTM.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("setup: AbortMsg should return to the matrix: stack has %d screens, want 1", n)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AbortMsg did not cancel the popped flight screen's in-flight driveCmd within 2s")
	}
}

// TestFlightBackMsgCancelsInFlightDriveCmd is TestFlightAbortMsgCancelsInFlightDriveCmd's
// sibling for the other way a flight screen gets popped: pressing Esc (BackMsg), which had the
// exact same gap.
func TestFlightBackMsgCancelsInFlightDriveCmd(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, false, nil, ctx.Err()
	}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, hung)}
	initCmd := fs.Init()
	if initCmd == nil {
		t.Fatal("setup: flight screen's Init produced no command")
	}
	root = root.push(fs)
	go runBatch(initCmd)

	rootTM, _ := root.Update(flight.BackMsg{})
	root = rootTM.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("setup: BackMsg should pop the flight screen: stack has %d screens, want 1", n)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BackMsg did not cancel the popped flight screen's in-flight driveCmd within 2s")
	}
}

// TestRootNoticeClearsOnNextKeypress: the root's own notice is transient, same convention as
// every screen's own notice field — it should not linger forever once the operator moves on.
func TestRootNoticeClearsOnNextKeypress(t *testing.T) {
	m := sized(t)
	m = openPR(t, m)
	if !strings.Contains(plain(m), "not wired yet") {
		t.Fatal("setup: notice not shown after OpenPRMsg")
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if strings.Contains(plain(m), "not wired yet") {
		t.Error("root notice still shown after a later keypress")
	}
}

// TestDeployNewPushesTagsScreen is M6's tag-picker counterpart to TestPromotePushesPlanScreen:
// d on the matrix screen pushes internal/app/tags on top, and esc from there pops back.
func TestDeployNewPushesTagsScreen(t *testing.T) {
	m := sized(t)
	m, cmd := pressD(t, m)
	msg := cmd()
	if _, ok := msg.(matrix.OpenTagsMsg); !ok {
		t.Fatalf("d's command yields %T, want matrix.OpenTagsMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after d, want 2", n)
	}
	// No tagsFn was supplied (sized(t) passes nil), so the picker's own error state shows
	// rather than hanging — proving the nil case is handled, not just the happy path.
	if v := plain(m); !strings.Contains(v, "hoist · deploy") || !strings.Contains(v, "ghcr.io/example/counta  →  app-production") {
		t.Errorf("tags screen view missing its own header:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the tags screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// TestDirectRequestedMsgShowsHonestNotice and TestSelectedMsgShowsHonestNotice are the
// honesty-fix regression tests for a round-N finding: the root used to pop straight back to the
// matrix on tags.SelectedMsg/DirectRequestedMsg with no notice at all — indistinguishable, at a
// glance, from "that worked" — even though neither message is wired to an actual write yet (no
// screen in this codebase drives a real promotion; hoist promote is still CLI-only). Both must
// still pop back to the matrix (unchanged) AND leave a plain, honest notice on it naming what
// was (and wasn't) done. The window is widened past the default 80 columns so the assertions
// aren't fighting the status bar's own truncation (internal/ui.StatusBar) rather than testing
// the notice's content.
// Choosing a tag now opens the deploy confirm screen rather than popping back with a notice
// saying nothing happened. Both picker messages take the same route: the D gesture chooses a
// mode, not a change, so it still has to look at the diff.
func TestSelectedMsgOpensTheDeployConfirmScreen(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := pressD(t, m)
	m, _ = m.Update(cmd())

	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/web",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	stack := m.(Model).stack
	if n := len(stack); n != 2 {
		t.Fatalf("stack has %d screens, want 2 (matrix + deploy confirm)", n)
	}
	if _, ok := stack[len(stack)-1].(deployScreen); !ok {
		t.Fatalf("top screen is %T, want the deploy confirm", stack[len(stack)-1])
	}
	v := plain(m)
	if !strings.Contains(v, "v2") || !strings.Contains(v, "app-production") {
		t.Errorf("confirm screen should name the image and the env:\n%s", v)
	}
	// The whole point of the screen: the bytes are on it before anything is written.
	if !strings.Contains(v, "image:") {
		t.Errorf("confirm screen should show the diff:\n%s", v)
	}
}

// TestDeployConfirmScreenCarriesTheProductionWarning is the wiring half of the warning the CLI
// already renders: the TUI builds its own deploy plan, so attaching the warning in cmd/hoist
// alone would have left the one screen where the operator actually presses enter as the only
// surface that never mentions production. Asserted against the same screen opened with an
// empty production list, so it cannot pass on the word "production" appearing in the header's
// own env name.
func TestDeployConfirmScreenCarriesTheProductionWarning(t *testing.T) {
	open := func(t *testing.T, envs config.EnvsConfig) string {
		t.Helper()
		r, err := gitops.Discover(fixtureRoot, "")
		if err != nil {
			t.Fatal(err)
		}
		var tm tea.Model = New(r, []string{"ghcr.io/"}, envs, nil, Promotion{}, nil, apprestart.Funcs{})
		tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})
		tm, _ = tm.Update(tags.SelectedMsg{
			ImageRepo: "ghcr.io/example/web",
			Tag:       "v2",
			Digest:    "sha256:" + strings.Repeat("a", 64),
			Target:    "app-production",
		})
		return plain(tm)
	}

	if v := open(t, config.EnvsConfig{}); strings.Contains(v, "is a production env") {
		t.Fatalf("no env is configured production here; the warning must not fire:\n%s", v)
	}
	v := open(t, config.EnvsConfig{Production: []string{"app-production"}})
	if !strings.Contains(v, "app-production is a production env") {
		t.Fatalf("the confirm screen must name the production target:\n%s", v)
	}
	// Informational, never blocking: the diff and the enter hint both stay.
	if !strings.Contains(v, "image:") || !strings.Contains(v, "enter deploy") {
		t.Fatalf("the warning must not displace the diff or the confirmation:\n%s", v)
	}
}

// A tag with no occurrence in the target env cannot be deployed. The operator gets the reason
// on the matrix rather than an empty confirm screen.
func TestSelectedMsgReportsAnUndeployableChoice(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := pressD(t, m)
	m, _ = m.Update(cmd())

	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/not-in-this-env",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	if n := len(m.(Model).stack); n != 1 {
		t.Fatalf("an undeployable choice should return to the matrix: stack has %d screens", n)
	}
	if v := plain(m); !strings.Contains(v, "cannot deploy") {
		t.Errorf("notice should say why it cannot be deployed:\n%s", v)
	}
}

// TestWithMatrixNoticeFindsMatrixWhenNotOnTop is finding 6's own regression test (round N,
// Copilot): withMatrixNotice's doc comment says it sets the notice on the matrix screen
// "wherever it actually sits in the stack", but an earlier revision only ever checked
// m.stack[top] — true only because every real caller today happens to reach it exactly
// there (tags.SelectedMsg/DirectRequestedMsg pop straight back to a matrix that's always
// immediately below, since matrix.OpenTagsMsg is the only thing that ever pushes a tags
// screen). Build a stack where the matrix is deliberately NOT on top — matrix, then a plan
// screen pushed on top of it, mirroring TestPromotePushesPlanScreen — and confirm
// withMatrixNotice still finds and updates it rather than silently doing nothing.
func TestWithMatrixNoticeFindsMatrixWhenNotOnTop(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	m, _ = m.Update(cmd())
	stack := m.(Model).stack
	if n := len(stack); n != 2 {
		t.Fatalf("stack has %d screens after p, want 2", n)
	}
	if _, ok := stack[len(stack)-1].(matrixScreen); ok {
		t.Fatal("fixture precondition: the plan screen, not the matrix, must be on top")
	}

	const notice = "test-notice-not-on-top"
	m2 := m.(Model).withMatrixNotice(notice)
	m3 := m2.pop() // drop the plan screen back off to see the matrix's own view
	if v := plain(m3); !strings.Contains(v, notice) {
		t.Fatalf("withMatrixNotice should have found the matrix even though it wasn't on top:\n%s", v)
	}
}

// drainTags runs a tea.Cmd (and every cmd it in turn produces) to completion, exactly as a
// real tea.Program would deliver messages to the whole app model — needed here because the
// tags screen's Init kicks off an async ListFunc/MetaFunc load (internal/app/tags.Model.Init),
// which TestDeployNewPushesTagsScreen never exercises (its nil tagsFn errors synchronously).
// Mirrors internal/app/tags/model_test.go's own drain helper, one level up the screen stack.
func drainTags(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			var next tea.Cmd
			for _, c := range batch {
				if c == nil {
					continue
				}
				sub := c()
				if _, isTick := sub.(spinner.TickMsg); isTick {
					continue
				}
				var mc tea.Cmd
				m, mc = m.Update(sub)
				next = tea.Batch(next, mc)
			}
			cmd = next
			continue
		}
		if _, isTick := msg.(spinner.TickMsg); isTick {
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
}

// TestDeployNewThreadsRealStagingTag is the regression test for AGENTS.md invariant 4 / M6's
// production/staging tag-mismatch warning, guarding against the exact bug a sibling M6 attempt
// shipped: its matrix.OpenTagsMsg handler called tags.New with the paired staging env's
// currently-running tag hardcoded to "", so the mismatch note could never render from the real
// running app (only from a unit test calling tags.New directly). This test drives the real
// app-wiring path — app.Model.Update's own matrix.OpenTagsMsg case, not a direct tags.New call
// — with a config naming app-staging as app-production's pair, against the fixture repo where
// app-production/web and app-staging/web genuinely carry different tags
// (v202601010101/v202602150930). If a future change reintroduces a hardcoded "" (or otherwise
// drops the real lookup), hasStagingMismatch/stagingTag never reach tags.New with real values,
// the note never renders, and this test fails.
func TestDeployNewThreadsRealStagingTag(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	tagsFn := func(string) (bool, tags.RegTagsFunc, tags.GitTagsFunc, tags.MetaFunc) {
		regTagsFn := func(context.Context) ([]string, error) {
			return []string{"v202601010101"}, nil
		}
		metaFn := func(_ context.Context, _ string) (registry.ImageMeta, error) {
			return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("a", 64)}, nil
		}
		return false, regTagsFn, nil, metaFn
	}
	m := New(r, []string{"ghcr.io/"}, envs, nil, Promotion{}, tagsFn, apprestart.Funcs{})
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	msg := matrix.OpenTagsMsg{ImageRepo: "ghcr.io/example/web", Target: "app-production"}
	tm, cmd := tm.Update(msg)
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after OpenTagsMsg, want 2", n)
	}
	tm = drainTags(tm, cmd)

	v := plain(tm)
	if !strings.Contains(v, "app-staging") || !strings.Contains(v, "v202602150930") {
		t.Errorf("tags screen view is missing the real staging-mismatch note (want app-staging / v202602150930, the fixture's actual app-staging/web tag):\n%s", v)
	}
}

// TestQuitKeyTypedIntoTagsFilterDoesNotQuit is round 5's finding 3 regression test: the root's
// global quit binding used to run before the top screen's own key handling ever saw the press,
// so typing "q" into the tag picker's filter box quit the whole program instead of landing in
// the filter query. Drives the real app-wiring path (matrix.OpenTagsMsg through app.Model.Update,
// like TestDeployNewThreadsRealStagingTag above), opens the filter with "/", then types "q".
func TestQuitKeyTypedIntoTagsFilterDoesNotQuit(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	tagsFn := func(string) (bool, tags.RegTagsFunc, tags.GitTagsFunc, tags.MetaFunc) {
		regTagsFn := func(context.Context) ([]string, error) {
			return []string{"v1", "v2"}, nil
		}
		metaFn := func(_ context.Context, _ string) (registry.ImageMeta, error) {
			return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("a", 64)}, nil
		}
		return false, regTagsFn, nil, metaFn
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, tagsFn, apprestart.Funcs{})
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	msg := matrix.OpenTagsMsg{ImageRepo: "ghcr.io/example/web", Target: "app-production"}
	tm, cmd := tm.Update(msg)
	tm = drainTags(tm, cmd)

	tm, _ = tm.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	tm, cmd = tm.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("typing q into the tag picker's filter quit the program")
		}
	}
	if v := plain(tm); !strings.Contains(v, "filter: > q") {
		t.Errorf("filter should have gained a q character, view:\n%s", v)
	}
}

func TestBackgroundColorRethemes(t *testing.T) {
	m := sized(t)
	if !m.(Model).styles.Dark {
		t.Fatal("default theme is not dark")
	}
	m, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
	if m.(Model).styles.Dark {
		t.Error("theme did not follow a light background")
	}
	if got := plain(m); !strings.Contains(got, "env app-production") {
		t.Error("view broke after retheme")
	}
	m, _ = m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if !m.(Model).styles.Dark {
		t.Error("theme did not follow a dark background")
	}
}

func TestViewUsesAltScreen(t *testing.T) {
	if !sized(t).View().AltScreen {
		t.Error("view is not in the alternate screen")
	}
}

// Confirming on the deploy screen starts a real promotion, and carries the mode with it. This
// is the end of the path the tag picker used to dead-end: matrix -> d -> picker -> confirm ->
// a write.
func TestDeployStartMsgStartsAPromotionWithItsMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		confirmed  bool
		wantDirect bool
	}{
		{"PR mode", deploy.ModePR, false, false},
		{"direct mode", deploy.ModeDirect, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got StartOpts
			called := false
			promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts, _ func(string)) (engine.PromotionState, flight.DriveFunc, error) {
				called, got = true, opts
				return engine.PromotionState{}, nil, nil
			}}
			m := sizedWithPromotion(t, promo)
			_, cmd := m.Update(deploy.StartMsg{
				Plan:      gitops.Plan{Variant: gitops.VariantDeploy, TargetEnv: "app-production"},
				Mode:      tc.mode,
				Confirmed: tc.confirmed,
				Target:    "app-production",
				Image:     "ghcr.io/example/web:v2",
			})
			if cmd == nil {
				t.Fatal("confirming a deploy produced no command")
			}
			buildResultFrom(t, cmd)
			if !called {
				t.Fatal("startPromotion was never called for a confirmed deploy")
			}
			if got.Direct != tc.wantDirect {
				t.Errorf("StartOpts.Direct = %v, want %v", got.Direct, tc.wantDirect)
			}
			if got.Confirmed != tc.confirmed {
				t.Errorf("StartOpts.Confirmed = %v, want %v", got.Confirmed, tc.confirmed)
			}
		})
	}
}

// TestEscOnTheDeployScreenPopsIt: the deploy screen emits deploy.BackMsg on Esc, but the root
// had no case for it, so the message was forwarded to the top screen — the deploy screen — which
// fed it to its own viewport. Esc did nothing and the screen could not be left (Copilot, PR #72).
func TestEscOnTheDeployScreenPopsIt(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := pressD(t, m)
	m, _ = m.Update(cmd())
	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/web",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("fixture precondition: stack has %d screens, want 2", n)
	}

	// Esc goes to the screen, which asks the root to pop it; the root must act on that ask.
	m2, escCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if escCmd == nil {
		t.Fatal("esc on the deploy screen produced no command")
	}
	m3, _ := m2.Update(escCmd())
	if n := len(m3.(Model).stack); n != 1 {
		t.Fatalf("esc left %d screens on the stack, want 1 (back to the matrix)", n)
	}
}

// R on the matrix opens the restart screen for the family under the cursor, and the matrix stays
// beneath it: a restart is small and repeatable, so backing out should land on the cell it
// started from.
func TestRestartKeyOpensTheRestartScreen(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	read := func(_ context.Context, env string, names []string) (restart.Plan, error) {
		p := restart.Plan{Env: env}
		for _, n := range names {
			p.Targets = append(p.Targets, rollout.DeploymentStatus{
				Namespace: env, Name: n, Replicas: 2, Strategy: "RollingUpdate", ReadinessProbes: 1,
			})
		}
		return p, nil
	}
	var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, nil,
		apprestart.Funcs{Read: read, Interval: time.Millisecond})
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	tm, cmd := tm.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd == nil {
		t.Fatal("R produced no command")
	}
	tm, cmd = tm.Update(cmd())
	if cmd != nil {
		tm, _ = tm.Update(cmd())
	}

	stack := tm.(Model).stack
	if n := len(stack); n != 2 {
		t.Fatalf("stack has %d screens, want 2 (matrix + restart)", n)
	}
	if _, ok := stack[len(stack)-1].(restartScreen); !ok {
		t.Fatalf("top screen is %T, want the restart screen", stack[len(stack)-1])
	}
	if v := plain(tm); !strings.Contains(v, "hoist · restart") {
		t.Errorf("the screen should name the operation:\n%s", v)
	}
}

// With no cluster wired in, R says so on the matrix rather than opening a screen that can do
// nothing at all.
func TestRestartKeyWithNoClusterSaysSo(t *testing.T) {
	m := sized(t) // Promotion{} and a zero apprestart.Funcs: no Read
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		m, _ = m.Update(cmd())
	}
	if n := len(m.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens, want 1: nothing should have opened", n)
	}
	if v := plain(m); !strings.Contains(v, "needs a cluster connection") {
		t.Errorf("the matrix should say why:\n%s", v)
	}
}

// The in-flight pane (M10): the root lists at boot, hands the matrix what it found, and r
// on the matrix resumes through the same promotionBuiltMsg path a confirmed plan takes.
func TestInFlightListingReachesTheMatrixAndResumeOpensTheFlightScreen(t *testing.T) {
	listed := 0
	resumed := ""
	parked := engine.PromotionState{ID: "5pr6sd333t", SourceEnv: "app-staging", TargetEnv: "app-production",
		PR: &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}}
	inFlight := InFlight{
		List: func(context.Context) ([]flight.Summary, error) {
			listed++
			return []flight.Summary{flight.Summarize(parked, false, []engine.StepStatus{
				{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepCommitted, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepPushed, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepPROpened, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepCIGreen, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepApproved, Observation: engine.Observation{Waiting: true}},
			}, nil)}, nil
		},
		Resume: func(_ context.Context, id string) (engine.PromotionState, flight.DriveFunc, error) {
			resumed = id
			return parked, func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
				return s, true, nil, nil
			}, nil
		},
	}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, nil, apprestart.Funcs{}).WithInFlight(inFlight)
	var m tea.Model = root
	init := root.Init()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Init batches the listing with the theme request and the tick; run the listing.
	var got inFlightMsg
	for _, c := range init().(tea.BatchMsg) {
		if c == nil {
			continue
		}
		if msg, ok := c().(inFlightMsg); ok {
			got = msg
		}
	}
	if listed != 1 || len(got.list) != 1 {
		t.Fatalf("listed %d times, msg %+v", listed, got)
	}
	m, _ = m.Update(got)
	if v := plain(m); !strings.Contains(v, "in flight (1)") || !strings.Contains(v, "hoist approve 5pr6sd333t") {
		t.Fatalf("the matrix did not receive the listing:\n%s", v)
	}
	// r: resume, via a command that yields promotionBuiltMsg, which pushes the flight screen.
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'r', Text: "r"})
	if cmd == nil {
		t.Fatal("r produced no command")
	}
	m, cmd = m.Update(cmd()) // matrix.ResumeMsg -> the root's resume command
	if cmd == nil {
		t.Fatal("ResumeMsg produced no command")
	}
	built := cmd()
	if resumed != "5pr6sd333t" {
		t.Fatalf("Resume called with %q", resumed)
	}
	m, _ = m.Update(built)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after resume, want 2 (matrix, flight)", n)
	}
	if v := plain(m); !strings.Contains(v, "5pr6sd333t") || !strings.Contains(v, "app-staging → app-production") {
		t.Fatalf("flight screen not showing the resumed promotion:\n%s", v)
	}
	// A tick while the flight screen is on top does not list; back on the matrix it does.
	_, cmd = m.Update(inFlightTickMsg{})
	if msg := cmd(); msg != nil {
		if _, ok := msg.(inFlightMsg); ok {
			t.Fatal("a tick with the flight screen on top must not list")
		}
	}
	if listed != 1 {
		t.Fatalf("listed %d times after a tick off the matrix", listed)
	}
	m, cmd = m.Update(flight.BackMsg{})
	if cmd == nil {
		t.Fatal("popping back to the matrix must re-list at once")
	}
	if _, ok := cmd().(inFlightMsg); !ok || listed != 2 {
		t.Fatalf("pop re-list: listed %d", listed)
	}
	if v := plain(m); !strings.Contains(v, "FAMILY") {
		t.Fatalf("not back on the matrix:\n%s", v)
	}
}

// With nothing wired, r says so instead of panicking, and no listing is attempted.
func TestInFlightUnwiredDegrades(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(matrix.ResumeMsg{ID: "x"})
	if v := plain(m); !strings.Contains(v, "resuming a promotion is not wired up") {
		t.Fatalf("notice missing:\n%s", v)
	}
	if v := plain(m); strings.Contains(v, "in flight") {
		t.Fatalf("no pane without a listing:\n%s", v)
	}
}

// openPR delivers flight.OpenPRMsg the way the runtime does: the handler returns a command
// that runs the launcher (#56: it used to run inside Update and freeze the loop), and the
// command's result is fed back. "display" mode returns no command at all.
func openPR(t *testing.T, m tea.Model) tea.Model {
	t.Helper()
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		m, _ = m.Update(cmd())
	}
	return m
}

// The launcher runs outside Update: the handler returns a command and the launcher is not
// called until that command runs, so a slow browser cannot block the event loop (#56).
func TestFlightOpenPRLaunchesOutsideUpdate(t *testing.T) {
	calls := 0
	m := sizedWithPromotion(t, Promotion{OpenURL: func(string) error { calls++; return nil }})
	_, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd == nil {
		t.Fatal("OpenPRMsg must return the launch as a command")
	}
	if calls != 0 {
		t.Fatal("the launcher ran inside Update")
	}
	if _, ok := cmd().(openURLResultMsg); !ok || calls != 1 {
		t.Fatalf("running the command: calls=%d", calls)
	}
}

// deployHistory carries every declared reference across to the confirm screen, Declared
// first, so a split env's summary can name them all (#151).
func TestDeployHistoryCarriesEveryDeclaredRef(t *testing.T) {
	one := image.Ref{Repo: "ghcr.io/example/web", Tag: "v1"}
	old := image.Ref{Repo: "ghcr.io/example/web", Tag: "v0"}
	h := deployHistory(nil, &tags.Declared{Ref: one, Refs: []image.Ref{one, old}}, time.Time{}, "")
	if h.Declared != one || len(h.DeclaredRefs) != 2 || h.DeclaredRefs[0] != one || h.DeclaredRefs[1] != old {
		t.Fatalf("deployHistory = %+v; want Declared v1 and DeclaredRefs [v1 v0]", h)
	}
	if h := deployHistory(nil, nil, time.Time{}, "none"); h.Declared.Repo != "" || h.DeclaredRefs != nil {
		t.Fatalf("no declared: %+v", h)
	}
}

// The root builds the matrix with no cluster question and installs one through WithDrift
// afterwards, exactly as cmd/hoist does; until each env's answer lands the notes must say
// the cluster is being asked, or a hung request reads as a finished comparison.
func TestWithDriftMarksEveryEnvPendingUntilItAnswers(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	drift := func(_ context.Context, _ string) (map[string][]image.Ref, error) {
		return map[string][]image.Ref{}, nil
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, nil, apprestart.Funcs{}).WithDrift(drift)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	if v := plain(tm); !strings.Contains(v, "… asking the cluster what") {
		t.Fatalf("before any answer the notes must say the cluster is being asked:\n%s", v)
	}
}

// C pushes the config screen with the text WithConfigView supplied, esc pops it; without
// WithConfigView, C is a notice, not a blank screen (#104).
func TestConfigKeyPushesConfigScreen(t *testing.T) {
	m := sized(t)
	m = m.(Model).WithConfigView("/home/me/.config/hoist/config.yaml", true, "poll:\n    ci: 20s\n")
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'C', Text: "C"})
	if cmd == nil {
		t.Fatal("C produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenConfigMsg); !ok {
		t.Fatalf("C's command yields %T, want matrix.OpenConfigMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after C, want 2", n)
	}
	if v := plain(m); !strings.Contains(v, "/home/me/.config/hoist/config.yaml") || !strings.Contains(v, "ci: 20s") {
		t.Errorf("config screen lacks the path or the text:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the config screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
	// Positive control for the unwired case.
	bare := sized(t)
	bare, _ = bare.Update(matrix.OpenConfigMsg{})
	if n := len(bare.(Model).stack); n != 1 || !strings.Contains(plain(bare), "no config to show") {
		t.Errorf("unwired C: stack %d, view:\n%s", n, plain(bare))
	}
}

// TestRepoRefreshedMsgUpdatesTheRootRepoToo is the round-2 review finding against PR #182: the
// matrix screen's own WithRepo (called from its nested Update on matrix.RepoRefreshedMsg) only
// ever replaced the matrix's OWN internal *gitops.Repo — the root's own m.repo, which plan.New,
// the tag picker's StagingMismatch/DeclaredIn, restart.Targets and gitops.BuildDeployPlan all
// read directly, stayed whatever New was built with at boot forever. An F5 that revealed a new
// occurrence would show it in the table (matrix's own copy) while every plan opened afterward
// silently kept building from the stale boot-time snapshot missing it.
func TestRepoRefreshedMsgUpdatesTheRootRepoToo(t *testing.T) {
	tm := sized(t)
	m := tm.(Model)
	before := m.repo
	if before == nil {
		t.Fatal("fixture precondition: sized(t) must boot with a non-nil repo")
	}

	fresh := &gitops.Repo{Root: before.Root, AppsRoot: before.AppsRoot, Envs: map[string]*gitops.Env{}}
	// Gen 0 matches a freshly-built matrix.Model's own zero-value repoGen (askRepoRefresh was
	// never called in this test, so nothing has advanced it) — the same "not stale" answer a
	// real F5 round-trip would get.
	tm2, _ := tm.Update(matrix.RepoRefreshedMsg{Gen: 0, Repo: fresh})
	m2 := tm2.(Model)

	if m2.repo != fresh {
		t.Fatalf("root repo = %p, want the RepoRefreshedMsg's own repo (%p) adopted — plan.New and friends must see what F5 just found", m2.repo, fresh)
	}
}

// TestNoticeIsOnScreen is #164's regression: every screen renders through ui.Frame.Render,
// which emits exactly `height` lines, so a notice appended after that landed on row
// height+1 and the alternate screen buffer never showed it — an in-flight refusal on the
// deploy confirm screen was indistinguishable from a dead enter key.
//
// The existing notice tests (TestStartMsgShowsNoticeOnBuildError and friends) could not
// catch it: strings.Contains over the whole view is true whether or not the line is on the
// terminal. This asserts the SHAPE — exactly `height` lines, the notice on the last one, and
// the screen still drawn above it — at both golden sizes (AGENTS.md §4.8).
func TestNoticeIsOnScreen(t *testing.T) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m := sized(t)
			m, _ = m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			// A real refusal, not a synthetic string: this is the exact message the
			// operator could not read (#164, #166).
			const notice = "could not start promotion: promotion x5hz5gszie targeting spritz-staging is still in flight (at direct-pushed); run `hoist resume x5hz5gszie` instead of starting a second one"
			root := m.(Model)
			root.notice = notice
			lines := strings.Split(ansi.Strip(root.View().Content), "\n")
			if len(lines) != size.h {
				t.Fatalf("view is %d lines on a %d-row terminal; a notice must take rows FROM the screen, never add one past the bottom", len(lines), size.h)
			}
			for i, line := range lines {
				if w := ansi.StringWidth(line); w > size.w {
					t.Errorf("line %d is %d cells wide, over %d:\n%s", i+1, w, size.w, line)
				}
			}
			// The notice wraps, so it is the terminal's last ROWS: its head must sit
			// within the final ui.NoticeMaxLines, and its tail on the very last row.
			tail := strings.Join(lines[len(lines)-ui.NoticeMaxLines:], "\n")
			if !strings.Contains(tail, "could not start promotion") {
				t.Errorf("the notice does not start within the last %d rows; got:\n%s", ui.NoticeMaxLines, tail)
			}
			if !strings.Contains(lines[len(lines)-1], "starting a second one") {
				t.Errorf("the notice does not end on the terminal's last row; got:\n%s", lines[len(lines)-1])
			}
			// Positive control: the screen the notice explains is still drawn above it, so
			// this cannot pass by rendering the notice alone.
			if !strings.Contains(strings.Join(lines[:len(lines)-1], "\n"), "FAMILY") {
				t.Error("the matrix is gone from above the notice")
			}
		})
	}
}

// TestNoticeTooLongForTheTerminalIsCapped: a git or forge transport error runs long, and the
// notice's rows are taken from the screen it is explaining — so it is wrapped and capped at
// ui.NoticeMaxLines rather than allowed to push that screen away.
func TestNoticeTooLongForTheTerminalIsCapped(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	root := m.(Model)
	root.notice = "could not start promotion: " + strings.Repeat("a very long transport error ", 40)
	lines := strings.Split(ansi.Strip(root.View().Content), "\n")
	if len(lines) != height {
		t.Fatalf("view is %d lines on a %d-row terminal, want exactly %d", len(lines), height, height)
	}
	notice := 0
	for _, line := range lines {
		if strings.Contains(line, "very long transport error") || strings.Contains(line, "could not start promotion") {
			notice++
		}
	}
	if notice != ui.NoticeMaxLines {
		t.Errorf("notice took %d rows, want it capped at %d", notice, ui.NoticeMaxLines)
	}
	if !strings.Contains(lines[len(lines)-1], "…") {
		t.Errorf("a capped notice must mark its overflow with an ellipsis; last line:\n%s", lines[len(lines)-1])
	}
}
