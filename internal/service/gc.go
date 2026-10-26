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
	// OrphanWorktrees and RegistryEntries count the orphaned worktrees and the registry cache
	// entries among Removed — what `hoist promotions` weighs against its hint threshold.
	OrphanWorktrees int
	RegistryEntries int
}

// Collectable is how much a `hoist gc` run would remove of the two kinds that pile up: orphaned
// worktrees and registry cache entries past the prune age.
type Collectable struct {
	Worktrees       int
	RegistryEntries int
}

// Collectable counts what GC would remove, using GC's own dry run so no check is restated: the
// same orphan evidence and the same prune age. It leaves out the landed-promotion cleanup
// (cleanupLanded), the one part of GC that asks origin and the forge, because the listing that
// asks for this has just run it. It removes nothing and talks to no forge or cluster; git is
// asked only locally.
func (s *Service) Collectable(ctx context.Context) (Collectable, error) {
	rep, err := s.gc(ctx, GCOpts{DryRun: true}, true)
	return Collectable{Worktrees: rep.OrphanWorktrees, RegistryEntries: rep.RegistryEntries}, err
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
//  3. repo-view directories (<CacheDir>/repo-view/<hash>) of clones that are no longer
//     configured (#247): see removeStaleRepoViews;
//  4. registry cache entries not used for registryCacheMaxAge.
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
	return s.gc(ctx, o, false)
}

// gc is GC; skipLanded leaves out step 1 (see Collectable).
func (s *Service) gc(ctx context.Context, o GCOpts, skipLanded bool) (GCReport, error) {
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
		if skipLanded {
			continue
		}
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
		counted := false
		for _, line := range lines {
			rep.Removed = append(rep.Removed, line+" (no state file)")
			if !counted && (strings.HasPrefix(line, "would remove worktree ") || strings.HasPrefix(line, "removed worktree ")) {
				rep.OrphanWorktrees++
				counted = true
			}
		}
		if err != nil {
			rep.file(filepath.Join(root, e.Name()), err)
		}
		if cerr := ctx.Err(); cerr != nil {
			return rep, cerr
		}
	}

	s.removeStaleRepoViews(ctx, cache, o.DryRun, &rep)
	if cerr := ctx.Err(); cerr != nil {
		return rep, cerr
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
	rep.RegistryEntries = len(pruned)
	for _, p := range pruned {
		rep.Removed = append(rep.Removed, fmt.Sprintf("%s registry cache entry %s (unused for over %d days)", verb, p, int(registryCacheMaxAge.Hours()/24)))
	}
	if err != nil {
		rep.Failed = append(rep.Failed, "could not prune the registry cache: "+redact.Strings(err.Error()))
	}
	return rep, nil
}

// repoViewNameLen is the length of a repo-view directory name: repoViewDir keys it by the first
// 8 bytes of a SHA-256, hex-encoded.
const repoViewNameLen = 16

// isRepoViewName reports whether name is a directory name repoViewDir could have produced.
func isRepoViewName(name string) bool {
	if len(name) != repoViewNameLen {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// removeStaleRepoViews removes the cached origin views (repoViewDir) that no configured clone
// hashes to. The name is a one-way hash of the clone path, so staleness is decided by hashing
// every clone this run knows about — each spelling, as configured and as resolved, since keeping
// a view that was needed is the safe error — and removing the rest. A view holds only a
// detached checkout of origin's base branch, which refreshRepoView recreates from scratch on
// every refresh, so nothing in it is worth keeping once no clone refers to it. Only a directory
// whose name is a hash hoist would produce, and which is a git worktree (its .git file names
// the clone it is registered with), is removed; anything else is kept and named. The owning
// clone is read from that file: when it still exists the removal goes through it, so its
// .git/worktrees registration goes too; when it is gone, so is the registration, and the
// directory alone is removed.
func (s *Service) removeStaleRepoViews(ctx context.Context, cache string, dryRun bool, rep *GCReport) {
	root := filepath.Join(cache, "repo-view")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			rep.Failed = append(rep.Failed, "could not read the repo view cache: "+redact.Strings(err.Error()))
		}
		return
	}
	wanted := map[string]bool{}
	hold := func(dir string) {
		if dir == "" {
			return
		}
		for _, d := range []string{dir, resolvedPath(dir)} {
			if v, err := repoViewDir(d); err == nil {
				wanted[filepath.Base(v)] = true
			}
		}
	}
	hold(s.settings.RepoDir)
	if cfg := s.settings.Config; cfg != nil {
		for _, r := range cfg.Repos {
			hold(r.Dir)
		}
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		dir := filepath.Join(root, e.Name())
		switch {
		case !isRepoViewName(e.Name()):
			rep.Kept = append(rep.Kept, "kept "+dir+": its name is not a repo view hash")
			continue
		case !e.IsDir():
			rep.Kept = append(rep.Kept, "kept "+dir+": it is not a directory")
			continue
		case wanted[e.Name()]:
			continue
		}
		clone, ok := worktreeOwner(dir)
		if !ok {
			rep.Kept = append(rep.Kept, "kept "+dir+": it is not a git worktree")
			continue
		}
		if !dryRun {
			var err error
			if _, serr := os.Stat(clone); serr == nil {
				err = s.Git().RemoveWorktree(ctx, clone, dir)
			} else {
				err = os.RemoveAll(dir)
			}
			if err != nil {
				rep.Failed = append(rep.Failed, "could not remove repo view "+dir+": "+redact.Strings(err.Error()))
				continue
			}
		}
		rep.Removed = append(rep.Removed, fmt.Sprintf("%s repo view %s (its clone is no longer configured)", verb, dir))
	}
}

// worktreeOwner reads the clone a linked worktree is registered with from its .git file
// ("gitdir: <clone>/.git/worktrees/<name>"). ok is false when dir has no such file.
func worktreeOwner(dir string) (clone string, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", false
	}
	gitdir, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !found {
		return "", false
	}
	gitdir = filepath.Clean(strings.TrimSpace(gitdir))
	marker := string(filepath.Separator) + ".git" + string(filepath.Separator) + "worktrees" + string(filepath.Separator)
	i := strings.LastIndex(gitdir, marker)
	if i <= 0 {
		return "", false
	}
	return gitdir[:i], true
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
