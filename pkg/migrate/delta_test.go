package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
)

func rev(tag, sha string) Revision {
	return Revision{Ref: image.Ref{Repo: "ghcr.io/example/app", Tag: tag}, SHA: sha, Source: SourceGitTag, Detail: tag}
}

func day(n int) time.Time { return time.Date(2026, 3, n, 0, 0, 0, 0, time.UTC) }

func fc(sha, subject string, d int) forge.Commit {
	return forge.Commit{SHA: sha, Subject: subject, Body: "", Author: "dev", Date: day(d)}
}

func TestDeltaUnresolvedEndIsTyped(t *testing.T) {
	f := &forge.Fake{}
	_, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: Revision{Ref: image.Ref{Repo: "x", Tag: "v3"}, Source: SourceUnknown}, Migrations: "db/migrate/"})
	if !errors.Is(err, ErrUnresolved) || !strings.Contains(err.Error(), "v3") {
		t.Fatalf("err = %v; want ErrUnresolved naming v3", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("no forge call for an unresolvable delta; got %v", f.Calls)
	}
}

func TestDeltaSameRevisionMakesNoCall(t *testing.T) {
	f := &forge.Fake{}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v1", shaA), Migrations: "db/migrate/"})
	if err != nil || d.Direction != DirectionSame || len(f.Calls) != 0 {
		t.Fatalf("d=%+v err=%v calls=%v", d, err, f.Calls)
	}
}

func TestDeltaForwardListsNewestFirstAndShortCircuitsWithoutMigrations(t *testing.T) {
	f := &forge.Fake{Comparisons: map[string]forge.Comparison{
		shaA + "..." + shaB: {Status: "ahead", AheadBy: 2, Total: 2,
			Commits: []forge.Commit{fc("c1", "older", 1), fc("c2", "newer", 2)},
			Files:   []string{"app/a.rb", "app/b.rb"}},
	}}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Direction != DirectionForward || d.Total != 2 || d.Truncated {
		t.Fatalf("d = %+v", d)
	}
	if d.Commits[0].SHA != "c2" || d.Commits[1].SHA != "c1" {
		t.Fatalf("order = %s,%s; want newest first", d.Commits[0].SHA, d.Commits[1].SHA)
	}
	if d.MigrationCommits != 0 || len(d.Migrations) != 0 {
		t.Fatalf("migrations = %v", d.Migrations)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "CommitsTouching") || strings.HasPrefix(c, "CommitFiles") {
			t.Fatalf("complete file list with no migration must not attribute; calls = %v", f.Calls)
		}
	}
}

// The filter is the whole point of the package, so this is the test that must be able to
// fail (AGENTS.md §8): three commits, three conventions, exactly one matches the prefix, and
// the positive control shows a different prefix picks a different commit.
func TestDeltaKeepsOnlyMigrationsPrefix(t *testing.T) {
	f := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			shaA + "..." + shaB: {Status: "ahead", Total: 3,
				Commits: []forge.Commit{fc("c1", "rails migration", 1), fc("c2", "app only", 2), fc("c3", "flyway migration", 3)},
				Files:   []string{"db/migrate/20260301_x.rb", "app/models/x.rb", "app/y.rb", "migrations/V2__y.sql"}},
		},
		Touching: map[string][]string{
			shaB + " db/migrate": {"c1"},
			shaB + " migrations": {"c3"},
		},
		FilesBySHA: map[string][]string{
			"c1": {"db/migrate/20260301_x.rb", "app/models/x.rb"},
			"c2": {"app/y.rb"},
			"c3": {"migrations/V2__y.sql"},
		},
	}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"db/migrate/20260301_x.rb"}, d.Migrations); diff != "" {
		t.Fatalf("Migrations (-want +got):\n%s", diff)
	}
	if d.MigrationCommits != 1 {
		t.Fatalf("MigrationCommits = %d, want 1", d.MigrationCommits)
	}
	// Attribution lands on the commit that added the file, newest-first order preserved.
	if d.Commits[2].SHA != "c1" || len(d.Commits[2].Migrations) != 1 || d.Commits[0].Migrations != nil || d.Commits[1].Migrations != nil {
		t.Fatalf("commits = %+v", d.Commits)
	}

	// Positive control: another convention finds the other commit and nothing else.
	f.Calls = nil
	d, err = Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "migrations/"})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"migrations/V2__y.sql"}, d.Migrations); diff != "" {
		t.Fatalf("Migrations under migrations/ (-want +got):\n%s", diff)
	}
	if d.MigrationCommits != 1 || d.Commits[0].SHA != "c3" || len(d.Commits[0].Migrations) != 1 {
		t.Fatalf("commits = %+v", d.Commits)
	}
}

