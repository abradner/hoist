// Package git drives the git binary — never go-git (AGENTS.md §4.6) — so that a commit made
// on the user's behalf inherits the user's own signing configuration (gpg.format=ssh,
// user.signingkey, 1Password's gpg.ssh.program, includeIf) exactly as if the user had typed
// the command themselves. Every mutating call is scoped to a worktree created under
// $XDG_CACHE_HOME/hoist/worktrees/<id> from the user's own clone: the worktree shares the
// clone's .git directory (and therefore its config and its object store) but has its own
// HEAD, index and working files, so nothing here ever touches the user's own checked-out
// branch, working tree or index.
//
// Git is the seam pkg/forge and internal/engine are tested against without a network call:
// ExecGit is exercised against real temporary git repositories (a bare "origin" and a clone
// of it), never a fake, because the properties this package exists to prove — worktree
// creation and reuse, signed commits, push and remote observation — are properties of the
// git binary's actual behavior, not of a model of it.
package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Git is the seam between internal/engine and the git binary. Every method is scoped to
// either cloneDir (the user's own clone — read and worktree-administration operations only,
// never a write to its checked-out branch or index) or worktreeDir (a linked worktree of
// cloneDir, where all file writes and commits happen).
type Git interface {
	// Worktree creates a linked worktree of cloneDir at worktreeDir, checked out on branch,
	// creating branch from base if it does not already exist locally. It is idempotent: a
	// worktreeDir already registered on branch is left exactly as it is (never reset, so a
	// commit made by an earlier, killed run of the same promotion is never discarded); a
	// worktreeDir that exists on disk but is not a registered worktree (a stale directory
	// left by a run killed before `git worktree add` finished its own bookkeeping) is
	// cleaned up and recreated, never silently rm -rf'd without that check.
	Worktree(ctx context.Context, cloneDir, worktreeDir, branch, base string) error
	// RemoveWorktree removes the linked worktree at worktreeDir. Callers must not call this
	// while any process still has worktreeDir as its current directory (AGENTS.md known bug
	// class: deleting a worktree out from under a spawned git process races and can fail in
	// platform-specific ways) — remove only after every child using it has exited. Removing
	// an already-absent worktree is not an error.
	RemoveWorktree(ctx context.Context, cloneDir, worktreeDir string) error
	// LsRemoteBranch reports origin's current tip of branch, ok=false when the ref does not
	// exist there. This is the one source of truth Observe uses for "has this been pushed"
	// — never a locally cached belief.
	LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (sha string, ok bool, err error)
	// LsTreeBlob reports the blob hash of path in rev's tree, ok=false when rev cannot be
	// resolved (nothing committed yet) or path is not present in it.
	LsTreeBlob(ctx context.Context, worktreeDir, rev, path string) (blob string, ok bool, err error)
	// RevParse resolves rev to a commit sha inside worktreeDir, ok=false when rev cannot be
	// resolved (e.g. HEAD before any commit exists). Added beyond the brief's listed shape:
	// Observe needs to read the worktree's current HEAD without creating a commit to find
	// out, and ls-tree alone reports a blob's content, never the commit id that holds it.
	RevParse(ctx context.Context, worktreeDir, rev string) (sha string, ok bool, err error)
	// Commit stages exactly paths (relative to worktreeDir) and commits message, running
	// through the worktree's inherited signing configuration. It runs under timeout; if the
	// commit has not returned after 5s, onWaiting is called exactly once (this is the
	// interactive 1Password SSH-sign approval, not a hang the caller should give up on). A
	// timeout is reported as an error satisfying errors.Is(err, ErrTimeout) — a retryable
	// failure, never a crash — and never leaves a stale index.lock behind for the next
	// attempt to trip over.
	Commit(ctx context.Context, worktreeDir, message string, paths []string, timeout time.Duration, onWaiting func()) (sha string, err error)
	// Push pushes worktreeDir's branch to remote/branch. A rejected non-fast-forward push
	// (something else moved the branch) is reported as an error distinguishable by the
	// caller re-querying LsRemoteBranch — Push itself never retries with --force.
	Push(ctx context.Context, worktreeDir, remote, branch string) error
	// HashObject returns the git blob hash content would have, without requiring it to be
	// committed or even written to the object database. Used to precompute ExpectedBlobs
	// from a planned edit's "after" bytes before any commit exists.
	HashObject(ctx context.Context, worktreeDir string, content []byte) (blob string, err error)
	// DiffNameOnly lists the repo-relative paths that differ between fromRev and toRev,
	// exactly as `git diff --name-only` reports them. Used to confirm a commit changed
	// exactly the planned paths and nothing more (AGENTS.md §4.2: "a one-line-per-occurrence
	// diff is the whole review surface for a production change") — a pre-commit hook or a
	// pre-existing staged change riding along in the same commit must be caught here, not
	// discovered only once the branch is pushed.
	DiffNameOnly(ctx context.Context, worktreeDir, fromRev, toRev string) (paths []string, err error)
	// WorktreeBranch reports whether worktreeDir is registered as a linked worktree of
	// cloneDir, and if so, which branch it is checked out on. ok=false means it is not
	// currently registered — absent, or a stale directory git's own registry does not know
	// about — which callers must treat identically to "not set up yet". This is the only
	// trustworthy way to answer "is this worktree actually mine, on the right branch": the
	// mere presence of a ".git" file at worktreeDir proves nothing about which clone or
	// branch it resolves to (a stale pointer from an unrelated prior state, or one that
	// happens to resolve into cloneDir's own real git dir but on a different branch).
	WorktreeBranch(ctx context.Context, cloneDir, worktreeDir string) (branch string, ok bool, err error)
}

