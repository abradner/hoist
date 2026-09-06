package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/pkg/forge"
)

// ErrUnresolved is returned (wrapped, with the reason) when a delta cannot be computed
// because one end has no revision. Screens render it as a named gap — "no commit history:
// v3 has no revision label and me/app has no tag named v3" — never as an empty list.
var ErrUnresolved = errors.New("migrate: revision unresolved")

// Direction is the relationship between From and To.
type Direction string

const (
	// DirectionForward means To is ahead of From; Commits are what ships.
	DirectionForward Direction = "forward"
	// DirectionRollback means To is behind From; Commits are what is being UN-applied, and
	// Migrations the migrations being reverted — the more dangerous case, which a plain
	// forward compare would report as zero commits.
	DirectionRollback Direction = "rollback"
	// DirectionDiverged means neither contains the other; Commits are those only in To.
	DirectionDiverged Direction = "diverged"
	// DirectionSame means identical revisions.
	DirectionSame Direction = "same"
)

// Commit is a forge.Commit plus the migration files it added or changed under the delta's
// prefix (nil when none).
type Commit struct {
	SHA, Subject, Body, Author string
	Date                       time.Time
	Migrations                 []string
}

// Delta is what ships between two revisions. Commits are newest first, the order every
// screen lists them. Migrations is the distinct migration paths across the whole range,
// sorted, and MigrationCommits how many commits carry one. Total and Truncated come from the
// forge; when Truncated, Commits is the newest len(Commits) of Total and Migrations was
// attributed through CommitsTouching, which is bounded by the oldest *returned* commit's date
// — so under truncation the migration count is a floor, and the screen says so.
type Delta struct {
	From, To         Revision
	Direction        Direction
	Commits          []Commit
	Total            int
	Truncated        bool
	Migrations       []string
	MigrationCommits int
	// Prefix and PrefixSource record which migrations path applied and where it came from
	// (MigrationsPath), so the screen can say "migrations under db/migrate/ · from
	// me/app's .hoist.yaml". Prefix is "" when migrations are disabled for this app.
	Prefix       string
	PrefixSource string
}

// DeltaIn is Comparer.Delta's input. Migrations is the path prefix that marks a migration
// file ("db/migrate/"); "" disables attribution (MigrationsDisabled). A prefix, not a glob:
// the field is a string so a glob form later is an additive change.
type DeltaIn struct {
	From, To   Revision
	Migrations string
}

// Comparer computes deltas against one app repo's forge.
type Comparer struct {
	Forge forge.Forge
}

// Delta implements the algorithm described on the package: compare, detect a rollback and
// re-compare the other way, then attribute migrations with as few calls as the answer needs
// — none when the forge's own range file list is complete and holds no migration, else one
// CommitsTouching plus one CommitFiles per commit that touched the prefix.
func (c Comparer) Delta(ctx context.Context, in DeltaIn) (Delta, error) {
	out := Delta{From: in.From, To: in.To, Prefix: in.Migrations}
	if !in.From.Resolved() {
		return out, fmt.Errorf("%w: %s has no known revision", ErrUnresolved, describe(in.From))
	}
	if !in.To.Resolved() {
		return out, fmt.Errorf("%w: %s has no known revision", ErrUnresolved, describe(in.To))
	}
	if in.From.SHA == in.To.SHA {
		out.Direction = DirectionSame
		return out, nil
	}
	base, head := in.From.SHA, in.To.SHA
	cmp, err := c.Forge.Compare(ctx, base, head)
	if err != nil {
		return out, fmt.Errorf("migrate: comparing %s...%s: %w", short(base), short(head), err)
	}
	switch cmp.Status {
	case "behind":
		// To is an ancestor of From: a rollback. What matters is what is being un-applied,
		// which is the range the other way round.
		out.Direction = DirectionRollback
		base, head = in.To.SHA, in.From.SHA
		if cmp, err = c.Forge.Compare(ctx, base, head); err != nil {
			return out, fmt.Errorf("migrate: comparing %s...%s: %w", short(base), short(head), err)
		}
	case "diverged":
		out.Direction = DirectionDiverged
	case "identical":
		out.Direction = DirectionSame
		return out, nil
	default:
		out.Direction = DirectionForward
	}
	out.Total, out.Truncated = cmp.Total, cmp.Truncated
	out.Commits = make([]Commit, 0, len(cmp.Commits))
	for i := len(cmp.Commits) - 1; i >= 0; i-- { // newest first
		fc := cmp.Commits[i]
		out.Commits = append(out.Commits, Commit{SHA: fc.SHA, Subject: fc.Subject, Body: fc.Body, Author: fc.Author, Date: fc.Date})
	}
	if in.Migrations == "" || len(cmp.Commits) == 0 {
		return out, nil
	}
	if !cmp.FilesTruncated && !anyUnder(cmp.Files, in.Migrations) {
		// The forge listed every changed file in the range and none is a migration: done,
		// with no further calls.
		return out, nil
	}
	oldest := cmp.Commits[0].Date
	touching, err := c.Forge.CommitsTouching(ctx, head, strings.TrimSuffix(in.Migrations, "/"), oldest)
	if err != nil {
		return out, fmt.Errorf("migrate: finding commits under %s: %w", in.Migrations, err)
	}
	inRange := map[string]int{}
	for i, mc := range out.Commits {
		inRange[mc.SHA] = i
	}
	seen := map[string]bool{}
	for _, sha := range touching {
		i, ok := inRange[sha]
		if !ok {
			continue
		}
		files, _, err := c.Forge.CommitFiles(ctx, sha)
		if err != nil {
			return out, fmt.Errorf("migrate: listing files of %s: %w", short(sha), err)
		}
		for _, f := range files {
			if strings.HasPrefix(f, in.Migrations) {
				out.Commits[i].Migrations = append(out.Commits[i].Migrations, f)
				seen[f] = true
			}
		}
		if len(out.Commits[i].Migrations) > 0 {
			sort.Strings(out.Commits[i].Migrations)
			out.MigrationCommits++
		}
	}
	for f := range seen {
		out.Migrations = append(out.Migrations, f)
	}
	sort.Strings(out.Migrations)
	return out, nil
}

func anyUnder(files []string, prefix string) bool {
	for _, f := range files {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

func describe(r Revision) string {
	if r.Ref.Tag != "" {
		return r.Ref.Tag
	}
	return r.Ref.String()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
