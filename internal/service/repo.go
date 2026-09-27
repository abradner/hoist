package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// RepoMode picks how LoadRepo reads the GitOps repo: from the operator's own clone directly
// (the CLI's own `gitops.Discover(RepoDir)`), or from a cached view of origin/<base> (the
// TUI's own repoview.go behavior, moved here unchanged).
type RepoMode int

const (
	// RepoFromClone discovers directly from Settings.RepoDir, exactly what `hoist plan`/
	// `hoist promote` have always done: a pure local disk read of the operator's own
	// checkout, no network needed.
	RepoFromClone RepoMode = iota
	// RepoFromOrigin fetches origin/Settings.Base into a cached worktree first (refreshRepoView
	// below) and discovers from THAT — the TUI's own answer to "what does origin actually look
	// like right now", read without ever touching the operator's own checkout or working tree
	// (AGENTS.md §4.6). Falls back to RepoFromClone's own local read when origin can't be
	// reached at all, since browsing the matrix must stay possible offline (principle 5).
	RepoFromOrigin
)

// RepoView is one snapshot of the GitOps repo: the discovered Repo, the directory it was
// discovered from, and — for RepoFromOrigin — whether that came from the cached origin view
// or fell back to the clone, and why.
type RepoView struct {
	Repo *gitops.Repo
	Dir  string
	// FromOrigin is true when Dir is the cached origin view (refreshRepoView succeeded), false
	// when LoadRepo fell back to the clone itself (RepoFromOrigin's fetch failed) or was asked
	// for RepoFromClone directly.
	FromOrigin bool
	// Fallback is refreshRepoView's own error when RepoFromOrigin had to fall back to the
	// clone. nil whenever FromOrigin is true, or for RepoFromClone.
	Fallback error
}

// LoadRepo reads the GitOps repo per mode and stores the result as s's current view (Repo()
// below reads it back). Called at TUI boot and by RefreshRepo (F5); a CLI command that only
// ever discovers once may ignore the stored view and just use the returned RepoView.
func (s *Service) LoadRepo(ctx context.Context, mode RepoMode) (RepoView, error) {
	var view RepoView
	switch mode {
	case RepoFromOrigin:
		dir := s.settings.RepoDir
		fresh, err := refreshRepoView(ctx, s.Git(), s.settings.RepoDir, s.settings.Base)
		if err != nil {
			view.Fallback = err
		} else {
			dir = fresh
			view.FromOrigin = true
		}
		r, derr := gitops.Discover(dir, s.settings.AppsRoot)
		if derr != nil {
			return RepoView{}, derr
		}
		view.Repo, view.Dir = r, dir
	default: // RepoFromClone
		r, err := gitops.Discover(s.settings.RepoDir, s.settings.AppsRoot)
		if err != nil {
			return RepoView{}, err
		}
		view = RepoView{Repo: r, Dir: s.settings.RepoDir}
	}
	s.mu.Lock()
	s.view = view
	s.mu.Unlock()
	return view, nil
}

// RefreshRepo is F5's own re-read of origin: RepoFromOrigin, so the matrix and a plan built
// moments later never disagree about which origin/<base> either was reading.
func (s *Service) RefreshRepo(ctx context.Context) (RepoView, error) {
	return s.LoadRepo(ctx, RepoFromOrigin)
}

// Repo returns the current RepoView — whatever the most recent LoadRepo/RefreshRepo stored.
// The zero value (Repo == nil) means LoadRepo has never been called.
func (s *Service) Repo() RepoView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view
}

// repoViewDir is where the TUI's own read of origin/<base> lives — under engine.CacheDir, the
// same $XDG_CACHE_HOME/hoist root every other hoist cache uses (never ~/Library), alongside
// promotion worktrees (engine.WorktreeDir) but keyed by the SELECTED REPO's own clone path
// rather than a promotion id: this cache is reused across TUI boots and F5 refreshes for as
// long as the operator points hoist at the same clone, not created fresh per promotion. Two
// different --repo values (or two repos[] entries) never collide, since each hashes to its own
// subdirectory.
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
// refreshes rather than a throwaway per call: the cache directory persists under repoViewDir,
// removed and recreated on every call rather than updated in place. That is cheap, not a
// second clone — WorktreeAtRef shares object data with cloneDir's own .git via git's own
// worktree mechanism, so this is a file-tree sync against already-local objects; only
// FetchBranch talks to the network, and only for what changed since the last refresh.
//
// This is what makes the matrix's initial read and every later F5 describe origin, not
// whatever the operator's own working tree happens to hold — hoist's own writes (a direct push
// in particular) never touch that working tree at all (§4.6), so a repo read from disk the
// ordinary way goes stale the moment hoist itself writes anything. checkRepoViewCurrent
// (below) is the narrower check that's left: has origin/<base> moved again since this view was
// last refreshed.
func refreshRepoView(ctx context.Context, g git.Git, cloneDir, base string) (viewDir string, err error) {
	if _, _, err := g.FetchBranch(ctx, cloneDir, "origin", base); err != nil {
		return "", fmt.Errorf("fetching origin/%s: %w", base, err)
	}
	viewDir, err = repoViewDir(cloneDir)
	if err != nil {
		return "", err
	}
	// Mirrors pkg/git.Exec's own resolveBase reasoning: prefer the remote-tracking ref
	// whenever it exists, fully qualified either way so a tag named like the branch or like
	// "origin/<base>" can never win the short-name lookup (issue #100).
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

// CheckRepoViewCurrent refuses a confirm whose plan (built from viewDir, at whatever moment
// refreshRepoView last ran — TUI boot or a since F5) no longer matches origin/<base>'s present
// tip. Deliberately NOT checkCloneCurrentForBase's per-file blob comparison: that function's
// whole shape assumes cloneDir's working tree tracks refs/heads/<base>, the local branch —
// true for an ordinary checkout, false by construction for viewDir, which is deliberately
// checked out from origin, never the local branch. What's left to verify is simpler: re-fetch
// origin/<base> and compare its SHA to what viewDir was checked out at.
//
// viewDir empty or equal to cloneDir (refreshRepoView itself failed, the caller fell back to
// the clone) skips this check entirely — there is nothing cached to compare against.
//
// Exported (unlike repoViewDir/refreshRepoView) because cmd/hoist/wiring.go's
// buildStartPromotion still calls this directly in this train — StartPromotion itself moves
// into this package only in PR D.
func CheckRepoViewCurrent(ctx context.Context, g git.Git, cloneDir, base, viewDir string) error {
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

// shortRev shortens a full sha for an error message, exactly as cmd/hoist's own shortRev did.
func shortRev(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