// ErrTimeout marks a Commit that did not return within its timeout — most often the
// interactive 1Password SSH-sign approval never arriving. It is retryable: the caller should
// leave the worktree alone (no half-applied commit is possible; see Commit's doc comment) and
// try again, rather than treating this as a crash.
var ErrTimeout = errors.New("git: timed out")

// signingWaitAfter is how long Commit waits before telling the caller it is waiting on
// something interactive (AGENTS.md §4.6): "waiting for signing approval" after 5s.
const signingWaitAfter = 5 * time.Second

// Exec is the real Git, implemented by shelling out to the git binary found on PATH.
type Exec struct {
	// Bin overrides the git binary; "" means "git" from PATH. Tests never need this — HOME
	// and GIT_CONFIG_GLOBAL isolate the git config instead — but it exists for a caller that
	// wants an explicit path.
	Bin string
}

func (e Exec) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return "git"
}

// run executes git with args in dir, returning combined stdout (stderr is folded into the
// returned error, never lost).
func (e Exec) run(ctx context.Context, dir string, args ...string) (string, error) {
	stdout, _, err := e.runRaw(ctx, dir, args...)
	return stdout, err
}

// runRaw is run's underlying primitive: it also reports the exit code (-1 when the process
// never started at all) so a caller can tell "ran and exited nonzero with no output" — e.g.
// `show-ref` reporting "not found" — from a real invocation failure.
func (e Exec) runRaw(ctx context.Context, dir string, args ...string) (stdout string, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, e.bin(), args...)
	cmd.Dir = dir
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return out.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return out.String(), exitErr.ExitCode(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), runErr, strings.TrimSpace(stderr.String()))
	}
	return out.String(), -1, fmt.Errorf("git %s: %w", strings.Join(args, " "), runErr)
}

// Worktree implements Git.
func (e Exec) Worktree(ctx context.Context, cloneDir, worktreeDir, branch, base string) error {
	if err := guardDisposablePath(cloneDir, worktreeDir); err != nil {
		return err
	}
	entries, err := e.listWorktrees(ctx, cloneDir)
	if err != nil {
		return fmt.Errorf("git worktree list: %w", err)
	}
	if entry, ok := findWorktree(entries, worktreeDir); ok {
		if entry.branch == branch {
			return nil // already set up for this promotion — reuse, never reset.
		}
		if err := e.RemoveWorktree(ctx, cloneDir, worktreeDir); err != nil {
			return fmt.Errorf("worktree %s is registered on %q, not %q, and could not be removed to retry: %w", worktreeDir, entry.branch, branch, err)
		}
	} else if _, statErr := os.Stat(worktreeDir); statErr == nil {
		// Present on disk but not in git's own registry: a stale directory from a run killed
		// before `git worktree add` finished its bookkeeping. Try git's own cleanup first
		// (it may still hold administrative files under .git/worktrees/), then remove the
		// directory itself. Never rm -rf without having checked the registry above first —
		// and guardDisposablePath above already refused if worktreeDir could reach cloneDir.
		_ = e.RemoveWorktree(ctx, cloneDir, worktreeDir)
		if err := os.RemoveAll(worktreeDir); err != nil {
			return fmt.Errorf("removing stale worktree directory %s: %w", worktreeDir, err)
		}
	}

	branchExists, err := e.localBranchExists(ctx, cloneDir, branch)
	if err != nil {
		return err
	}
	var args []string
	if branchExists {
		args = []string{"worktree", "add", worktreeDir, branch}
	} else {
		args = []string{"worktree", "add", "-b", branch, worktreeDir, base}
	}
	if _, err := e.run(ctx, cloneDir, args...); err != nil {
		// A concurrent invocation of the exact same promotion may have won the race to
		// create this exact worktree/branch between our check above and this call. Re-check
		// the registry once before treating this as a real failure.
		if entries2, lerr := e.listWorktrees(ctx, cloneDir); lerr == nil {
			if entry, ok := findWorktree(entries2, worktreeDir); ok && entry.branch == branch {
				return nil
			}
		}
		return err
	}
	return nil
}

