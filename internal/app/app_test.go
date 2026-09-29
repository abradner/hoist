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

	"github.com/abradner/hoist/internal/app/activity"
	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

const (
	fixtureRoot = "../../testdata/repo"
	width       = 80
	height      = 24
)

// testPlanFunc stands in for svc.Plan (internal/service, service:Plan PR B) in tests that never
// wire cmd/hoist's own adaptors: BuildDeployPlan/WarnDeployIntoProduction for a deploy,
// BuildPlanWith with no resolution for a promotion — this package must never import
// internal/service's own resolution machinery (AGENTS.md §4.8: cmd/hoist owns that adapter), so
// a fake stands in for it here exactly as it would for any other cmd/hoist-built function value.
func testPlanFunc(promotable []string, envs config.EnvsConfig) plan.Func {
	return func(_ context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		if req.Deploy != nil {
			pl, err := gitops.BuildDeployPlan(req.Repo, req.Target, *req.Deploy, promotable)
			if err != nil {
				return service.PlannedChange{}, err
			}
			service.WarnDeployIntoProduction(&pl, envs)
			return service.PlannedChange{Plan: pl, Repo: req.Repo}, nil
		}
		pl, err := gitops.BuildPlanWith(req.Repo, req.Source, req.Target, promotable, req.Overrides, nil)
		if err != nil {
			return service.PlannedChange{}, err
		}
		return service.PlannedChange{Plan: pl, Repo: req.Repo}, nil
	}
}

// fixedDriver is a session.Driver whose Step always answers Done with the fixed state a test
// configured — the app-layer fake for a Start/Resume closure that used to build a trivial
// identity-echoing flight.DriveFunc (`return s, true, nil, nil`). A real Driver owns its own
// state independently of what a caller passes each Step call (see session.Driver's own doc
// comment), so this fake's state is fixed at construction rather than threaded through. Every
// current caller wants exactly this "done immediately, no error" shape; a test that needs
// something else (an error, statuses, blocking) uses funcDriver below instead.
type fixedDriver struct {
	state engine.PromotionState
}

func driverAlways(state engine.PromotionState) session.Driver {
	return &fixedDriver{state: state}
}

func (d *fixedDriver) Step(context.Context) (service.Tick, error) {
	return service.Tick{State: d.state, Done: true}, nil
}
func (d *fixedDriver) State() engine.PromotionState { return d.state }
func (d *fixedDriver) OverrideCINone()              { d.state.CINoneOverride = true }

// funcDriver adapts a plain Step function to session.Driver, for a test whose fake needs a
// custom body (calling a progress callback, blocking on ctx, counting calls) rather than a fixed
// answer — the app-layer twin of internal/app/flight's own hungDrive/stubDrive test fakes.
type funcDriver struct {
	StepFunc     func(ctx context.Context) (service.Tick, error)
	StateFunc    func() engine.PromotionState
	OverrideFunc func()
}

func (f funcDriver) Step(ctx context.Context) (service.Tick, error) { return f.StepFunc(ctx) }
func (f funcDriver) State() engine.PromotionState {
	if f.StateFunc != nil {
		return f.StateFunc()
	}
	return engine.PromotionState{}
}
func (f funcDriver) OverrideCINone() {
	if f.OverrideFunc != nil {
		f.OverrideFunc()
	}
}

// sized returns the root model after Init and the first WindowSizeMsg, as a running
// program would deliver them — no terminal involved. Promotion is the zero value: Start and
// OpenURL both nil, matching a caller that hasn't wired cmd/hoist's real adaptors in yet (see
// TestStartMsgWithNoStartPromotionShowsNotice and TestFlightOpenPRMsgShowsNotice below).
func sized(t *testing.T) tea.Model {
	t.Helper()
	return sizedWithPromotion(t, testPromo{})
}

// sizedWithPromotion is sized's general form, for tests that need a fake Start/OpenURL wired
// in without cmd/hoist's own pkg/git/pkg/forge adaptors (this package must never import
// those — AGENTS.md §4.8). promo is the test-only testPromo (fakeservice_test.go): the old
// Promotion.Start field, deleted from app.go's own Promotion in the Service-seam refactor, lives
// on here so every existing fixture keeps its exact shape — only the wrapping type's name
// changed. Start (if set) becomes the fakeService driving app.Service.StartPromotion.
func sizedWithPromotion(t *testing.T, promo testPromo) tea.Model {
	t.Helper()
	// A zero testPromo (promo.Start == nil) must still leave m.svc == nil — the "not wired up"
	// convention every unwired-adaptor test in this file asserts against (a non-nil *fakeService
	// with a nil startFn would panic instead of degrading to a notice the moment
	// plan.StartMsg/deploy.StartMsg reached svc.StartPromotion).
	var svc Service
	if promo.Start != nil {
		svc = &fakeService{startFn: promo.Start}
	}
	return sizedWithService(t, svc, Promotion{Poll: promo.Poll, OpenURL: promo.OpenURL, OpenPRMode: promo.OpenPRMode})
}

// sizedWithService is sizedWithPromotion's underlying general form, for tests that need to
// drive List/Resume/Abandon through a *fakeService directly (the in-flight pane, abandon)
// rather than only Start/OpenURL.
func sizedWithService(t *testing.T, svc Service, promo Promotion) tea.Model {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, testPlanFunc([]string{"ghcr.io/"}, config.EnvsConfig{}), svc, promo, nil, apprestart.Funcs{})
	_ = m.Init()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return tm
}

// sessionBuildCmd descends into the two nested batches m.start()/matrix.ResumeMsg's own case now
// produce — tea.Batch(fs.Init(), tea.Batch(buildOrResumeCmd, listenCmd)), exactly the shape
// session.Controller.Start/Resume build (internal/app/session/controller.go) — to reach the
// actual build/resume call, still uncalled. Never the OTHER inner element (listenCmd): calling it
// blocks reading an empty channel, the same reason the old extractBuildCmd this replaces never
// called fs.Init() either. A test that changes that shape needs to update this comment along
// with it, per AGENTS.md §10 meta-rule 2. Returned uncalled so a caller that needs to run it in
// its own goroutine (a test proving cancellation actually reaches a hung call) can.
func sessionBuildCmd(t *testing.T, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil command")
	}
	outer, ok := cmd().(tea.BatchMsg)
	if !ok || len(outer) < 2 || outer[1] == nil {
		t.Fatalf("command yields %#v, want tea.BatchMsg(fs.Init(), sessCmd)", cmd)
	}
	inner, ok := outer[1]().(tea.BatchMsg)
	if !ok || len(inner) < 1 || inner[0] == nil {
		t.Fatalf("session command yields %#v, want tea.BatchMsg(buildCmd, listenCmd)", outer[1])
	}
	return inner[0]
}

// attach runs m.start()'s (or matrix.ResumeMsg's) own returned command through to the point a
// real promotion id exists and the flight screen has been mirrored onto it (session.ChangeBuilt)
// — stopping there, deliberately: the entry is left Busy (onBuilt issues its own first Step
// immediately, matching a freshly-adopted real Driver's own busy-on-construction convention). It
// returns the model and the next tea.Cmd (the first Step call itself), uncalled, for a caller
// that wants to drive further with stepOnce.
func attach(t *testing.T, m Model, startCmd tea.Cmd) (Model, tea.Cmd) {
	t.Helper()
	built := sessionBuildCmd(t, startCmd)()
	tm, cmd := m.Update(built)
	return tm.(Model), cmd
}

// stepOnce runs one session.Controller Step call (the plain, unbatched func stepCmd builds) and
// feeds its result back through Update — the mirrored equivalent of the old
// driveResultFrom/onDriveResult path, now one layer down.
// firstStepOfPokeBatch unwraps session.Controller.Poke/OverrideCINone's own returned cmd — since
// a rearm now batches its stepCmd together with a fresh listenCmd (so progress keeps flowing
// after R/c re-arm one, AGENTS.md §9's own goroutine-leak lesson applies here too) — and returns
// only the stepCmd, uncalled, exactly as stepOnce expects. Never call the batch's second element
// directly: listenCmd blocks on an unbuffered select until a progress line arrives or its ctx is
// done, and neither happens synchronously in a test.
func firstStepOfPokeBatch(t *testing.T, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 1 || batch[0] == nil {
		t.Fatalf("Poke/OverrideCINone command yields %#v, want tea.BatchMsg(stepCmd, listenCmd)", cmd)
	}
	return batch[0]
}

func stepOnce(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil step command")
	}
	tm, _ := m.Update(cmd())
	return tm.(Model)
}

// drainRoot runs cmd through the root's Update, recursively, via uitest.Drain — for a test that
// needs a whole async chain (Train 2 design PR 4's completion-triggered refresh/relist, batched
// together in apply's own returned cmd) to actually run to completion, rather than stopping one
// level in like stepOnce/attach do for a test that only needs the first result. Never run this on
// a command that reaches session.Controller.Init's own recurring listTickMsg chain — that uses a
// real tea.Tick (Config.After's own default) and would block for a real Config.ListEvery (30s
// default) the one time this is misused on it; every fixture below only ever drains a chain that
// starts from a stepMsg/builtMsg result, never Init() itself.
func drainRoot(m Model, cmd tea.Cmd) Model {
	return uitest.Drain(m, cmd, func(m Model, msg tea.Msg) (Model, tea.Cmd) {
		tm, cmd := m.Update(msg)
		return tm.(Model), cmd
	})
}

// rootSessionInit runs the root's own Init() down to session.Controller's own boot listing —
// tea.Batch(tea.RequestBackgroundColor, screenCmd, m.sess.Init()), itself
// tea.Batch(listCmd, tickCmd) (internal/app/session/controller.go's own Init) — returning the
// listCmd, uncalled. Never the tick element: calling it sleeps for a real ListEvery.
func rootSessionInit(t *testing.T, root Model) tea.Cmd {
	t.Helper()
	msg := root.Init()()
	outer, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("root Init() = %#v, want tea.BatchMsg — a wired backend always adds session.Controller.Init's own listing to the batch", msg)
	}
	// tea.Batch drops nils and collapses to the bare cmd when only one survives, so the batch's
	// own shape (which of bg/screen/sess.Init() are non-nil) isn't fixed — find sess.Init()'s own
	// nested batch (tea.Batch(listCmd, tickCmd), internal/app/session/controller.go's own Init)
	// by looking for the one element that itself yields a further BatchMsg; nothing else in the
	// root's own Init does.
	for _, c := range outer {
		if c == nil {
			continue
		}
		inner, ok := c().(tea.BatchMsg)
		if !ok || len(inner) < 1 || inner[0] == nil {
			continue
		}
		return inner[0]
	}
	t.Fatal("no nested session.Controller.Init() batch found in root Init()")
	return nil
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
func plain(m tea.Model) string { return ansi.Strip(m.View().Content) }

// latestActivityText returns the root's own most recent activity.Entry.Text, or "" if nothing
// has been logged yet — the direct-field-access replacement for the old root.notice string, since
// Model.activity (internal/app/activity) is the append-only log that succeeded it (Train 2 design
// PR9): a test that used to read root.notice now reads the log's own latest entry instead.
func latestActivityText(m Model) string {
	e, ok := m.activity.Latest()
	if !ok {
		return ""
	}
	return e.Text
}

// pressD presses t (T3-04: was d) on the matrix and, when the cell has several first-party
// images (the fixture's first family, counta, has two), accepts the chooser's first option
// with enter — the same image the pre-M10 "first sorted repo" rule picked silently. Returns
// the command the matrix emitted (matrix.OpenTagsMsg's cmd). Kept named pressD: it is called
// from many pre-existing tests below and renaming every call site is out of this change's
// scope — only the key it presses moved.
func pressD(t *testing.T, m tea.Model) (tea.Model, tea.Cmd) {
	t.Helper()
	m, cmd := m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	if cmd != nil {
		if _, ok := cmd().(matrix.OpenTagsMsg); ok {
			return m, cmd
		}
	}
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("t, then enter on the chooser, produced no command")
	}
	return m, cmd
}

