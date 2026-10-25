package restart

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// mixedRepo is a one-env (app-staging) GitOps repo declaring, per family, the "Kind/name"
// workloads given. dataFamily declares one of every kind that matters here: a Deployment, a
// StatefulSet and a DaemonSet (all three restartable) and a CronJob (not). The control property:
// exactly one workload per kind, distinct names, so a test can tell which kind was dropped.
func mixedRepo(t *testing.T, families map[string][]string) *gitops.Repo {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	podTemplate := "  template:\n    spec:\n      containers:\n        - name: NAME\n          image: ghcr.io/example/NAME:v1\n"
	manifest := map[string]string{
		"Deployment":  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: NAME\nspec:\n" + podTemplate,
		"StatefulSet": "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: NAME\nspec:\n" + podTemplate,
		"DaemonSet":   "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: NAME\nspec:\n" + podTemplate,
		"CronJob": "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: NAME\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n" +
			"          containers:\n            - name: NAME\n              image: ghcr.io/example/NAME:v1\n",
	}
	for fam, decls := range families {
		write("cluster/apps/app-staging-"+fam+".yaml",
			"apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: "+fam+"-app-staging\n  namespace: argocd\n"+
				"spec:\n  source:\n    path: cluster/apps/app-staging/"+fam+"\n  destination:\n    namespace: app-staging\n")
		for _, d := range decls {
			kind, name, _ := strings.Cut(d, "/")
			body := strings.ReplaceAll(manifest[kind], "NAME", name)
			write("cluster/apps/app-staging/"+fam+"/"+strings.ToLower(kind)+"-"+name+".yaml", body)
		}
	}
	r, err := gitops.Discover(root, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var dataFamily = map[string][]string{"data": {"Deployment/web", "StatefulSet/db", "DaemonSet/agent", "CronJob/backup"}}

// #171: a family's StatefulSet and DaemonSet are restart targets, beside its Deployment, and its
// CronJob is named as skipped rather than vanishing. Before this, Targets kept Deployments only:
// the StatefulSet and the DaemonSet were dropped without a word.
func TestTargetsIncludeStatefulSetsAndDaemonSets(t *testing.T) {
	r := mixedRepo(t, dataFamily)
	targets, skipped, err := Targets(r, "app-staging", []string{"data"})
	if err != nil {
		t.Fatal(err)
	}
	want := []gitops.Workload{{Kind: "Deployment", Name: "web"}, {Kind: "StatefulSet", Name: "db"}, {Kind: "DaemonSet", Name: "agent"}}
	if !slices.Equal(targets, want) {
		t.Errorf("targets = %v, want %v (kind order, then name)", targets, want)
	}
	if !slices.Equal(skipped, []gitops.Workload{{Kind: "CronJob", Name: "backup"}}) {
		t.Errorf("skipped = %v, want the CronJob named", skipped)
	}
}

// A family with nothing restartable is refused, and the refusal names what it did find — not a
// bare "no Deployment", which read as if the family were empty.
func TestTargetsNamesWhatItFoundWhenNothingIsRestartable(t *testing.T) {
	r := mixedRepo(t, map[string][]string{"jobs": {"CronJob/backup"}})
	_, _, err := Targets(r, "app-staging", []string{"jobs"})
	if err == nil {
		t.Fatal("a family with only a CronJob must be refused")
	}
	for _, want := range []string{"Deployment, StatefulSet or DaemonSet", "CronJob/backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

// The whole path for the new kinds, through the same Read → Do → Observe the CLI and the TUI both
// call: every target is read by its own kind, patched by its own kind, and watched by its own
// kind. The positive control is the Deployment, which this path always restarted.
func TestRestartStampsEveryKindInTheFamily(t *testing.T) {
	r := mixedRepo(t, dataFamily)
	targets, _, err := Targets(r, "app-staging", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &rollout.Fake{Strict: true}
	f.SetDeployment("app-staging", "web", rollout.DeploymentStatus{Namespace: "app-staging", Name: "web", Replicas: 2, Strategy: "RollingUpdate", Complete: true})
	f.SetWorkload(rollout.WorkloadStatus{Kind: rollout.KindStatefulSet, Namespace: "app-staging", Name: "db", Replicas: 3, Strategy: "RollingUpdate", Complete: true})
	f.SetWorkload(rollout.WorkloadStatus{Kind: rollout.KindDaemonSet, Namespace: "app-staging", Name: "agent", Replicas: 4, Strategy: "RollingUpdate", Complete: true})

	ctx := context.Background()
	pl, err := Read(ctx, f, "app-staging", targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Targets) != 3 || len(pl.Absent) != 0 {
		t.Fatalf("plan = %+v, want all three targets present", pl)
	}
	if pl.Noun() != "workload" || pl.Label(gitops.Workload{Kind: "StatefulSet", Name: "db"}) != "StatefulSet/db" {
		t.Errorf("a mixed plan must name each target's kind: noun %q", pl.Noun())
	}
	at := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)
	done, err := Do(ctx, f, pl, at)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(done, targets) {
		t.Errorf("done = %v, want %v", done, targets)
	}
	for _, want := range []string{"Restart Deployment app-staging/web", "Restart StatefulSet app-staging/db", "Restart DaemonSet app-staging/agent"} {
		if !slices.Contains(f.Calls, want) {
			t.Errorf("no %q call: %v", want, f.Calls)
		}
	}
	progress, err := Observe(ctx, f, "app-staging", targets, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range progress {
		if !pr.Done || pr.Superseded || pr.Manual != "" {
			t.Errorf("%s/%s = %+v, want done with this restart's own stamp", pr.Kind, pr.Name, pr)
		}
	}
}

// An OnDelete StatefulSet is stamped — the restart is asked for and the write is made, as kubectl
// makes it — but its controller will not roll it, so Observe reports it Manual, final and never
// Done. Reporting it Done would be a false "rolled"; leaving it pending would wait forever for a
// rollout that will not come.
func TestObserveReportsOnDeleteAsStampedNotRolled(t *testing.T) {
	db := gitops.Workload{Kind: rollout.KindStatefulSet, Name: "db"}
	f := &rollout.Fake{Strict: true}
	f.SetWorkload(rollout.WorkloadStatus{Kind: db.Kind, Namespace: "app-staging", Name: db.Name, Replicas: 3, Strategy: rollout.StrategyOnDelete})

	ctx := context.Background()
	pl, err := Read(ctx, f, "app-staging", []gitops.Workload{db})
	if err != nil {
		t.Fatal(err)
	}
	if c := pl.Targets[0].GracefulRestartConcerns(); len(c) == 0 || !strings.Contains(c[0], "OnDelete") {
		t.Errorf("an OnDelete StatefulSet must be warned about before it is restarted, got %v", c)
	}
	at := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)
	if _, err := Do(ctx, f, pl, at); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Calls, "Restart StatefulSet app-staging/db") {
		t.Fatalf("the OnDelete StatefulSet was not stamped: %v", f.Calls)
	}
	progress, err := Observe(ctx, f, "app-staging", []gitops.Workload{db}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 {
		t.Fatalf("progress = %+v", progress)
	}
	pr := progress[0]
	if pr.Done || pr.Superseded || pr.Blocked != "" || !strings.Contains(pr.Manual, "OnDelete") {
		t.Errorf("OnDelete progress = %+v, want Manual naming OnDelete and never Done", pr)
	}
}
