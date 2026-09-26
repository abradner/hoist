package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/git"
)

// runGitHost/outGitHost/gitHostCmd/newRepoFixture are a minimal, self-contained mirror of
// cmd/hoist's own fixture helpers (promote_test.go) — just enough git plumbing to prove
// refreshRepoView/CheckRepoViewCurrent's own promises, never a real GitHub repo or cluster
// (AGENTS.md hard constraints).
func gitHostCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-c", "safe.bareRepository=all"}, args...)...)
	cmd.Dir = dir
	return cmd
}

func runGitHost(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitHostCmd(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func outGitHost(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitHostCmd(dir, args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// newRepoFixture builds a local bare "origin" and a clone of it with one seed commit, and
// points HOME/XDG_* at a throwaway sandbox for the duration of the test — the same isolation
// cmd/hoist's newPromoteFixture uses, trimmed to just what repoViewDir/refreshRepoView need
// (no gitops wrapper/deployment scaffolding, no config file, no forge: this file never drives
// a promotion, only the repo-view cache).
func newRepoFixture(t *testing.T) (cloneDir string) {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	// The [receive]/[gc]/[maintenance] block keeps git from spawning a detached
	// `maintenance run --auto` on push, which can still be writing under the bare origin's
	// objects/ when t.TempDir cleans up (#123).
	const cfgFile = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n" +
		"[receive]\n\tautogc = false\n[gc]\n\tauto = 0\n\tautoDetach = false\n[maintenance]\n\tauto = false\n\tautoDetach = false\n"
	if err := os.WriteFile(gitconfig, []byte(cfgFile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))

	seed := t.TempDir()
	runGitHost(t, "", "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, seed, "add", ".")
	runGitHost(t, seed, "commit", "-q", "-m", "seed")

	origin := filepath.Join(t.TempDir(), "origin.git")
	runGitHost(t, "", "init", "-q", "--bare", "-b", "main", origin)
	runGitHost(t, seed, "remote", "add", "origin", origin)
	runGitHost(t, seed, "push", "-q", "origin", "main")

	clone := filepath.Join(t.TempDir(), "clone")
	runGitHost(t, "", "clone", "-q", origin, clone)
	return clone
}

// pushFromASeparateClone simulates a write that never touches cloneDir's own working tree —
// exactly what hoist's own direct-mode push does (AGENTS.md §4.6) and exactly the scenario
// CheckRepoViewCurrent/refreshRepoView exist to handle: origin moves, cloneDir's own checkout
// does not.
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
	clone := newRepoFixture(t)
	beforeHead, _, err := (git.Exec{}).RevParse(context.Background(), clone, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	newSHA := pushFromASeparateClone(t, clone)

	viewDir, viewedSHA, err := refreshRepoView(context.Background(), git.Exec{}, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}
	if _, err := os.Stat(filepath.Join(viewDir, "elsewhere.txt")); err != nil {
		t.Errorf("cached view at %s does not contain origin's own new file: %v", viewDir, err)
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

// TestRefreshRepoViewReusesCacheOnSecondCall: a second refresh (F5, or a second TUI boot) must
// succeed against the SAME cache path, picking up whatever origin has moved to since — proving
// the cache directory is genuinely reused (removed and recreated), not merely working by
// accident on a fresh temp dir every time.
func TestRefreshRepoViewReusesCacheOnSecondCall(t *testing.T) {
	clone := newRepoFixture(t)
	g := git.Exec{}

	first, _, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("first refreshRepoView: %v", err)
	}
	newSHA := pushFromASeparateClone(t, clone)

	second, sha, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("second refreshRepoView: %v", err)
	}
	if first != second {
		t.Errorf("cache path changed between calls: %s vs %s, want the same reused directory", first, second)
	}
	if sha != newSHA {
		t.Errorf("second refresh SHA = %s, want origin's new tip %s", sha, newSHA)
	}
}

// TestCheckRepoViewCurrentAcceptsAStaleLocalBranch is the regression test for the exact design
// bug caught while building this: a per-file blob comparison against refs/heads/<base> (the
// LOCAL branch) would misfire here, because a cached repo view is deliberately checked out
// from origin, never the local branch — so whenever the local branch is behind origin (exactly
// what hoist's own direct-mode writes produce), that comparison would flag a perfectly current
// view as "uncommitted local changes". CheckRepoViewCurrent must not: the local branch's own
// staleness is irrelevant to it, only whether origin has moved again since the view was
// refreshed.
func TestCheckRepoViewCurrentAcceptsAStaleLocalBranch(t *testing.T) {
	clone := newRepoFixture(t)
	g := git.Exec{}

	pushFromASeparateClone(t, clone)

	_, viewedSHA, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}

	if err := CheckRepoViewCurrent(context.Background(), g, clone, "main", viewedSHA); err != nil {
		t.Errorf("CheckRepoViewCurrent refused a view that is still current against origin: %v", err)
	}
}

// TestCheckRepoViewCurrentRefusesWhenOriginMovesAgain: the one case this check exists to catch
// — origin advanced again after the view was built (a race between the operator looking at the
// matrix and confirming a plan, or simply a stale F5).
func TestCheckRepoViewCurrentRefusesWhenOriginMovesAgain(t *testing.T) {
	clone := newRepoFixture(t)
	g := git.Exec{}
	_, viewedSHA, err := refreshRepoView(context.Background(), g, clone, "main")
	if err != nil {
		t.Fatalf("refreshRepoView: %v", err)
	}

	pushFromASeparateClone(t, clone)

	err = CheckRepoViewCurrent(context.Background(), g, clone, "main", viewedSHA)
	if err == nil {
		t.Fatal("CheckRepoViewCurrent accepted a view origin has since moved past")
	}
	if !strings.Contains(err.Error(), "has moved since this plan was built") {
		t.Errorf("error = %v, want it to name the actual cause", err)
	}
}

// countingSlowGit wraps a real git.Exec and, on FetchBranch only (refreshRepoView's own first
// call, and therefore the first thing two concurrent LoadRepo(RepoFromOrigin) calls would race
// on), tracks how many calls are inside the method AT ONCE and sleeps briefly before returning —
// widening the race window so two calls that are allowed to overlap actually would, rather than
// happening to interleave correctly by luck on a fast machine.
type countingSlowGit struct {
	git.Exec
	inflight int32
	maxSeen  int32
}

func (g *countingSlowGit) FetchBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	n := atomic.AddInt32(&g.inflight, 1)
	for {
		prevMax := atomic.LoadInt32(&g.maxSeen)
		if n <= prevMax || atomic.CompareAndSwapInt32(&g.maxSeen, prevMax, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	defer atomic.AddInt32(&g.inflight, -1)
	return g.Exec.FetchBranch(ctx, dir, remote, branch)
}

// TestLoadRepoSerialised is Train 2 design PR 4's service-side fix (internal/service/repo.go's
// own refreshMu doc comment): two LoadRepo(RepoFromOrigin) calls racing against the same clone
// (a completion-triggered refresh and an F5 the operator happens to press in the same instant,
// or any other caller reaching Service directly) must never run refreshRepoView concurrently —
// that function removes and recreates ONE FIXED cached worktree, so overlapping calls can
// corrupt it (index.lock contention, a worktree registration torn between the two). This proves
// it at the Service layer, independent of the matrix's own UI-level askRepoRefresh coalescing:
// two goroutines call LoadRepo at once against a git client engineered to widen the race window,
// and the max concurrent FetchBranch count it observed must be exactly 1.
func TestLoadRepoSerialised(t *testing.T) {
	clone := newRepoFixture(t)
	g := &countingSlowGit{}
	set := Settings{RepoDir: clone, Base: "main"}
	svc := New(set, Deps{
		Git:   func() git.Git { return g },
		Store: FileStore{},
		Now:   time.Now,
	})

	// The fixture has no `kind: Application` wrappers at all, so gitops.Discover (which
	// LoadRepo calls after refreshRepoView, still inside the same lock) always errors — beside
	// the point here: this test proves FetchBranch never overlaps, not that the fixture is a
	// promotable repo. Every real caller (cmd/hoist, the TUI) points RepoDir at an actual
	// GitOps repo, exercised elsewhere (TestRefreshRepoViewReadsOrigin and friends).
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.LoadRepo(context.Background(), RepoFromOrigin)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&g.maxSeen); got != 1 {
		t.Errorf("max concurrent FetchBranch calls = %d, want 1 (refreshMu must serialize LoadRepo(RepoFromOrigin))", got)
	}
}

// TestCheckRepoViewCurrentSkipsWhenNoView: the runTUI boot-fallback shape — an empty viewedSHA
// (refreshRepoView itself failed and the caller fell back to the clone, or this is a clone-mode
// view, which never sets RepoView.SHA at all) means there is no captured origin SHA to have gone
// stale, so nothing here should refuse.
func TestCheckRepoViewCurrentSkipsWhenNoView(t *testing.T) {
	clone := newRepoFixture(t)
	g := git.Exec{}
	if err := CheckRepoViewCurrent(context.Background(), g, clone, "main", ""); err != nil {
		t.Errorf("CheckRepoViewCurrent should be a no-op for an empty viewedSHA: %v", err)
	}
}