func TestViewSnapshot(t *testing.T) {
	m := sized(t)
	got := plain(m)
	// T3-04: the selected env is named by the header's own cursor marker (▸ APP-PRODUCTION)
	// rather than a footer "env <name>" phrase — the footer's own room goes to the write verbs
	// instead (v2·01a), and at 80 columns that already leaves no room for "? help" to stay
	// unshortened, so the overlay hint reads "? more" per keys.Footer's own rule.
	for _, want := range []string{"FAMILY", "▸ APP-PRODUCTION", "APP-STAGING", "v202602201200", "2 images", "external", "? more"} {
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
	// The two-column help layout (T3-04) truncates a long Desc to its own column width, so the
	// substring checked here is short enough to survive that.
	if !strings.Contains(v, "promote into the curs") {
		t.Errorf("help line missing:\n%s", v)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if v := plain(m); strings.Contains(v, "promote into the curs") {
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

// TestStartMsgWithNoStartPromotionShowsNotice: a caller that hasn't wired a Service in (a nil
// svc, sized's own default) must show a clear notice on confirm rather than pushing a broken
// flight screen or panicking on a nil call — the same nil-adaptor convention plan.Func and
// flight.OpenPRMsg's OpenURL already use.
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
// svc.StartPromotion off the Update call stack (it can talk to a real git remote/forge, so it
// must not run directly inside Update — mirrors plan.Func's own loadCmd), and a
// successful promotionBuiltMsg then pushes the flight screen with the real state and
// Driver it returned — no more nil, no more a bare {SourceEnv, TargetEnv}. The fake driveFn
// is a trivial non-nil stub, never nil: a real svc.StartPromotion success always builds one
// (production svc.StartPromotion never returns a nil Drive alongside a nil error), and a
// nil driveFn here would now hit the promotionBuiltMsg nil-driveFn guard (Copilot's PR #50
// finding — see TestPromotionBuiltMsgNilDriveFnShowsNotice) instead of exercising this test's
// actual subject, the successful push.
func TestStartMsgBuildsFlightScreenOnSuccess(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	called := false
	promo := testPromo{Start: func(_ context.Context, p gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		called = true
		if p.SourceEnv != "app-staging" || p.TargetEnv != "app-production" {
			t.Errorf("startPromotion called with unexpected plan: %+v", p)
		}
		return wantState, driverAlways(wantState), nil
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
	mm, _ := attach(t, m.(Model), cmd)
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	if n := len(mm.stack); n != 2 {
		t.Fatalf("stack has %d screens once the build lands, want 2 (mirrored into the same screen, not pushed again)", n)
	}
	if v := plain(mm); !strings.Contains(v, "app-staging → app-production") || !strings.Contains(v, wantState.ID) {
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
	promo := testPromo{Start: func(_ context.Context, p gitops.Plan, _ startOpts, progress func(string)) (engine.PromotionState, session.Driver, error) {
		// Preflight: exactly what svc.StartPromotion's own Hooks.Progress calls do.
		progress("checking your checkout against origin/main")
		progress("claiming " + p.TargetEnv + " and checking for a conflicting promotion")
		s := engine.PromotionState{ID: "abcd1234", SourceEnv: p.SourceEnv, TargetEnv: p.TargetEnv}
		return s, funcDriver{StepFunc: func(context.Context) (service.Tick, error) {
			// Drive: exactly what newDriverFor's own wrapped save does — call the SAME
			// progress closure the preflight above just used, from a call that only
			// happens after the build goroutine that constructed it has already
			// returned. This is the exact shape that panicked.
			progress("branched: acted")
			s.History = append(s.History, engine.HistoryEntry{Step: engine.StepBranched, Detail: "acted"})
			return service.Tick{State: s, Done: true}, nil
		}}, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked reaching drive with a live progress callback: %v", r)
			}
		}()
		tm, cmd := m.Update(msg)
		mm, stepCmd := attach(t, tm.(Model), cmd)
		if stepCmd == nil {
			t.Fatal("session.ChangeBuilt's own first Step command is nil")
		}
		mm = stepOnce(t, mm, stepCmd)
		if v := plain(mm); !strings.Contains(v, "acted") {
			t.Errorf("flight screen view missing the real drive result:\n%s", v)
		}
	}()
}

// TestPromotionBuiltMsgStampsCurrentBuildGen is removed for Train 2's session controller (PR 2
// design): the generation that guards a build's own result from being stale is now
// session.Controller's own private entry.gen, never a field app.go holds or a test at this
// layer can observe directly. Its guarantee is proven where it is now enforced —
// TestStaleStepMsgAfterPokeIsDropped and TestControllerIsCopyOnWrite in
// internal/app/session/controller_test.go.

// TestEscFromBuildingTruncatesPastThePlanScreenUnderneath replaces
// TestEscFromBuildingWithAPlanScreenUnderneathStillLands (audit UX-H6/FB-H2, a follow-up to Train
// 2 design PR 3): esc on the flight screen must land on the matrix even with a plan screen still
// underneath it, not on that plan screen — landing there left it still ticked and ready, so Enter
// would start the very drive esc just left watching. This is the plan-screen-still-underneath
// shape TestEscDuringBuildKeepsBuilding does not cover (that one starts with only the matrix
// underneath): esc here must close BOTH the flight screen and the plan screen in one gesture, and
// the build that keeps running in the background still lands and is tracked once its result
// arrives, even though nothing is on top to mirror it onto any more.
func TestEscFromBuildingTruncatesPastThePlanScreenUnderneath(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return wantState, driverAlways(wantState), nil
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
	buildCmd := sessionBuildCmd(t, cmd) // the request's own build call, not yet run

	// The operator backs out of the building flight screen (Esc). This must close BOTH the flight
	// screen AND the plan screen underneath it, landing on the matrix — not pop just one and leave
	// the plan screen (with its still-ticked selection) reachable again.
	m, _ = m.Update(flight.BackMsg{})
	if n := len(m.(Model).stack); n != 1 {
		t.Fatalf("flight.BackMsg left stack at %d screens, want 1 (truncated past the plan screen, straight to the matrix)", n)
	}

	built := buildCmd() // ...now the still-running build's own result lands.
	before := len(m.(Model).stack)
	m, _ = m.Update(built)
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed to %d screens processing the build's own result, want unchanged at %d (nothing left on the stack to mirror onto)", n, before)
	}
	if !m.(Model).sess.Running("abcd1234") {
		t.Error("the build backed out of via esc must still land and be tracked — PR 3's own point")
	}
}

// TestSecondStartForSameTargetIsRefused replaces TestStalePromotionBuiltMsgFromSupersededStartMsgIsDropped
// (PR #50 round-4 review finding #4, Codex): pre-session app.go let a second StartMsg for the
// same target env silently supersede an outstanding first one (buildGen dropped the stale
// result once it eventually landed). session.Controller.Start no longer allows that state to
// exist at all — a second Start for a target env it is already tracking is refused outright
// with ErrTargetBusy (AGENTS.md invariant 5, session.Controller.Start's own doc comment,
// TestSecondStartSameTargetRefused in internal/app/session/controller_test.go) — so there is no
// "stale second build" scenario left to guard against at this layer; this proves the refusal
// itself reaches the operator as a notice, and that the backend is never called a second time.
func TestSecondStartForSameTargetIsRefused(t *testing.T) {
	calls := 0
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		calls++
		return engine.PromotionState{ID: "only-one"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}

	tm, cmd1 := m.Update(msg)
	if cmd1 == nil {
		t.Fatal("first StartMsg produced no command")
	}
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after the first StartMsg, want 2 (matrix, the building flight screen)", n)
	}

	// The first request's own backend call is lazy — issuing the tea.Cmd never runs it, only
	// the runtime (or a test) calling it does — so this deliberately never calls cmd1 at all:
	// the point is that the SECOND request never even reaches the point of issuing one.
	before := calls

	tm2, cmd2 := tm.Update(msg)
	if cmd2 != nil {
		t.Errorf("a second StartMsg for the same target produced a command: %#v", cmd2())
	}
	if n := len(tm2.(Model).stack); n != 2 {
		t.Errorf("stack changed to %d screens on the refused second StartMsg, want unchanged at 2", n)
	}
	if calls != before {
		t.Errorf("startPromotion called %d additional time(s) for the refused second StartMsg, want 0", calls-before)
	}
	// The bottom row truncates to one line (Model.bottomLine's own doc comment) and this
	// message is long enough to be cut before "already running" — the full reason is what the
	// activity log entry itself carries, read here rather than the necessarily-truncated view.
	if got := latestActivityText(tm2.(Model)); !strings.Contains(got, "already running") {
		t.Errorf("activity entry missing the refusal reason: %q", got)
	}
}

// TestDoubleEnterStartsOnce is Train 2 design PR 8's own regression test for FB-L4: two Enter
// keypresses on a loaded, ready plan screen, back to back with NOTHING drained in between (the
// attacker is the queued second Enter, exactly as fast as bubbletea can deliver two keys before
// the first one's own command has even had a chance to run) must still reach
// session.Controller.Start — and so app.Service.StartPromotion — exactly once. Before plan.Model
// gained its own one-shot starting guard (model.go), pressing Enter twice like this emitted TWO
// plan.StartMsg values; TestSecondStartForSameTargetIsRefused already proves the SECOND one is
// refused by session.Controller's own same-target bookkeeping (so no second drive ever actually
// started), but the operator would see the "already running" refusal notice flash across a
// promotion that had, in fact, just started successfully — confusing, and needless plumbing for
// something the plan screen itself can simply not ask for twice. This test would fail on the old
// behavior only if it counted the notice, not the call count session.Controller already
// protects; instead it directly counts app.Service.StartPromotion, so it is pinned to the
// guard's own purpose (no wasted second attempt) rather than restating
// TestSecondStartForSameTargetIsRefused's assertion under a new name.
func TestDoubleEnterStartsOnce(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production", "app-production": "app-staging"}}
	planFn := func(_ context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		pl, err := gitops.BuildPlanWith(req.Repo, req.Source, req.Target, []string{"ghcr.io/"}, req.Overrides, nil)
		if err != nil {
			return service.PlannedChange{}, err
		}
		return service.PlannedChange{Plan: pl, Repo: req.Repo}, nil
	}
	var calls int
	svc := &fakeService{startFn: func(_ context.Context, p gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		calls++
		return engine.PromotionState{ID: "abcd1234", SourceEnv: p.SourceEnv, TargetEnv: p.TargetEnv}, driverAlways(engine.PromotionState{ID: "abcd1234"}), nil
	}}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	var tm tea.Model = New(r, []string{"ghcr.io/"}, envs, planFn, svc, Promotion{}, nil, apprestart.Funcs{})
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	// p: open the plan screen for app-staging (envs.Pairs sends it straight to app-production,
	// stateLoading — no manual env-select step needed), then land its real load.
	tm, cmd := press(t, tm, uitest.Key("p"))
	tm, cmd = tm.Update(cmd()) // matrix.OpenPlanMsg -> pushes the screen, returns its Init()
	loadCmd := extractPlanLoadCmd(t, cmd)
	tm, _ = tm.Update(loadCmd())
	if v := plain(tm); !strings.Contains(v, "app-production") {
		t.Fatalf("setup: plan screen not showing a loaded, ready plan:\n%s", v)
	}

	// Two Enters, queued: neither of these tea.Cmd results is invoked until after BOTH
	// keypresses have already been processed by Update, exactly what "no draining in between"
	// means — a real bubbletea program can deliver a second keypress before the runtime has
	// gotten around to running the first one's own command.
	tm1, cmd1 := press(t, tm, uitest.Key("enter"))
	_, cmd2 := press(t, tm1, uitest.Key("enter"))

	if cmd1 == nil {
		t.Fatal("first enter produced no command")
	}
	if cmd2 != nil {
		// A command here at all would mean the guard let a second StartMsg through — fatal
		// before even trying to run it (sessionBuildCmd would panic on the wrong shape, and
		// running it risks the real listenCmd's blocking channel read, sessionBuildCmd's own
		// doc comment).
		t.Fatal("second, immediately-queued enter produced a command — the starting guard did not hold")
	}

	// Only now is cmd1 actually drained: cmd1() is the plan screen's own emitted plan.StartMsg,
	// fed into the root exactly as attachedWithDriver's own fixture does, so this reaches
	// m.start -> session.Controller.Start for real. cmd2 has nothing left to drain, which is
	// the point — the guard meant there was only ever one command to run. Never drainRoot here:
	// it would also invoke the real Start's own listenCmd, a channel read that blocks forever
	// absent a runtime (sessionBuildCmd's own doc comment).
	tm2, startCmd := tm1.Update(cmd1())
	m1, stepCmd := attach(t, tm2.(Model), startCmd)
	m1 = stepOnce(t, m1, stepCmd)

	if calls != 1 {
		t.Fatalf("StartPromotion called %d times for two back-to-back Enters, want exactly 1", calls)
	}
	if n := len(m1.sess.Live()); n != 0 {
		t.Errorf("session.Controller.Live() has %d entries after the one real start finished (driverAlways answers Done immediately), want 0 — a second, still-running entry would mean a second start actually landed", n)
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
	promo := testPromo{Start: func(_ context.Context, p gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
	sessionBuildCmd(t, cmd)()
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
	promo := testPromo{Start: func(_ context.Context, p gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
	sessionBuildCmd(t, cmd)()
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
// step-selection machinery (M6/PR #43's engine.DirectCommitStep) was not present on the
// original branch this test was written against — the TUI's start adaptor of the day always
// built engine.AllSteps regardless of what the operator chose, and its call signature carried
// no Mode at all. A StartMsg confirmed with Mode: plan.ModeDirect must therefore never reach
// startPromotion as anything but direct — silently driving PR mode instead would mean the
// confirm screen told the operator "commit straight to the branch, no PR" and then opened one
// anyway. A direct-mode confirm reaches the start function AS direct: startOpts carries it, and
// service.Mode/AllDirectSteps honour it in production.
//
// Confirmed rides along with Direct because reaching ModeDirect already required the plan
// screen's own keypress-then-huh.Confirm gesture, which is exactly what
// engine.DirectCommitGateStep asks Confirmed to attest — and the gate re-derives the production
// refusal independently regardless.
func TestStartMsgCarriesDirectModeThrough(t *testing.T) {
	var got startOpts
	called := false
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, opts startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
	sessionBuildCmd(t, cmd)() // the start call happens off the Update stack
	if !called {
		t.Fatal("startPromotion was never called for a direct-mode confirm")
	}
	if !got.Direct {
		t.Error("startOpts.Direct = false for a ModeDirect confirm: the promotion would open a PR the operator declined")
	}
	if !got.Confirmed {
		t.Error("startOpts.Confirmed = false: the gate would refuse a gesture the operator actually completed")
	}
}

// The PR path must not accidentally inherit direct mode.
func TestStartMsgPRModeIsNotDirect(t *testing.T) {
	var got startOpts
	called := false
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, opts startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
	sessionBuildCmd(t, cmd)()
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

// TestPromotionBuiltMsgNilDriveFnShowsNotice is removed for Train 2's session controller: the
// nil-error/nil-driveFn contract app.go's own promotionBuiltMsg handler used to defend against
// is now a contract between session.Controller.Start's own buildCmd and whatever Backend it
// wraps (internal/app/session/controller.go's toBuiltMsg) — app.go never sees a raw driveFn at
// all any more, so there is nothing left at this layer to construct the malformed message this
// test fed in. Flagged as a real gap in session's own toBuiltMsg (no equivalent guard exists
// there today) rather than silently dropped — see this PR's final report.

// TestStartMsgShowsNoticeOnBuildError: buildPromotionForConfirm's own refusals (a real
// in-flight conflict, missing github config, a claim failure) must surface as a notice on the
// screen that popped up plan.StartMsg (plan, still on top — the flight screen is never
// pushed) rather than crashing.
func TestStartMsgShowsNoticeOnBuildError(t *testing.T) {
	wantErr := errors.New("promotion existing-id targeting app-production is still in flight (at pr-opened: open); run `hoist resume existing-id` instead of starting a second one")
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
	// known — popBuildFailed (app.go's own apply(), on session.ChangeBuildFailed) is what takes
	// the stack back to `before` once the error actually arrives, not the absence of a push.
	if n := len(m.(Model).stack); n != before+1 {
		t.Fatalf("stack has %d screens right after StartMsg, want %d (the building flight screen pushed)", n, before+1)
	}
	m, _ = m.Update(sessionBuildCmd(t, cmd)())
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack ended at %d screens after a construction error, want back to %d (the building screen popped)", n, before)
	}
	// The bottom row truncates to one line; the full reason lives on the activity entry itself.
	if got := latestActivityText(m.(Model)); !strings.Contains(got, "still in flight") {
		t.Errorf("activity entry missing the construction-error reason: %q", got)
	}
}

// TestStartMsgBoundedByPollDeadline: mirrors flight.Model's own TestDriveCmdBoundedByPollDeadline
// — a startPromotion call that hangs (blocks on ctx.Done() rather than ever returning) must not
// stall the plan screen forever. m.poll.Deadline bounds the call the same way it bounds
// flight.Model.driveCmd's own DriveFunc call, so this returns with ctx's deadline error instead
// of the goroutine blocking indefinitely.
func TestStartMsgBoundedByPollDeadline(t *testing.T) {
	hung := func(ctx context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		<-ctx.Done()
		return engine.PromotionState{}, nil, ctx.Err()
	}
	promo := testPromo{Start: hung, Poll: flight.PollDurations{Deadline: 20 * time.Millisecond}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	tm, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	buildCmd := sessionBuildCmd(t, cmd)
	done := make(chan tea.Msg, 1)
	go func() { done <- buildCmd() }()
	select {
	case built := <-done:
		tm2, _ := tm.Update(built)
		if v := plain(tm2); !strings.Contains(v, "context deadline exceeded") {
			t.Errorf("view missing the deadline-exceeded notice:\n%s", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartMsg's command did not return within 2s of a 20ms poll.Deadline — a hung startPromotion call can still stall the plan screen forever")
	}
}

// TestBackingOutNoLongerCancelsOutstandingBuild replaces TestBackingOutCancelsOutstandingBuild
// (Train 2 design PR 3, the operator's own decision, inverting PR #50's own final-round finding):
// esc on the building flight screen used to interrupt the outstanding startPromotion call's own
// context via session.Controller.CancelBuild — PR 3 removes that call from flight.BackMsg's
// handler entirely (app.go), so the build now keeps running to completion regardless of whether
// the operator is still watching it. This proves the outstanding call's ctx is NOT cancelled: the
// hung fixture never sees ctx.Err() within the wait window, and only unblocks once the test itself
// releases it.
func TestBackingOutNoLongerCancelsOutstandingBuild(t *testing.T) {
	release := make(chan struct{})
	sawCancel := make(chan error, 1)
	hung := func(ctx context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		select {
		case <-ctx.Done():
			sawCancel <- ctx.Err()
			return engine.PromotionState{}, nil, ctx.Err()
		case <-release:
			return engine.PromotionState{ID: "abcd1234"}, driverAlways(engine.PromotionState{ID: "abcd1234"}), nil
		}
	}
	promo := testPromo{Start: hung}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2 (matrix, the building flight screen)", n)
	}
	buildCmd := sessionBuildCmd(t, cmd)
	built := make(chan tea.Msg, 1)
	go func() { built <- buildCmd() }()

	// Esc on the building flight screen: PR 3's own point is that this does nothing to the
	// outstanding build's ctx any more.
	m, _ = m.Update(flight.BackMsg{})

	select {
	case err := <-sawCancel:
		t.Fatalf("the build's ctx was cancelled (err=%v); esc must no longer cancel a building screen's own build", err)
	case <-time.After(100 * time.Millisecond):
		// No cancellation observed within a generous window — exactly what PR 3 asks for.
	}
	close(release)
	select {
	case msg := <-built:
		tm, _ := m.Update(msg)
		if !tm.(Model).sess.Running("abcd1234") {
			t.Error("the build that kept running after esc must still land and be tracked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the build never completed after being released")
	}
}

// extractPlanLoadCmd descends into plan.Model.Init()'s own tea.Batch(spinner.Tick, loadCmd()) —
// the same shape sessionBuildCmd descends into a different batch for — to reach the load call
// itself, still uncalled, so a test can run two plan screens' loads in whatever order it likes
// without either one blocking on the other.
func extractPlanLoadCmd(t *testing.T, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil plan Init cmd")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 2 || batch[1] == nil {
		t.Fatalf("plan Init cmd = %#v, want tea.Batch(spinner.Tick, loadCmd)", cmd)
	}
	return batch[1]
}

// TestPlanEarlierResolveCannotLandOnNewPlan proves internal/app/scope's own Foreign guard (Train
// 2 design PR 5, audit FB-H3): a plan screen's async load is still a live tea.Cmd after the
// operator backs out of it — esc pops the screen, but nothing cancels the outstanding call, that
// is a separate piece of work (PR 6's owned context.Context) — so a second p press for a
// different source env can already be showing its own answer by the time the first one's load
// finally resolves. Attacker: the first plan screen's own loadedMsg, released last. Control: the
// second plan's own result is what actually lands, checked before the attacker is ever released.
func TestPlanEarlierResolveCannotLandOnNewPlan(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production", "app-production": "app-staging"}}
	planFn := func(_ context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		pl, err := gitops.BuildPlanWith(req.Repo, req.Source, req.Target, []string{"ghcr.io/"}, req.Overrides, nil)
		if err != nil {
			return service.PlannedChange{}, err
		}
		return service.PlannedChange{Plan: pl, Repo: req.Repo}, nil
	}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	var tm tea.Model = New(r, []string{"ghcr.io/"}, envs, planFn, nil, Promotion{}, nil, apprestart.Funcs{})
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	source1 := tm.(Model).stack[0].(matrixScreen).CurrentEnv()

	// p: opens the first plan screen instance, for source1.
	tm, cmd := press(t, tm, uitest.Key("p"))
	tm, cmd = tm.Update(cmd()) // matrix.OpenPlanMsg -> pushes the screen, returns its Init()
	loadCmd1 := extractPlanLoadCmd(t, cmd)

	// esc: plan.BackMsg pops back to the matrix, but the outstanding load above is untouched.
	tm, cmd = press(t, tm, uitest.Key("esc"))
	tm, _ = tm.Update(cmd())
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens after esc, want 1 (matrix only)", n)
	}

	// Move the column cursor so the second plan is for a different source env. (l used to be a
	// Right alias; it is retired — the proposed keymap binds it to the activity log instead.)
	tm, _ = press(t, tm, uitest.Key("right"))
	source2 := tm.(Model).stack[0].(matrixScreen).CurrentEnv()
	if source2 == "" || source2 == source1 {
		t.Fatalf("setup: moving the column did not change the source env (%q -> %q)", source1, source2)
	}

	// p again: a second, distinct plan screen instance, for source2.
	tm, cmd = press(t, tm, uitest.Key("p"))
	tm, cmd = tm.Update(cmd())
	loadCmd2 := extractPlanLoadCmd(t, cmd)

	// Control: the second instance's own load lands, and its answer is what shows.
	tm, _ = tm.Update(loadCmd2())
	want := plain(tm)
	if !strings.Contains(want, source2) {
		t.Fatalf("second plan's header does not name its own source %q:\n%s", source2, want)
	}

	// Attacker, released last: the first instance's load, superseded before it ever answered.
	// Without internal/app/scope's Foreign guard this lands on the screen now on top (the
	// second instance) and overwrites what the operator is looking at with source1's rows.
	tm, _ = tm.Update(loadCmd1())
	got := plain(tm)
	if got != want {
		t.Fatalf("an earlier plan screen's late result changed what is on screen:\nbefore:\n%s\nafter:\n%s", want, got)
	}
}

// typeIntoRoot presses every rune of s as a real keypress through the root's own Update, the
// way an operator types into whichever dialog the top screen currently has open — mirrors
// internal/app/plan's own typeInto, one layer up. Per-key commands are dropped rather than
// drained: a focused text input answers every keypress with a cursor-blink command that sleeps
// before reporting.
func typeIntoRoot(t *testing.T, tm tea.Model, s string) tea.Model {
	t.Helper()
	for _, r := range s {
		tm, _ = tm.Update(uitest.Key(string(r)))
	}
	return tm
}

// TestPopClosesScreenCtx proves the root's own pop calls Close on the screen it removes
// (AGENTS.md §4.8's scope bullet): esc popping the watch screen cancels an outstanding poll
// that is still genuinely in flight, not merely one that already finished on its own (a poll
// that returns instantly would have its own scope.DoCtx-derived per-call ctx cancelled by its
// own `defer cancel()` regardless of Close ever running — proving nothing about pop itself). The
// fake Read blocks on ctx.Done(), released only by the outstanding poll's own cancellation or by
// the test as a last resort, mirroring TestBackingOutNoLongerCancelsOutstandingBuild's own shape.
func TestPopClosesScreenCtx(t *testing.T) {
	started := make(chan struct{})
	sawCancel := make(chan error, 1)
	release := make(chan struct{})
	build := func(_, _ string) (watch.Funcs, error) {
		return watch.Funcs{
			Read: func(ctx context.Context) (watch.Snapshot, error) {
				close(started)
				select {
				case <-ctx.Done():
					sawCancel <- ctx.Err()
					return watch.Snapshot{}, ctx.Err()
				case <-release:
					return watch.Snapshot{}, nil
				}
			},
			Now: time.Now,
		}, nil
	}
	var tm tea.Model = sized(t).(Model).WithWatch(build)
	tm, cmd := tm.Update(matrix.OpenWatchMsg{Family: "counta", Target: "app-production"})
	if cmd == nil {
		t.Fatal("OpenWatchMsg produced no command")
	}
	pollDone := make(chan tea.Msg, 1)
	go func() { pollDone <- cmd() }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the fake Read was never called")
	}

	// esc pops the watch screen while its poll is still genuinely outstanding.
	tm, cmd = tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc on the watch screen produced no command")
	}
	tm, _ = tm.Update(cmd())
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("esc did not pop back to the matrix: stack has %d screens", n)
	}

	select {
	case err := <-sawCancel:
		if err == nil {
			t.Fatal("the outstanding read saw ctx.Done() but ctx.Err() was nil")
		}
	case <-time.After(2 * time.Second):
		close(release) // let the goroutine finish so it doesn't leak past the test
		t.Fatal("popping the watch screen never cancelled its outstanding read")
	}
	<-pollDone
}

// TestFailedStartThenOverrideLoadsHistory proves FB-M3's fix end to end: Enter no longer cancels
// the plan screen's own scope (internal/app/plan.Model.Update, the StartMsg case), so when the
// building screen a failed Start leaves behind pops back to this same plan screen, its history
// can still load. Attacker: the plan screen's own ctx, which the pre-fix code cancelled the
// instant Enter was pressed — proved by asserting ctx.Err()==nil from INSIDE the fake Delta the
// operator's own follow-up override triggers, the same live context the screen has held since
// before Enter (plan's own override path only re-mints the ID, not the ctx — see
// internal/app/plan/model.go's updateOverride).
func TestFailedStartThenOverrideLoadsHistory(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production"}}
	planFn := testPlanFunc([]string{"ghcr.io/"}, envs)
	var gotCtxErr error
	deltaCalls := 0
	hist := history.Funcs{
		Delta: func(ctx context.Context, _, _ image.Ref) (migrate.Delta, error) {
			deltaCalls++
			gotCtxErr = ctx.Err()
			return migrate.Delta{}, nil
		},
	}
	wantErr := errors.New("promotion existing-id targeting app-production is still in flight (at pr-opened: open); run `hoist resume existing-id` instead of starting a second one")
	svc := &fakeService{startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{}, nil, wantErr
	}}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, envs, planFn, svc, Promotion{}, nil, apprestart.Funcs{}).WithHistory(hist)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	// T3-04: p promotes INTO the cursor column, with the source taken from the one reverse pair
	// (envs.pairs) when exactly one exists. envs.Pairs only configures app-staging->app-
	// production, so land the column cursor on app-production (the TARGET) for p to go
	// straight to loading rather than the plan screen's own "promote into … from…" prompt.
	if tm.(Model).stack[0].(matrixScreen).CurrentEnv() != "app-production" {
		tm, _ = tm.Update(uitest.Key("right"))
	}

	// p: open and fully load the plan screen (testPlanFunc/the fake Delta never block).
	tm, cmd := press(t, tm, uitest.Key("p"))
	tm = drainRoot(tm.(Model), cmd)
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after p, want 2 (matrix, plan)", n)
	}
	if deltaCalls == 0 {
		t.Fatal("setup: the initial load never asked for any history")
	}

	// enter: confirm the plan (emits plan.StartMsg), then feed that to the root, which is what
	// actually pushes the building screen (m.start). The building screen goes up before the
	// failure is known.
	tm, cmd = press(t, tm.(Model), uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter on the confirmed plan produced no command")
	}
	tm, cmd = tm.Update(cmd())
	if cmd == nil {
		t.Fatal("plan.StartMsg produced no command")
	}
	if n := len(tm.(Model).stack); n != 3 {
		t.Fatalf("stack has %d screens right after enter, want 3 (matrix, plan, the building screen)", n)
	}

	// The build fails: popBuildFailed pops the building screen back to the plan screen.
	tm2, _ := tm.(Model).Update(sessionBuildCmd(t, cmd)())
	m2 := tm2.(Model)
	if n := len(m2.stack); n != 2 {
		t.Fatalf("stack has %d screens after the failed build, want 2 (matrix, plan)", n)
	}
	if _, ok := m2.stack[1].(planScreen); !ok {
		t.Fatalf("top screen after the failed build is %T, want the plan screen", m2.stack[1])
	}

	// o, a valid override, enter: the operator's own follow-up, which re-loads history through
	// the SAME scope this screen has held since before Enter (only its ID changes).
	tm = tea.Model(m2)
	tm, _ = tm.Update(uitest.Key("o"))
	if !strings.Contains(plain(tm), "override digest") {
		t.Fatalf("o did not open the override dialog:\n%s", plain(tm))
	}
	tm = typeIntoRoot(t, tm, "ghcr.io/example/counta:v9@sha256:"+strings.Repeat("a", 64))
	deltaCalls = 0
	tm, cmd = tm.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter on a valid override produced no command")
	}
	_ = drainRoot(tm.(Model), cmd)
	if deltaCalls == 0 {
		t.Fatal("the override never re-asked for history")
	}
	if gotCtxErr != nil {
		t.Fatalf("the override's own history fetch ran under a cancelled context: %v — Enter must not have cancelled the plan screen's scope", gotCtxErr)
	}
}

// TestSupersedingStartMsgCancelsPreviousBuild is removed: the scenario it guarded (a second
// StartMsg for the same target env silently superseding an outstanding first one) can no longer
// occur at all — session.Controller.Start refuses a second Start for a target env it is already
// tracking outright (ErrTargetBusy, TestSecondStartForSameTargetIsRefused above) rather than
// letting two builds for the same target race. There is nothing left to cancel on "supersede"
// because there is no longer a state where two builds for one target coexist even briefly.

// TestBuildAndDriveShareOneDeadline replaces TestFlightScreenSharesBuildDeadlineWithDrive
// (Copilot's PR #50 round-7 finding), re-proved against session.Controller's own mechanism
// (Train 2 design): session.Controller.Start builds ONE ctx (newCtx, from cfg.Deadline) at the
// moment Start is called, before the backend's own StartPromotion call ever runs, and reuses it
// for the whole entry's life — every later Step call included (controller.go's own
// stepCmd(e.ctx, ...)) — so the time the build itself takes counts against the same budget the
// drive polls against, never a fresh window per phase. This proves the guarantee: a build that
// consumes most of a tiny deadline still leaves the very first Step call bounded by only
// whatever is left, not a fresh full window.
func TestBuildAndDriveShareOneDeadline(t *testing.T) {
	const total = 200 * time.Millisecond
	const buildSleep = 150 * time.Millisecond // leaves ~50ms of the budget for the first Step
	hungDrive := funcDriver{StepFunc: func(ctx context.Context) (service.Tick, error) {
		<-ctx.Done()
		return service.Tick{}, ctx.Err()
	}}
	promo := testPromo{
		Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			time.Sleep(buildSleep)
			return engine.PromotionState{ID: "abcd1234"}, hungDrive, nil
		},
		Poll: flight.PollDurations{Deadline: total},
	}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	// ctx/deadlineAt are fixed HERE, by session.Controller.Start inside this StartMsg handler —
	// before Start's own buildSleep ever runs.
	tm, cmd := m.Update(msg)
	built := sessionBuildCmd(t, cmd)() // runs Start's own buildSleep synchronously in this goroutine

	mm, stepCmd := tm.(Model).Update(built)
	if stepCmd == nil {
		t.Fatal("session.ChangeBuilt's own first Step command is nil")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- stepCmd() }()
	select {
	case stepMsg := <-done:
		mm, _ = mm.(Model).Update(stepMsg)
		if v := plain(mm); !strings.Contains(v, "deadline exceeded") {
			t.Errorf("view missing a deadline-exceeded notice after the shared budget ran out:\n%s", v)
		}
	case <-time.After(120 * time.Millisecond): // comfortably above the ~50ms shared remainder,
		// comfortably below a fresh, unshared 200ms window measured from roughly this same point
		t.Fatal("the first Step call did not report the shared deadline in time — poll.Deadline was NOT shared with the build")
	}
}

// TestStartMsgErrorNoticeIsRedacted: buildPromotionForConfirm's error can embed a git/forge
// transport message carrying a credential (a token in a remote URL, say) — the root notice must
// scrub it the same way plan.Model.View and flight.Model.View already redact their own rendered
// output, rather than leaking it to the terminal unredacted.
func TestStartMsgErrorNoticeIsRedacted(t *testing.T) {
	const secret = "ghp_totallysecrettoken1234567890"
	redact.Register(secret)
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{}, nil, fmt.Errorf("push failed: authentication using %s rejected", secret)
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	m, _ = m.Update(sessionBuildCmd(t, cmd)())
	v := plain(m)
	if strings.Contains(v, secret) {
		t.Errorf("bottom row leaks the registered secret unredacted:\n%s", v)
	}
	// The bottom row truncates to one line (Model.bottomLine) and this message is long enough
	// that the redaction marker itself can be cut off before it — the activity screen's own
	// full, never-truncated rendering (activity.Lines' own doc comment) is where the redaction
	// guarantee is actually provable end to end.
	root := m.(Model)
	full := activityScreen{activity.New(root.activity, time.Now)}.View()
	if strings.Contains(full, secret) {
		t.Errorf("activity screen leaks the registered secret unredacted:\n%s", full)
	}
	if !strings.Contains(full, redact.Redacted) {
		t.Errorf("activity screen missing %q for the redacted entry:\n%s", redact.Redacted, full)
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
	promo := testPromo{OpenURL: func(url string) error {
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
	promo := testPromo{OpenURL: func(_ string) error {
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
	promo := testPromo{
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
	promo := testPromo{OpenPRMode: "display"}
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
	promo := testPromo{
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
	promo := testPromo{
		OpenPRMode: "both",
		OpenURL:    func(_ string) error { return errors.New("no such browser") },
	}
	m := sizedWithPromotion(t, promo)
	m = openPR(t, m)
	v := plain(m)
	if !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL after a failed launch in both mode:\n%s", v)
	}
	// The bottom row truncates to one line and this message is long enough to lose its tail;
	// the full text (URL and error both) lives on the activity entry itself.
	if got := latestActivityText(m.(Model)); !strings.Contains(got, "no such browser") {
		t.Errorf("activity entry missing the launch error in both mode: %q", got)
	}
}

// attachedWithDriver runs a real StartMsg through to a mirrored, attached flight screen driven
// by driver — the fixture the drive-outlives-the-screen tests below need: a real
// session.Controller entry, tracked under id, whose first Step call driver answers.
func attachedWithDriver(t *testing.T, id string, driver session.Driver) (Model, tea.Cmd) {
	t.Helper()
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: id}, driver, nil
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	return attach(t, tm.(Model), cmd)
}

// TestEscFromFlightLeavesDriveRunning is Train 2 design PR 3's central behaviour change, replacing
// the three tests that used to prove the opposite (x's own "return to matrix"/"cancels the
// in-flight drive" pair, and esc's identical cancelling twin — x itself is now retired): the
// operator's own decision is that leaving the flight screen never stops a drive — the branch, PR
// (or direct push) and every later step keep happening exactly as if the flight screen were still
// open. This proves the entry's ctx is NOT cancelled, that a poll delivered after esc still Steps
// it (a cancelled ctx would have made the fake driver return ctx.Err() instead), and that the
// matrix's own in-flight pane still advances from that Step's result.
func TestEscFromFlightLeavesDriveRunning(t *testing.T) {
	drv := &funcDriver{
		StateFunc: func() engine.PromotionState { return engine.PromotionState{ID: "abcd1234"} },
	}
	drv.StepFunc = func(ctx context.Context) (service.Tick, error) {
		if err := ctx.Err(); err != nil {
			return service.Tick{}, err
		}
		return service.Tick{State: drv.StateFunc(), Waiting: true, Wait: time.Hour}, nil
	}
	m, stepCmd := attachedWithDriver(t, "abcd1234", drv)
	if n := len(m.stack); n != 2 {
		t.Fatalf("setup: stack has %d screens once attached, want 2 (matrix, flight)", n)
	}
	mm := stepOnce(t, m, stepCmd) // settle the entry at Busy=false before esc

	tm, cmd := tea.Model(mm).Update(flight.BackMsg{})
	root := tm.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("esc should pop back to the matrix: stack has %d screens, want 1", n)
	}
	if !root.sess.Running("abcd1234") {
		t.Fatal("esc must leave the entry tracked — the drive keeps running")
	}
	if cmd == nil {
		t.Fatal("popping back to the matrix must re-list at once")
	}
	cmd() // the Relist call itself

	// R would refuse (ErrBusy) while a Step is outstanding, but nothing is: prove the drive is
	// truly alive by poking it and watching it actually Step, the way a real poll would land on
	// its own after esc with nobody driving it interactively.
	sess, pokeCmd, err := root.sess.Poke("abcd1234")
	if err != nil {
		t.Fatalf("Poke after esc: %v (a cancelled ctx would refuse or the entry would be gone)", err)
	}
	root.sess = sess
	if pokeCmd == nil {
		t.Fatal("Poke produced no Step command")
	}
	tm2, _ := root.Update(pokeCmd())
	root = tm2.(Model)
	if !root.sess.Running("abcd1234") {
		t.Fatal("the promotion vanished after a Step landed post-esc")
	}
}

// TestEnterReattachesWithoutSecondResume: r/enter on the matrix's in-flight pane for a promotion
// already running here (session.Controller.Running) must push a fresh flight screen mirroring the
// SAME entry rather than asking the backend to Resume it a second time — the attacker is a rival
// Backend.Resume call that would construct a second, independent Driver racing the first one
// under a second BuildID. The fixture's own *fakeService leaves ResumeFn unset entirely
// (fakeservice_test.go's own contract: an unwired method panics loudly rather than silently
// succeeding), so a rival call is caught two ways here: the re-attached screen's own BuildID must
// equal the ORIGINAL one (never a fresh one Resume would mint), and there must still be exactly
// one entry tracked — a real second Resume, wired to this same fake, would have panicked the
// moment its own returned command ran.
func TestEnterReattachesWithoutSecondResume(t *testing.T) {
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: "abcd1234"}, driverAlways(engine.PromotionState{ID: "abcd1234"}), nil
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, _ := attach(t, tm.(Model), cmd) // Busy=true: the entry is Running the instant it's built
	if n := len(mm.stack); n != 2 {
		t.Fatalf("setup: stack has %d screens once attached, want 2", n)
	}
	before, ok := mm.sess.Snapshot("abcd1234")
	if !ok {
		t.Fatal("setup: promotion not tracked once attached")
	}

	// esc: stop watching, drive keeps running (TestEscFromFlightLeavesDriveRunning's own proof).
	tm2, _ := tea.Model(mm).Update(flight.BackMsg{})
	root := tm2.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("esc did not pop back to the matrix: stack has %d screens, want 1", n)
	}

	// r/enter on the pane: matrix.ResumeMsg is the message either key emits.
	tm3, cmd3 := root.Update(matrix.ResumeMsg{ID: "abcd1234"})
	root3 := tm3.(Model)
	if cmd3 != nil {
		// Only reached if Controller.Resume mistakenly built a fresh resume command for an id
		// it already tracks — running it here (rather than leaving it uncalled) is deliberate:
		// against this fixture's ResumeFn-unset fakeService that would panic outright.
		tm4, _ := root3.Update(cmd3())
		root3 = tm4.(Model)
	}
	if n := len(root3.stack); n != 2 {
		t.Fatalf("re-attach should push exactly one flight screen: stack has %d screens, want 2", n)
	}
	fs, ok := root3.stack[1].(flightScreen)
	if !ok {
		t.Fatalf("top screen is %T, want the re-attached flightScreen", root3.stack[1])
	}
	if _, build := fs.Attached(); build != before.Build {
		t.Errorf("re-attach produced BuildID %d, want the original %d — a second build exists", build, before.Build)
	}
	if n := len(root3.sess.Live()); n != 1 {
		t.Errorf("session.Controller tracks %d entries after re-attach, want exactly 1", n)
	}
	if v := plain(root3); !strings.Contains(v, "abcd1234") {
		t.Errorf("re-attached flight screen missing the promotion's id:\n%s", v)
	}
}

// TestEscDuringBuildKeepsBuilding: esc on the BUILDING flight screen (no real id yet — the
// preflight window) must leave that build running too, the Building-phase twin of
// TestEscFromFlightLeavesDriveRunning. TestBackingOutNoLongerCancelsOutstandingBuild proves the
// ctx-level half of this same guarantee directly; this one proves the build actually lands and is
// tracked afterward — proved here by observing the outstanding
// startPromotion call actually complete rather than see ctx.Err().
func TestEscDuringBuildKeepsBuilding(t *testing.T) {
	landed := make(chan engine.PromotionState, 1)
	promo := testPromo{Start: func(ctx context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		// A real startPromotion would take real wall-clock time; this fixture only needs to
		// prove the ctx it was handed is still alive by the time esc has already returned —
		// checking ctx.Err() here, synchronously, is enough (no goroutine/sleep needed) and
		// avoids flaking on a busy CI runner.
		s := engine.PromotionState{ID: "abcd1234"}
		landed <- s
		return s, driverAlways(s), ctx.Err()
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2 (matrix, the building flight screen)", n)
	}
	buildCmd := sessionBuildCmd(t, cmd) // not yet run

	tm2, _ := tm.Update(flight.BackMsg{})
	if n := len(tm2.(Model).stack); n != 1 {
		t.Fatalf("esc during Building should pop to the matrix: stack has %d screens, want 1", n)
	}

	built := buildCmd() // ...now the build actually runs, ctx un-cancelled.
	select {
	case s := <-landed:
		if s.ID != "abcd1234" {
			t.Fatalf("startPromotion saw an unexpected state: %+v", s)
		}
	default:
		t.Fatal("startPromotion's own closure never ran")
	}
	// The result still lands (session.Controller.onBuilt's own gen check has nothing stale to
	// drop here — this entry was never cancelled) and the promotion is tracked, matching "the
	// build keeps running" rather than being silently dropped as stale.
	tm3, _ := tm2.Update(built)
	root := tm3.(Model)
	if !root.sess.Running("abcd1234") {
		t.Fatal("a build backed out of via esc must still land and be tracked — PR 3's own point")
	}
}

// TestEnterOnBuildingPaneEntryReattaches is P1 #2 from t2-review.md, driven through real
// keypresses end to end: plan -> enter (start) -> esc during Build (back to the matrix, which
// re-lists and so picks the still-Building entry up on its pane) -> enter on that pane entry ->
// the SAME BuildID re-attached on a fresh flight screen. Before this fix, matrix.ResumeMsg for an
// id-less pane entry carried ID: "" and the root's case called session.Controller.Resume(""),
// which always fails (Find("") never matches anything in Resume's own byID map) — so the pane's
// enter/r gesture on exactly the window it exists for (preflight, or the up-to-120s signing
// wait) always ended in "could not start promotion: …" instead of re-attaching.
func TestEnterOnBuildingPaneEntryReattaches(t *testing.T) {
	// The fake fakeService this constructs has no ResumeFn set — Backend.Resume (fakeService.Resume,
	// fakeservice_test.go) panics if called at all. That is not what actually catches a wrong fix
	// here, though: unlike TestEnterReattachesWithoutSecondResume (which deliberately runs the
	// resulting resume command to force the panic), this test never runs resumeCmd — a wrong fix
	// that still routes through session.Controller.Resume("") just builds an entry under a blank
	// id and fails the assertions below (wrong BuildID, wrong screen, wrong Live() count), the
	// same as any other logic bug would.
	release := make(chan struct{})
	promo := testPromo{Start: func(ctx context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		<-release // never resolves until the test says so — stays Building throughout
		return engine.PromotionState{}, nil, ctx.Err()
	}}
	m := sizedWithPromotion(t, promo)
	defer close(release)

	tm, startCmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	root := tm.(Model)
	if n := len(root.stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2 (matrix, the building flight screen)", n)
	}
	before, ok := root.stack[1].(flightScreen)
	if !ok {
		t.Fatalf("top screen is %T, want flightScreen", root.stack[1])
	}
	_, build := before.Attached()
	if build == 0 {
		t.Fatal("setup: Start produced a zero BuildID")
	}
	_ = startCmd // deliberately never run: the build stays outstanding (like esc during a real signing wait)

	// esc: back to the matrix, which re-lists at once (truncateToMatrix's own doc comment).
	tm2, relistCmd := root.Update(flight.BackMsg{})
	root2 := tm2.(Model)
	if n := len(root2.stack); n != 1 {
		t.Fatalf("esc during Building should pop to the matrix: stack has %d screens, want 1", n)
	}
	if relistCmd == nil {
		t.Fatal("esc from the matrix produced no relist command")
	}
	// Run the listing (a listMsg, session.Event) through the root so apply()'s ChangeListed case
	// re-merges this session's own live entries (mergeInFlight) into the matrix's pane — the
	// Building entry has no state file yet, so nothing but session.Controller.Live() shows it.
	tm3, _ := root2.Update(relistCmd())
	root3 := tm3.(Model)

	view := plain(root3)
	if !strings.Contains(view, "starting") {
		t.Fatalf("matrix pane does not show the Building entry as \"starting\":\n%s", view)
	}
	if strings.Contains(view, "started 0001-01-01") || strings.Contains(view, "started 292277") {
		t.Fatalf("matrix pane rendered the zero StartedAt literally:\n%s", view)
	}

	// tab then enter on the pane (T3-04): matrix.ResumeMsg is what it emits — with exactly one
	// entry and no id yet, it must carry Build, not an empty ID.
	var top tea.Model = root3
	top, _ = top.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	top, resumeCmd := press(t, top, tea.KeyPressMsg{Code: tea.KeyEnter})
	if resumeCmd == nil {
		t.Fatal("tab+enter on the in-flight pane produced no command")
	}
	msg := resumeCmd()
	rm, ok := msg.(matrix.ResumeMsg)
	if !ok {
		t.Fatalf("enter on the pane emitted %#v, want matrix.ResumeMsg", msg)
	}
	if rm.ID != "" {
		t.Fatalf("ResumeMsg.ID = %q, want empty — this entry has no promotion id yet", rm.ID)
	}
	if rm.Build != build {
		t.Fatalf("ResumeMsg.Build = %d, want %d", rm.Build, build)
	}

	// The root's own case: this must re-attach the SAME BuildID without ever reaching
	// session.Controller.Resume/Backend.Resume (fakeService.Resume panics — see above).
	tm4, _ := top.Update(msg)
	root4 := tm4.(Model)
	if n := len(root4.stack); n != 2 {
		t.Fatalf("re-attach should push exactly one flight screen: stack has %d screens, want 2", n)
	}
	fs, ok := root4.stack[1].(flightScreen)
	if !ok {
		t.Fatalf("top screen is %T, want the re-attached flightScreen", root4.stack[1])
	}
	if _, b := fs.Attached(); b != build {
		t.Errorf("re-attach produced BuildID %d, want the original %d — a second build exists", b, build)
	}
	if n := len(root4.sess.Live()); n != 1 {
		t.Errorf("session.Controller tracks %d entries after re-attach, want exactly 1", n)
	}
}

// TestReobserveKeyThroughRoot: a real "r" keypress on an attached flight screen, driven through
// the root's own Update exactly as a running program would — the top screen's own key handling
// emits flight.ReobserveMsg, and the root's case (unchanged by PR 3) answers it by calling
// session.Controller.Poke, which drives one more real Step call against the fake driver.
func TestReobserveKeyThroughRoot(t *testing.T) {
	var stepCalls int
	drv := funcDriver{
		StepFunc: func(context.Context) (service.Tick, error) {
			stepCalls++
			return service.Tick{State: engine.PromotionState{ID: "abcd1234"}, Waiting: true, Wait: time.Hour}, nil
		},
		StateFunc: func() engine.PromotionState { return engine.PromotionState{ID: "abcd1234"} },
	}
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: "abcd1234"}, drv, nil
	}}
	m := sizedWithPromotion(t, promo)
	tm, cmd := m.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	mm, stepCmd := attach(t, tm.(Model), cmd)
	mm = stepOnce(t, mm, stepCmd) // settle Busy=false so R is not refused
	if stepCalls != 1 {
		t.Fatalf("setup: %d step calls, want 1", stepCalls)
	}

	var top tea.Model = mm
	top, reobserveCmd := press(t, top, tea.KeyPressMsg{Code: 'r', Text: "r"})
	if reobserveCmd == nil {
		t.Fatal("r on the attached flight screen produced no command")
	}
	top, pokeCmd := top.Update(reobserveCmd()) // flight.ReobserveMsg reaches the root
	if pokeCmd == nil {
		t.Fatal("r's ReobserveMsg produced no re-drive command")
	}
	top.Update(firstStepOfPokeBatch(t, pokeCmd)()) // the actual Step call
	if stepCalls != 2 {
		t.Errorf("Step called %d time(s) after R, want 2 (the setup call plus exactly one re-drive)", stepCalls)
	}
}

// TestNoticeSurvivesKeypress inverts the old TestRootNoticeClearsOnNextKeypress: the root's
// activity row (Model.activity, replacing the old transient notice string, Train 2 design PR9)
// is deliberately NOT cleared on the operator's next keypress — that was exactly the #164-shaped
// problem this package exists to end, a real refusal disappearing the moment an unrelated key
// landed. It is only ever replaced by a newer entry, or dismissed by opening the log (l).
func TestNoticeSurvivesKeypress(t *testing.T) {
	m := sized(t)
	m = openPR(t, m)
	if !strings.Contains(plain(m), "not wired yet") {
		t.Fatal("setup: notice not shown after OpenPRMsg")
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if !strings.Contains(plain(m), "not wired yet") {
		t.Error("the activity row was cleared by an unrelated keypress — it must survive until a newer entry replaces it")
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
		var tm tea.Model = New(r, []string{"ghcr.io/"}, envs, testPlanFunc([]string{"ghcr.io/"}, envs), nil, Promotion{}, nil, apprestart.Funcs{})
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
	m := New(r, []string{"ghcr.io/"}, envs, nil, nil, Promotion{}, tagsFn, apprestart.Funcs{})
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
// TestQStillTypesIntoFilter is T3-03's own name for this regression (round 5, finding 3): q
// unbound everywhere but the matrix must still fall through to a screen mid-text-entry rather
// than being swallowed as the "unbound" hint gesture — a filter query typing "q" is text, not a
// key press asking about quitting.
func TestQStillTypesIntoFilter(t *testing.T) {
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
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, tagsFn, apprestart.Funcs{})
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
	if got := plain(m); !strings.Contains(got, "APP-PRODUCTION") {
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
			var got startOpts
			called := false
			promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, opts startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
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
			sessionBuildCmd(t, cmd)()
			if !called {
				t.Fatal("startPromotion was never called for a confirmed deploy")
			}
			if got.Direct != tc.wantDirect {
				t.Errorf("startOpts.Direct = %v, want %v", got.Direct, tc.wantDirect)
			}
			if got.Confirmed != tc.confirmed {
				t.Errorf("startOpts.Confirmed = %v, want %v", got.Confirmed, tc.confirmed)
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

// TestEscFromDeployFlightTruncatesPastTheDeployConfirmScreenUnderneath is the deploy path's twin
// of TestEscFromBuildingTruncatesPastThePlanScreenUnderneath (audit UX-H6/FB-H2): picker → deploy
// confirm → enter starts the drive and pushes the flight screen directly on top of the deploy
// confirm screen (m.start's own doc comment); esc from there must land on the matrix, not back on
// that confirm screen still holding the same diff and ready to start the identical deploy again on
// Enter.
func TestEscFromDeployFlightTruncatesPastTheDeployConfirmScreenUnderneath(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", TargetEnv: "app-production"}
	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return wantState, driverAlways(wantState), nil
	}}
	m := sizedWithPromotion(t, promo)
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
		t.Fatalf("fixture precondition: stack has %d screens after opening the deploy confirm, want 2", n)
	}

	// enter, on the real deploy confirm screen — not deploy.StartMsg constructed by hand — so this
	// proves the actual onKey gesture, not just the message it emits.
	tm, startCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if startCmd == nil {
		t.Fatal("enter on the deploy confirm screen produced no command")
	}
	m2, cmd2 := tm.Update(startCmd())
	if cmd2 == nil {
		t.Fatal("deploy.StartMsg with a wired startPromotion produced no command")
	}
	if n := len(m2.(Model).stack); n != 3 {
		t.Fatalf("stack has %d screens right after enter, want 3 (matrix, deploy confirm, the building flight screen)", n)
	}
	mm, _ := attach(t, m2.(Model), cmd2) // Busy=true: the entry is Running the instant it's built

	tm3, cmd3 := tea.Model(mm).Update(flight.BackMsg{})
	root := tm3.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("esc left %d screens on the stack, want 1 (truncated past the deploy confirm screen, straight to the matrix)", n)
	}
	if !root.sess.Running("abcd1234") {
		t.Fatal("esc must leave the drive tracked and running")
	}
	if cmd3 == nil {
		t.Fatal("truncating to the matrix must re-list at once")
	}
	cmd3()
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
	var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil,
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

// TestRestartEscNotesRolloutContinues is FB-L3's own root-level regression: esc while a confirmed
// restart is still starting must leave the activity log naming that the rollout continues
// (apprestart.BackMsg.RollingContinues, restart/model.go's own doc comment), rather than the
// screen just vanishing with no record of anything having been asked for at all. Prove a new
// test can fail (AGENTS.md §8): removing the `if msg.RollingContinues` branch in app.go's own
// apprestart.BackMsg case makes this fail for the right reason (checked by hand before landing).
func TestRestartEscNotesRolloutContinues(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	read := func(_ context.Context, env string, names []string) (restart.Plan, error) {
		p := restart.Plan{Env: env}
		for _, n := range names {
			p.Targets = append(p.Targets, rollout.DeploymentStatus{
				Namespace: env, Name: n, Replicas: 1, Strategy: "RollingUpdate",
			})
		}
		return p, nil
	}
	// Do never actually returns within the test — esc fires before its command is ever run, so
	// the screen's own state is still stateStarting (start()'s own synchronous transition) the
	// moment BackMsg is emitted, exactly the window FB-L3 is about.
	do := func(ctx context.Context, _ restart.Plan, _ time.Time) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	observe := func(context.Context, string, []string, time.Time) ([]restart.Progress, error) { return nil, nil }
	var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil,
		apprestart.Funcs{Read: read, Do: do, Observe: observe, Interval: time.Millisecond})
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	tm, cmd := tm.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if cmd != nil {
		tm, cmd = tm.Update(cmd())
		if cmd != nil {
			tm, _ = tm.Update(cmd())
		}
	}
	if _, ok := tm.(Model).stack[len(tm.(Model).stack)-1].(restartScreen); !ok {
		t.Fatalf("setup: top screen is %T, want the restart screen", tm.(Model).stack[len(tm.(Model).stack)-1])
	}

	// enter: not production, so this starts immediately (restart.Model.onKey's own m.start()).
	tm, startCmd := tm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = startCmd // deliberately never run — see do's own comment above

	tm, backCmd := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd != nil {
		tm, _ = tm.Update(backCmd())
	}
	if _, ok := tm.(Model).stack[len(tm.(Model).stack)-1].(matrixScreen); !ok {
		t.Fatalf("esc should pop back to the matrix; top is %T", tm.(Model).stack[len(tm.(Model).stack)-1])
	}
	if v := plain(tm); !strings.Contains(v, "rollout continues") {
		t.Errorf("the activity row should name that the rollout continues:\n%s", v)
	}
}

// TestRestartEarlierStartedDropped proves internal/app/scope's own Foreign guard for the restart
// screen (Train 2 design PR 5): the DoFunc call enter starts is still a live tea.Cmd after the
// operator backs out with esc — nothing cancels it, that is PR 6's owned context.Context, a
// separate piece of work — so a second restart screen, for a different env, can already be
// mid-start (its own state == stateStarting) by the time the first screen's Do call finally
// answers. Attacker: the first instance's own startedMsg, landing on a second instance that is
// ALSO in stateStarting — the one state restart.Model's own guard (state == stateStarting) would
// let through on its own, so this proves the scope.Foreign guard is doing real work, not just
// restating a check the state machine already made. Control: the second instance's own started
// answer, landing afterward, is what actually moves it past stateStarting.
func TestRestartEarlierStartedDropped(t *testing.T) {
	read := func(_ context.Context, env string, names []string) (restart.Plan, error) {
		p := restart.Plan{Env: env}
		for _, n := range names {
			p.Targets = append(p.Targets, rollout.DeploymentStatus{Namespace: env, Name: n, Replicas: 1, Strategy: "RollingUpdate"})
		}
		return p, nil
	}
	do := func(_ context.Context, p restart.Plan, _ time.Time) ([]string, error) {
		var names []string
		for _, tg := range p.Targets {
			names = append(names, tg.Name)
		}
		return names, nil
	}
	// Observe is required alongside Do (restart.Model.New refuses one without the other,
	// AGENTS.md §4.8's FB-L3 fix) even though this test never lets a drive reach it.
	observe := func(context.Context, string, []string, time.Time) ([]restart.Progress, error) {
		return nil, nil
	}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil,
		apprestart.Funcs{Read: read, Do: do, Observe: observe, Interval: time.Minute})
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})

	// R: the first restart screen instance, for the current column's env.
	tm, cmd := press(t, tm, tea.KeyPressMsg{Code: 'R', Text: "R"})
	tm, cmd = tm.Update(cmd()) // matrix.OpenRestartMsg -> pushes the screen, returns Init() (the plan read)
	tm, _ = tm.Update(cmd())   // the read resolves synchronously: state -> stateConfirm

	// enter: not production (config.EnvsConfig{} names none), so this starts directly.
	tm, cmd = press(t, tm, tea.KeyPressMsg{Code: tea.KeyEnter})
	startedCmd1 := cmd // the Do call, uncalled: answering it would flip state -> stateRolling

	// esc: pops back to the matrix; the start above is still outstanding.
	tm, cmd = press(t, tm, tea.KeyPressMsg{Code: tea.KeyEsc})
	tm, _ = tm.Update(cmd())
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens after esc, want 1 (matrix only)", n)
	}

	// Move the column so the second restart instance targets a different env. (l used to be a
	// Right alias; it is retired — the proposed keymap binds it to the activity log instead.)
	tm, _ = press(t, tm, tea.KeyPressMsg{Code: tea.KeyRight})

	// R again: a second, distinct restart screen instance, driven to stateStarting exactly like
	// the first — the shared state a state-only guard could not tell apart.
	tm, cmd = press(t, tm, tea.KeyPressMsg{Code: 'R', Text: "R"})
	tm, cmd = tm.Update(cmd())
	tm, _ = tm.Update(cmd())
	tm, cmd = press(t, tm, tea.KeyPressMsg{Code: tea.KeyEnter})
	startedCmd2 := cmd

	// Attacker, released first: the first instance's started answer, landing on the second
	// instance while it is itself still in stateStarting.
	before := plain(tm)
	tm, _ = tm.Update(startedCmd1())
	if got := plain(tm); got != before {
		t.Fatalf("an earlier restart screen's late started answer changed the screen:\nbefore:\n%s\nafter:\n%s", before, got)
	}

	// Control: the second instance's own started answer is what actually moves it on.
	tm, _ = tm.Update(startedCmd2())
	if v := plain(tm); strings.Contains(v, "not yet restarted") {
		t.Fatalf("the second restart's own started answer should have moved it past confirm/starting:\n%s", v)
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

// The in-flight pane (M10): the root lists at boot (via session.Controller.Init, Train 2
// design), hands the matrix what it found, and r on the matrix resumes through the same
// session.Controller.Resume path a confirmed plan's Start now takes. The old "a tick while the
// flight screen is on top must not list" assertion is gone: session.Controller lists on its own
// schedule regardless of which screen is on top (it has no notion of "the matrix" at all) —
// harmless, since the merge into the matrix's own pane (apply()'s ChangeListed case) only ever
// matters once the matrix is shown again. What still matters, and is still asserted here: the
// boot listing reaches the pane, resuming attaches the flight screen, and popping back to the
// matrix (flight.BackMsg) re-lists at once rather than waiting for the next tick.
func TestInFlightListingReachesTheMatrixAndResumeOpensTheFlightScreen(t *testing.T) {
	listed := 0
	resumed := ""
	parked := engine.PromotionState{ID: "5pr6sd333t", SourceEnv: "app-staging", TargetEnv: "app-production",
		PR: &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}}
	inFlight := fakeInFlight{
		List: func(context.Context) ([]service.Listed, error) {
			listed++
			return []service.Listed{{State: parked, Done: false, Statuses: []engine.StepStatus{
				{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepCommitted, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepPushed, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepPROpened, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepCIGreen, Observation: engine.Observation{Satisfied: true}},
				{Step: engine.StepApproved, Observation: engine.Observation{Waiting: true}},
			}}}, nil
		},
		Resume: func(_ context.Context, id string) (engine.PromotionState, session.Driver, error) {
			resumed = id
			return parked, driverAlways(parked), nil
		},
	}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, svcWithInFlight(inFlight), Promotion{}, nil, apprestart.Funcs{})
	var m tea.Model = root
	listCmd := rootSessionInit(t, root)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(listCmd())
	if listed != 1 {
		t.Fatalf("listed %d times, want 1", listed)
	}
	if v := plain(m); !strings.Contains(v, "in flight · 1") || !strings.Contains(v, "hoist approve 5pr6sd333t") {
		t.Fatalf("the matrix did not receive the listing:\n%s", v)
	}
	// tab then enter (T3-04: was r): resume the pane's own cursor, via session.Controller.Resume
	// — the same attach path a confirmed plan's Start now takes.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("tab+enter produced no command")
	}
	tm, resumeCmd := m.Update(cmd()) // matrix.ResumeMsg -> the root's own case
	if resumeCmd == nil {
		t.Fatal("ResumeMsg produced no command")
	}
	mm, _ := attach(t, tm.(Model), resumeCmd)
	if resumed != "5pr6sd333t" {
		t.Fatalf("Resume called with %q", resumed)
	}
	if n := len(mm.stack); n != 2 {
		t.Fatalf("stack has %d screens after resume, want 2 (matrix, flight)", n)
	}
	if v := plain(mm); !strings.Contains(v, "5pr6sd333t") || !strings.Contains(v, "app-staging → app-production") {
		t.Fatalf("flight screen not showing the resumed promotion:\n%s", v)
	}
	tm2, cmd := tea.Model(mm).Update(flight.BackMsg{})
	if cmd == nil {
		t.Fatal("popping back to the matrix must re-list at once")
	}
	cmd() // the Relist call itself
	if listed != 2 {
		t.Fatalf("listed %d times after popping back to the matrix, want 2", listed)
	}
	if v := plain(tm2); !strings.Contains(v, "FAMILY") {
		t.Fatalf("not back on the matrix:\n%s", v)
	}
}

