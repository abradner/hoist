package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestRepo creates a bare "origin" repo and a clone of it, with an isolated HOME and
// global git config so the test never reads (or could accidentally invoke) the developer's
// own ~/.gitconfig or 1Password signing setup (AGENTS.md §9 gotcha on hermetic tests).
// Returns the clone directory and the bare origin's directory.
func newTestRepo(t *testing.T) (cloneDir, originDir string) {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	const cfg = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[tag]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(gitconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))

	originDir = filepath.Join(t.TempDir(), "origin.git")
	if err := runHost(t, "", "init", "--bare", "-b", "main", originDir); err != nil {
		t.Fatal(err)
	}

	seed := t.TempDir()
	if err := runHost(t, "", "init", "-b", "main", seed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, seed, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, seed, "commit", "-m", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, seed, "remote", "add", "origin", originDir); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, seed, "push", "origin", "main"); err != nil {
		t.Fatal(err)
	}

	cloneDir = filepath.Join(t.TempDir(), "clone")
	if err := runHost(t, "", "clone", originDir, cloneDir); err != nil {
		t.Fatal(err)
	}
	return cloneDir, originDir
}

func runHost(t *testing.T, dir string, args ...string) error {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git %s: %s", strings.Join(args, " "), out)
	}
	return err
}

func ctx() context.Context { return context.Background() }

// gitPath resolves a git-path (a worktree's index.lock lives under its own private admin
// directory, not a plain .git/<name> join, since .git in a linked worktree is a pointer file).
func gitPath(t *testing.T, dir, rel string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-path", rel).CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --git-path %s: %v: %s", rel, err, out)
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

func TestWorktreeCreatesAndReuses(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec

	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/abc123", "main"); err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}

	// Reuse: a second call with the same branch must not fail or reset anything.
	if err := os.WriteFile(filepath.Join(wt, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/abc123", "main"); err != nil {
		t.Fatalf("Worktree (reuse): %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "marker.txt")); err != nil {
		t.Fatalf("reuse should not have touched the worktree: %v", err)
	}
}

func TestWorktreeRecoversFromStaleDirectory(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	// Simulate a run killed after mkdir but before `git worktree add` registered anything:
	// a plain directory with no .git file, not in git's worktree registry at all.
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/abc123", "main"); err != nil {
		t.Fatalf("Worktree should recover from a stale directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("worktree not created after stale cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatalf("leftover file from the stale directory should be gone, stat err = %v", err)
	}
}

func TestWorktreeThenCommitSurvivesReuse(t *testing.T) {
	cloneDir, originDir := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	branch := "hoist/app-production/abc123"
	if err := g.Worktree(ctx(), cloneDir, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "app.yaml"), []byte("image: v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := g.Commit(ctx(), wt, "promote\n\nhoist-id: abc123\n", []string{"app.yaml"}, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sha == "" {
		t.Fatal("Commit returned empty sha")
	}

	if err := g.Push(ctx(), wt, "origin", branch); err != nil {
		t.Fatalf("Push: %v", err)
	}
	remoteSHA, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch)
	if err != nil || !ok {
		t.Fatalf("LsRemoteBranch: sha=%s ok=%v err=%v", remoteSHA, ok, err)
	}
	if remoteSHA != sha {
		t.Fatalf("pushed sha %s, remote reports %s", sha, remoteSHA)
	}

	// A "killed and restarted" process reuses the exact same worktree/branch: Worktree must
	// be a no-op that keeps the existing commit, never reset it.
	if err := g.Worktree(ctx(), cloneDir, wt, branch, "main"); err != nil {
		t.Fatalf("Worktree (resume): %v", err)
	}
	sha2, ok, err := g.RevParse(ctx(), wt, "HEAD")
	if err != nil || !ok || sha2 != sha {
		t.Fatalf("HEAD after resume = %q ok=%v err=%v, want %q", sha2, ok, err, sha)
	}

	// Idempotent push: pushing the same content again must succeed (already up to date), not
	// error, since local and remote tips already agree.
	if err := g.Push(ctx(), wt, "origin", branch); err != nil {
		t.Fatalf("Push (idempotent): %v", err)
	}
	_ = originDir
}

func TestPushRejectsNonFastForwardConflict(t *testing.T) {
	cloneDir, originDir := newTestRepo(t)
	branch := "hoist/app-production/conflict"
	var g Exec
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.Worktree(ctx(), cloneDir, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "app.yaml"), []byte("image: mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Commit(ctx(), wt, "mine\n", []string{"app.yaml"}, 30*time.Second, nil); err != nil {
		t.Fatal(err)
	}

	// Someone/something else creates the same branch name on origin directly, with
	// different content and no relation to our commit.
	other := filepath.Join(t.TempDir(), "other-clone")
	if err := runHost(t, "", "clone", originDir, other); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, other, "checkout", "-b", branch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "someone-else.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, other, "add", "someone-else.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, other, "commit", "-m", "someone else"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, other, "push", "origin", branch); err != nil {
		t.Fatal(err)
	}

	// Our push of an unrelated branch must be rejected, not force-pushed.
	err := g.Push(ctx(), wt, "origin", branch)
	if err == nil {
		t.Fatal("Push should have been rejected as a non-fast-forward conflict")
	}
	remoteSHA, ok, lerr := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch)
	if lerr != nil || !ok {
		t.Fatalf("LsRemoteBranch after rejected push: %v %v", ok, lerr)
	}
	ourSHA, _, _ := g.RevParse(ctx(), wt, "HEAD")
	if remoteSHA == ourSHA {
		t.Fatal("remote should still hold someone else's commit, not ours")
	}
}

