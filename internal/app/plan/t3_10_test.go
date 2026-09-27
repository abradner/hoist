package plan

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
)

// TestEmptyStateNamesTheConfig is T3-10's own "empty states for plan" sweep item (UX-M18's own
// convention, matrix's emptyView): a repo with only one discovered env — nothing else to
// promote to or from — must say what to check, not just that nothing was found.
func TestEmptyStateNamesTheConfig(t *testing.T) {
	single := &gitops.Repo{Root: "/repo", Envs: map[string]*gitops.Env{"solo": {Name: "solo"}}}

	toCase := New(single, nil, config.EnvsConfig{}, "solo", "", noneFunc(nil), history.Funcs{})
	if toCase.err == nil {
		t.Fatal("setup: expected an error for a single-env repo")
	}
	for _, want := range []string{"repos[].envs.pairs", "apps_root", "--apps-root"} {
		if !strings.Contains(toCase.err.Error(), want) {
			t.Errorf("buildEnvSelect error = %q, want it to name %q", toCase.err.Error(), want)
		}
	}

	fromCase := New(single, nil, config.EnvsConfig{}, "", "solo", noneFunc(nil), history.Funcs{})
	if fromCase.err == nil {
		t.Fatal("setup: expected an error for a single-env repo")
	}
	for _, want := range []string{"repos[].envs.pairs", "apps_root", "--apps-root"} {
		if !strings.Contains(fromCase.err.Error(), want) {
			t.Errorf("buildSourceSelect error = %q, want it to name %q", fromCase.err.Error(), want)
		}
	}
}