// TestPRURLInActivity: session.ChangeBuilt's own "started" activity entry names the PR when
// Controller already has one — almost never true for a brand-new Start (the preflight that
// produces this Change runs before anything is pushed), but a resumed drive can already carry
// one, exactly like TestInFlightListingReachesTheMatrixAndResumeOpensTheFlightScreen's own
// fixture just above.
func TestPRURLInActivity(t *testing.T) {
	parked := engine.PromotionState{ID: "5pr6sd333t", SourceEnv: "app-staging", TargetEnv: "app-production",
		PR: &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}}
	svc := svcWithInFlight(fakeInFlight{
		Resume: func(_ context.Context, _ string) (engine.PromotionState, session.Driver, error) {
			return parked, driverAlways(parked), nil
		},
	})
	m := sizedWithService(t, svc, Promotion{}).(Model)
	tm, cmd := m.Update(matrix.ResumeMsg{ID: "5pr6sd333t"})
	root, _ := attach(t, tm.(Model), cmd)

	e, ok := root.activity.Latest()
	if !ok {
		t.Fatal("no activity entry after the resumed drive attached")
	}
	// Exact text, not just Contains(id): a resumed entry used to carry no source/target at all
	// (session.Controller.Resume, unlike Start, never had them to seed the entry with), so this
	// read "started 5pr6sd333t ( → )" — Contains(id) alone passed on that blank text just fine
	// (found in review of ceaccb2).
	if want := "started 5pr6sd333t (app-staging → app-production)"; e.Text != want {
		t.Errorf("activity entry Text = %q, want %q", e.Text, want)
	}
	if e.URL != "https://forge.example.invalid/pr/103" {
		t.Errorf("activity entry URL = %q, want the resumed promotion's PR URL", e.URL)
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
	m := sizedWithPromotion(t, testPromo{OpenURL: func(string) error { calls++; return nil }})
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
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil, apprestart.Funcs{}).WithDrift(drift)
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
	// T3-04: config moves from capital C to lower-case c (it only opens a screen to look at
	// something, like every other lower-case verb on the matrix).
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'c', Text: "c"})
	if cmd == nil {
		t.Fatal("c produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenConfigMsg); !ok {
		t.Fatalf("c's command yields %T, want matrix.OpenConfigMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after c, want 2", n)
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

// TestRepoRefreshedMsgFailureSurvivesAKeypress is P2 #7 from t2-review.md: a failed F5 used to
// become the matrix's own notice, which — like every matrix.Model.notice — is cleared the very
// next keypress (matrix/model.go's own `m.notice = ""` on every key). F5 is async: the operator
// can easily have pressed another key (moved the cursor, opened help) by the time the failure
// actually lands, so the refusal was routinely never read at all — the #164 shape the root's
// activity log exists to end. This asserts the failure reaches the activity log instead, and
// that an unrelated keypress afterward does not clear it.
func TestRepoRefreshedMsgFailureSurvivesAKeypress(t *testing.T) {
	tm := sized(t)
	tm2, _ := tm.Update(matrix.RepoRefreshedMsg{Gen: 0, Err: errors.New("dial tcp: no route to host")})
	m2 := tm2.(Model)
	if got := latestActivityText(m2); !strings.Contains(got, "no route to host") {
		t.Fatalf("activity log = %q, want it to carry the refresh failure", got)
	}
	m3, _ := press(t, m2, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if got := latestActivityText(m3.(Model)); !strings.Contains(got, "no route to host") {
		t.Errorf("the refresh failure was cleared by an unrelated keypress: latest activity is now %q", got)
	}
}

// TestDoneShowsNewTagWithoutF5 is Train 2 design PR 4's central promise: a promotion the
// operator watched finish (session.ChangeDone) refreshes the matrix's own repo read exactly as
// F5 does, so the new tag shows up without the operator pressing F5 themselves — and the pane
// re-lists afterward rather than waiting for the next tick. This drives app.Model.apply directly
// (an unexported method, same package) with a synthetic Change rather than a real driver/Step
// round trip: apply is exactly the wiring Train 2 design PR 4 adds, and testing it directly
// avoids a real Driver.Step's own scheduled next-poll command, which uses a real tea.Tick
// (session.Config.After's default) that would otherwise sleep for a real MinTick/Wait duration
// the instant a test drained it.
func TestDoneShowsNewTagWithoutF5(t *testing.T) {
	var listCalls int
	svc := &fakeService{
		ListFn: func(context.Context) ([]service.Listed, error) {
			listCalls++
			return nil, nil
		},
	}
	tm := sizedWithService(t, svc, Promotion{})
	m := tm.(Model)

	fresh, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	var refreshCalls int
	m = m.WithRefreshRepo(func(context.Context) (*gitops.Repo, string, error) {
		refreshCalls++
		return fresh, "", nil
	})

	before := listCalls
	m2, cmd := m.apply([]session.Change{{Kind: session.ChangeDone, Build: 1, ID: "abcd1234"}})
	m3 := drainRoot(m2, cmd)

	if refreshCalls != 1 {
		t.Fatalf("RefreshRepoFunc called %d times on Done, want exactly 1", refreshCalls)
	}
	if m3.repo != fresh {
		t.Errorf("root repo = %p, want the completion-triggered refresh's own repo (%p) adopted, matching what F5 already does", m3.repo, fresh)
	}
	ms, ok := m3.stack[0].(matrixScreen)
	if !ok {
		t.Fatal("stack[0] is not the matrix screen")
	}
	if ms.Repo() != fresh {
		t.Error("the matrix's own repo was not updated by the completion-triggered refresh — the cell would still show the old tag")
	}
	if listCalls <= before {
		t.Errorf("List not called again after the drive finished, calls before=%d after=%d — Done must relist, not just refresh", before, listCalls)
	}
}

// TestLandedRefreshesOnce proves apply's refresh/relist fire at MOST ONCE per call, never once
// per Change item in the batch — the case that matters is session.Controller.Update's own
// documented shape where a single Step that both lands and finishes produces ChangeLanded AND
// ChangeDone together for the same build (its own doc comment: "a stepMsg that both lands and
// finishes" produces two Changes). Refreshing twice for what the operator experiences as one
// event would not corrupt anything (matrix's own refreshingRepo/refreshAgain coalescing would
// catch the second one), but it is still a pointless second fetch this test exists to keep out.
// A bare ChangeStepped (still in flight, nothing landed) is the negative control: it must
// trigger no refresh at all.
func TestLandedRefreshesOnce(t *testing.T) {
	svc := &fakeService{}
	tm := sizedWithService(t, svc, Promotion{})
	m := tm.(Model)

	fresh, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	var refreshCalls int
	m = m.WithRefreshRepo(func(context.Context) (*gitops.Repo, string, error) {
		refreshCalls++
		return fresh, "", nil
	})

	m2, cmd := m.apply([]session.Change{{Kind: session.ChangeStepped, Build: 1, ID: "abcd1234"}})
	m2 = drainRoot(m2, cmd)
	if refreshCalls != 0 {
		t.Fatalf("refresh called %d times on a mere Stepped change, want 0", refreshCalls)
	}

	// The real shape ChangeLanded ships in: alongside ChangeDone, in the same batch, for the
	// same build (a promotion that lands and finishes in its very last Step).
	m3, cmd3 := m2.apply([]session.Change{
		{Kind: session.ChangeLanded, Build: 1, ID: "abcd1234"},
		{Kind: session.ChangeDone, Build: 1, ID: "abcd1234"},
	})
	_ = drainRoot(m3, cmd3)
	if refreshCalls != 1 {
		t.Fatalf("refresh called %d times for a Landed+Done batch, want exactly 1", refreshCalls)
	}
}

// TestLandedDriveRefreshesMatrixEndToEnd is P3 #10 from t2-review.md: TestDoneShowsNewTagWithoutF5
// and TestLandedRefreshesOnce both drive app.Model.apply directly with a synthetic
// session.Change, which proves apply's own wiring but never that a real
// Start -> Driver.Step -> session.Controller sequence actually produces the Change apply reacts
// to — the one-line joint (app.go's own case session.Event -> apply) had no test driving it from
// a real Change. This goes through the real path instead: plan.StartMsg -> the real
// testPromo-shaped Start closure -> a real Driver whose first Step call both lands and finishes
// in the same tick (Direct, PushedSHA set — the documented "a stepMsg that both lands and
// finishes" shape, session.Controller.Update's own doc comment) — using Promotion's own Now/After
// seam (added by this fix) so nothing here waits on a real clock. It asserts the
// completion-triggered refresh (RequestRefresh, the same path F5 takes) actually fires and the
// matrix cell renders the NEW tag, not just that a repo pointer changed.
func TestLandedDriveRefreshesMatrixEndToEnd(t *testing.T) {
	before := &gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{
		"app-staging": {Name: "app-staging", Families: map[string]*gitops.Family{
			"web": {Name: "web", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: "ghcr.io/example/web", Tag: "v1"}}}},
		}},
	}}
	after := &gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{
		"app-staging": {Name: "app-staging", Families: map[string]*gitops.Family{
			"web": {Name: "web", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: "ghcr.io/example/web", Tag: "v2"}}}},
		}},
	}}

	drv := funcDriver{
		StepFunc: func(context.Context) (service.Tick, error) {
			return service.Tick{
				State: engine.PromotionState{ID: "abcd1234", Direct: true, PushedSHA: "deadbeef"},
				Done:  true,
			}, nil
		},
		StateFunc: func() engine.PromotionState {
			return engine.PromotionState{ID: "abcd1234", Direct: true, PushedSHA: "deadbeef"}
		},
	}
	svc := &fakeService{startFn: func(context.Context, gitops.Plan, startOpts, func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: "abcd1234", Direct: true}, drv, nil
	}}

	fixedNow := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	promo := Promotion{
		Now:   func() time.Time { return fixedNow },
		After: func(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd { return func() tea.Msg { return f(fixedNow) } },
	}
	m := New(before, []string{"ghcr.io/"}, config.EnvsConfig{}, testPlanFunc([]string{"ghcr.io/"}, config.EnvsConfig{}), svc, promo, nil, apprestart.Funcs{})
	_ = m.Init()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = tm.(Model)

	if v := plain(m); !strings.Contains(v, "v1") {
		t.Fatalf("setup: matrix does not show the original tag v1:\n%s", v)
	}

	var refreshCalls int
	m = m.WithRefreshRepo(func(context.Context) (*gitops.Repo, string, error) {
		refreshCalls++
		return after, "", nil
	})

	tm2, startCmd := m.Update(plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"},
		Mode:   plan.ModeDirect,
		Source: "app-staging", Target: "app-production",
	})
	m2, stepCmd := attach(t, tm2.(Model), startCmd)
	m3 := drainRoot(m2, stepCmd)

	if refreshCalls != 1 {
		t.Fatalf("RefreshRepoFunc called %d times after a real landed+done drive, want exactly 1", refreshCalls)
	}

	// The refresh happens in the background while the flight screen is still on top (exactly
	// like a real F5 mid-promotion) — esc back to the matrix to see the cell it already updated.
	tm4, backCmd := tea.Model(m3).Update(flight.BackMsg{})
	m4 := drainRoot(tm4.(Model), backCmd)
	if v := plain(m4); !strings.Contains(v, "v2") {
		t.Fatalf("matrix cell does not show the new tag after the completion-triggered refresh:\n%s", v)
	}
}

