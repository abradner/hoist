package restart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// Targets reads the family→Deployment mapping out of the repo, because "family" is a concept the
// repo defines and the cluster does not: a namespace holds several families' workloads, so
// listing it would restart far more than was asked for.
func TestTargetsAreScopedToTheNamedFamilies(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	all, err := Targets(r, "app-production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Skipf("fixture has %d Deployments in app-production; need 2+ to prove narrowing", len(all))
	}
	var fam string
	for name := range r.Envs["app-production"].Families {
		fam = name
		break
	}
	one, err := Targets(r, "app-production", []string{fam})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) >= len(all) {
		t.Errorf("naming a family should narrow the set: %d of %d", len(one), len(all))
	}
	if _, err := Targets(r, "app-production", []string{"no-such-family"}); err == nil {
		t.Error("naming a family that does not exist must be refused, not silently ignored")
	}
	if _, err := Targets(r, "no-such-env", nil); err == nil {
		t.Error("an unknown env must be refused")
	}
	if _, err := Targets(nil, "app-production", nil); err == nil {
		t.Error("a nil repo must be refused, not dereferenced")
	}
	// Jobs and CronJobs are not restarted: re-running one is a different operation with
	// different consequences.
	for _, name := range all {
		if strings.Contains(name, "purge") {
			t.Errorf("a CronJob was included as a restart target: %v", all)
		}
	}
	// Sorted, so two runs of the same request restart in the same order.
	for i := 1; i < len(all); i++ {
		if all[i-1] > all[i] {
			t.Errorf("targets are not sorted: %v", all)
		}
	}
}

// A transient read failure leaves that Deployment unfinished rather than failing the whole
// watch. By the time anything calls Observe the pods have already been restarted and there is no
// resume mode to come back through, so one connection reset would otherwise force the operator to
// abandon monitoring or restart everything again just to watch it (Copilot, PR #81).
func TestObserveKeepsWatchingThroughATransientReadError(t *testing.T) {
	f := &rollout.Fake{}
	f.SetDeployment("app-staging", "web", rollout.DeploymentStatus{Namespace: "app-staging", Name: "web"})
	f.DeploymentErr = errors.New("connection reset by peer")

	at := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	got, err := Observe(context.Background(), f, "app-staging", []string{"web"}, at)
	if err != nil {
		t.Fatalf("a transient read must not fail the watch: %v", err)
	}
	if len(got) != 1 || got[0].Done || got[0].Superseded || got[0].Blocked != "" {
		t.Fatalf("the Deployment should simply be unfinished, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "could not read it just now") {
		t.Errorf("the detail should say why it is still pending: %q", got[0].Detail)
	}

	// A Deployment that has gone away is different: it is not coming back mid-rollout, and
	// waiting for it is waiting for nothing.
	f.DeploymentErr = fmt.Errorf("reading Deployment: %w", rollout.ErrNotFound)
	if _, err := Observe(context.Background(), f, "app-staging", []string{"web"}, at); !errors.Is(err, rollout.ErrNotFound) {
		t.Errorf("a missing Deployment must end the watch, got %v", err)
	}
}
