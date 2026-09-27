package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/resolve"
)

// mockupPlanModel reproduces docs/tui/frames/v2-plan-confirm-80x24.txt's own data as closely as
// a real plan.Model can, built directly (like noOpFixture/t3_09_test.go) rather than through a
// real discovery+resolution round trip: three ordinary repos (orders/marketing/web — the fixture
// repo has no "orders" image, so this stands in for it exactly as deploy's own mockup fixture
// stands ghcr.io/example/web in for ghcr.io/example/app) each with a migration-carrying delta
// totalling the mockup's own "41 commits · 3 migrations", plus one already-current row (worker,
// Row.NoOp) for the greyed-row line. Every non-fixable difference from the mockup — the boxed
// ┬/┼ column-header grid, the per-repo "split in staging"/"runs 2 builds" annotation (data this
// screen does not compute; see AGENTS.md T3-05's own detail-pane data gaps) — is recorded in the
// mockup-diff report rather than reproduced here.
func mockupPlanModel(t *testing.T) Model {
	t.Helper()
	r := discoverFixture(t)
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{Production: []string{"app-production"}}, "app-staging", "app-production", noneFunc([]string{"ghcr.io/"}), history.Funcs{})

	orders := ref("ghcr.io/example/orders:v2026011510@sha256:" + strings.Repeat("1", 64))
	ordersNew := ref("ghcr.io/example/orders:v2026022012@sha256:" + strings.Repeat("2", 64))
	marketing := ref("ghcr.io/example/marketing:sha-000011100000@sha256:" + strings.Repeat("3", 64))
	marketingNew := ref("ghcr.io/example/marketing:sha-1a2b3c4d5e6f@sha256:" + strings.Repeat("4", 64))
	web := ref("ghcr.io/example/web:v2026010101@sha256:" + strings.Repeat("5", 64))
	webNew := ref("ghcr.io/example/web:v2026021509@sha256:" + strings.Repeat("6", 64))
	workerRef := ref("ghcr.io/example/worker:v2026022012@sha256:" + strings.Repeat("7", 64))

	pl := gitops.Plan{
		SourceEnv: "app-staging", TargetEnv: "app-production",
		Edits: []gitops.Edit{
			edit("cluster/apps/app-production/orders/app.yaml", orders, ordersNew),
			edit("cluster/apps/app-production/marketing/app.yaml", marketing, marketingNew),
			edit("cluster/apps/app-production/web/app.yaml", web, webNew),
			edit("cluster/apps/app-production/worker/app.yaml", workerRef, workerRef),
		},
	}
	res := map[string]resolve.Resolution{
		"ghcr.io/example/orders":    {Repo: "ghcr.io/example/orders", Ref: ordersNew, Source: resolve.SourcePods, Detail: "resolved"},
		"ghcr.io/example/marketing": {Repo: "ghcr.io/example/marketing", Ref: marketingNew, Source: resolve.SourcePods, Detail: "resolved"},
		"ghcr.io/example/web":       {Repo: "ghcr.io/example/web", Ref: webNew, Source: resolve.SourcePods, Detail: "resolved"},
		"ghcr.io/example/worker":    {Repo: "ghcr.io/example/worker", Ref: workerRef, Source: resolve.SourcePods, Detail: "resolved"},
	}

	m.state = stateReady
	m.plan = pl
	m.rows = DeriveRows(pl, res)
	m.prefix = CommonPrefix(m.rows)
	m.buildMultiSelect()
	// histFn.Delta only needs to be non-nil (totalsSection's own "history not wired" gate) —
	// the deltas below are set directly, never fetched through this.
	m.histFn = history.Funcs{
		Mapped: func(string) bool { return true },
		Delta:  func(context.Context, image.Ref, image.Ref) (migrate.Delta, error) { return migrate.Delta{}, nil },
	}
	m.deltas = map[string]history.State{
		"ghcr.io/example/orders":    {Loaded: true, Delta: migrate.Delta{Direction: migrate.DirectionForward, Commits: make([]migrate.Commit, 18), Total: 18, Migrations: []string{"a", "b"}}},
		"ghcr.io/example/marketing": {Loaded: true, Delta: migrate.Delta{Direction: migrate.DirectionForward, Commits: make([]migrate.Commit, 12), Total: 12, Migrations: []string{"c"}}},
		"ghcr.io/example/web":       {Loaded: true, Delta: migrate.Delta{Direction: migrate.DirectionForward, Commits: make([]migrate.Commit, 11), Total: 11}},
	}
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	// Cursor on "orders" (index 1 among the sorted Selectable rows: marketing, orders, web),
	// matching the mockup's own hovered row.
	m.moveCursorTo(1)
	m = m.refreshRight()
	return m
}

// TestPlanMockupGolden is T3-09's own mockup comparison for v2·05b (80x24).
func TestPlanMockupGolden(t *testing.T) {
	m := mockupPlanModel(t)
	uitest.Golden(t, "plan-mockup", m.View(), 80, 24)
}