// TestEarlierF5AnswerCannotOverwriteCompletionRefresh: the attacker is a slow F5 the operator
// pressed before a drive landed, whose answer (carrying the OLD repo, at generation 0 — a freshly
// built matrix.Model's own zero-value repoGen, exactly TestRepoRefreshedMsgUpdatesTheRootRepoToo's
// own convention for "the F5 that never actually advanced anything") arrives AFTER the
// completion-triggered refresh already adopted the new one. matrix.RepoRefreshedMsg's own
// repoGen guard (unchanged by this PR) is what has to catch this: askRepoRefresh bumps repoGen to
// a nonzero, process-wide-unique generation exactly as F5's own key handler always did, so the
// earlier F5's answer carries a now-stale generation and must be dropped, never allowed to
// regress the repo.
func TestEarlierF5AnswerCannotOverwriteCompletionRefresh(t *testing.T) {
	svc := &fakeService{}
	tm := sizedWithService(t, svc, Promotion{})
	m := tm.(Model)

	oldRepo, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	newRepo, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m = m.WithRefreshRepo(func(context.Context) (*gitops.Repo, string, error) { return newRepo, "", nil })

	m2, cmd := m.apply([]session.Change{{Kind: session.ChangeDone, Build: 1, ID: "abcd1234"}})
	m3 := drainRoot(m2, cmd)
	if m3.repo != newRepo {
		t.Fatalf("completion refresh did not land: root repo = %p, want %p", m3.repo, newRepo)
	}

	// The slow F5's own answer finally lands, carrying the OLD repo at generation 0 — necessarily
	// stale, since askRepoRefresh's nextRepoGen counter only ever counts up from 1 and the
	// completion refresh above already consumed one such generation.
	tm4, _ := tea.Model(m3).Update(matrix.RepoRefreshedMsg{Gen: 0, Repo: oldRepo})
	m4 := tm4.(Model)
	if m4.repo != newRepo {
		t.Errorf("an earlier F5 answer overwrote the completion refresh: root repo = %p, want it to stay %p (the earlier answer must be dropped as stale)", m4.repo, newRepo)
	}
}

