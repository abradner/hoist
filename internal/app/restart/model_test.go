package restart

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
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

// drain runs a command the way the runtime would, one level deep, feeding its message back.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 20; i++ {
		msg := cmd()
		if msg == nil {
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
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
	for _, want := range []string{"hoist restart", "app-staging", "web", "1 replica", "never restarted this way", "enter restart"} {
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
		m, cmd = m.Update(tickMsg{})
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
		m, cmd = m.Update(tickMsg{})
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
		if _, ok := cmd().(startedMsg); ok {
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