// Attribution asks the forge only about commits that touched the prefix — never one
// CommitFiles per commit in the range.
func TestDeltaAttributesThroughCommitsTouchingOnly(t *testing.T) {
	f := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			shaA + "..." + shaB: {Status: "ahead", Total: 3,
				Commits: []forge.Commit{fc("c1", "a", 1), fc("c2", "b", 2), fc("c3", "c", 3)},
				Files:   []string{"db/migrate/1.rb", "app/x.rb"}},
		},
		Touching:   map[string][]string{shaB + " db/migrate": {"c2", "zzz-not-in-range"}},
		FilesBySHA: map[string][]string{"c2": {"db/migrate/1.rb"}, "zzz-not-in-range": {"db/migrate/9.rb"}},
	}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil {
		t.Fatal(err)
	}
	var fileCalls []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "CommitFiles") {
			fileCalls = append(fileCalls, c)
		}
	}
	if diff := cmp.Diff([]string{"CommitFiles c2"}, fileCalls); diff != "" {
		t.Fatalf("CommitFiles calls (-want +got):\n%s", diff)
	}
	// One second before the oldest compared commit: GitHub's since is exclusive, and the
	// boundary commit's own migrations must not vanish (Copilot, M10 train).
	if !strings.Contains(strings.Join(f.Calls, "\n"), "CommitsTouching "+shaB+" db/migrate since=2026-02-28T23:59:59") {
		t.Fatalf("CommitsTouching must be bounded one second before the oldest commit's date; calls = %v", f.Calls)
	}
	if d.MigrationCommits != 1 || d.Migrations[0] != "db/migrate/1.rb" {
		t.Fatalf("d = %+v", d)
	}
}

// A truncated file list means "no migration seen" is not "no migration": attribution runs.
func TestDeltaTruncatedFileListStillAttributes(t *testing.T) {
	f := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			shaA + "..." + shaB: {Status: "ahead", Total: 1, Commits: []forge.Commit{fc("c1", "a", 1)}, Files: []string{"app/x.rb"}, FilesTruncated: true},
		},
		Touching:   map[string][]string{shaB + " db/migrate": {"c1"}},
		FilesBySHA: map[string][]string{"c1": {"db/migrate/1.rb"}},
	}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil || d.MigrationCommits != 1 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

// A rollback: To is behind From. A plain forward compare reports zero commits — the most
// dangerous case rendered as the safest — so the delta compares the other way and lists the
// migrations being un-applied.
func TestDeltaRollbackComparesTheOtherWay(t *testing.T) {
	f := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			shaB + "..." + shaA: {Status: "behind", BehindBy: 1, Total: 0},
			shaA + "..." + shaB: {Status: "ahead", AheadBy: 1, Total: 1, Commits: []forge.Commit{fc("c1", "adds index", 1)}, Files: []string{"db/migrate/1.rb"}},
		},
		Touching:   map[string][]string{shaB + " db/migrate": {"c1"}},
		FilesBySHA: map[string][]string{"c1": {"db/migrate/1.rb"}},
	}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v2", shaB), To: rev("v1", shaA), Migrations: "db/migrate/"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Direction != DirectionRollback || len(d.Commits) != 1 || d.MigrationCommits != 1 {
		t.Fatalf("d = %+v", d)
	}
}

func TestDeltaDivergedAndIdentical(t *testing.T) {
	f := &forge.Fake{Comparisons: map[string]forge.Comparison{
		shaA + "..." + shaB: {Status: "diverged", Total: 1, Commits: []forge.Commit{fc("c1", "a", 1)}},
		shaA + "..." + shaC: {Status: "identical"},
	}}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB)})
	if err != nil || d.Direction != DirectionDiverged || len(d.Commits) != 1 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	d, err = Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v1b", shaC)})
	if err != nil || d.Direction != DirectionSame {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

func TestDeltaTruncationIsCarried(t *testing.T) {
	f := &forge.Fake{Comparisons: map[string]forge.Comparison{
		shaA + "..." + shaB: {Status: "ahead", Total: 400, Truncated: true, Commits: []forge.Commit{fc("c1", "a", 1)}},
	}}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB)})
	if err != nil || !d.Truncated || d.Total != 400 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

func TestDeltaUnknownRefPropagates(t *testing.T) {
	f := &forge.Fake{} // nothing seeded: the fake answers ErrUnknownRef
	_, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB)})
	if !errors.Is(err, forge.ErrUnknownRef) {
		t.Fatalf("err = %v", err)
	}
}

// CommitsTouching proved c2 touched the migrations path; if the forge then caps c2's file
// list before the migration appears, "no migrations" would be a false negative. The delta
// says its migration count is a floor instead.
func TestDeltaMarksACappedFileListAsAFloor(t *testing.T) {
	f := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			shaA + "..." + shaB: {Status: "ahead", Total: 2,
				Commits: []forge.Commit{fc("c1", "a", 1), fc("c2", "b", 2)},
				Files:   []string{"app/x.rb"}, FilesTruncated: true},
		},
		Touching:    map[string][]string{shaB + " db/migrate": {"c2"}},
		FilesBySHA:  map[string][]string{"c2": {"app/y.rb"}},
		FilesCapped: map[string]bool{"c2": true},
	}
	d, err := Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.FilesTruncated {
		t.Fatalf("FilesTruncated must be set when a touching commit's file list was capped: %+v", d)
	}
	// Positive control: the same shape with the list complete is not a floor.
	f.FilesCapped = nil
	d, err = Comparer{Forge: f}.Delta(context.Background(), DeltaIn{From: rev("v1", shaA), To: rev("v2", shaB), Migrations: "db/migrate/"})
	if err != nil || d.FilesTruncated {
		t.Fatalf("complete list: FilesTruncated=%v err=%v", d.FilesTruncated, err)
	}
}
