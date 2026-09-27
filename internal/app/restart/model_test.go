package restart

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/rollout"
)

// fakeFuncs is the whole of this screen's world, recorded so a test can see what it asked for.
type fakeFuncs struct {
	plan     restart.Plan
	readErr  error
	doErr    error
	doneUpTo int
	progress [][]restart.Progress
	restarts int
	obs      int
}

func (f *fakeFuncs) funcs() Funcs {
	return Funcs{
		Read: func(context.Context, string, []string) (restart.Plan, error) {
			return f.plan, f.readErr
		},
		Do: func(_ context.Context, p restart.Plan, _ time.Time) ([]string, error) {
			f.restarts++
			if f.doErr != nil {
				return p.Names()[:f.doneUpTo], f.doErr
			}
			return p.Names(), nil
		},
		Observe: func(context.Context, string, []string, time.Time) ([]restart.Progress, error) {
			i := f.obs
			f.obs++
			if i >= len(f.progress) {
				i = len(f.progress) - 1
			}
			return f.progress[i], nil
		},
		Interval: time.Millisecond,
	}
}

func onePlan() restart.Plan {
	return restart.Plan{
		Env: "app-staging",
		Targets: []rollout.DeploymentStatus{{
			Namespace: "app-staging", Name: "web",
			Replicas: 1, Strategy: "RollingUpdate",
			Images: []rollout.ContainerImage{{Name: "web", Image: "ghcr.io/example/web:v1"}},
		}},
	}
}

// drain runs a command the way the runtime would, recursively, via uitest.Drain — which unpacks
// every tea.BatchMsg in order (AGENTS.md §9 entry 7). This package's own busyMarker is a plain
// static string, never a ticking spinner (P3 #9, t2-review.md), but Init/start/the Rolling
// transition can still batch other real work alongside a scope.Do call, so a hand-rolled
// single-level unwrap would mishandle that batch the same way a nested one would.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	return uitest.Drain(m, cmd, func(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) })
}

func ready(t *testing.T, f *fakeFuncs, production bool) Model {
	t.Helper()
	m := New("app-staging", "web", []string{"web"}, production, f.funcs(), ui.NewStyles(true)).SetSize(120, 30)
	return drain(t, m, m.Init())
}