func TestLsTreeBlobAndHashObjectAgree(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	branch := "hoist/app-production/blob"
	if err := g.Worktree(ctx(), cloneDir, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	content := []byte("image: ghcr.io/example/app:v2@sha256:" + strings.Repeat("a", 64) + "\n")
	if err := os.WriteFile(filepath.Join(wt, "app.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := g.HashObject(ctx(), wt, content)
	if err != nil {
		t.Fatalf("HashObject: %v", err)
	}
	if _, err := g.Commit(ctx(), wt, "promote\n", []string{"app.yaml"}, 30*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	got, ok, err := g.LsTreeBlob(ctx(), wt, "HEAD", "app.yaml")
	if err != nil || !ok {
		t.Fatalf("LsTreeBlob: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("LsTreeBlob = %s, HashObject predicted %s", got, want)
	}
	if _, ok, err := g.LsTreeBlob(ctx(), wt, "HEAD", "does-not-exist.yaml"); err != nil || ok {
		t.Fatalf("LsTreeBlob for a missing path: ok=%v err=%v", ok, err)
	}
}

// TestIsAncestor exercises git.Git.IsAncestor directly against a real repo: the ancestor/self
// cases IsAncestor must report true for, the non-ancestor case it must report false for (never
// an error — a sibling branch's tip is a perfectly resolvable rev that just isn't an ancestor),
// and the genuinely-unresolvable case it must report as a real error, never silently folded into
// false. Added for M4 hardening finding #2 (round 3): MergedStep.Observe's revert-vs-superseded
// distinction is exactly this method, so it earns its own direct coverage independent of
// MergedStep's own (necessarily more indirect) tests in internal/engine.
func TestIsAncestor(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/ancestor", "main"); err != nil {
		t.Fatal(err)
	}
	base, ok, err := g.RevParse(ctx(), wt, "HEAD")
	if err != nil || !ok {
		t.Fatalf("RevParse HEAD: ok=%v err=%v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(wt, "child.txt"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	child, err := g.Commit(ctx(), wt, "child\n", []string{"child.txt"}, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	if isAncestor, err := g.IsAncestor(ctx(), wt, base, child); err != nil || !isAncestor {
		t.Fatalf("base should be an ancestor of child: isAncestor=%v err=%v", isAncestor, err)
	}
	if isAncestor, err := g.IsAncestor(ctx(), wt, child, child); err != nil || !isAncestor {
		t.Fatalf("a commit should be its own ancestor per git's own definition: isAncestor=%v err=%v", isAncestor, err)
	}
	if isAncestor, err := g.IsAncestor(ctx(), wt, child, base); err != nil || isAncestor {
		t.Fatalf("child must not be reported as an ancestor of base: isAncestor=%v err=%v", isAncestor, err)
	}
	if _, err := g.IsAncestor(ctx(), wt, "not-a-real-sha", child); err == nil {
		t.Fatal("an unresolvable ancestor rev should be a real error, not a false negative")
	}
}

// TestObjectExists is the direct real-git regression for MergedStep.mergeWasReverted's own
// negative-path contract (round-9 finding: only exercised indirectly via engine-level tests
// before this) — a well-formed but missing sha must be exists=false, err=nil (never treated as
// a failure, since this is the ordinary "the base was reset past this commit" case), while a
// genuinely malformed rev is a real error, so revert-detection logic can't silently regress
// without a dedicated test in this package noticing.
func TestObjectExists(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	var g Exec
	head, ok, err := g.RevParse(ctx(), cloneDir, "HEAD")
	if err != nil || !ok {
		t.Fatalf("RevParse HEAD: ok=%v err=%v", ok, err)
	}

	if exists, err := g.ObjectExists(ctx(), cloneDir, head); err != nil || !exists {
		t.Fatalf("HEAD should exist: exists=%v err=%v", exists, err)
	}
	const wellFormedButMissing = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if exists, err := g.ObjectExists(ctx(), cloneDir, wellFormedButMissing); err != nil {
		t.Fatalf("a well-formed but missing sha must not be a real error, got: %v", err)
	} else if exists {
		t.Fatal("a sha no commit in this repo ever produced must not report exists=true")
	}
	if _, err := g.ObjectExists(ctx(), cloneDir, "not-a-real-sha"); err == nil {
		t.Fatal("a malformed rev should be a real error, not a false negative")
	}
}

// TestFetchBranchMissingBranchReturnsNotOK is FetchBranch's real-git regression for classifying
// a branch that plain does not exist on the remote: ok=false, err=nil, not a stderr-text match.
// This exercises the real fix (an authoritative LsRemoteBranch re-check after the fetch errors)
// end to end against genuine git, not a canned error string.
func TestFetchBranchMissingBranchReturnsNotOK(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	var g Exec
	sha, ok, err := g.FetchBranch(ctx(), cloneDir, "origin", "no-such-branch-ever-pushed")
	if err != nil {
		t.Fatalf("a genuinely missing branch should not be an error, got: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for a missing branch, got sha=%q", sha)
	}
}

// TestFetchBranchRealFailureIsNotMisclassifiedAsMissing is the discriminating half of that
// regression: a fetch failure for a reason OTHER than "branch missing" (here, a remote name that
// isn't configured at all, so both the fetch and the authoritative LsRemoteBranch re-check fail
// the same way) must still surface as a real error — proving the fix classifies by asking the
// remote, not by treating every fetch failure as "not found". A stderr-text match keyed on
// git's specific "couldn't find remote ref" wording could not tell these two failures apart if
// git's phrasing for "no such remote" ever happened to overlap; asking the remote directly can.
func TestFetchBranchRealFailureIsNotMisclassifiedAsMissing(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	var g Exec
	_, ok, err := g.FetchBranch(ctx(), cloneDir, "no-such-remote-configured", "main")
	if err == nil {
		t.Fatalf("a nonexistent remote should be a real error, not silently ok=false (ok=%v)", ok)
	}
	if ok {
		t.Fatal("should never report ok=true alongside an error")
	}
}

// TestDeleteRemoteBranchRealFailureIsNotMisclassifiedAsGone is DeleteRemoteBranch's counterpart
// to TestFetchBranchRealFailureIsNotMisclassifiedAsMissing: a delete failure for a reason other
// than "branch already gone" (a remote name that isn't configured, so the authoritative
// LsRemoteBranch re-check also fails) must surface as a real error, not be silently swallowed as
// idempotent success.
func TestDeleteRemoteBranchRealFailureIsNotMisclassifiedAsGone(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	var g Exec
	err := g.DeleteRemoteBranch(ctx(), cloneDir, "no-such-remote-configured", "some-branch")
	if err == nil {
		t.Fatal("a nonexistent remote should be a real error, not silently treated as already-gone")
	}
}

// fakeGitTrapping writes a wrapper script at Exec.Bin that intercepts exactly one subcommand
// for one branch name (emitting the fixed stderr text a stderr-string-matching implementation
// used to key off) and delegates every other invocation — including the authoritative
// LsRemoteBranch re-check the fix now performs — straight to the real git binary found on
// PATH. This is what makes the two tests below actually discriminating: a real branch that
// genuinely exists on origin, with git's OWN historical error text asserting the opposite,
// proves the fix trusts LsRemoteBranch over the message rather than merely exercising a case
// where both approaches happen to agree (a bad remote name fails identically either way, which
// is not a discriminating test by itself).
func fakeGitTrapping(t *testing.T, subcommand, branch, stderrMsg string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	fake := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"" + subcommand + "\" ]; then\n" +
		"  for a in \"$@\"; do\n" +
		"    if [ \"$a\" = \"" + branch + "\" ]; then\n" +
		"      echo \"" + stderrMsg + "\" >&2\n" +
		"      exit 128\n" +
		"    fi\n" +
		"  done\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return fake
}

// TestFetchBranchTrustsLsRemoteOverMisleadingStderrText is the discriminating regression for
// the string-matching fix: a branch that GENUINELY exists on origin, fetched through a git
// wrapper that emits git's own historical "couldn't find remote ref" wording anyway (standing in
// for a different git version/locale producing that same text for an unrelated reason), must
// NOT be silently reported as missing. The pre-fix code matched that exact substring and would
// have returned ok=false, err=nil here — truthfully wrong, since LsRemoteBranch (delegated to
// real git by the wrapper) can see the branch is really there.
func TestFetchBranchTrustsLsRemoteOverMisleadingStderrText(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	const branch = "genuinely-exists"
	if err := runHost(t, cloneDir, "checkout", "-q", "-b", branch); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, cloneDir, "push", "-q", "origin", branch); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, cloneDir, "checkout", "-q", "main"); err != nil {
		t.Fatal(err)
	}

	fake := fakeGitTrapping(t, "fetch", branch, "fatal: couldn't find remote ref refs/heads/"+branch)
	g := Exec{Bin: fake}
	_, ok, err := g.FetchBranch(ctx(), cloneDir, "origin", branch)
	if err == nil {
		t.Fatalf("a branch that genuinely exists must not be reported as missing just because git's stderr text says so (ok=%v)", ok)
	}
}

// TestDeleteRemoteBranchTrustsLsRemoteOverMisleadingStderrText is DeleteRemoteBranch's
// counterpart: a branch that is genuinely STILL on origin (the delete didn't really happen),
// deleted through a wrapper that emits git's own historical "remote ref does not exist" wording
// anyway, must not be silently reported as successfully deleted.
func TestDeleteRemoteBranchTrustsLsRemoteOverMisleadingStderrText(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	const branch = "still-there"
	if err := runHost(t, cloneDir, "checkout", "-q", "-b", branch); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, cloneDir, "push", "-q", "origin", branch); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, cloneDir, "checkout", "-q", "main"); err != nil {
		t.Fatal(err)
	}

	fake := fakeGitTrapping(t, "push", branch, "error: unable to delete 'refs/heads/"+branch+"': remote ref does not exist")
	g := Exec{Bin: fake}
	if err := g.DeleteRemoteBranch(ctx(), cloneDir, "origin", branch); err == nil {
		t.Fatal("a branch that genuinely still exists must not be reported as already-gone just because git's stderr text says so")
	}
	if _, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch); err != nil || !ok {
		t.Fatalf("sanity check: the branch should genuinely still be on origin, untouched: ok=%v err=%v", ok, err)
	}
}

