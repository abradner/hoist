package tags

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
)

// The commit pane (M10, #85 screen 02/07/08): what is actually in the build under the cursor
// — how many commits ahead of what the env declares, their subjects, and which of them carry
// a migration. Derived here with no terminal dependency; model.go lays it out.

// Declared is what the target env's manifests declare for this image repo today: the
// reference every row is compared against ("v3 is 14 commits ahead of v1"), and the
// occurrence whose line the live-age blame dates.
type Declared struct {
	Ref        image.Ref
	Occurrence gitops.Occurrence
}

// DeclaredIn finds the target env's declared reference for imageRepo: the first occurrence,
// by family then file then line, so the answer is stable when the env carries several. ok is
// false when the env has no occurrence of the repo at all (a first deploy — there is nothing
// to compare with, and gitops.BuildDeployPlan would refuse the write anyway).
func DeclaredIn(repo *gitops.Repo, imageRepo, target string) (Declared, bool) {
	if repo == nil {
		return Declared{}, false
	}
	env, ok := repo.Envs[target]
	if !ok {
		return Declared{}, false
	}
	fams := make([]string, 0, len(env.Families))
	for name := range env.Families {
		fams = append(fams, name)
	}
	sort.Strings(fams)
	var found []gitops.Occurrence
	for _, name := range fams {
		for _, o := range env.Families[name].Occurrences {
			if o.Ref.Repo == imageRepo {
				found = append(found, o)
			}
		}
	}
	if len(found) == 0 {
		return Declared{}, false
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].File != found[j].File {
			return found[i].File < found[j].File
		}
		return found[i].Line < found[j].Line
	})
	return Declared{Ref: found[0].Ref, Occurrence: found[0]}, true
}

// Delta is one tag's commit history against the declared reference, as loaded: Loaded is
// false while the fetch is in flight, Err carries an ErrUnresolved (a named gap) or a real
// failure, and Delta the answer.
type deltaState struct {
	Loaded bool
	Delta  migrate.Delta
	Err    error
}

// PaneLine is one rendered line of the commit pane, with the role the model colours it by.
type PaneLine struct {
	Text string
	// Role: "head" (the summary line), "commit", "migration" (a commit carrying one), "more",
	// "gap" (a reason no history is shown) or "wait".
	Role string
	// Index is the commit's position in Delta.Commits for commit/migration lines, else -1.
	Index int
}

// Summary words the delta's head line: "v3 is 14 commits ahead of v1 · 2 migrations", the
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
	case d.MigrationCommits > 0 || len(d.Migrations) > 0:
		word := "migrations"
		if len(d.Migrations) == 1 {
			word = "migration"
		}
		migrations = fmt.Sprintf(" · %d %s", len(d.Migrations), word)
		if d.Truncated {
			migrations += " (at least)"
		}
	}
	switch d.Direction {
	case migrate.DirectionSame:
		return fmt.Sprintf("%s is what %s declares", cursor, target)
	case migrate.DirectionRollback:
		return fmt.Sprintf("%s is %s behind %s — a rollback%s", cursor, commits(), declared, strings.Replace(migrations, "migration", "migration reverted", 1))
	case migrate.DirectionDiverged:
		return fmt.Sprintf("%s and %s diverged: %s only in %s%s", cursor, declared, commits(), cursor, migrations)
	default:
		return fmt.Sprintf("%s is %s ahead of %s%s", cursor, commits(), declared, migrations)
	}
}

// PaneLines renders the pane's lines for a tag: the head, then up to room-1 commits (the
// cursor's neighbourhood when the list is longer) and a "…N more" trailer. A gap — no app
// repo, an unresolved revision, a forge error — is one sentence. room is the lines
// available; the head always fits.
func PaneLines(st deltaState, cursor, declared, target, imageRepo string, mapped bool, room int) []PaneLine {
	switch {
	case !mapped:
		return []PaneLine{{Text: fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", imageRepo), Role: "gap", Index: -1}}
	case !st.Loaded:
		return []PaneLine{{Text: fmt.Sprintf("… reading %s's history", cursor), Role: "wait", Index: -1}}
	case st.Err != nil && errors.Is(st.Err, migrate.ErrUnresolved):
		reason := strings.TrimSpace(strings.TrimPrefix(st.Err.Error(), migrate.ErrUnresolved.Error()+":"))
		return []PaneLine{{Text: "no commit history — " + reason, Role: "gap", Index: -1}}
	case st.Err != nil:
		return []PaneLine{{Text: "commit history unavailable — " + st.Err.Error(), Role: "gap", Index: -1}}
	}
	d := st.Delta
	lines := []PaneLine{{Text: Summary(d, cursor, declared, target), Role: "head", Index: -1}}
	if len(d.Commits) == 0 || room <= 1 {
		return lines
	}
	show := room - 1
	more := 0
	if len(d.Commits) > show {
		more = len(d.Commits) - (show - 1)
		show--
	}
	for i := 0; i < show && i < len(d.Commits); i++ {
		c := d.Commits[i]
		role := "commit"
		text := fmt.Sprintf("%s  %s", short(c.SHA), c.Subject)
		if len(c.Migrations) > 0 {
			role = "migration"
		}
		lines = append(lines, PaneLine{Text: text, Role: role, Index: i})
	}
	if more > 0 {
		lines = append(lines, PaneLine{Text: fmt.Sprintf("…%d more", more), Role: "more", Index: -1})
	}
	return lines
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
