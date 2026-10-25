package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/rollout"
)

// workloadsFixture is newPromoteFixture with a StatefulSet "db" and a DaemonSet "agent" added to
// app-production's one family, beside its Deployment "app", and the matching live objects on a
// fake cluster. dbStrategy is the StatefulSet's update strategy as the cluster reports it. With
// production set, app-production is listed under envs.production. The control property: the
// Deployment is the kind restart always handled, so it is the positive control for the other two.
func workloadsFixture(t *testing.T, dbStrategy string, production bool) (cfgPath string, f *rollout.Fake) {
	t.Helper()
	cfgPath, clone, _ := newPromoteFixture(t)
	fam := filepath.Join(clone, "cluster", "apps", "app-production", "app")
	for file, body := range map[string]string{
		"statefulset.yaml": "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: db\nspec:\n  template:\n    spec:\n      containers:\n        - name: db\n          image: ghcr.io/example/db:v1\n",
		"daemonset.yaml":   "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: agent\nspec:\n  template:\n    spec:\n      containers:\n        - name: agent\n          image: ghcr.io/example/agent:v1\n",
	} {
		if err := os.WriteFile(filepath.Join(fam, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if production {
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		withProd := strings.Replace(string(data),
			"    promotable: [ghcr.io/example/]\n",
			"    promotable: [ghcr.io/example/]\n    envs:\n      production: [app-production]\n", 1)
		if withProd == string(data) {
			t.Fatal("fixture config shape changed; promotable insertion point not found")
		}
		if err := os.WriteFile(cfgPath, []byte(withProd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f = restartFake(t, rolled("app-production"))
	f.Strict = true
	f.SetWorkload(rollout.WorkloadStatus{
		Kind: rollout.KindStatefulSet, Namespace: "app-production", Name: "db",
		Replicas: 3, Strategy: dbStrategy, MaxUnavailable: 1, ReadinessProbes: 1,
		Complete: dbStrategy != rollout.StrategyOnDelete, Detail: "statefulset rolling update complete",
	})
	f.SetWorkload(rollout.WorkloadStatus{
		Kind: rollout.KindDaemonSet, Namespace: "app-production", Name: "agent",
		Replicas: 4, Strategy: "RollingUpdate", MaxUnavailable: 1, MaxSurge: 1, ReadinessProbes: 1,
		Complete: true, Detail: "daemon set successfully rolled out",
	})
	return cfgPath, f
}

func restartCalls(f *rollout.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			out = append(out, c)
		}
	}
	return out
}

// #171: `hoist restart` stamps a family's StatefulSet and DaemonSet as well as its Deployment, and
// watches all three to the end. Before this, both were silently skipped — the run reported "all 1
// Deployment(s) rolled" for a family of three workloads.
func TestRestartRollsStatefulSetsAndDaemonSets(t *testing.T) {
	cfgPath, f := workloadsFixture(t, "RollingUpdate", false)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	want := []string{"Restart Deployment app-production/app", "Restart StatefulSet app-production/db", "Restart DaemonSet app-production/agent"}
	if got := restartCalls(f); !slices.Equal(got, want) {
		t.Errorf("restart calls = %v, want %v", got, want)
	}
	for _, s := range []string{"3 workload(s)", "StatefulSet/db", "DaemonSet/agent", "4 pod(s)", "all 3 workload(s) rolled"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output missing %q:\n%s", s, out.String())
		}
	}
}

// Production's gate is the same for every kind: no acknowledgement, no write to any of them; the
// exact env repeated, all three roll.
func TestRestartProductionGateCoversEveryKind(t *testing.T) {
	cfgPath, f := workloadsFixture(t, "RollingUpdate", true)
	base := []string{"--config", cfgPath, "restart", "--env", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(base, &out, &errOut); got == 0 {
		t.Fatal("restarting production with no acknowledgement must be refused")
	}
	if got := restartCalls(f); len(got) != 0 {
		t.Fatalf("a refused production restart reached the cluster: %v", got)
	}
	out.Reset()
	errOut.Reset()
	if got := run(append(base, "--confirm-production", "app-production"), &out, &errOut); got != 0 {
		t.Fatalf("an acknowledged production restart should proceed: %s", errOut.String())
	}
	if got := restartCalls(f); len(got) != 3 {
		t.Errorf("acknowledged, every kind should roll: %v", got)
	}
}

// An OnDelete StatefulSet is warned about before anything is written, is stamped like the others,
// and is then reported as stamped and not rolled — never counted in "rolled", and never waited on
// until the deadline. Principle 5 keeps it a warning (exit 0: the restart did what a restart can);
// principle 1 keeps the closing line from claiming a rollout that did not happen.
func TestRestartOnDeleteWarnsAndIsNotReportedAsRolled(t *testing.T) {
	cfgPath, f := workloadsFixture(t, rollout.StrategyOnDelete, false)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production", "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("dry run: exit %d; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "warning: update strategy is OnDelete") {
		t.Errorf("the dry run must warn that an OnDelete StatefulSet will not roll:\n%s", out.String())
	}
	if got := restartCalls(f); len(got) != 0 {
		t.Fatalf("a dry run restarted something: %v", got)
	}

	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	if got := restartCalls(f); !slices.Contains(got, "Restart StatefulSet app-production/db") {
		t.Errorf("the OnDelete StatefulSet should still be stamped: %v", got)
	}
	for _, s := range []string{
		"warning: update strategy is OnDelete",
		"StatefulSet/db: stamped, not rolled",
		"2 of 3 workload(s) rolled; not rolled, waiting for their pods to be deleted: StatefulSet/db",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output missing %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "all 3") {
		t.Errorf("an OnDelete StatefulSet was counted as rolled:\n%s", out.String())
	}
}