// TestNoticeIsOnScreen is #164's regression, adapted for the activity row that replaced the old
// transient notice (Train 2 design PR9): every screen renders through ui.Frame.Render, which
// emits exactly `height` lines, so a row appended after that landed on row height+1 and the
// alternate screen buffer never showed it — an in-flight refusal on the deploy confirm screen
// was indistinguishable from a dead enter key.
//
// The existing notice tests (TestStartMsgShowsNoticeOnBuildError and friends) could not
// catch it: strings.Contains over the whole view is true whether or not the line is on the
// terminal. This asserts the SHAPE — exactly `height` lines, the row on the last one, and the
// screen still drawn above it — at both golden sizes (AGENTS.md §4.8).
func TestNoticeIsOnScreen(t *testing.T) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m := sized(t)
			m, _ = m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			// A real refusal, not a synthetic string: this is the exact message the
			// operator could not read (#164, #166).
			const text = "could not start promotion: promotion x5hz5gszie targeting spritz-staging is still in flight (at direct-pushed); run `hoist resume x5hz5gszie` instead of starting a second one"
			root := m.(Model).noteErr(text)
			lines := strings.Split(ansi.Strip(root.View().Content), "\n")
			if len(lines) != size.h {
				t.Fatalf("view is %d lines on a %d-row terminal; the activity row must take a row FROM the screen, never add one past the bottom", len(lines), size.h)
			}
			for i, line := range lines {
				if w := ansi.StringWidth(line); w > size.w {
					t.Errorf("line %d is %d cells wide, over %d:\n%s", i+1, w, size.w, line)
				}
			}
			// Unlike the old notice (wrapped over up to ui.NoticeMaxLines), the activity row is
			// exactly one line: the message's own head, plus the "l: activity" hint.
			last := lines[len(lines)-1]
			if !strings.Contains(last, "could not start promotion") {
				t.Errorf("the activity row does not start with the failure text; got:\n%s", last)
			}
			if !strings.Contains(last, "l: activity (1)") {
				t.Errorf("the activity row is missing its own hint; got:\n%s", last)
			}
			// Positive control: the screen the row explains is still drawn above it, so this
			// cannot pass by rendering the row alone.
			if !strings.Contains(strings.Join(lines[:len(lines)-1], "\n"), "FAMILY") {
				t.Error("the matrix is gone from above the activity row")
			}
		})
	}
}

