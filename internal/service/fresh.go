package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// freshInputs carries the digests/reasons a promotion plan was actually resolved with — what
// checkFreshBase needs to rebuild the identical plan against origin's own fresh tree for direct
// mode's own occurrence cross-check below (BuildPlanWith must be handed the SAME inputs
// BuildPlan/Plan used, never a second, independent resolution attempt — Plan's own doc comment).
// Set by Service.Plan for every promotion it builds (plan.go); nil for a deploy (checkFreshBase
// branches on Plan.IsDeploy before ever consulting it). A caller with no PlannedChange behind
// its plan at all (the TUI's own ticked-and-filtered confirm) passes nil, and freshDigestsFor
// falls back to recovering digests from the plan's own edits instead — Divergence 8's TUI rule,
// preserved unchanged.
type freshInputs struct {
	digests map[string]image.Ref
	reasons map[string]string
}

// checkCloneCurrentForBase confirms, for every distinct file the plan's edits reference, that
// planning against the clone's (cloneDir's) on-disk content — exactly what gitops.Discover and
// BuildPlan already read, above, to build this plan — still matches what a brand-new
// promotion's worktree will actually be built from. Two independent questions, neither of which
// this package can repair directly (AGENTS.md §4.6 forbids ever bringing cloneDir's own
// checked-out files up to date via `git reset --hard`/`merge --ff-only` — that would touch "the
// user's own checked-out branch, working tree or index" even when that checkout happens to be on
// base itself — so a mismatch is only ever refused, never silently fixed):
//
//  1. Is cloneDir's disk clean relative to what its local base branch has actually committed?
//     (This check's original, round-1 scope: an uncommitted local edit means the plan was built
//     from content nothing in git — local or origin — has ever recorded.)
//  2. Does the local base branch's own committed content agree with origin/<base>'s CURRENT
//     tip — fetched fresh by this function itself, first, via the same git.Git.FetchBranch
//     direct mode's own publish step already calls? A stale remote-tracking ref used to be this
//     function's own gap (round 5, finding 1: nothing ever fetched before this ran, so
//     "as last fetched" was unbounded staleness, not a documented limitation) — closed here by
//     never trusting a cached ref at all.
//
// Question 2 runs unconditionally, never gated by which side is "ahead": pkg/git.Exec.Worktree's
// own resolveBase prefers origin/<base> over the local branch of the same name whenever that ref
// exists, full stop — it does not care whether local is behind, caught up, or itself ahead with
// an unpushed commit. A previous revision of this function trusted local's bytes whenever local
// was ahead of (or equal to) origin, which was exactly backwards (round 5, finding 2): an
// unpushed local commit changing a planned file is just as untrustworthy as origin having moved
// independently, because the worktree this promotion actually commits into is seeded from origin,
// not from that unpushed local content, regardless. A mismatch is refused unless it equals
// exactly what applying this promotion's own edits to the clone's current content would produce
// (the resume-safety carve-out: that shape is this exact promotion's own prior, successful
// direct-mode push, or any other route to the identical end state — not foreign drift — and
// Drive's own re-observation, AGENTS.md §4.1, is what correctly reports it done rather than this
// function refusing a legitimate resume).
//
// Moved unchanged from cmd/hoist/promote.go's checkCloneCurrentForBase.
func checkCloneCurrentForBase(ctx context.Context, g git.Git, cloneDir, base string, edits []gitops.Edit) error {
	_, onOrigin, err := g.FetchBranch(ctx, cloneDir, "origin", base)
	if err != nil {
		return fmt.Errorf("fetching origin/%s to confirm the clone is current: %w", base, err)
	}

	// Fully qualified refs, not the short names: rev-parse takes any revision, so a tag
	// named like the branch would otherwise pass as the local branch, and a tag named
	// origin/<base> would pass as a remote-tracking ref no prune could ever remove (Copilot on
	// PR #99). The tree reads below use the same qualified refs: `git ls-tree` resolves a short
	// name by ref precedence too, under which a tag named like the branch wins, so a divergent
	// tag "main" would otherwise have its tree compared instead of the branch's (issue #100).
	// Only the messages keep the short names.
	localRef := "refs/heads/" + base
	localSHA, localOK, err := g.RevParse(ctx, cloneDir, localRef)
	if err != nil {
		return err
	}
	originRef := "origin/" + base
	originFullRef := "refs/remotes/" + originRef
	originSHA, originOK, err := g.RevParse(ctx, cloneDir, originFullRef)
	if err != nil {
		return err
	}
	// A base that does not resolve at all used to fall through to the per-file check, where
	// every LsTreeBlob against it failed and the whole plan was reported as "uncommitted local
	// changes" — a safe refusal, but one that sent the operator looking for edits that did not
	// exist when the real cause was a typo in --base or a branch never fetched (issue #34).
	// onOrigin is FetchBranch's own answer from the remote, not the remote-tracking ref, which
	// is a cached belief that outlives a branch deleted on origin (Copilot on PR #94).
	if !onOrigin {
		if originOK {
			return fmt.Errorf("origin no longer has a branch %q (a stale %s remains in %s) — check the --base name, or prune with `git fetch --prune origin`, and re-run", base, originRef, cloneDir)
		}
		if !localOK {
			return fmt.Errorf("%q does not resolve in %s and origin has no such branch — check the --base name and re-run", base, cloneDir)
		}
		return fmt.Errorf("origin has no branch %q, only %s does — push it, or check the --base name, and re-run", base, cloneDir)
	}
	if !localOK {
		return fmt.Errorf("%s has no local branch %q, only %s — check out or fetch it locally (`git branch %s %s`) and re-run", cloneDir, base, originRef, base, originRef)
	}
	// targetRef is exactly what pkg/git.Exec.Worktree's own resolveBase would build a brand-new
	// promotion branch from: origin/<base> whenever that ref exists at all — the bare local
	// branch name only when there is none to prefer (no "origin" remote configured, or nothing
	// ever fetched from it — some tests construct exactly that; resolveBase's own fallback).
	// targetFullRef is the same choice fully qualified, for the object reads.
	targetRef, targetFullRef, targetSHA := base, localRef, localSHA
	if originOK {
		targetRef, targetFullRef, targetSHA = originRef, originFullRef, originSHA
	}

	byFile := map[string][]gitops.Edit{}
	var files []string
	for _, e := range edits {
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)

	var dirty, stale []string
	for _, f := range files {
		p, err := gitops.ResolvePath(cloneDir, f)
		if err != nil {
			return err
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading %s from %s: %w", f, cloneDir, err)
		}
		curBlob, err := g.HashObject(ctx, cloneDir, cur)
		if err != nil {
			return err
		}
		localBlob, ok, err := g.LsTreeBlob(ctx, cloneDir, localRef, f)
		if err != nil {
			return err
		}
		if !ok || curBlob != localBlob {
			dirty = append(dirty, f)
			continue // already refusing this file; no need to also check it against the target.
		}
		if targetRef == base {
			continue // no origin/<base> ref exists at all: the local branch IS the target.
		}
		targetBlob, ok, err := g.LsTreeBlob(ctx, cloneDir, targetFullRef, f)
		if err != nil {
			return err
		}
		if ok && targetBlob == curBlob {
			continue // local already matches what the worktree will actually be built from.
		}
		after, err := gitops.ApplyBytes(cur, byFile[f])
		if err != nil {
			return err
		}
		afterBlob, err := g.HashObject(ctx, cloneDir, after)
		if err != nil {
			return err
		}
		if !ok || targetBlob != afterBlob {
			stale = append(stale, f)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%s has uncommitted local changes not yet in %q for: %s — a plan built from that content can't be trusted; commit, stash or discard the local changes and re-run", cloneDir, base, strings.Join(dirty, ", "))
	}
	if len(stale) > 0 {
		// Deliberately one neutral phrasing regardless of which side is "ahead": a directional
		// word ("fallen behind", "has an unpushed commit") would be wrong exactly when local and
		// origin have each moved independently (a real divergence, not a simple lead/lag) — this
		// question no longer cares which direction the mismatch runs, only that it exists, so
		// the message doesn't claim a direction the mechanism itself doesn't distinguish.
		return fmt.Errorf(
			"%s's local %q (%s) disagrees with %s (%s) — which is what a new promotion's worktree is actually built from — for: %s; reconcile (fetch/push as appropriate) and re-run",
			cloneDir, base, shortRev(localSHA), targetRef, shortRev(targetSHA), strings.Join(stale, ", "),
		)
	}
	return nil
}

// discoverAtFreshBase fetches origin/base fresh (never a cached ref) and checks out its exact
// current tip into a throwaway, detached worktree — never cloneDir's own checked-out branch or
// working files (AGENTS.md §4.6) — so a caller can discover and plan against exactly what
// origin/base currently holds, independent of whatever cloneDir's own local disk happens to
// show. Used only by checkFreshBase below, as a cross-check: planning itself still reads from
// cloneDir, exactly as it always has (see checkCloneCurrentForBase's own doc comment for why
// that source, and its own validate-and-refuse dance, is kept — resolving a promotable digest
// from the "manifest" source, in particular, means the source env's own local content is meant
// to be authoritative, unpushed edits included, not silently overridden by whatever origin
// happens to hold).
//
// Returns the snapshot directory and a cleanup func the caller must call once done reading from
// it (nothing here needs to survive past that one comparison; cleanup is idempotent-safe to call
// more than once or after a partial failure). Moved unchanged from cmd/hoist/promote.go.
func discoverAtFreshBase(ctx context.Context, g git.Git, cloneDir, base string) (dir string, cleanup func(), err error) {
	if _, _, err := g.FetchBranch(ctx, cloneDir, "origin", base); err != nil {
		return "", nil, fmt.Errorf("fetching origin/%s: %w", base, err)
	}
	// Mirrors pkg/git.Exec's own resolveBase: prefer the remote-tracking ref whenever it
	// exists, falling back to the bare branch name only for a repo with no "origin" configured
	// at all, or one nothing has ever fetched from — resolveBase's own doc comment explains why
	// (some tests construct exactly that; a real clone always has one after the fetch above).
	// Fully qualified either way, for the same reason as checkCloneCurrentForBase's reads: a tag
	// named "origin/<base>" or "<base>" would otherwise win the short-name lookup and the
	// snapshot would be checked out from the tag (issue #100).
	ref := "refs/heads/" + base
	if _, ok, rerr := g.RevParse(ctx, cloneDir, "refs/remotes/origin/"+base); rerr != nil {
		return "", nil, rerr
	} else if ok {
		ref = "refs/remotes/origin/" + base
	}
	tmp, err := os.MkdirTemp("", "hoist-direct-discover-*")
	if err != nil {
		return "", nil, err
	}
	snap := filepath.Join(tmp, "base")
	cleanup = func() {
		_ = g.RemoveWorktree(context.Background(), cloneDir, snap)
		_ = os.RemoveAll(tmp)
	}
	if err := g.WorktreeAtRef(ctx, cloneDir, snap, ref); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("checking out a throwaway snapshot of %s: %w", ref, err)
	}
	return snap, cleanup, nil
}

