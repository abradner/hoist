package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
)

// registryCacheMaxAge is how long a registry cache entry may go unused before GC removes it.
// Fixed rather than configured: the cache is a few kilobytes per image and an entry removed too
// early costs one registry round trip, so there is nothing here worth a config key.
const registryCacheMaxAge = 90 * 24 * time.Hour

// GCOpts configures one GC call. DryRun makes every check and observation a real run makes and
// reports what it would remove, removing nothing. "Observation" includes what every landing
// check does: a fetch of origin's base branch into the clone's remote-tracking ref. A dry run
// removes nothing; it is not a run that touches nothing.
type GCOpts struct {
	DryRun bool
}

// GCReport is what GC did, as lines for a person: Removed is one line per thing removed (worded
// "would …" under DryRun); Kept is one line per thing GC looked at, could have been expected to
// remove, and deliberately left, with the reason; Failed is one line per thing it could not
// decide about or could not remove — an observation or a removal that errored — which is not a
// decision to keep it and is what makes `hoist gc` exit non-zero.
type GCReport struct {
	Removed []string
	Kept    []string
	Failed  []string
}

// file sorts err for what into Kept (a refusal: GC looked and decided) or Failed (anything else).
func (r *GCReport) file(what string, err error) {
	var refused *engine.CleanupRefusedError
	if errors.As(err, &refused) {
		r.Kept = append(r.Kept, "kept "+what+": "+refused.Reason)
		return
	}
	r.Failed = append(r.Failed, "could not clean up "+what+": "+redact.Strings(err.Error()))
}