// The screen shows what will roll, with the reasons it may not be seamless, before anything is
// written. A restart has no diff — nothing in the manifest changes — so this list IS the thing
// the operator is confirming.
func TestScreenShowsTheTargetsAndTheirWarnings(t *testing.T) {
	f := &fakeFuncs{plan: onePlan()}
	m := ready(t, f, false)
	v := m.View()
	for _, want := range []string{"hoist · restart", "app-staging", "web", "1 replica", "never restarted this way", "enter restart"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	// The concerns come from the live spec, through rollout.DeploymentStatus.
	if !strings.Contains(v, "no redundancy") {
		t.Errorf("a single-replica Deployment should say so:\n%s", v)
	}
	if !strings.Contains(v, "unpinned image") {
		t.Errorf("a mutable tag should be called out:\n%s", v)
	}
	if f.restarts != 0 {
		t.Error("nothing may be restarted before the operator confirms")
	}
}

// Enter on a non-production env restarts and follows the rollout to the end.
func TestEnterRestartsAndFollowsTheRollout(t *testing.T) {
	f := &fakeFuncs{
		plan: onePlan(),
		progress: [][]restart.Progress{
			{{Name: "web", Detail: "1 of 1 updated"}},
			{{Name: "web", Done: true, Detail: "successfully rolled out"}},
		},
	}
	m := ready(t, f, false)
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	m = drain(t, m, cmd)
	// Tick through until it settles.
	for i := 0; i < 10 && m.state != stateDone && m.state != stateFailed; i++ {
		m, cmd = m.Update(scope.Result[tickMsg]{From: m.scope.ID, V: tickMsg{}})
		m = drain(t, m, cmd)
	}
	if m.state != stateDone {
		t.Fatalf("state = %v, want done (notice: %q)", m.state, m.notice)
	}
	if f.restarts != 1 {
		t.Errorf("expected exactly one restart, got %d", f.restarts)
	}
	if !strings.Contains(m.View(), "all rolled") {
		t.Errorf("the screen should say it finished:\n%s", m.View())
	}
}

// Production takes a second, deliberate gesture — §4.5's PR-and-approval gate cannot apply to
// something that commits nothing, so this stands in its place. Driven through real keys: the
// bool huh writes to lives on a superseded Model copy, so setting it directly proves nothing.
func TestProductionNeedsTheExtraConfirmation(t *testing.T) {
	f := &fakeFuncs{plan: onePlan(), progress: [][]restart.Progress{{{Name: "web", Done: true}}}}
	m := ready(t, f, true)
	if !strings.Contains(m.View(), "production") {
		t.Errorf("the header should say this is production:\n%s", m.View())
	}

	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.CapturesText() {
		t.Fatal("enter on production must open the confirmation, not restart")
	}
	if f.restarts != 0 {
		t.Fatal("nothing may be restarted before the confirmation is answered")
	}
	m = drain(t, m, cmd)

	// Answering no restarts nothing.
	n, _ := m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	n, _ = n.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if f.restarts != 0 {
		t.Errorf("declining must not restart: %d", f.restarts)
	}
	if !strings.Contains(n.View(), "not restarted") {
		t.Errorf("declining should say so:\n%s", n.View())
	}

	// Answering yes does.
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if f.restarts != 1 {
		t.Errorf("confirming should restart exactly once, got %d", f.restarts)
	}
	if m.CapturesText() {
		t.Error("the confirmation should be closed once it is answered")
	}
	if m.state != stateRolling && m.state != stateDone {
		t.Errorf("state = %v, want the restart under way", m.state)
	}
}

// A restart something else supersedes mid-flight is not this screen's success to claim.
func TestSupersededRestartIsNotReportedAsSuccess(t *testing.T) {
	f := &fakeFuncs{
		plan:     onePlan(),
		progress: [][]restart.Progress{{{Name: "web", Superseded: true}}},
	}
	m := ready(t, f, false)
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	for i := 0; i < 5 && m.state == stateRolling; i++ {
		m, cmd = m.Update(scope.Result[tickMsg]{From: m.scope.ID, V: tickMsg{}})
		m = drain(t, m, cmd)
	}
	if m.state != stateFailed {
		t.Fatalf("state = %v, want failed", m.state)
	}
	if !strings.Contains(m.View(), "restarted by something else") {
		t.Errorf("the screen should say what actually happened:\n%s", m.View())
	}
}

// A read that fails leaves a screen that says why, not an empty one.
func TestReadFailureIsReported(t *testing.T) {
	f := &fakeFuncs{readErr: errors.New("kube: connection refused")}
	m := ready(t, f, false)
	if m.state != stateFailed {
		t.Fatalf("state = %v, want failed", m.state)
	}
	if !strings.Contains(m.View(), "connection refused") {
		t.Errorf("the failure should be on screen:\n%s", m.View())
	}
	// And enter does nothing from there.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		if _, ok := cmd().(scope.Result[startedMsg]); ok {
			t.Error("enter must not restart after a failed read")
		}
	}
}

// Esc asks to be popped, from any state.
func TestEscAsksToBePopped(t *testing.T) {
	m := ready(t, &fakeFuncs{plan: onePlan()}, false)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
}