// occurrencePositionKey identifies an occurrence by WHERE it is, never by its current value —
// the same physical scalar discovered from two different snapshots (cloneDir's disk, origin's
// fresh tree) shares this key even when the two disagree on content, which is exactly the case
// checkCloneCurrentForBase already handles separately. Moved unchanged from cmd/hoist/promote.go.
func occurrencePositionKey(o gitops.Occurrence) string {
	return fmt.Sprintf("%s#%d:%d", o.File, o.Line, o.Col)
}

// deployRefOf recovers the single image reference a deploy plan writes. Exact, not a guess:
// gitops.BuildDeployPlan sets Edit.New to the one caller-named ref on every edit it produces,
// and refuses to produce a plan with no edits at all — so the first edit's New is that ref
// whenever p.IsDeploy() holds. Moved unchanged from cmd/hoist/wiring.go.
func deployRefOf(p gitops.Plan) image.Ref {
	if len(p.Edits) == 0 {
		return image.Ref{}
	}
	return p.Edits[0].New
}

// freshDigestsFor picks the digests/reasons checkFreshBase rebuilds the fresh plan with: fresh's
// own values when the caller had a resolution behind this plan (the CLI's own already-completed
// resolution, carried on PlannedChange), else recovered from pl's own edits — Divergence 8's TUI
// rule, unchanged: the point of this check is whether origin's tree has an occurrence THIS plan
// cannot see, so the two plans must differ only in the tree they were built from; re-running
// resolution here could also move the refs and turn a resolution change into a phantom missing
// occurrence. Edit.New is the resolved ref for its repo by construction.
func freshDigestsFor(pl gitops.Plan, fresh *freshInputs) (map[string]image.Ref, map[string]string) {
	if fresh != nil {
		return fresh.digests, fresh.reasons
	}
	digests := make(map[string]image.Ref, len(pl.Edits))
	reasons := make(map[string]string, len(pl.Edits))
	for _, e := range pl.Edits {
		digests[e.New.Repo] = e.New
		reasons[e.New.Repo] = "the confirmed plan's own edit"
	}
	return digests, reasons
}

