package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// repoViewDir is where the TUI's own read of origin/<base> lives — under engine.CacheDir,
// the same $XDG_CACHE_HOME/hoist root every other hoist cache uses (never ~/Library),
// alongside promotion worktrees (engine.WorktreeDir) but keyed by the SELECTED REPO's own
// clone path rather than a promotion id: this cache is reused across TUI boots and F5
// refreshes for as long as the operator points hoist at the same clone, not created fresh per
// promotion. Two different --repo values (or two repos[] entries) never collide, since each
// hashes to its own subdirectory.
func repoViewDir(cloneDir string) (string, error) {
	dir, err := engine.CacheDir()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(filepath.Clean(cloneDir)))
	return filepath.Join(dir, "repo-view", hex.EncodeToString(h[:8])), nil
}

// refreshRepoView fetches origin/<base> into cloneDir's own .git and points a cached worktree
// at it — the TUI's own answer to "what does origin actually look like right now", read
// without ever touching the operator's own checkout or working tree (AGENTS.md §4.6: the
// user's checkout, branch and index are never touched). Reused across TUI boots and F5
// refreshes rather than a throwaway per call the way discoverAtFreshBase's own direct-mode
// check works (promote.go): the cache directory persists under repoViewDir, removed and
// recreated on every call rather than updated in place. That is cheap, not a second clone —
// WorktreeAtRef shares object data with cloneDir's own .git via git's own worktree mechanism,
// so this is a file-tree sync against already-local objects; only FetchBranch talks to the
// network, and only for what changed since the last refresh.
//
// This is what makes the matrix's initial read and every later F5 describe origin, not
// whatever the operator's own working tree happens to hold — hoist's own writes (a direct
// push in particular) never touch that working tree at all (§4.6), so a repo read from disk
// the ordinary way goes stale the moment hoist itself writes anything, and the very next
// thing the operator tries to plan used to be refused (checkCloneCurrentForBase, comparing
// against the operator's own local branch) for describing a promotion built from content that
// had already fallen behind origin — not because the operator did anything wrong, but because
// hoist's own last write moved origin without ever moving what hoist itself had just read
// from. Since buildStartPromotion's own plan is now built from this same cached view
// (wiring.go), it is, by construction, never stale against the ref it was built from —
// checkRepoViewCurrent (below) is the narrower check that's left: has origin/<base> moved
// again since this view was last refreshed.
func refreshRepoView(ctx context.Context, g git.Git, cloneDir, base string) (viewDir string, err error) {
	if _, _, err := g.FetchBranch(ctx, cloneDir, "origin", base); err != nil {
		return "", fmt.Errorf("fetching origin/%s: %w", base, err)
	}
	viewDir, err = repoViewDir(cloneDir)
	if err != nil {
		return "", err
	}
	// Mirrors pkg/git.Exec's own resolveBase and discoverAtFreshBase's identical reasoning
	// (promote.go): prefer the remote-tracking ref whenever it exists, fully qualified either
	// way so a tag named like the branch or like "origin/<base>" can never win the short-name
	// lookup (issue #100).
	ref := "refs/heads/" + base
	if _, ok, rerr := g.RevParse(ctx, cloneDir, "refs/remotes/origin/"+base); rerr != nil {
		return "", rerr
	} else if ok {
		ref = "refs/remotes/origin/" + base
	}
	if err := g.RemoveWorktree(ctx, cloneDir, viewDir); err != nil {
		return "", fmt.Errorf("clearing the previous cached repo view: %w", err)
	}
	if err := g.WorktreeAtRef(ctx, cloneDir, viewDir, ref); err != nil {
		return "", fmt.Errorf("checking out a cached view of %s: %w", ref, err)
	}
	return viewDir, nil
}

// checkRepoViewCurrent refuses a confirm whose plan (built from viewDir, at whatever moment
// refreshRepoView last ran — TUI boot or a since F5) no longer matches origin/<base>'s
// present tip. Deliberately NOT checkCloneCurrentForBase's per-file blob comparison: that
// function's whole shape assumes cloneDir's working tree tracks refs/heads/<base>, the local
// branch — true for an ordinary checkout, false by construction for viewDir, which is
// deliberately checked out from origin, never the local branch (refreshRepoView's own doc
// comment). Reusing it unchanged here would flag every promotion whose local branch is behind
// origin as "uncommitted local changes", the exact false refusal this whole mechanism exists
// to stop. What's actually left to verify is simpler: re-fetch origin/<base> and compare its
// SHA to what viewDir was checked out at — if they agree, nothing has changed since the
// operator was looking at the matrix; if they don't, origin moved in the meantime and the
// plan may already be describing something that no longer matches, so this refuses and names
// the fix (re-open the matrix, or press F5, and re-plan) rather than silently promoting
// against a target the operator never actually saw.
//
// viewDir empty (refreshRepoView itself failed at boot, runTUI's own fallback) skips this
// check entirely — there is nothing cached to compare against, and the plan came from
// cloneDir's own working tree exactly as it did before #PR7, which checkNoMissingOccurrenceAt-
// FreshBase (direct mode) and the ordinary PR-review-on-GitHub path already cover.
func checkRepoViewCurrent(ctx context.Context, g git.Git, cloneDir, base, viewDir string) error {
	if viewDir == "" || viewDir == cloneDir {
		return nil
	}
	viewedSHA, ok, err := g.RevParse(ctx, viewDir, "HEAD")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: cached repo view has no HEAD", viewDir)
	}
	freshSHA, _, err := g.FetchBranch(ctx, cloneDir, "origin", base)
	if err != nil {
		return fmt.Errorf("re-fetching origin/%s to confirm the plan is still current: %w", base, err)
	}
	if freshSHA != "" && freshSHA != viewedSHA {
		return fmt.Errorf("origin/%s has moved since this plan was built (was %s, now %s) — refresh (F5, or reopen) and re-plan", base, shortRev(viewedSHA), shortRev(freshSHA))
	}
	return nil
}

// buildRefreshRepoFunc is F5's own re-read of origin (matrix.RefreshRepoFunc) — the same
// refreshRepoView + gitops.Discover pair runTUI's own boot does, so the matrix and a plan
// built moments later never disagree about which origin/<base> either was reading. appsRoot
// is eff.appsRoot, closed over at wiring time (cmd/hoist owns the adaptor, §4.8 — the matrix
// package never sees it).
func buildRefreshRepoFunc(g git.Git, cloneDir, base, appsRoot string) matrix.RefreshRepoFunc {
	return func(ctx context.Context) (*gitops.Repo, error) {
		viewDir, err := refreshRepoView(ctx, g, cloneDir, base)
		if err != nil {
			return nil, err
		}
		return gitops.Discover(viewDir, appsRoot)
	}
}