func TestLsTreeBlobBeforeAnyCommit(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/empty", "main"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := g.LsTreeBlob(ctx(), wt, "HEAD^{tree}:app.yaml", "app.yaml"); err == nil && ok {
		t.Fatal("unexpected success against a nonsense rev")
	}
	if _, ok, err := g.RevParse(ctx(), wt, "does-not-exist-branch"); err != nil || ok {
		t.Fatalf("RevParse of a nonexistent rev: ok=%v err=%v", ok, err)
	}
}

func TestCommitSigningTimeoutIsRetryable(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/hang", "main"); err != nil {
		t.Fatal(err)
	}
	// A fake gpg.ssh.program that hangs forever stands in for an interactive 1Password
	// prompt that never gets answered — proving the timeout fires and is reported as
	// retryable, not as a crash.
	hang := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 120\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "gpg.format", "ssh"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "gpg.ssh.program", hang); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "user.signingkey", "ssh-ed25519 AAAAtest"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "commit.gpgsign", "true"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "app.yaml"), []byte("image: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := g.RevParse(ctx(), wt, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// signingWaitAfter (5s) fires the "waiting for signing approval" callback, so the
	// per-call timeout here must exceed it to observe both the callback and the timeout.
	var waited int
	start := time.Now()
	_, err = g.Commit(ctx(), wt, "promote\n", []string{"app.yaml"}, 7*time.Second, func() { waited++ })
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error %v does not satisfy errors.Is(_, ErrTimeout)", err)
	}
	if waited == 0 {
		t.Error("onWaiting was never called")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("Commit took %s to give up on a 7s timeout", elapsed)
	}

	// The worktree must be left inspectable: no new commit was created (HEAD is exactly
	// where it was before the attempt), and a stale index.lock must not block the next one.
	if sha, ok, rerr := g.RevParse(ctx(), wt, "HEAD"); rerr != nil || !ok || sha != baseline {
		t.Fatalf("HEAD after a timed-out commit = %q ok=%v err=%v, want unchanged %q", sha, ok, rerr, baseline)
	}
	lockPath := gitPath(t, wt, "index.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("index.lock at %s should have been cleaned up, stat err = %v", lockPath, err)
	}

	if err := runHost(t, wt, "config", "commit.gpgsign", "false"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Commit(ctx(), wt, "promote\n", []string{"app.yaml"}, 30*time.Second, nil); err != nil {
		t.Fatalf("retry after disabling signing should succeed: %v", err)
	}
}