// GC removes what finished promotions leave on this machine, and nothing else:
//
//  1. the worktree and local branch of every promotion with a live state file that is observed
//     landed — the same CleanupLanded the listing and the drivers call, so this is where a
//     promotion that landed before that cleanup existed gets tidied;
//  2. orphaned worktrees: a directory under <CacheDir>/worktrees that no live state file names,
//     left by a promotion abandoned or archived before cleanup existed, or whose state file was
//     deleted by hand;
//  3. registry cache entries not used for registryCacheMaxAge.
//
// An orphan has no state file, so nothing can be re-observed about it; what stands in for that
// is evidence it is hoist's own worktree and holds nothing uncommitted: its name is a promotion
// id, it is a real directory (engine.PromotionWorktree), it is registered as a worktree of a
// configured clone on that id's own hoist/<env>/<id> branch, and `git status` shows nothing in
// it. Anything short of that is kept and named in the report. A commit that exists only on an
// orphan's branch is NOT a reason to keep it: that is exactly what an abandoned promotion
// leaves — a one-line-per-occurrence image edit that promoting again makes again, as a new
// commit. This is the one place hoist deletes a commit that may exist nowhere else, which is why
// orphans are removed only here, on an explicit command with a dry run, and never automatically.
// (Automatic cleanup of a landed promotion checks the opposite: service.tipIsOnTheRemote.)
//
// The worktrees directory is read BEFORE the state files are listed: a promotion saves its state
// file before it creates its worktree, so every worktree seen in that first read already has its
// state file by the time the second read happens, and a promotion starting mid-GC can never be
// mistaken for an orphan.
func (s *Service) GC(ctx context.Context, o GCOpts) (GCReport, error) {
	var rep GCReport

	cache, err := engine.CacheDir()
	if err != nil {
		return rep, err
	}
	root := filepath.Join(cache, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return rep, err
	}
	states, err := s.deps.Store.List()
	if err != nil {
		return rep, err
	}

	live := make(map[string]bool, len(states))
	for _, st := range states {
		live[st.ID] = true
		lines, err := s.cleanupLanded(ctx, st, o.DryRun)
		for _, line := range lines {
			rep.Removed = append(rep.Removed, fmt.Sprintf("%s (promotion %s into %s, landed)", line, st.ID, st.TargetEnv))
		}
		if err != nil {
			rep.file(st.ID, err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return rep, cerr
		}
	}

	for _, e := range entries {
		if live[e.Name()] {
			continue
		}
		lines, err := s.removeOrphan(ctx, e.Name(), o.DryRun)
		for _, line := range lines {
			rep.Removed = append(rep.Removed, line+" (no state file)")
		}
		if err != nil {
			rep.file(filepath.Join(root, e.Name()), err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return rep, cerr
		}
	}

	now := time.Now()
	if s.deps.Now != nil {
		now = s.deps.Now()
	}
	pruned, err := registry.PruneCache(registryCacheMaxAge, now, o.DryRun)
	verb := "removed"
	if o.DryRun {
		verb = "would remove"
	}
	for _, p := range pruned {
		rep.Removed = append(rep.Removed, fmt.Sprintf("%s registry cache entry %s (unused for over %d days)", verb, p, int(registryCacheMaxAge.Hours()/24)))
	}
	if err != nil {
		rep.Failed = append(rep.Failed, "could not prune the registry cache: "+err.Error())
	}
	return rep, nil
}

// removeOrphan removes the worktree directory named name under the hoist cache, and its branch,
// when — and only when — it is shown to be a promotion's own leftover worktree with nothing
// uncommitted in it. See GC's doc comment for the rule; every refusal is a
// *engine.CleanupRefusedError naming which part of it failed.
func (s *Service) removeOrphan(ctx context.Context, name string, dryRun bool) ([]string, error) {
	dir, err := engine.PromotionWorktree(name)
	if err != nil {
		return nil, err
	}
	g := s.Git()
	clone, branch, found := "", "", false
	var askErr error
	for _, c := range s.configuredClones() {
		// A configured checkout that cannot be asked (moved, deleted, not a repository) must not
		// stop the others being asked — but it has not been ruled out either.
		on, registered, err := g.WorktreeBranch(ctx, c, dir)
		if err != nil {
			if askErr == nil {
				askErr = fmt.Errorf("could not ask %s whether this is its worktree: %w", c, err)
			}
			continue
		}
		if registered {
			clone, branch, found = c, on, true
			break
		}
	}
	if !found && askErr != nil {
		// No clone that could be asked owns it, and one could not be asked: ownership was not
		// decided, which is a failure to check, not a decision to keep.
		return nil, askErr
	}
	if !found {
		return nil, &engine.CleanupRefusedError{ID: name, Reason: "it is not a registered worktree of any configured clone"}
	}
	if !isPromotionBranch(branch, name) {
		return nil, &engine.CleanupRefusedError{ID: name, Reason: fmt.Sprintf("it is on %q, not this id's own hoist/<env>/%s branch", branch, name)}
	}
	dirty, err := g.WorktreeDirty(ctx, dir)
	if err != nil {
		return nil, err
	}
	if dirty {
		return nil, &engine.CleanupRefusedError{ID: name, Reason: "it has uncommitted changes"}
	}
	tip, _, err := g.RevParse(ctx, clone, "refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	// The state files were listed once, at the start. A promotion re-run under this id since
	// then has saved a state file and is using this worktree: look again, last thing.
	if st, err := s.deps.Store.Load(name); err != nil {
		return nil, err
	} else if st != nil {
		return nil, &engine.CleanupRefusedError{ID: name, Reason: "a promotion with this id has started since the sweep began"}
	}
	return s.removeWorktreeAndBranch(ctx, clone, dir, branch, removal{dryRun: dryRun, tip: tip})
}

// isPromotionBranch reports whether branch is exactly hoist/<env>/<id> for some single-segment
// env — the only branch name hoist ever creates for promotion id (engine.BranchName).
func isPromotionBranch(branch, id string) bool {
	env, ok := strings.CutPrefix(branch, "hoist/")
	if !ok {
		return false
	}
	env, ok = strings.CutSuffix(env, "/"+id)
	return ok && env != "" && !strings.ContainsAny(env, `/\`) && engine.BranchName(env, id) == branch
}

// configuredClones is every checkout this run knows about: the selected repo's, and each
// repos[] entry's, deduplicated and in a stable order.
func (s *Service) configuredClones() []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir == "" {
			return
		}
		if key := resolvedPath(dir); !seen[key] {
			seen[key] = true
			out = append(out, dir)
		}
	}
	add(s.settings.RepoDir)
	if cfg := s.settings.Config; cfg != nil {
		for _, r := range cfg.Repos {
			add(r.Dir)
		}
	}
	sort.Strings(out)
	return out
}
