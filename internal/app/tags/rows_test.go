package tags

import (
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/registry"
)

func date(y int, m time.Month) time.Time { return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC) }

// TestDeriveRowsMappedPrefersGitTagDatesOverRegistry is AGENTS.md invariant 3's core case: a
// mapped repo sorts by the app repo's own git tag dates, newest first, and lists everything
// else after — never interleaved by guesswork.
func TestDeriveRowsMappedPrefersGitTagDatesOverRegistry(t *testing.T) {
	regTags := []string{"v1", "v2", "v3", "sha-abc123", "latest"}
	gitTags := []forge.GitTag{
		{Name: "v1", Date: date(2026, 1)},
		{Name: "v2", Date: date(2026, 3)},
		{Name: "v3", Date: date(2026, 2)},
	}
	rows := DeriveRows(regTags, gitTags, true)
	var order []string
	for _, r := range rows {
		order = append(order, r.Tag)
	}
	// v2 (March) > v3 (Feb) > v1 (Jan), then the unmatched two in their incoming order.
	want := []string{"v2", "v3", "v1", "sha-abc123", "latest"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if !rows[0].HasGitDate || !rows[1].HasGitDate || !rows[2].HasGitDate {
		t.Fatalf("first three rows should carry HasGitDate, got %+v", rows[:3])
	}
	if rows[3].HasGitDate || rows[4].HasGitDate {
		t.Fatalf("unmatched rows must not carry HasGitDate, got %+v", rows[3:])
	}
}

// TestDeriveRowsUnmappedIgnoresGitTags: an unmapped repo is unordered by DeriveRows entirely
// (Reorder handles the Created-based fallback once metadata loads) — gitTags is ignored even
// if a caller mistakenly passed some.
func TestDeriveRowsUnmappedIgnoresGitTags(t *testing.T) {
	regTags := []string{"b", "a", "c"}
	gitTags := []forge.GitTag{{Name: "a", Date: date(2026, 1)}}
	rows := DeriveRows(regTags, gitTags, false)
	for _, r := range rows {
		if r.HasGitDate {
			t.Fatalf("unmapped repo must never set HasGitDate, got %+v", r)
		}
	}
	if rows[0].Tag != "b" || rows[1].Tag != "a" || rows[2].Tag != "c" {
		t.Fatalf("unmapped rows should keep incoming order, got %v", rows)
	}
}

func TestReorderUnmappedSortsByLoadedCreatedDescending(t *testing.T) {
	rows := []Row{
		{Tag: "old", MetaLoaded: true, Meta: metaAt(date(2020, 1))},
		{Tag: "notyet"},
		{Tag: "new", MetaLoaded: true, Meta: metaAt(date(2026, 1))},
	}
	got := Reorder(rows, false)
	if got[0].Tag != "new" || got[1].Tag != "old" || got[2].Tag != "notyet" {
		t.Fatalf("Reorder = %v, want new, old, notyet (loaded-by-date, then unloaded)", tagsOf(got))
	}
}

// TestReorderMappedIsANoOp: a mapped repo's order is fixed at DeriveRows and never re-ranked
// by a row's own Created once it loads (AGENTS.md invariant 3: "prefer... over", not "merge
// with").
func TestReorderMappedIsANoOp(t *testing.T) {
	rows := []Row{
		{Tag: "a", HasGitDate: true, GitDate: date(2020, 1)},
		{Tag: "b"},
	}
	rows[1].MetaLoaded = true
	rows[1].Meta = metaAt(date(2030, 1)) // far newer than a's git date
	got := Reorder(rows, true)
	if got[0].Tag != "a" || got[1].Tag != "b" {
		t.Fatalf("Reorder(mapped=true) must be a no-op, got %v", tagsOf(got))
	}
}

func TestFilterCaseInsensitiveSubstring(t *testing.T) {
	rows := []Row{{Tag: "v1.2.3"}, {Tag: "SHA-abc"}, {Tag: "latest"}}
	got := Filter(rows, "SHA")
	if len(got) != 1 || got[0].Tag != "SHA-abc" {
		t.Fatalf("Filter = %v, want just SHA-abc", tagsOf(got))
	}
	if len(Filter(rows, "")) != 3 {
		t.Fatal("empty query should return every row")
	}
}

func TestIndexOf(t *testing.T) {
	rows := []Row{{Tag: "a"}, {Tag: "b"}}
	if IndexOf(rows, "b") != 1 {
		t.Fatal("expected index 1")
	}
	if IndexOf(rows, "nope") != -1 {
		t.Fatal("expected -1 for a tag not present")
	}
}

func TestShortDigest(t *testing.T) {
	got := ShortDigest("sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd")
	if got != "abcdefabcdef" {
		t.Fatalf("ShortDigest = %q, want the first 12 hex chars", got)
	}
	if ShortDigest("sha256:short") != "short" {
		t.Fatal("a digest shorter than 12 chars should be returned unchanged")
	}
}

// repoWithOccurrence builds a one-env, one-family repo naming imageRepo:v1 in "app-staging" —
// every StagingMismatch test below wants exactly that env/tag, so both are fixed here rather
// than threaded through as parameters every call site would pass the same value for.
func repoWithOccurrence(imageRepo string) *gitops.Repo {
	const env, tag = "app-staging", "v1"
	return &gitops.Repo{
		Envs: map[string]*gitops.Env{
			env: {
				Name: env,
				Families: map[string]*gitops.Family{
					"app": {
						Name: "app",
						Occurrences: []gitops.Occurrence{
							{Ref: image.Ref{Repo: imageRepo, Tag: tag}},
						},
					},
				},
			},
		},
	}
}

// TestStagingMismatchReportsDifferingTag is the attacker/adversary shape AGENTS.md's testing
// rule asks for even on a warn-only path: an input (a production target whose paired staging
// env runs a different tag) that must make the assertion observe a real mismatch, not just
// echo the function's own definition back at itself.
func TestStagingMismatchReportsDifferingTag(t *testing.T) {
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	repo := repoWithOccurrence("ghcr.io/example/app")
	stagingEnv, stagingTag, ok := StagingMismatch(repo, "ghcr.io/example/app", "app-production", envs)
	if !ok {
		t.Fatal("expected a staging tag to compare against")
	}
	if stagingEnv != "app-staging" || stagingTag != "v1" {
		t.Fatalf("got env=%q tag=%q, want app-staging/v1", stagingEnv, stagingTag)
	}
}

func TestStagingMismatchFalseWhenNotProduction(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production"}}
	repo := repoWithOccurrence("ghcr.io/example/app")
	if _, _, ok := StagingMismatch(repo, "ghcr.io/example/app", "app-production", envs); ok {
		t.Fatal("a non-production target must never report a staging mismatch (principle 5: informational only where it applies at all)")
	}
}

func TestStagingMismatchFalseWithNoPairedEnv(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"app-production"}}
	repo := repoWithOccurrence("ghcr.io/example/app")
	if _, _, ok := StagingMismatch(repo, "ghcr.io/example/app", "app-production", envs); ok {
		t.Fatal("no envs.pairs entry names app-production as a target; there is no staging env to compare")
	}
}

func TestStagingMismatchFalseWhenStagingHasNoOccurrence(t *testing.T) {
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	repo := repoWithOccurrence("ghcr.io/example/other")
	if _, _, ok := StagingMismatch(repo, "ghcr.io/example/app", "app-production", envs); ok {
		t.Fatal("staging has no occurrence of this image repo; nothing to compare")
	}
}

func metaAt(created time.Time) registry.ImageMeta {
	return registry.ImageMeta{Created: created}
}

func tagsOf(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Tag
	}
	return out
}