// RemoveWorktree implements Git. Removing an absent worktree is treated as success: git
// itself errors on a path it doesn't recognise, which here just means there is nothing to
// clean up.
func (e Exec) RemoveWorktree(ctx context.Context, cloneDir, worktreeDir string) error {
	if err := guardDisposablePath(cloneDir, worktreeDir); err != nil {
		return err
	}
	if _, err := e.run(ctx, cloneDir, "worktree", "remove", "--force", worktreeDir); err != nil {
		if strings.Contains(err.Error(), "is not a working tree") || strings.Contains(err.Error(), "not a valid path") {
			return nil
		}
		if _, statErr := os.Stat(worktreeDir); os.IsNotExist(statErr) {
			return nil
		}
		return err
	}
	return nil
}

type worktreeEntry struct {
	path, branch string
}

// listWorktrees parses `git worktree list --porcelain`.
func (e Exec) listWorktrees(ctx context.Context, cloneDir string) ([]worktreeEntry, error) {
	out, err := e.run(ctx, cloneDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []worktreeEntry
	var cur worktreeEntry
	flush := func() {
		if cur.path != "" {
			entries = append(entries, cur)
		}
		cur = worktreeEntry{}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return entries, sc.Err()
}

// findWorktree looks up path among entries, comparing resolved (symlink-free) paths so that
// a platform where the OS temp dir is itself a symlink (macOS: /tmp -> /private/tmp) still
// matches correctly.
func findWorktree(entries []worktreeEntry, path string) (worktreeEntry, bool) {
	want := resolvePath(path)
	for _, e := range entries {
		if resolvePath(e.path) == want {
			return e, true
		}
	}
	return worktreeEntry{}, false
}

// guardDisposablePath refuses any operation on worktreeDir that could reach cloneDir —
// the user's own clone — whether by removing it directly or by removing a directory that
// contains it. Every caller in this package is only ever handed a path under a cache
// directory this milestone's own code generated (pkg/git has no such concept itself, so it
// cannot check "is this the cache dir" — it can only check "is this NOT the clone"), but
// RemoveWorktree ends in an unconditional os.RemoveAll, and nothing in this file's own
// signatures stops a future caller from passing the wrong path. This is the one place that
// makes that mistake fail loudly instead of deleting the user's real checkout.
func guardDisposablePath(cloneDir, worktreeDir string) error {
	wt := resolvePath(worktreeDir)
	cd := resolvePath(cloneDir)
	if wt == "" || wt == string(filepath.Separator) {
		return fmt.Errorf("refusing to operate on %q: not a real path", worktreeDir)
	}
	if wt == cd {
		return fmt.Errorf("refusing to remove %q: it is the clone directory, not a disposable worktree", worktreeDir)
	}
	if rel, err := filepath.Rel(wt, cd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove %q: the clone directory %q is inside it", worktreeDir, cloneDir)
	}
	return nil
}

func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func (e Exec) localBranchExists(ctx context.Context, cloneDir, branch string) (bool, error) {
	_, exitCode, err := e.runRaw(ctx, cloneDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	switch {
	case err == nil:
		return true, nil
	case exitCode == 1:
		// `show-ref --quiet` exits 1 with no output when the ref simply does not exist —
		// that is "not found", not a failure worth surfacing.
		return false, nil
	default:
		return false, err
	}
}

// LsRemoteBranch implements Git.
func (e Exec) LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (string, bool, error) {
	out, err := e.run(ctx, cloneDir, "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil
	}
	fields := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	if len(fields) == 0 {
		return "", false, fmt.Errorf("git ls-remote: unparseable output %q", out)
	}
	return fields[0], true, nil
}

// LsTreeBlob implements Git.
func (e Exec) LsTreeBlob(ctx context.Context, worktreeDir, rev, path string) (string, bool, error) {
	out, err := e.run(ctx, worktreeDir, "ls-tree", rev, "--", path)
	if err != nil {
		if isMissingRevision(err) {
			return "", false, nil
		}
		return "", false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil
	}
	// "<mode> <type> <hash>\t<path>"
	tab := strings.IndexByte(out, '\t')
	if tab < 0 {
		return "", false, fmt.Errorf("git ls-tree: unparseable output %q", out)
	}
	fields := strings.Fields(out[:tab])
	if len(fields) != 3 {
		return "", false, fmt.Errorf("git ls-tree: unparseable output %q", out)
	}
	return fields[2], true, nil
}

// RevParse implements Git.
func (e Exec) RevParse(ctx context.Context, worktreeDir, rev string) (string, bool, error) {
	out, exitCode, err := e.runRaw(ctx, worktreeDir, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		// `--quiet` suppresses rev-parse's own error text and just exits 1 when rev cannot
		// be resolved (nothing committed yet, or a name that names nothing) — that is "not
		// found", not a failure worth surfacing.
		if exitCode == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil
	}
	return out, true, nil
}

func isMissingRevision(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Not a valid object name") ||
		strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "ambiguous argument")
}

// Commit implements Git.
func (e Exec) Commit(ctx context.Context, worktreeDir, message string, paths []string, timeout time.Duration, onWaiting func()) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("git commit: no paths to stage")
	}
	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := e.run(ctx, worktreeDir, addArgs...); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	msgFile, err := os.CreateTemp("", "hoist-commit-msg-*.txt")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(msgFile.Name()) }()
	if _, err := msgFile.WriteString(message); err != nil {
		_ = msgFile.Close()
		return "", err
	}
	if err := msgFile.Close(); err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.bin(), "-C", worktreeDir, "commit", "-F", msgFile.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// exec.CommandContext's default cancellation only signals cmd.Process itself — a signing
	// helper (1Password's SSH-sign helper, or anything else the commit spawns) that forks its
	// own child can outlive the context deadline entirely, and cmd.Wait can then block far
	// past timeout waiting for a stdio pipe a surviving grandchild still holds open. Put the
	// spawned process in its own group and, on cancellation, kill the whole group (negative
	// pid) rather than just cmd.Process. WaitDelay is belt-and-suspenders per exec.Cmd's own
	// documentation for this exact pattern: it bounds how long Wait keeps waiting for stdio to
	// close after the process is gone, so a leak the group-kill somehow misses still can't hang
	// the caller.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("git commit: starting: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var once sync.Once
	warn := time.NewTimer(signingWaitAfter)
	defer warn.Stop()
	var cmdErr error