// TestCommitTimeoutKillsWholeProcessTree proves Commit's timeout kills the whole process
// group a signing helper spawns, not just the direct git process. A fake gpg.ssh.program
// (like TestCommitSigningTimeoutIsRetryable's, but this one forks its own background child
// before hanging, standing in for a signing helper that forks a grandchild) — that grandchild
// sleeps far longer than Commit's timeout and then writes a marker file. Without the fix,
// only the direct git process gets signaled; the grandchild is orphaned and keeps running,
// and Commit can block on a stdio pipe fd the grandchild still holds until the grandchild
// itself exits (the reported bug: ~5.33s wall time against a 200ms timeout). With the fix,
// Commit returns close to its own timeout and the grandchild is dead, not just eventually
// quiet.
func TestCommitTimeoutKillsWholeProcessTree(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/tree", "main"); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "marker")
	pidfile := filepath.Join(tmp, "child.pid")
	script := fmt.Sprintf("#!/bin/sh\n( sleep 3; : > %q ) &\necho $! > %q\nsleep 120\n", marker, pidfile)
	hang := filepath.Join(tmp, "hang.sh")
	if err := os.WriteFile(hang, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "gpg.format", "ssh"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "gpg.ssh.program", hang); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "user.signingkey", "ssh-ed25519 AAAAtest"); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, wt, "config", "commit.gpgsign", "true"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "app.yaml"), []byte("image: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := g.Commit(ctx(), wt, "promote\n", []string{"app.yaml"}, 700*time.Millisecond, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error %v does not satisfy errors.Is(_, ErrTimeout)", err)
	}
	// The bug reproduces as Commit blocking near the grandchild's own sleep duration (3s)
	// instead of its 700ms timeout; a bound comfortably below 3s but above the timeout still
	// catches that regression without being flaky about scheduler noise.
	if elapsed > 2*time.Second {
		t.Fatalf("Commit took %s to return against a 700ms timeout (grandchild likely still held stdio open or was left running)", elapsed)
	}

	pidBytes, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("signing helper's grandchild never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("unparseable pid file: %v", err)
	}
	if perr := syscall.Kill(pid, 0); perr == nil {
		t.Fatalf("grandchild pid %d is still alive shortly after Commit returned", pid)
	} else if !errors.Is(perr, syscall.ESRCH) {
		t.Fatalf("unexpected error probing grandchild pid %d: %v", pid, perr)
	}

	// Wait past the grandchild's own sleep and confirm it never got to write the marker —
	// belt-and-suspenders alongside the pid check above (immune to pid reuse).
	time.Sleep(3200 * time.Millisecond)
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("marker file exists — grandchild survived and completed its sleep (stat err=%v)", statErr)
	}
}

