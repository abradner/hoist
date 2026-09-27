package tags

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

// mockupNow matches the T3-05/T3-07 design's own "now" convention: a fixed instant the "3 days
// ago"/"4 weeks ago"/"2 months ago" wording in docs/tui/frames/v2-tags-80x24.txt was authored
// against.
var mockupNow = time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)

// mockupDelta reproduces v2·06a's own commit pane data — the same 14-commit, 2-migration delta
// v2-deploy-confirm-80x24.txt also renders (T3-08's own mockup fixture mirrors this), since both
// frames illustrate the same v1→v3 build.
func mockupDelta(_ context.Context, from, to image.Ref) (migrate.Delta, error) {
	commits := []migrate.Commit{
		{SHA: "4a1c2ef0000", Subject: "Add rate limiting to the public API", Date: mockupNow.Add(-2 * 24 * time.Hour), Author: "dev"},
		{SHA: "e9b0d310000", Subject: "Fix N+1 query when resolving digests", Date: mockupNow.Add(-4 * 24 * time.Hour), Author: "dev"},
		{SHA: "77c0ffe0000", Subject: "db: add index on events.created_at", Date: mockupNow.Add(-5 * 24 * time.Hour), Author: "dev", Migrations: []string{"db/migrate/20260225T101500_add_events_created_at_index.rb"}},
		{SHA: "1b2d3e40000", Subject: "Bump temporal SDK to 1.31", Date: mockupNow.Add(-6 * 24 * time.Hour), Author: "dev"},
		{SHA: "6f8a90c0000", Subject: "Drop the legacy /v1/export endpoint", Date: mockupNow.Add(-7 * 24 * time.Hour), Author: "dev"},
		{SHA: "a3e91b20000", Subject: "db: backfill events.tenant_id", Date: mockupNow.Add(-8 * 24 * time.Hour), Author: "dev", Migrations: []string{"db/migrate/20260301T090200_backfill_events_tenant_id.rb"}},
		{SHA: "9d2c4e10000", Subject: "Retry the registry HEAD on 429", Date: mockupNow.Add(-9 * 24 * time.Hour), Author: "dev"},
		{SHA: "c0ffee1a000", Subject: "Log the resolved digest at startup", Date: mockupNow.Add(-10 * 24 * time.Hour), Author: "dev"},
		{SHA: "5e6f7a80000", Subject: "Move health checks to /healthz", Date: mockupNow.Add(-11 * 24 * time.Hour), Author: "dev"},
		{SHA: "d4c3b2a0000", Subject: "Tidy the Dockerfile layers", Date: mockupNow.Add(-12 * 24 * time.Hour), Author: "dev"},
	}
	for i := 0; i < 4; i++ {
		commits = append(commits, migrate.Commit{SHA: strings.Repeat(string(rune('e'+i)), 10), Subject: "older change", Author: "dev", Date: mockupNow.Add(-20 * 24 * time.Hour)})
	}
	return migrate.Delta{
		From: migrate.Revision{Ref: from, SHA: "1111111", Source: migrate.SourceGitTag}, To: migrate.Revision{Ref: to, SHA: "3333333", Source: migrate.SourceLabel},
		Direction: migrate.DirectionForward, Commits: commits, Total: len(commits),
		Migrations:       []string{"db/migrate/20260225T101500_add_events_created_at_index.rb", "db/migrate/20260301T090200_backfill_events_tenant_id.rb"},
		MigrationCommits: 2, Prefix: "db/migrate/", PrefixSource: migrate.PrefixFromDefault,
	}, nil
}

func mockupLiveAge(_ context.Context, _ gitops.Occurrence) (migrate.LineAge, error) {
	return migrate.LineAge{SHA: "abc", Since: mockupNow.Add(-34 * 24 * time.Hour)}, nil
}