loop:
	for {
		select {
		case cmdErr = <-waitCh:
			break loop
		case <-warn.C:
			if onWaiting != nil {
				once.Do(onWaiting)
			}
		}
	}

	if cmdErr != nil {
		removeIndexLock(e, worktreeDir)
		if cctx.Err() != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%w: commit did not complete within %s (likely waiting on interactive signing approval)", ErrTimeout, timeout)
		}
		return "", fmt.Errorf("git commit: %w: %s", cmdErr, strings.TrimSpace(stderr.String()))
	}

	sha, ok, err := e.RevParse(context.WithoutCancel(ctx), worktreeDir, "HEAD")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("git commit: succeeded but HEAD could not be resolved")
	}
	return sha, nil
}

// removeIndexLock clears a leftover index.lock after a killed or timed-out commit, so the
// worktree Observe re-inspects next time is not blocked by a lock nothing will ever release
// (AGENTS.md invariant: no half-applied commit, and no half-applied *lock* either).
// Best-effort: an inability to locate or remove it is not itself a fatal error, since the
// commit's own failure is already being reported.
func removeIndexLock(e Exec, worktreeDir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := e.run(ctx, worktreeDir, "rev-parse", "--git-path", "index.lock")
	if err != nil {
		return
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(worktreeDir, p)
	}
	_ = os.Remove(p)
}

// Push implements Git.
func (e Exec) Push(ctx context.Context, worktreeDir, remote, branch string) error {
	ref := "refs/heads/" + branch + ":refs/heads/" + branch
	_, err := e.run(ctx, worktreeDir, "push", remote, ref)
	return err
}

// HashObject implements Git.
func (e Exec) HashObject(ctx context.Context, worktreeDir string, content []byte) (string, error) {
	cmd := exec.CommandContext(ctx, e.bin(), "-C", worktreeDir, "hash-object", "--stdin")
	cmd.Stdin = bytes.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// DiffNameOnly implements Git.
func (e Exec) DiffNameOnly(ctx context.Context, worktreeDir, fromRev, toRev string) ([]string, error) {
	out, err := e.run(ctx, worktreeDir, "diff", "--name-only", fromRev, toRev)
	if err != nil {
		return nil, err
	}
	var paths []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, sc.Err()
}

// WorktreeBranch implements Git.
func (e Exec) WorktreeBranch(ctx context.Context, cloneDir, worktreeDir string) (string, bool, error) {
	entries, err := e.listWorktrees(ctx, cloneDir)
	if err != nil {
		return "", false, fmt.Errorf("git worktree list: %w", err)
	}
	entry, ok := findWorktree(entries, worktreeDir)
	if !ok {
		return "", false, nil
	}
	return entry.branch, true, nil
}