// TestDeleteRemoteBranchIsIdempotent is MergedStep's own resume property at the git layer
// (M4): deleting a branch that was already pushed succeeds, and deleting it again — a resumed
// process re-attempting cleanup after a kill between merge and delete — must also succeed
// rather than error on "already gone".
func TestDeleteRemoteBranchIsIdempotent(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	branch := "hoist/app-production/cleanup"
	var g Exec
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.Worktree(ctx(), cloneDir, wt, branch, "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.Push(ctx(), wt, "origin", branch); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch); err != nil || !ok {
		t.Fatalf("sanity check: branch should exist before deletion: ok=%v err=%v", ok, err)
	}

	if err := g.DeleteRemoteBranch(ctx(), cloneDir, "origin", branch); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if _, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch); err != nil || ok {
		t.Fatalf("branch should be gone: ok=%v err=%v", ok, err)
	}
	if err := g.DeleteRemoteBranch(ctx(), cloneDir, "origin", branch); err != nil {
		t.Fatalf("DeleteRemoteBranch (already gone) should be idempotent: %v", err)
	}
}

func TestRemoveWorktreeIdempotent(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	var g Exec
	if err := g.Worktree(ctx(), cloneDir, wt, "hoist/app-production/rm", "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveWorktree(ctx(), cloneDir, wt); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if err := g.RemoveWorktree(ctx(), cloneDir, wt); err != nil {
		t.Fatalf("RemoveWorktree (already gone): %v", err)
	}
}