// TestNoticeTooLongForTheTerminalIsCapped: a git or forge transport error runs long, and the
// activity row's own line is taken from the screen it is explaining — so, unlike the old notice
// (wrapped and capped at ui.NoticeMaxLines), it is truncated to exactly one line rather than
// allowed to push that screen away or grow past a single row. The full error is never lost: it
// stays on the entry itself, readable in full on the activity screen (l) —
// TestLongErrorFullInActivityView (internal/app/activity) is that half's own regression test.
func TestNoticeTooLongForTheTerminalIsCapped(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	root := m.(Model).noteErr("could not start promotion: " + strings.Repeat("a very long transport error ", 40))
	lines := strings.Split(ansi.Strip(root.View().Content), "\n")
	if len(lines) != height {
		t.Fatalf("view is %d lines on a %d-row terminal, want exactly %d", len(lines), height, height)
	}
	rows := 0
	for _, line := range lines {
		if strings.Contains(line, "very long transport error") || strings.Contains(line, "could not start promotion") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("the activity row took %d terminal rows, want exactly 1 (never wrapped over several, unlike the old notice)", rows)
	}
	if !strings.Contains(lines[len(lines)-1], "…") {
		t.Errorf("a truncated activity row must mark its overflow with an ellipsis; last line:\n%s", lines[len(lines)-1])
	}
}

