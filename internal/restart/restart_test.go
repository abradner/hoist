package restart

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
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
