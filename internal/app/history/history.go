// Package history holds the function types through which screens ask about commit history
// and migration deltas — types only, no Bubble Tea, no adaptor construction. Three screens
// share them (the tag picker, the deploy confirm, the plan confirm), which is why they live
// here rather than being declared three times; cmd/hoist builds the values
// (buildHistoryFuncs), exactly as it does for plan.ResolveFunc and tags.BuildFunc
// (AGENTS.md §4.8: screens take function values, never adaptors).
package history

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
)

// RevisionFunc resolves one image reference to a git revision in its app repo.
type RevisionFunc func(ctx context.Context, ref image.Ref) (migrate.Revision, error)

// DeltaFunc is what a screen calls, once: everything between resolving both ends, fetching
// their labels, choosing the migrations prefix and caching happens inside it. An
// unresolvable end is a migrate.ErrUnresolved the screen renders as a named gap; an
// unmapped image repo is the same error with a reason naming repos[].apps.
type DeltaFunc func(ctx context.Context, from, to image.Ref) (migrate.Delta, error)

// LiveAgeFunc dates one manifest occurrence in the gitops repo: when the line last changed.
type LiveAgeFunc func(ctx context.Context, occ gitops.Occurrence) (migrate.LineAge, error)

// Funcs is the bundle a screen is handed. Mapped answers, without a network call, whether
// an image repo has an app repo at all — so a screen can say "no app repo in repos[].apps"
// before spending a spinner on it. A zero Funcs (every field nil) is "no history available";
// screens check for nil and degrade.
type Funcs struct {
	Mapped   func(imageRepo string) bool
	Revision RevisionFunc
	Delta    DeltaFunc
	LiveAge  LiveAgeFunc
}

// State is one delta as a screen holds it: Loaded is false while the fetch is in flight,
// Err carries a migrate.ErrUnresolved (a named gap) or a real failure, Delta the answer.
type State struct {
	Loaded bool
	Delta  migrate.Delta
	Err    error
}

// Line is one rendered line of a commit pane, with the role a screen colours it by.
type Line struct {
	Text string
	// Role: "head" (the summary line), "commit", "migration" (a commit carrying one), "more",
	// "gap" (a reason no history is shown) or "wait".
	Role string
	// Index is the commit's position in Delta.Commits for commit/migration lines, else -1.
	Index int
}

// Summary words a delta's head line: "v3 is 14 commits ahead of v1 · 2 migrations", the
// rollback and diverged forms, "v3 is what app-production declares" for the same revision.
func Summary(d migrate.Delta, cursor, declared, target string) string {
	n := len(d.Commits)
	commits := func() string {
		s := fmt.Sprintf("%d commits", n)
		if n == 1 {
			s = "1 commit"
		}
		if d.Truncated {
			s = fmt.Sprintf("%d of %d commits", n, d.Total)
		}
		return s
	}
	migrations := ""
	switch {
	case d.Prefix == "":
		migrations = " · migrations not tracked for this app"
	case len(d.Migrations) == 0 && d.MigrationsIncomplete:
		// A commit touched the migrations path and the forge capped its file list before any
		// migration showed: zero is not the answer, and saying nothing would read as zero.
		migrations = " · migrations unknown (a commit's file list was capped)"
	case d.MigrationCommits > 0 || len(d.Migrations) > 0:
		word := "migrations"
		if len(d.Migrations) == 1 {
			word = "migration"
		}
		migrations = fmt.Sprintf(" · %d %s", len(d.Migrations), word)
		if d.MigrationsIncomplete {
			migrations += " (at least)"
		}
	}
	switch d.Direction {
	case migrate.DirectionSame:
		return fmt.Sprintf("%s is what %s declares", cursor, target)
	case migrate.DirectionRollback:
		// "2 migrations reverted", "1 migration reverted"; the untracked form is unchanged.
		reverted := migrations
		switch {
		case len(d.Migrations) == 0:
		case strings.Contains(migrations, " migrations"):
			reverted = strings.Replace(migrations, " migrations", " migrations reverted", 1)
		default:
			reverted = strings.Replace(migrations, " migration", " migration reverted", 1)
		}
		return fmt.Sprintf("%s is %s behind %s — a rollback%s", cursor, commits(), declared, reverted)
	case migrate.DirectionDiverged:
		return fmt.Sprintf("%s and %s diverged: %s only in %s%s", cursor, declared, commits(), cursor, migrations)
	default:
		return fmt.Sprintf("%s is %s ahead of %s%s", cursor, commits(), declared, migrations)
	}
}

// Lines renders a pane's lines for one delta: the head, then up to room-1 commits windowed
// so that the commit at index at is among them, with "…N earlier" and "…N more" trailers for
// what the window leaves out. A gap — no app repo, an unresolved revision, a forge error — is
// one sentence. room is the lines available; the head always fits. at is -1 for no cursor.
func Lines(st State, cursor, declared, target, imageRepo string, mapped bool, room, at int) []Line {
	switch {
	case !mapped:
		return []Line{{Text: fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", imageRepo), Role: "gap", Index: -1}}
	case !st.Loaded:
		return []Line{{Text: fmt.Sprintf("… reading %s's history", cursor), Role: "wait", Index: -1}}
	case st.Err != nil && errors.Is(st.Err, migrate.ErrUnresolved):
		reason := strings.TrimSpace(strings.TrimPrefix(st.Err.Error(), migrate.ErrUnresolved.Error()+":"))
		return []Line{{Text: "no commit history — " + reason, Role: "gap", Index: -1}}
	case st.Err != nil:
		return []Line{{Text: "commit history unavailable — " + st.Err.Error(), Role: "gap", Index: -1}}
	}
	d := st.Delta
	lines := []Line{{Text: Summary(d, cursor, declared, target), Role: "head", Index: -1}}
	n := len(d.Commits)
	if n == 0 || room <= 1 {
		return lines
	}
	start, end := window(n, room-1, at)
	if start > 0 {
		lines = append(lines, Line{Text: fmt.Sprintf("…%d earlier", start), Role: "more", Index: -1})
	}
	for i := start; i < end; i++ {
		c := d.Commits[i]
		role := "commit"
		if len(c.Migrations) > 0 {
			role = "migration"
		}
		lines = append(lines, Line{Text: fmt.Sprintf("%s  %s", ShortSHA(c.SHA), c.Subject), Role: role, Index: i})
	}
	if end < n {
		lines = append(lines, Line{Text: fmt.Sprintf("…%d more", n-end), Role: "more", Index: -1})
	}
	return lines
}

// window picks [start, end) of n commits for room lines, keeping index at visible: the
// trailers ("…N earlier", "…N more") each take a line when present. A cursor past what the
// first page shows used to vanish, while enter opened a commit the operator never saw.
func window(n, room, at int) (start, end int) {
	if room <= 0 {
		return 0, 0
	}
	if n <= room {
		return 0, n
	}
	if at < 0 {
		at = 0
	}
	if at >= n {
		at = n - 1
	}
	// A first page has one trailer ("more"); a later page has "earlier" and maybe "more".
	fit := room - 1
	if at < fit {
		return 0, fit
	}
	fit = room - 2 // both trailers
	if fit < 1 {
		fit = 1
	}
	start = at - fit + 1
	end = start + fit
	if end >= n { // last page: no "more" trailer, so one more row fits
		end = n
		start = max(0, n-(room-1))
	}
	return start, end
}

// ShortSHA is the first seven characters of a sha.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