// Self-review finding (M3 pre-open): a caller bug that resolved worktreeDir to the clone
// directory itself — or a directory containing it — would hit RemoveWorktree's unconditional
// os.RemoveAll with no protection from git itself. guardDisposablePath refuses both shapes
// before anything destructive runs.
func TestWorktreeRefusesToTargetTheCloneItself(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	var g Exec
	// git's own "worktree remove" already refuses a path that is the main working tree, so
	// this case is covered twice over (belt-and-suspenders, not proof guardDisposablePath
	// alone is load-bearing here); TestWorktreeRefusesADirectoryContainingTheClone below is
	// the one that actually fails without the guard.
	if err := g.RemoveWorktree(ctx(), cloneDir, cloneDir); err == nil {
		t.Fatal("RemoveWorktree(worktreeDir == cloneDir) succeeded, want a refusal")
	}
}

func TestWorktreeRefusesADirectoryContainingTheClone(t *testing.T) {
	parent := t.TempDir()
	cloneDir := filepath.Join(parent, "clone")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runHost(t, parent, "init", "-q", "clone"); err != nil {
		t.Fatal(err)
	}
	var g Exec
	// parent contains cloneDir; removing "parent" would delete the clone too.
	if err := g.RemoveWorktree(ctx(), cloneDir, parent); err == nil {
		t.Fatal("RemoveWorktree(worktreeDir containing cloneDir) succeeded, want a refusal")
	}
}

// TestWorktreeRefusesADirectoryInsideTheClone is Finding D: the opposite direction from
// TestWorktreeRefusesADirectoryContainingTheClone above. A worktreeDir that is itself a
// subdirectory of cloneDir (e.g. a misconfigured $XDG_CACHE_HOME pointing inside the user's
// own checkout) must be refused too — os.RemoveAll(worktreeDir) would otherwise delete a real
// subtree of the user's own clone, and the existing "cloneDir inside worktreeDir" check does
// not catch this: it only ever inspects the relationship in the other direction.
func TestWorktreeRefusesADirectoryInsideTheClone(t *testing.T) {
	cloneDir, _ := newTestRepo(t)
	worktreeDir := filepath.Join(cloneDir, "subdir-inside-the-clone")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var g Exec
	if err := g.RemoveWorktree(ctx(), cloneDir, worktreeDir); err == nil {
		t.Fatal("RemoveWorktree(worktreeDir inside cloneDir) succeeded, want a refusal")
	}
	if _, err := os.Stat(worktreeDir); err != nil {
		t.Fatalf("worktreeDir should still exist after the refusal: %v", err)
	}
}