// TestSummaryForUnconfiguredRepoNamesWhyAndCannotReobserve pins the orphan-state coverage lost
// when cmd/hoist/inflight_test.go's TestBuildInFlightFuncsListsAndNamesTheUnobservable was
// deleted (t1-review.md P1 #2): a service.Listed for a repo that has since left the config file
// must still turn into a flight.Summary whose Err names "not in the config file" (never dropped,
// never a bare "unconfigured" with no reason) and whose Verdict reads "cannot re-observe" —
// exactly as cmd/hoist's old observeForList wording did.
func TestSummaryForUnconfiguredRepoNamesWhyAndCannotReobserve(t *testing.T) {
	listed := service.Listed{
		State:        engine.PromotionState{ID: "orphan01", RepoFullName: "someone/else"},
		Unconfigured: true,
	}
	sum := summaryFor(listed)
	if !strings.Contains(sum.Err, "not in the config file") {
		t.Fatalf("summaryFor(Unconfigured).Err = %q, want it to contain %q", sum.Err, "not in the config file")
	}
	if v := sum.Verdict(); v != "cannot re-observe" {
		t.Fatalf("summaryFor(Unconfigured).Verdict() = %q, want %q", v, "cannot re-observe")
	}
}

// TestPlanStartMsgCarriesItsOwnPlannedView pins t1-review.md's P2-a finding: the plan.StartMsg
// case (app.go, the plan.StartMsg branch building service.StartRequest) must pass THIS message's
// own View through to svc.StartPromotion as StartRequest.View — not nil (which would make
// StartPromotion silently read s.Repo() at call time instead, service.StartRequest.View's own
// doc comment) and not some other view. This is the freshness-check plumbing t1-review.md P2 #6
// added: a plan built against view A must be checked against view A, even if the service's own
// current view has since moved to B (an F5 refresh landing between building the plan and
// confirming it). Mutation check: replacing `View: &view` with `View: nil` at the plan.StartMsg
// call site (app.go, ~line 652) makes this test fail with "StartRequest.View = <nil>, want ...".
func TestPlanStartMsgCarriesItsOwnPlannedView(t *testing.T) {
	wantView := service.RepoView{Dir: "/plan-view-a", FromOrigin: true, SHA: "plan-view-a-sha"}
	var got *service.RepoView
	svc := &fakeService{
		onStart: func(req service.StartRequest) { got = req.View },
		startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			return engine.PromotionState{}, driverAlways(engine.PromotionState{}), nil
		},
	}
	m := sizedWithService(t, svc, Promotion{})
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}, View: wantView}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("plan.StartMsg produced no command")
	}
	sessionBuildCmd(t, cmd)()
	if got == nil {
		t.Fatal("StartRequest.View = <nil>, want a pointer to the plan.StartMsg's own View")
	}
	if *got != wantView {
		t.Errorf("StartRequest.View = %+v, want the plan.StartMsg's own View %+v", *got, wantView)
	}
}

// TestDeployStartMsgCarriesItsOwnPlannedView is TestPlanStartMsgCarriesItsOwnPlannedView's twin
// for the deploy.StartMsg path (app.go's deploy.StartMsg case, ~line 1007), including the
// WithView plumbing that carries service.PlannedChange.View from the matrix's openDeploy through
// deploy.Model.WithView into this message (deploy/model.go's own StartMsg.View doc comment).
// Mutation check: replacing `View: &view` with `View: nil` at the deploy.StartMsg call site
// makes this test fail the same way.
func TestDeployStartMsgCarriesItsOwnPlannedView(t *testing.T) {
	wantView := service.RepoView{Dir: "/deploy-view-b", FromOrigin: true, SHA: "deploy-view-b-sha"}
	var got *service.RepoView
	svc := &fakeService{
		onStart: func(req service.StartRequest) { got = req.View },
		startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			return engine.PromotionState{}, driverAlways(engine.PromotionState{}), nil
		},
	}
	m := sizedWithService(t, svc, Promotion{})
	msg := deploy.StartMsg{
		Plan:   gitops.Plan{Variant: gitops.VariantDeploy, TargetEnv: "app-production"},
		Mode:   deploy.ModePR,
		Target: "app-production",
		Image:  "ghcr.io/example/web:v2",
		View:   wantView,
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("deploy.StartMsg produced no command")
	}
	sessionBuildCmd(t, cmd)()
	if got == nil {
		t.Fatal("StartRequest.View = <nil>, want a pointer to the deploy.StartMsg's own View")
	}
	if *got != wantView {
		t.Errorf("StartRequest.View = %+v, want the deploy.StartMsg's own View %+v", *got, wantView)
	}
}

// TestOpenDeployPlumbsPlannedViewToStartRequest closes the gap
// TestDeployStartMsgCarriesItsOwnPlannedView leaves open: that test builds deploy.StartMsg BY
// HAND, with its View already populated, so it never exercises openDeploy (app.go) itself —
// it would stay green even if openDeploy's own `.WithView(pc.View)` call were deleted. This test
// drives the real flow instead: tags.SelectedMsg (a picker selection) into openDeploy, which
// calls m.planFn and must carry the returned PlannedChange.View into deploy.New(...).WithView(...);
// pressing Enter on the resulting deploy confirm screen must then carry that same view, unbroken,
// through deploy.StartMsg into service.StartRequest.View.
//
// Mutation check: deleting the `.WithView(pc.View)` call in openDeploy (app.go, ~line 1124) makes
// this test fail with the zero RepoView instead of wantView.
func TestOpenDeployPlumbsPlannedViewToStartRequest(t *testing.T) {
	wantView := service.RepoView{Dir: "/open-deploy-view", FromOrigin: true, SHA: "open-deploy-view-sha"}
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	inner := testPlanFunc([]string{"ghcr.io/"}, config.EnvsConfig{})
	planFn := func(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		pc, err := inner(ctx, req)
		if err != nil {
			return pc, err
		}
		pc.View = wantView
		return pc, nil
	}
	var got *service.RepoView
	svc := &fakeService{
		onStart: func(req service.StartRequest) { got = req.View },
		startFn: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
			return engine.PromotionState{}, driverAlways(engine.PromotionState{}), nil
		},
	}

	var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, planFn, svc, Promotion{}, nil, apprestart.Funcs{})
	_ = tm.Init()
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})

	// The real flow: a picker selection reaches the root as tags.SelectedMsg, which openDeploy
	// (app.go) turns into a plan (via planFn) and a pushed deploy confirm screen.
	tm, _ = tm.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/web",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	stack := tm.(Model).stack
	if _, ok := stack[len(stack)-1].(deployScreen); !ok {
		t.Fatalf("top screen is %T, want the deploy confirm", stack[len(stack)-1])
	}

	// Enter on the confirm screen: the screen itself emits deploy.StartMsg (unpacked one level,
	// the same shape TestSelectedMsgOpensTheDeployConfirmScreen and friends drive by hand — the
	// real tea runtime would do this same re-dispatch).
	var cmd tea.Cmd
	tm, cmd = tm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the deploy confirm screen produced no command")
	}
	startMsg := cmd()
	if _, ok := startMsg.(deploy.StartMsg); !ok {
		t.Fatalf("enter yields %T, want deploy.StartMsg", startMsg)
	}
	_, cmd = tm.Update(startMsg)
	if cmd == nil {
		t.Fatal("deploy.StartMsg produced no command")
	}
	sessionBuildCmd(t, cmd)()

	if got == nil {
		t.Fatal("StartRequest.View = <nil>, want a pointer to the plan's own PlannedChange.View")
	}
	if *got != wantView {
		t.Errorf("StartRequest.View = %+v, want openDeploy's planned view %+v", *got, wantView)
	}
}

// TestViewRequestsAllKeysAsEscapeCodes: T3-01 asks every render for the Kitty-protocol feature
// that can tell a real shift+letter from a caps-lock letter — the one signal
// internal/ui/keys.Binding.Matches needs to reject caps lock on a terminal that grants it
// (train3-design.md's "Modifier" note). A terminal that doesn't support the request, or
// doesn't grant it, simply never sends a KeyboardEnhancementsMsg back — see
// TestKeyboardEnhancementsRecorded below for what happens when it does.
func TestViewRequestsAllKeysAsEscapeCodes(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	v := m.(Model).View()
	if !v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes {
		t.Error("View did not request KeyboardEnhancements.ReportAllKeysAsEscapeCodes")
	}
}

// TestKeyboardEnhancementsRecorded: the root stores whatever the terminal answers, for the one
// consumer that needs it later (T3-03's help overlay line, "caps lock ignored" vs "a capital
// counts as shift") — it is not itself a registry row, since internal/parity's parser only
// collects `case pkg.XMsg:` from app.go and explicitly skips tea.* messages
// (train3-design.md's "Parity and docs: parity has no change").
func TestKeyboardEnhancementsRecorded(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	msg := tea.KeyboardEnhancementsMsg{Flags: 1 << 3} // ReportAllKeysAsEscapeCodes bit
	m, cmd := m.Update(msg)
	if cmd != nil {
		t.Error("KeyboardEnhancementsMsg should produce no command")
	}
	if !m.(Model).kbd.SupportsAllKeysAsEscapeCodes() {
		t.Error("Model did not record the terminal's KeyboardEnhancementsMsg")
	}
}

