package matrix

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
)

// mockupRepo reproduces the v2·01a/v2·01b mockup frames' own data as closely as a real
// gitops.Repo can (docs/tui/frames/v2-matrix-80x24.txt, v2-matrix-120x40.txt): five families
// (orders, marketing, temporal, web, worker) across app-staging/app-production, with the same
// shape of pinned/drifted/external/split/unpinned cells the mockup shows. Exact digest strings
// differ from the mockup's own shorthand ("sha-1a2b3c4") — hoist's real cell text is always the
// full `sha256:` digest or a tag, which the mockup's illustration abbreviates; see the T3-05
// mockup-diff report for that and every other documented, non-fixable difference.
func mockupRepo() *gitops.Repo {
	r := &gitops.Repo{Root: "my-gitops", Envs: map[string]*gitops.Env{}}
	add := func(env, fam string, occs ...gitops.Occurrence) {
		e := r.Envs[env]
		if e == nil {
			e = &gitops.Env{Name: env, Families: map[string]*gitops.Family{}}
			r.Envs[env] = e
		}
		e.Families[fam] = &gitops.Family{Name: fam, Occurrences: occs}
	}
	o := occ
	add("app-staging", "orders", o("ghcr.io/example/orders", "v202602201200", digestA))
	add("app-production", "orders", o("ghcr.io/example/orders", "v202601151010", digestB))
	add("app-staging", "marketing", o("ghcr.io/example/marketing", "", digestA))
	add("app-production", "marketing", o("ghcr.io/example/marketing", "", digestA))
	add("app-staging", "temporal", o("docker.io/temporalio/auto-setup", "1.22", ""), o("docker.io/temporalio/admin-tools", "1.22", ""))
	add("app-production", "temporal", o("docker.io/temporalio/auto-setup", "1.22", ""), o("docker.io/temporalio/admin-tools", "1.22", ""))
	add("app-staging", "web", o("ghcr.io/example/web", "v1", digestA), o("ghcr.io/example/web", "v2", digestB))
	add("app-production", "web", o("ghcr.io/example/web", "v202601010101", digestC))
	add("app-staging", "worker", o("ghcr.io/example/worker", "v3", ""))
	add("app-production", "worker", o("ghcr.io/example/worker", "v2", ""))
	return r
}

func mockupInFlight() []flight.Summary {
	st := engine.PromotionState{
		ID: "5pr6sd333t", SourceEnv: "app-staging", TargetEnv: "app-production",
		PR:      &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"},
		History: []engine.HistoryEntry{{Step: engine.StepBranched, At: time.Date(2026, 3, 5, 11, 48, 0, 0, time.UTC)}},
	}
	return []flight.Summary{flight.Summarize(st, false, []engine.StepStatus{
		sat(engine.StepBranched), sat(engine.StepCommitted), sat(engine.StepPushed), sat(engine.StepPROpened), sat(engine.StepCIGreen),
		{Step: engine.StepApproved, Observation: engine.Observation{Waiting: true, Detail: "no approval comment yet"}},
	}, nil)}
}

// TestMatrixMockupGoldens is the T3-05 mockup comparison for v2·01a (80x24) and v2·01b
// (120x40): the fixture above, an in-flight promotion, and the cursor on app-staging/orders —
// the same cell the mockups show selected.
func TestMatrixMockupGoldens(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production"}, Production: []string{"app-production"}}
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	build := func(w, h int) Model {
		m := New(mockupRepo(), []string{"ghcr.io/"}, envs, nil).
			SetStyles(ui.NewStyles(true)).
			WithNow(func() time.Time { return now }).
			SetInFlight(mockupInFlight(), nil).
			SetSize(w, h)
		// Families sort alphabetically (Compute, unchanged by T3-04/05): marketing, orders,
		// temporal, web, worker — the mockup's own row order (orders first) is its own
		// illustration and not reproduced; "down" once lands the cursor on orders, the same
		// cell the mockup shows selected.
		return uitest.Keys(m, update, "down")
	}
	uitest.Golden(t, "matrix-mockup", ansi.Strip(build(80, 24).View()), 80, 24)
	uitest.Golden(t, "matrix-mockup", ansi.Strip(build(120, 40).View()), 120, 40)
}
