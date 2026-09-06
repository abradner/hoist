package history

import (
	"fmt"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/migrate"
)

func delta(n int, dir migrate.Direction) migrate.Delta {
	d := migrate.Delta{Direction: dir, Total: n, Prefix: "db/migrate/"}
	for i := 0; i < n; i++ {
		d.Commits = append(d.Commits, migrate.Commit{SHA: fmt.Sprintf("%07d", i), Subject: fmt.Sprintf("c%d", i)})
	}
	return d
}

// A cursor past the first page used to vanish: the pane always rendered commits 0..k while
// enter opened whichever index the cursor had reached. The window keeps the cursor visible.
func TestLinesKeepTheCursorVisible(t *testing.T) {
	st := State{Loaded: true, Delta: delta(14, migrate.DirectionForward)}
	indexes := func(at int) (idx []int, texts []string) {
		for _, l := range Lines(st, "v3", "v1", "prod", "ghcr.io/x/app", true, 6, at) {
			idx = append(idx, l.Index)
			texts = append(texts, l.Text)
		}
		return idx, texts
	}
	// No cursor: the first page and a "more" trailer, five lines under the head.
	if idx, texts := indexes(-1); fmt.Sprint(idx) != "[-1 0 1 2 3 -1]" || !strings.HasSuffix(texts[5], "10 more") {
		t.Fatalf("no cursor: idx=%v texts=%v", idx, texts)
	}
	// Cursor on 6: an "earlier" trailer, a window that holds 6, and a "more" trailer.
	idx, texts := indexes(6)
	if fmt.Sprint(idx) != "[-1 -1 4 5 6 -1]" || !strings.Contains(texts[1], "earlier") || !strings.Contains(texts[5], "more") {
		t.Fatalf("cursor 6: idx=%v texts=%v", idx, texts)
	}
	// Cursor on the last commit: the last page, no "more".
	idx, texts = indexes(13)
	if fmt.Sprint(idx) != "[-1 -1 10 11 12 13]" || !strings.Contains(texts[1], "10 earlier") {
		t.Fatalf("cursor last: idx=%v texts=%v", idx, texts)
	}
	// Everything fits: no trailers at all, whatever the cursor.
	small := State{Loaded: true, Delta: delta(3, migrate.DirectionForward)}
	if got := Lines(small, "v3", "v1", "prod", "r", true, 6, 2); len(got) != 4 || got[3].Index != 2 {
		t.Fatalf("small: %+v", got)
	}
}

func TestSummaryWordsARollbackAndAFloor(t *testing.T) {
	two := delta(3, migrate.DirectionRollback)
	two.Migrations = []string{"db/migrate/a.rb", "db/migrate/b.rb"}
	two.MigrationCommits = 2
	if got := Summary(two, "v1", "v3", "prod"); got != "v1 is 3 commits behind v3 — a rollback · 2 migrations reverted" {
		t.Fatalf("two: %q", got)
	}
	one := delta(1, migrate.DirectionRollback)
	one.Migrations = []string{"db/migrate/a.rb"}
	one.MigrationCommits = 1
	if got := Summary(one, "v1", "v3", "prod"); got != "v1 is 1 commit behind v3 — a rollback · 1 migration reverted" {
		t.Fatalf("one: %q", got)
	}
	untracked := delta(2, migrate.DirectionRollback)
	untracked.Prefix = ""
	if got := Summary(untracked, "v1", "v3", "prod"); !strings.HasSuffix(got, "a rollback · migrations not tracked for this app") {
		t.Fatalf("untracked: %q", got)
	}
	floor := delta(2, migrate.DirectionForward)
	floor.Migrations, floor.MigrationCommits, floor.MigrationsIncomplete = []string{"db/migrate/a.rb"}, 1, true
	if got := Summary(floor, "v3", "v1", "prod"); !strings.HasSuffix(got, "1 migration (at least)") {
		t.Fatalf("floor: %q", got)
	}
}

// Zero migrations found under a capped file list is not "no migrations": the surface says
// unknown rather than nothing (Copilot, #124).
func TestSummaryNamesAnUnknownMigrationCountUnderACap(t *testing.T) {
	d := delta(4, migrate.DirectionForward)
	d.MigrationsIncomplete = true
	if got := Summary(d, "v3", "v1", "prod"); !strings.Contains(got, "migrations unknown") {
		t.Fatalf("capped, none found: %q", got)
	}
	// Positive control: a complete list with none found says nothing about migrations.
	d.MigrationsIncomplete = false
	if got := Summary(d, "v3", "v1", "prod"); strings.Contains(got, "migration") {
		t.Fatalf("complete, none found: %q", got)
	}
}