// openWatchScreenForTest pushes the watch screen with an immediate, non-blocking snapshot —
// T3-03's own help-overlay and l-from-anywhere tests need a real keyed screen on top of the
// matrix, not the matrix itself.
func openWatchScreenForTest(t *testing.T) tea.Model {
	t.Helper()
	build := func(family, env string) (watch.Funcs, error) {
		return watch.Funcs{
			Read: func(context.Context) (watch.Snapshot, error) {
				return watch.Snapshot{App: family + "-" + env, Namespace: env}, nil
			},
			Interval: time.Hour, // no tick within the test's own lifetime
			Now:      time.Now,
		}, nil
	}
	var tm tea.Model = sized(t).(Model).WithWatch(build)
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	tm, cmd := tm.Update(matrix.OpenWatchMsg{Family: "counta", Target: "app-production"})
	if cmd != nil {
		// Runs the immediate first poll (Init's own doc comment: "hoist watch --once is this
		// screen's first paint") synchronously and stops — the resulting command is watch's own
		// tea.Tick-based poll cadence (Interval, deliberately an hour above), which would sleep
		// for real if this called it too, unlike drainTags' own generic loop.
		tm, _ = tm.Update(cmd())
	}
	if _, ok := tm.(Model).stack[len(tm.(Model).stack)-1].(watchScreen); !ok {
		t.Fatalf("setup: top screen is %T, want the watch screen", tm.(Model).stack[len(tm.(Model).stack)-1])
	}
	return tm
}

// TestInFlightIDsNamesEveryTrackedPromotion: cmd/hoist's own main.go prints these with their
// `hoist resume <id>` line once the program actually quits (keymap rule 4). Control: a fresh
// root with nothing running names none.
func TestInFlightIDsNamesEveryTrackedPromotion(t *testing.T) {
	m := sized(t).(Model)
	if ids := m.InFlightIDs(); len(ids) != 0 {
		t.Fatalf("a fresh root should track nothing: got %v", ids)
	}

	promo := testPromo{Start: func(_ context.Context, _ gitops.Plan, _ startOpts, _ func(string)) (engine.PromotionState, session.Driver, error) {
		return engine.PromotionState{ID: "abcd1234"}, driverAlways(engine.PromotionState{ID: "abcd1234"}), nil
	}}
	mm := sizedWithPromotion(t, promo).(Model)
	tm, cmd := mm.Update(plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}})
	attached, _ := attach(t, tm.(Model), cmd)
	if ids := attached.InFlightIDs(); len(ids) != 1 || ids[0] != "abcd1234" {
		t.Fatalf("InFlightIDs() = %v, want [abcd1234]", ids)
	}
}

// TestQuitOnlyFromMatrix is the audit keymap's own rule 4, proven at the root: q quits (with a
// confirm if a drive is running — already covered by TestQuitWithRunningDriveAsksFirst) only
// when the matrix is the only screen on the stack; anywhere else it is unbound and raises the
// transient hint instead, taking its own row out of the top screen exactly the way the activity
// row does (§9 entry 10) — never appended past the bottom, always exactly one row, at row
// height-1. Prove a new test can fail (AGENTS.md §8): dropping the `if _, onMatrix :=
// m.top().(matrixScreen); onMatrix` guard in app.go's own "q" case (so every screen quits again)
// makes this fail for the right reason (checked by hand before landing).
func TestQuitOnlyFromMatrix(t *testing.T) {
	// From the matrix: q quits.
	if _, cmd := press(t, sized(t), tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd == nil {
		t.Fatal("q on the matrix produced no command")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q on the matrix yields %T, want tea.QuitMsg", cmd())
	}

	// From the watch screen: q never quits, and raises the hint on its own row.
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			tm := openWatchScreenForTest(t)
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			tm, cmd := tm.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
			if cmd != nil {
				if _, ok := cmd().(tea.QuitMsg); ok {
					t.Fatal("q on the watch screen quit the program")
				}
			}
			lines := strings.Split(ansi.Strip(tm.(Model).View().Content), "\n")
			if len(lines) != size.h {
				t.Fatalf("view is %d lines on a %d-row terminal, want exactly %d", len(lines), size.h, size.h)
			}
			last := lines[len(lines)-1]
			if !strings.Contains(last, "q quits from the matrix") || !strings.Contains(last, "esc goes back") {
				t.Errorf("row %d (height-1) should carry the hint; got:\n%s", len(lines)-1, last)
			}
			if !strings.Contains(strings.Join(lines[:len(lines)-1], "\n"), "app-production") {
				t.Error("the watch screen is gone from above the hint row")
			}
		})
	}
}

// TestQuitHintClearsOnNextKey: the hint is transient — Model.hint's own doc comment — clearing
// unconditionally at the top of the very next keypress, whatever that key is.
func TestQuitHintClearsOnNextKey(t *testing.T) {
	tm := openWatchScreenForTest(t)
	tm, _ = tm.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if !strings.Contains(plain(tm), "q quits from the matrix") {
		t.Fatalf("setup: q should have raised the hint:\n%s", plain(tm))
	}
	tm, _ = tm.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if strings.Contains(plain(tm), "q quits from the matrix") {
		t.Errorf("the hint should have cleared on the next key:\n%s", plain(tm))
	}
}

// TestHelpOverlayGoldens: the overlay's own rendered shape for every screen this PR migrated,
// at both terminal sizes every screen is goldened at (AGENTS.md §4.8) — compared by hand against
// the approved v2·03 mockup's structure (NAVIGATE/ACT/VIEW/APP columns, the "shift+ keys always
// ask" line) in this PR's own report, since no mockup fixture exists for these four screens
// (train3-design.md's T3-03 scope: only help-matrix, in T3-04, gets a mockup diff).
func TestHelpOverlayGoldens(t *testing.T) {
	sizes := []struct{ w, h int }{{80, 24}, {120, 40}}

	// T3-04: the matrix now implements keyed too — help-matrix replaces the retired
	// matrix-help golden (matrix's own former bubbles help.Model view), compared to v2·03.
	t.Run("matrix", func(t *testing.T) {
		for _, size := range sizes {
			tm := sized(t)
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			uitest.Golden(t, "help-matrix", tm.(Model).View().Content, size.w, size.h)
		}
	})

	t.Run("watch", func(t *testing.T) {
		for _, size := range sizes {
			tm := openWatchScreenForTest(t)
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			uitest.Golden(t, "help-watch", tm.(Model).View().Content, size.w, size.h)
		}
	})

	t.Run("config", func(t *testing.T) {
		r, err := gitops.Discover(fixtureRoot, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, size := range sizes {
			var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil, apprestart.Funcs{}).
				WithConfigView("/tmp/hoist/config.yaml", true, "repos: []\n")
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			tm, _ = tm.Update(matrix.OpenConfigMsg{})
			tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			uitest.Golden(t, "help-config", tm.(Model).View().Content, size.w, size.h)
		}
	})

	t.Run("activity", func(t *testing.T) {
		for _, size := range sizes {
			tm := sized(t).(Model).note(activity.Info, "something happened", "", "")
			var tmodel tea.Model = tm
			tmodel, _ = tmodel.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			// l on the matrix now opens the activity log through the root's own generic
			// "any keyed screen" handling (T3-04) rather than a per-screen message.
			tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
			tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			uitest.Golden(t, "help-activity", tmodel.(Model).View().Content, size.w, size.h)
		}
	})

	t.Run("restart", func(t *testing.T) {
		read := func(_ context.Context, env string, names []string) (restart.Plan, error) {
			p := restart.Plan{Env: env}
			for _, n := range names {
				p.Targets = append(p.Targets, rollout.DeploymentStatus{Namespace: env, Name: n, Replicas: 1, Strategy: "RollingUpdate"})
			}
			return p, nil
		}
		r, err := gitops.Discover(fixtureRoot, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, size := range sizes {
			var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil,
				apprestart.Funcs{Read: read, Interval: time.Millisecond})
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: max(size.w, 300), Height: size.h})
			tm, cmd := tm.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
			tm = drainTags(tm, cmd)
			tm, _ = tm.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
			tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			uitest.Golden(t, "help-restart", tm.(Model).View().Content, size.w, size.h)
		}
	})
}

// TestHelpOverlayOnEveryKeyedScreen: ? opens the overlay for every screen this PR migrated
// (watch, restart, config, activity) — each keyed via internal/app/screen.go's own KeyScreen
// adapters — and esc returns to the identical view underneath. Prove a new test can fail
// (AGENTS.md §8): removing a screen's own KeyScreen() method (so it no longer implements keyed)
// makes ? do nothing for it, failing this test for the right reason (checked by hand on the
// watch screen before landing).
func TestHelpOverlayOnEveryKeyedScreen(t *testing.T) {
	t.Run("watch", func(t *testing.T) {
		tm := openWatchScreenForTest(t)
		before := plain(tm)
		tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		opened := plain(tm)
		if opened == before {
			t.Fatal("? did not change the view")
		}
		if !strings.Contains(opened, "help · watch") {
			t.Errorf("the overlay should name its own screen:\n%s", opened)
		}
		tm, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if got := plain(tm); got != before {
			t.Errorf("esc should return to the identical view:\nbefore:\n%s\nafter:\n%s", before, got)
		}
	})

	t.Run("config", func(t *testing.T) {
		r, err := gitops.Discover(fixtureRoot, "")
		if err != nil {
			t.Fatal(err)
		}
		var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil, apprestart.Funcs{}).
			WithConfigView("/tmp/hoist/config.yaml", true, "repos: []\n")
		tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
		tm, _ = tm.Update(matrix.OpenConfigMsg{})
		before := plain(tm)
		tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		if !strings.Contains(plain(tm), "help · config") {
			t.Errorf("the overlay should name its own screen:\n%s", plain(tm))
		}
		tm, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if got := plain(tm); got != before {
			t.Errorf("esc should return to the identical view:\nbefore:\n%s\nafter:\n%s", before, got)
		}
	})

	t.Run("activity", func(t *testing.T) {
		tm := sized(t).(Model).note(activity.Info, "something happened", "", "")
		var tmodel tea.Model = tm
		tmodel, _ = tmodel.Update(tea.WindowSizeMsg{Width: width, Height: height})
		tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
		before := plain(tmodel)
		tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		if !strings.Contains(plain(tmodel), "help · activity") {
			t.Errorf("the overlay should name its own screen:\n%s", plain(tmodel))
		}
		tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if got := plain(tmodel); got != before {
			t.Errorf("esc should return to the identical view:\nbefore:\n%s\nafter:\n%s", before, got)
		}
	})

	t.Run("restart", func(t *testing.T) {
		read := func(_ context.Context, env string, names []string) (restart.Plan, error) {
			p := restart.Plan{Env: env}
			for _, n := range names {
				p.Targets = append(p.Targets, rollout.DeploymentStatus{Namespace: env, Name: n, Replicas: 1, Strategy: "RollingUpdate"})
			}
			return p, nil
		}
		r, err := gitops.Discover(fixtureRoot, "")
		if err != nil {
			t.Fatal(err)
		}
		var tm tea.Model = New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil,
			apprestart.Funcs{Read: read, Interval: time.Millisecond})
		tm, _ = tm.Update(tea.WindowSizeMsg{Width: 300, Height: height})
		tm, cmd := tm.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
		tm = drainTags(tm, cmd)
		if _, ok := tm.(Model).stack[len(tm.(Model).stack)-1].(restartScreen); !ok {
			t.Fatalf("setup: top screen is %T, want the restart screen", tm.(Model).stack[len(tm.(Model).stack)-1])
		}
		before := plain(tm)
		tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		if !strings.Contains(plain(tm), "help · restart") {
			t.Errorf("the overlay should name its own screen:\n%s", plain(tm))
		}
		tm, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if got := plain(tm); got != before {
			t.Errorf("esc should return to the identical view:\nbefore:\n%s\nafter:\n%s", before, got)
		}
	})
}

// TestEscInHelpClosesOnlyHelp: with the overlay open, esc closes only it — the screen
// underneath is exactly what it was before ? was pressed, never popped itself.
func TestEscInHelpClosesOnlyHelp(t *testing.T) {
	tm := openWatchScreenForTest(t)
	before := plain(tm)
	tm, _ = tm.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	if !tm.(Model).helpOpen {
		t.Fatal("setup: ? should have opened the overlay")
	}
	tm, _ = tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if tm.(Model).helpOpen {
		t.Error("esc should have closed the overlay")
	}
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("esc in the overlay popped a screen: stack has %d, want 2 (matrix + watch)", n)
	}
	if got := plain(tm); got != before {
		t.Errorf("the screen underneath should be unchanged:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	// Every other key but esc/?/enter is swallowed while the overlay is open — ctrl+c still quits.
	tm2, _ := tm.(Model).Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	tm2, cmd := tm2.(Model).Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd != nil {
		t.Error("a swallowed key inside the overlay should produce no command")
	}
	if !tm2.(Model).helpOpen {
		t.Error("an unrelated key should not have closed the overlay")
	}
	_, cmd = tm2.(Model).Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should still quit while the overlay is open")
	}
}

// TestLOpensActivityFromWatch: l opens the activity log from the watch screen (the proposed
// keymap's "l activity log everywhere"), handled generically at the root for any keyed screen
// rather than a per-screen message (train3-design.md's own scope note).
func TestLOpensActivityFromWatch(t *testing.T) {
	tm := openWatchScreenForTest(t).(Model).note(activity.Info, "something happened", "", "")
	var tmodel tea.Model = tm
	tmodel, _ = tmodel.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	stack := tmodel.(Model).stack
	if _, ok := stack[len(stack)-1].(activityScreen); !ok {
		t.Fatalf("l did not open the activity screen; top is %T", stack[len(stack)-1])
	}
	if !strings.Contains(plain(tmodel), "something happened") {
		t.Errorf("the activity screen should show the log:\n%s", plain(tmodel))
	}
}

// TestWatchFooterNoContradiction: UX-H9's own acceptance check, at the root — the watch
// screen's footer never claims both "never refreshes" (it polls on a cadence) and a "poll now"
// verb that duplicates r's own "refresh" meaning everywhere else in the app.
func TestWatchFooterNoContradiction(t *testing.T) {
	tm := openWatchScreenForTest(t)
	v := plain(tm)
	if strings.Contains(v, "never refreshes") {
		t.Errorf("the watch footer should not claim it never refreshes:\n%s", v)
	}
	if strings.Contains(v, "poll now") {
		t.Errorf("the watch footer should say refresh, not poll now:\n%s", v)
	}
	if !strings.Contains(v, "r refresh") {
		t.Errorf("the watch footer should offer r refresh:\n%s", v)
	}
}
