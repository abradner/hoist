package restart

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// mixedTargets is a family declaring one Deployment, one StatefulSet (OnDelete) and one DaemonSet,
// with a CronJob the restart skips. The control property: every kind once, and exactly one of them
// (the StatefulSet) under OnDelete, so a test can tell "stamped, not rolled" from rolled.
var mixedTargets = []gitops.Workload{
	{Kind: rollout.KindDeployment, Name: "web"},
	{Kind: rollout.KindStatefulSet, Name: "db"},
	{Kind: rollout.KindDaemonSet, Name: "agent"},
}

func mixedPlan() restart.Plan {
	pinned := func(n string) []rollout.ContainerImage {
		return []rollout.ContainerImage{{Name: n, Image: "ghcr.io/example/" + n + ":v1@sha256:" + strings.Repeat("1", 64)}}
	}
	return restart.Plan{
		Env: "app-staging",
		Targets: []rollout.WorkloadStatus{
			{Kind: rollout.KindDeployment, Namespace: "app-staging", Name: "web", Replicas: 2, Strategy: "RollingUpdate", MaxSurge: 1, ReadinessProbes: 1, Images: pinned("web")},
			{Kind: rollout.KindStatefulSet, Namespace: "app-staging", Name: "db", Replicas: 3, Strategy: rollout.StrategyOnDelete, ReadinessProbes: 1, Images: pinned("db")},
			{Kind: rollout.KindDaemonSet, Namespace: "app-staging", Name: "agent", Replicas: 4, Strategy: "RollingUpdate", MaxSurge: 1, ReadinessProbes: 1, Images: pinned("agent")},
		},
	}
}

func readyMixed(t *testing.T, f *fakeFuncs) Model {
	t.Helper()
	m := New("app-staging", "data", mixedTargets, false, f.funcs(), ui.NewStyles(true)).
		WithSkipped([]gitops.Workload{{Kind: "CronJob", Name: "backup"}}).SetSize(120, 40)
	return drain(t, m, m.Init())
}

// #171, the TUI half: a family's StatefulSet and DaemonSet are listed beside its Deployment, each
// by kind, the OnDelete warning is on screen before enter, and the CronJob is named as not
// restarted rather than missing.
func TestScreenListsEveryRestartableKind(t *testing.T) {
	f := &fakeFuncs{plan: mixedPlan()}
	m := readyMixed(t, f)
	v := ansi.Strip(m.View())
	for _, want := range []string{
		"3 workloads in app-staging", "Deployment/web", "StatefulSet/db", "DaemonSet/agent",
		"update strategy is OnDelete", "CronJob/backup", "not restarted: a CronJob has no rollout",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		uitest.Golden(t, "restart-workloads", readyMixed(t, f).SetSize(size[0], size[1]).View(), size[0], size[1])
	}
}

// An OnDelete StatefulSet is stamped and then never rolls on its own. The screen settles once the
// others have rolled, and its state word never says "all rolled": the StatefulSet is marked as
// stamped and not rolled, and the screen stops polling for it.
func TestOnDeleteIsNeverReportedAsRolled(t *testing.T) {
	f := &fakeFuncs{
		plan: mixedPlan(),
		progress: [][]restart.Progress{{
			{Kind: rollout.KindDeployment, Name: "web", Done: true},
			{Kind: rollout.KindStatefulSet, Name: "db", Manual: "update strategy is OnDelete: the pod template is stamped, but its pods restart only when deleted"},
			{Kind: rollout.KindDaemonSet, Name: "agent", Done: true},
		}},
	}
	m := readyMixed(t, f)
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	for i := 0; i < 10 && m.state != stateDone && m.state != stateFailed; i++ {
		m, cmd = m.Update(scope.Result[tickMsg]{From: m.scope.ID, V: tickMsg{}})
		m = drain(t, m, cmd)
	}
	if m.state != stateDone {
		t.Fatalf("state = %v, want done once nothing is left that will roll (notice %q)", m.state, m.notice)
	}
	if f.restarts != 1 {
		t.Errorf("restarts = %d, want 1", f.restarts)
	}
	v := ansi.Strip(m.View())
	if strings.Contains(v, "all rolled") {
		t.Errorf("an OnDelete StatefulSet was reported as rolled:\n%s", v)
	}
	for _, want := range []string{"done · 1 not rolled (OnDelete)", "stamped, not rolled: update strategy is OnDelete"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	if p := m.pending(); len(p) != 0 {
		t.Errorf("still polling for %v after it settled", p)
	}
}