// mockupTagsModel reproduces docs/tui/frames/v2-tags-80x24.txt's own data as closely as a real
// tags.Model can: ghcr.io/example/app in app-staging, declaring v1 set 34 days ago, with v3/v2/v1
// releases plus a digest tag and a moving tag all built from the same v3 image (§91's own
// grouping), and the 14-commit/2-migration delta both this frame and v2-deploy-confirm-80x24.txt
// (T3-08) render. Digests here are the real full sha256 value (ShortDigest's own 12-hex-char
// form); the mockup's "3333333333" is its own 10-character illustration shorthand — see the
// mockup-diff report for this and every other documented, non-fixable difference.
func mockupTagsModel(t *testing.T) Model {
	t.Helper()
	regTags := []string{"v3", "v2", "v1", "sha-3333333", "latest"}
	gitTags := []forge.GitTag{
		{Name: "v1", Date: mockupNow.Add(-62 * 24 * time.Hour)},
		{Name: "v2", Date: mockupNow.Add(-28 * 24 * time.Hour)},
		{Name: "v3", Date: mockupNow.Add(-3 * 24 * time.Hour)},
	}
	metas := map[string]registry.ImageMeta{
		"v1":          {Digest: "sha256:" + strings.Repeat("1", 64), Created: mockupNow.Add(-62 * 24 * time.Hour)},
		"v2":          {Digest: "sha256:" + strings.Repeat("2", 64), Created: mockupNow.Add(-28 * 24 * time.Hour)},
		"v3":          {Digest: "sha256:" + strings.Repeat("3", 64), Created: mockupNow.Add(-3 * 24 * time.Hour)},
		"sha-3333333": {Digest: "sha256:" + strings.Repeat("3", 64), Created: mockupNow.Add(-3 * 24 * time.Hour)},
		"latest":      {Digest: "sha256:" + strings.Repeat("3", 64), Created: mockupNow.Add(-3 * 24 * time.Hour)},
	}
	regFn, gitFn := splitListFn(true, func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return regTags, gitTags, true, nil
	})
	d := Declared{
		Ref:        image.Ref{Repo: "ghcr.io/example/app", Tag: "v1", Digest: "sha256:" + strings.Repeat("1", 64)},
		Occurrence: gitops.Occurrence{Line: 21},
	}
	m := New("ghcr.io/example/app", "app-staging", Options{
		Mapped: true, RegTags: regFn, GitTags: gitFn, Meta: fixedMetas(metas),
		History: history.Funcs{
			Mapped:  func(string) bool { return true },
			Delta:   mockupDelta,
			LiveAge: mockupLiveAge,
		},
		Declared: &d,
		Now:      func() time.Time { return mockupNow },
	})
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v (err=%v)", m.state, m.err)
	}
	return m
}

// TestTagsMockupGolden is the T3-07 mockup comparison for v2·06a (80x24): the fixture above,
// cursor on v3 — the row the mockup shows selected (DeriveRows' own git-date order already
// lands the cursor there).
func TestTagsMockupGolden(t *testing.T) {
	m := mockupTagsModel(t)
	if m.selectedTag != "v3" {
		t.Fatalf("fixture precondition: cursor should start on v3, got %q", m.selectedTag)
	}
	uitest.Golden(t, "tags-mockup", m.View(), 80, 24)
}

// TestTagsErrorMockupGolden is the T3-07 mockup comparison for v2·06c (80x10): a registry
// credential-chain failure, wrapped and naming the fix, with the r-retries footer.
func TestTagsErrorMockupGolden(t *testing.T) {
	const msg = "could not list tags: ghcr.io answered 403 (denied) for every credential source tried: env, keychain, cluster; gh's own token cannot read packages. Add a token with read:packages to GHCR_TOKEN, or point registries[].cluster at the pull secret."
	d := Declared{Ref: image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"}}
	m := New("ghcr.io/example/app", "app-staging", Options{
		RegTags:  func(context.Context) ([]string, error) { return nil, errors.New(msg) },
		Meta:     fixedMetas(nil),
		Declared: &d,
	})
	m = m.SetSize(80, 10).SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.err == nil {
		t.Fatal("fixture precondition: the picker should be in its error state")
	}
	uitest.Golden(t, "tags-error-mockup", m.View(), 80, 10)
}