// TestEscDuringRollingSaysRollingContinues is FB-L3's own regression: esc while a restart the
// operator already confirmed is still starting or rolling must say so on its way out
// (BackMsg.RollingContinues), since the restart itself is a live cluster operation this package
// cannot cancel — the screen leaving is not the restart stopping. Prove a new test can fail
// (AGENTS.md §8): reverting RollingContinues to always false makes this fail for the right
// reason (checked by hand before this landed).
func TestEscDuringRollingSaysRollingContinues(t *testing.T) {
	m := ready(t, &fakeFuncs{plan: onePlan()}, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // stateStarting
	if m.state != stateStarting {
		t.Fatalf("setup: state = %v, want stateStarting", m.state)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	back, ok := cmd().(BackMsg)
	if !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
	if !back.RollingContinues {
		t.Error("esc while starting must set RollingContinues")
	}

	m, _ = m.Update(scope.Result[startedMsg]{From: m.scope.ID, V: startedMsg{at: time.Now().UTC()}})
	if m.state != stateRolling {
		t.Fatalf("setup: state = %v, want stateRolling", m.state)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	back, ok = cmd().(BackMsg)
	if !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
	if !back.RollingContinues {
		t.Error("esc while rolling must set RollingContinues")
	}
}

// TestEscBeforeStartDoesNotSayRollingContinues is the above's own control: esc from the plain
// confirm state (nothing started yet) must not claim a rollout is continuing.
func TestEscBeforeStartDoesNotSayRollingContinues(t *testing.T) {
	m := ready(t, &fakeFuncs{plan: onePlan()}, false)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	back, ok := cmd().(BackMsg)
	if !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
	if back.RollingContinues {
		t.Error("esc before a restart even started must not say RollingContinues")
	}
}

func TestRestartGolden(t *testing.T) {
	f := &fakeFuncs{plan: onePlan()}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := ready(t, f, true).SetSize(size[0], size[1])
		uitest.Golden(t, "restart", m.View(), size[0], size[1])
	}
	// The production dialog sits over the list.
	m := ready(t, f, true).SetSize(80, 24)
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if v := m.View(); !strings.Contains(v, "It is a production env") || !strings.Contains(v, "1 Deployment in app-staging") {
		t.Fatalf("dialog must sit over the screen:\n%s", v)
	}
	uitest.Golden(t, "restart-confirm", m.View(), 80, 24)
}

// TestRestartRollingGolden is #PR8/FB-M7's own visible-wait golden: the moment a restart has
// actually been asked for but no rollout progress has been observed yet, every target still
// shows the "…" pending marker (render's own case), and the header now carries busyMarker —
// before this PR, "rolling" here read identically to a screen that had silently wedged. The
// startedMsg is delivered directly, not through the real cmd chain the enter key would produce:
// that chain also schedules the observe poll, and this golden wants the deterministic frame
// right after the transition, not whatever the observe poll's own timing happens to have
// produced by the time a real run gets here.
func TestRestartRollingGolden(t *testing.T) {
	f := &fakeFuncs{plan: onePlan()}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := ready(t, f, false).SetSize(size[0], size[1])
		// enter (non-production) -> m.start(): state becomes stateStarting, the one state
		// scope.Result[startedMsg] is accepted from (its own doc comment). The real DoFunc cmd
		// this also returns is deliberately never invoked here — see the golden's own comment.
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.state != stateStarting {
			t.Fatalf("setup: state = %v, want stateStarting", m.state)
		}
		m, _ = m.Update(scope.Result[startedMsg]{From: m.scope.ID, V: startedMsg{at: time.Now().UTC()}})
		if m.state != stateRolling {
			t.Fatalf("setup: state = %v, want stateRolling", m.state)
		}
		uitest.Golden(t, "restart-rolling", m.View(), size[0], size[1])
	}
}

// TestBusyMarkerShowsOnlyWhileBusy pins busy()/headerSection's own busyMarker gating: reading,
// starting and rolling show it; the states an operator is not waiting on anything in (confirm,
// done, failed) must not — the marker shown there would be advertising work that already
// finished, or never started. Checks for the EXACT "<marker> <state word>" pairing
// headerSection builds, rather than scanning for the marker's own glyph in isolation:
// "app-staging" and "app-staging / web" already contain a bare "-" and "/", which a naive
// per-character scan would misread (found the hard way, back when this checked a spinner glyph
// instead — this test's own prior-revision history). P3 #9 (t2-review.md) replaced the
// never-ticked spinner.Model this used to check with the plain static busyMarker: the spinner
// was permanently frozen on its first frame, which reads as wedged rather than busy.
func TestBusyMarkerShowsOnlyWhileBusy(t *testing.T) {
	m := New("app-staging", "web", []string{"web"}, false, (&fakeFuncs{plan: onePlan()}).funcs(), ui.NewStyles(true)).SetSize(80, 24)
	hasMarker := func(m Model) bool {
		return strings.Contains(ansi.Strip(m.headerSection()), busyMarker+" "+m.stateWord())
	}

	if m.state != stateReading || !hasMarker(m) {
		t.Errorf("stateReading must show the busy marker in the header:\n%s", ansi.Strip(m.headerSection()))
	}

	m.state = stateConfirm
	if hasMarker(m) {
		t.Errorf("stateConfirm must not show the busy marker:\n%s", ansi.Strip(m.headerSection()))
	}

	m.state = stateStarting
	if !hasMarker(m) {
		t.Errorf("stateStarting must show the busy marker:\n%s", ansi.Strip(m.headerSection()))
	}

	m.state = stateRolling
	if !hasMarker(m) {
		t.Errorf("stateRolling must show the busy marker:\n%s", ansi.Strip(m.headerSection()))
	}

	m.state = stateDone
	if hasMarker(m) {
		t.Errorf("stateDone must not show the busy marker:\n%s", ansi.Strip(m.headerSection()))
	}

	m.state = stateFailed
	if hasMarker(m) {
		t.Errorf("stateFailed must not show the busy marker:\n%s", ansi.Strip(m.headerSection()))
	}
}

// TestStartedMsgFromAnotherInstanceIsForeign is the package-local half of app's own
// TestRestartEarlierStartedDropped: scope.Foreign drops a startedMsg stamped by an instance
// other than this one before it can touch this screen's state at all, without needing a whole
// root Model to prove it.
func TestStartedMsgFromAnotherInstanceIsForeign(t *testing.T) {
	f := &fakeFuncs{plan: onePlan()}
	a := ready(t, f, false)
	b := New("app-staging", "web", []string{"web"}, false, f.funcs(), ui.NewStyles(true)).SetSize(120, 30)
	if b.state != stateReading {
		t.Fatalf("setup: state = %v, want stateReading", b.state)
	}
	foreign := scope.Result[startedMsg]{From: a.scope.ID, V: startedMsg{done: []string{"web"}}}
	got, cmd := b.Update(foreign)
	if cmd != nil {
		t.Error("a foreign startedMsg produced a command")
	}
	if got.state != stateReading {
		t.Fatalf("a foreign startedMsg was accepted onto a screen that never asked for it: state=%v", got.state)
	}
}

// TestRestartObserveTimeout proves Observe is actually bounded (AGENTS.md §4.8, "every command
// has a deadline", FB-L3): a rollout-progress read that never returns on its own must still let
// the screen say so, rather than leaving the operator staring at "rolling" forever. ObserveTimeout
// is set to a few milliseconds — Funcs' own doc comment on why it exists — so this proves the
// real deadline path without a multi-second wait.
func TestRestartObserveTimeout(t *testing.T) {
	f := &fakeFuncs{plan: onePlan()}
	funcs := f.funcs()
	funcs.Interval = time.Millisecond
	funcs.ObserveTimeout = 20 * time.Millisecond
	funcs.Observe = func(ctx context.Context, _ string, _ []string, _ time.Time) ([]restart.Progress, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := New("app-staging", "web", []string{"web"}, false, funcs, ui.NewStyles(true)).SetSize(120, 30)
	m = drain(t, m, m.Init())
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if m.state != stateFailed {
		t.Fatalf("state = %v, want failed once the observe call timed out (notice: %q)", m.state, m.notice)
	}
	if !strings.Contains(m.notice, "deadline exceeded") {
		t.Fatalf("notice should say the read did not answer: %q", m.notice)
	}
}

// TestNewRefusesDoWithoutObserve is FB-L3's construction-time half: Do without Observe would
// leave a restart stranded in "rolling" forever (observe's own obs==nil guard silently breaks
// the tick chain), so New refuses outright rather than letting a real restart ever reach that
// state.
func TestNewRefusesDoWithoutObserve(t *testing.T) {
	funcs := Funcs{
		Read: func(context.Context, string, []string) (restart.Plan, error) { return onePlan(), nil },
		Do:   func(context.Context, restart.Plan, time.Time) ([]string, error) { return nil, nil },
	}
	m := New("app-staging", "web", []string{"web"}, false, funcs, ui.NewStyles(true)).SetSize(120, 30)
	if m.state != stateFailed {
		t.Fatalf("state = %v, want failed", m.state)
	}
	if !strings.Contains(m.notice, "Observe") {
		t.Fatalf("notice should name the missing Observe: %q", m.notice)
	}
	if cmd := m.Init(); cmd != nil {
		t.Error("Init on a refused screen must do nothing")
	}
}
