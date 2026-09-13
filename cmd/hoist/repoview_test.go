package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/git"
)

// pushFromASeparateClone simulates a write that never touches cloneDir's own working tree —
// exactly what hoist's own direct-mode push does (AGENTS.md §4.6) and exactly the scenario
// checkRepoViewCurrent/refreshRepoView exist to handle: origin moves, cloneDir's own checkout
// does not. Cloning fresh from cloneDir's own "origin" remote, committing and pushing from
// there is the simplest way to advance origin independently.
func pushFromASeparateClone(t *testing.T, cloneDir string) (newSHA string) {
	t.Helper()
	originURL := strings.TrimSpace(outGitHost(t, cloneDir, "remote", "get-url", "origin"))
	other := t.TempDir()
	runGitHost(t, "", "clone", "-q", originURL, other)
	if err := os.WriteFile(filepath.Join(other, "elsewhere.txt"), []byte("someone else's write\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, other, "add", "elsewhere.txt")
	runGitHost(t, other, "commit", "-q", "-m", "a write from somewhere else entirely")
	runGitHost(t, other, "push", "-q", "origin", "HEAD:main")
	return strings.TrimSpace(outGitHost(t, other, "rev-parse", "HEAD"))
}

// TestRefreshRepoViewReadsOrigin proves refreshRepoView's own core promise: the cached
// worktree it returns reflects origin/<base>, never cloneDir's own possibly-stale working
// tree, and never touches cloneDir's own checked-out branch or index (AGENTS.md §4.6) — the
// clone's own HEAD/working tree are exactly what they were before the call.
func TestRefreshRepoViewReadsOrigin(t *testing.T) {
	_, clone, _ := newPromoteFixture(t)
	beforeHead, _, err := (git.Exec{}).RevParse(context.Background(), clone, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	newSHA := pushFromASeparateClone(t, clone)

	viewDir, err := refreshRepoView(context.Background(), git.Exec{}, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}
	if _, err := os.Stat(filepath.Join(viewDir, "elsewhere.txt")); err != nil {
		t.Errorf("cached view at %s does not contain origin's own new file: %v", viewDir, err)
	}
	viewedSHA, ok, err := (git.Exec{}).RevParse(context.Background(), viewDir, "HEAD")
	if err != nil || !ok {
		t.Fatalf("cached view HEAD: ok=%v err=%v", ok, err)
	}
	if viewedSHA != newSHA {
		t.Errorf("cached view HEAD = %s, want origin's new tip %s", viewedSHA, newSHA)
	}

	afterHead, _, err := (git.Exec{}).RevParse(context.Background(), clone, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if afterHead != beforeHead {
		t.Errorf("cloneDir's own HEAD moved from %s to %s — refreshRepoView must never touch the operator's own checkout", beforeHead, afterHead)
	}
	if _, err := os.Stat(filepath.Join(clone, "elsewhere.txt")); err == nil {
		t.Error("cloneDir's own working tree picked up origin's new file — refreshRepoView must never touch it")
	}
}

// TestRefreshRepoViewReusesCacheOnSecondCall: a second refresh (F5, or a second TUI boot)
// must succeed against the SAME cache path, picking up whatever origin has moved to since —
// proving the cache directory is genuinely reused (removed and recreated), not merely
// working by accident on a fresh temp dir every time.
func TestRefreshRepoViewReusesCacheOnSecondCall(t *testing.T) {
	_, clone, _ := newPromoteFixture(t)
	g := git.Exec{}

	first, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("first refreshRepoView: %v", err)
	}
	newSHA := pushFromASeparateClone(t, clone)

	second, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("second refreshRepoView: %v", err)
	}
	if first != second {
		t.Errorf("cache path changed between calls: %s vs %s, want the same reused directory", first, second)
	}
	sha, ok, err := g.RevParse(context.Background(), second, "HEAD")
	if err != nil || !ok || sha != newSHA {
		t.Errorf("second refresh HEAD = %s ok=%v err=%v, want origin's new tip %s", sha, ok, err, newSHA)
	}
}

// TestCheckRepoViewCurrentAcceptsAStaleLocalBranch is the regression test for the exact
// design bug caught while building this: checkCloneCurrentForBase's own comparison (against
// refs/heads/<base>, the LOCAL branch) would misfire here, because a cached repo view is
// deliberately checked out from origin, never the local branch — so whenever the local
// branch is behind origin (exactly what hoist's own direct-mode writes produce, since they
// never touch the operator's checkout, AGENTS.md §4.6), that comparison would flag a
// perfectly current view as "uncommitted local changes". checkRepoViewCurrent must not: the
// local branch's own staleness is irrelevant to it, only whether origin has moved again since
// the view was refreshed.
func TestCheckRepoViewCurrentAcceptsAStaleLocalBranch(t *testing.T) {
	_, clone, _ := newPromoteFixture(t)
	g := git.Exec{}

	// Advance origin without ever touching clone's own checkout — a mutation-tested finding
	// (an adversarial review of #PR7) found the previous version of this test never actually
	// did this, so it kept passing even against checkCloneCurrentForBase's own old, wrong
	// comparison against refs/heads/<base>. clone's own refs/heads/main is now genuinely
	// stale relative to origin, exactly what hoist's own direct-mode writes produce
	// (AGENTS.md §4.6: they push straight to origin, never clone's own checkout).
	pushFromASeparateClone(t, clone)

	viewDir, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}

	// Nobody has touched origin again since viewDir was built from it — only clone's own
	// local branch is behind, which checkRepoViewCurrent must not care about.
	if err := checkRepoViewCurrent(context.Background(), g, clone, "main", viewDir); err != nil {
		t.Errorf("checkRepoViewCurrent refused a view that is still current against origin: %v", err)
	}
}

// TestCheckRepoViewCurrentRefusesWhenOriginMovesAgain: the one case this check exists to
// catch — origin advanced again after the view was built (a race between the operator
// looking at the matrix and confirming a plan, or simply a stale F5).
func TestCheckRepoViewCurrentRefusesWhenOriginMovesAgain(t *testing.T) {
	_, clone, _ := newPromoteFixture(t)
	g := git.Exec{}
	viewDir, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}

	pushFromASeparateClone(t, clone)

	err = checkRepoViewCurrent(context.Background(), g, clone, "main", viewDir)
	if err == nil {
		t.Fatal("checkRepoViewCurrent accepted a view origin has since moved past")
	}
	if !strings.Contains(err.Error(), "has moved since this plan was built") {
		t.Errorf("error = %v, want it to name the actual cause", err)
	}
}

// TestCheckRepoViewCurrentSkipsWhenNoView: the runTUI boot-fallback shape — viewDir equal to
// cloneDir (refreshRepoView itself failed and runTUI fell back to eff.repo) means there is no
// separate cached view to have gone stale, so nothing here should refuse.
func TestCheckRepoViewCurrentSkipsWhenNoView(t *testing.T) {
	_, clone, _ := newPromoteFixture(t)
	g := git.Exec{}
	if err := checkRepoViewCurrent(context.Background(), g, clone, "main", clone); err != nil {
		t.Errorf("checkRepoViewCurrent should be a no-op when viewDir == cloneDir: %v", err)
	}
	if err := checkRepoViewCurrent(context.Background(), g, clone, "main", ""); err != nil {
		t.Errorf("checkRepoViewCurrent should be a no-op for an empty viewDir: %v", err)
	}
}