// checkFreshBase is direct mode's own additional gap-closer alongside checkCloneCurrentForBase
// (round-N finding, "base-advanced-with-new-occurrence"): it independently discovers and plans
// from a throwaway, freshly-fetched snapshot of origin/base's current tree, then refuses if that
// discovers any occurrence — identified by file/line/column, never by its current value, since a
// differing value at an ALREADY-known position is exactly what checkCloneCurrentForBase already
// validates — that pl (built from the clone's own disk, same as always) does not already know
// about at all.
//
// Only direct mode needs this: only direct mode's own prior pushes can put origin/base ahead of
// the clone's local disk in a way the clone itself never observes (PushHeadTo never advances the
// clone's own checked-out branch — AGENTS.md §4.6 — and nothing else refreshes it either). The PR
// flow's own worktree is always built directly from the clone's content, whatever it is, and any
// staleness there surfaces as an ordinary merge conflict on GitHub — a softer failure this check
// does not need to guard against.
//
// Moved from cmd/hoist/promote.go's checkNoMissingOccurrenceAtFreshBase and wiring.go's own
// buildFresh closures: this is now the ONE place that rebuilds a plan against origin's fresh
// tree, branching on pl.IsDeploy() itself rather than taking a caller-supplied closure — a second
// copy of that branch (one per caller) is exactly the kind of drift AGENTS.md §8's "layered
// checks" section warns about.
func (s *Service) checkFreshBase(ctx context.Context, g git.Git, pl gitops.Plan, fresh *freshInputs) error {
	snap, cleanup, err := discoverAtFreshBase(ctx, g, s.settings.RepoDir, s.settings.Base)
	if err != nil {
		return err
	}
	defer cleanup()

	freshRepo, err := gitops.Discover(snap, s.settings.AppsRoot)
	if err != nil {
		return fmt.Errorf("discovering origin/%s's own current tree: %w", s.settings.Base, err)
	}

	var freshPlan gitops.Plan
	if pl.IsDeploy() {
		freshPlan, err = gitops.BuildDeployPlan(freshRepo, pl.TargetEnv, deployRefOf(pl), s.settings.Promotable)
	} else {
		digests, reasons := freshDigestsFor(pl, fresh)
		freshPlan, err = gitops.BuildPlanWith(freshRepo, pl.SourceEnv, pl.TargetEnv, s.settings.Promotable, digests, reasons)
	}
	if err != nil {
		return fmt.Errorf("planning against origin/%s's own current tree: %w", s.settings.Base, err)
	}

	known := make(map[string]bool, len(pl.Edits))
	for _, e := range pl.Edits {
		known[occurrencePositionKey(e.Occurrence)] = true
	}
	var missing []string
	for _, e := range freshPlan.Edits {
		if !known[occurrencePositionKey(e.Occurrence)] {
			missing = append(missing, fmt.Sprintf("%s (line %d)", e.File, e.Line))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"origin/%s has occurrence(s) %s's own local checkout doesn't know about at all: %s — fetch/merge to update your clone and re-run; direct mode never writes a file it can't already see locally",
		s.settings.Base, s.settings.RepoDir, strings.Join(missing, ", "),
	)
}
