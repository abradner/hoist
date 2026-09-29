package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
)

// mockupNow matches docs/tui/frames/v2-deploy-confirm-80x24.txt's own "declared 34 days" wording.
var mockupNow = time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)

// mockupDelta reproduces v2-deploy-confirm-80x24.txt's own commit-pane data — the same
// 14-commit, 2-migration delta internal/app/tags' own mockup fixture renders (T3-07's tags
// mockup mirrors this), since both frames illustrate the same v1→v3 build.
func mockupDeployDelta() *migrate.Delta {
	commits := []migrate.Commit{
		{SHA: "4a1c2ef0000", Subject: "Add rate limiting to the public API"},
		{SHA: "e9b0d310000", Subject: "Fix N+1 query when resolving digests"},
		{SHA: "77c0ffe0000", Subject: "db: add index on events.created_at", Migrations: []string{"db/migrate/20260225T101500_add_events_created_at_index.rb"}},
		{SHA: "1b2d3e40000", Subject: "Bump temporal SDK to 1.31"},
		{SHA: "6f8a90c0000", Subject: "Drop the legacy /v1/export endpoint"},
		{SHA: "a3e91b20000", Subject: "db: backfill events.tenant_id", Migrations: []string{"db/migrate/20260301T090200_backfill_events_tenant_id.rb"}},
		{SHA: "9d2c4e10000", Subject: "Retry the registry HEAD on 429"},
		{SHA: "c0ffee1a000", Subject: "Log the resolved digest at startup"},
		{SHA: "5e6f7a80000", Subject: "Move health checks to /healthz"},
		{SHA: "d4c3b2a0000", Subject: "Tidy the Dockerfile layers"},
	}
	for i := 0; i < 4; i++ {
		commits = append(commits, migrate.Commit{SHA: strings.Repeat(string(rune('e'+i)), 10), Subject: "older change"})
	}
	return &migrate.Delta{
		Direction: migrate.DirectionForward, Commits: commits, Total: len(commits),
		Migrations:       []string{"db/migrate/20260225T101500_add_events_created_at_index.rb", "db/migrate/20260301T090200_backfill_events_tenant_id.rb"},
		MigrationCommits: 2, Prefix: "db/migrate/", PrefixSource: migrate.PrefixFromDefault,
	}
}

// mockupDeployModel reproduces docs/tui/frames/v2-deploy-confirm-80x24.txt's own data as closely
// as a real deploy.Model can: the fixture repo's own ghcr.io/example/web plan stands in for the
// mockup's ghcr.io/example/app (testdata/repo has no such image; the header's own displayed
// image name is independent of the Plan's real occurrences, exactly as WithHistory's Declared is
// — see the mockup-diff report for this and every other documented, non-fixable difference).
func mockupDeployModel(t *testing.T) Model {
	t.Helper()
	m := unsizedFixture(t, config.EnvsConfig{}).SetSize(80, 24)
	m.image = "ghcr.io/example/app:v3@sha256:" + strings.Repeat("3", 64)
	return m.WithNow(func() time.Time { return mockupNow }).WithHistory(History{
		Delta:    mockupDeployDelta(),
		Declared: image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"},
		Since:    mockupNow.Add(-34 * 24 * time.Hour),
	})
}

// TestDeployMockupGolden is the T3-08 mockup comparison for v2·05a (80x24).
func TestDeployMockupGolden(t *testing.T) {
	m := mockupDeployModel(t)
	uitest.Golden(t, "deploy-mockup", m.View(), 80, 24)
}
