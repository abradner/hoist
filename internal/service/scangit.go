package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/abradner/hoist/pkg/git"
)

// scanGit is the git.Git one read-only scan over many promotion states makes its first pass
// through (FindInFlight, FindInFlightForEnv, List): it lists origin's branches once and fetches
// each base branch once for the whole scan, rather than once per state. Every finished PR-path
// promotion's MergedStep.Observe fetches the same base branch and then asks whether its own
// branch still exists — two connections to the remote per state, so a scan cost grew with every
// promotion an env had ever had (AGENTS.md §9 entry 15: eight finished promotions made `hoist
// promote` spend 105 of its first 108 seconds here, twice over because claimTarget scans before
// and after the claim). Only the git round trips are shared; the forge and the cluster are
// asked per state.
//
// The listing and the fetch are two reads of the remote at two instants, replayed to every state
// that asks, while each state's forge read is live. The steps' Observe methods were written for
// live calls in a fixed order and can misread that: a promotion merged mid-scan looks reverted
// against a base tip fetched before its merge, and a base reset mid-scan leaves a reverted
// promotion looking finished. So nothing a scan learned through a scanGit is used until
// unchanged has asked origin again, at the end, and found every branch the scan asked about —
// and every base tip it fetched — where the snapshot had it. If anything moved, or a pass
// failed, the scan is made again through the live git.Git, as if no snapshot existed.
//
// What that establishes is agreement at two instants, the start and the end of the scan, not
// continuity between them: a ref that moves away and comes back to the same commit inside one
// scan is not seen. The verdicts are then the ones a live walk reaches at the closing listing,
// which is as good an instant as any a live walk would have picked — but it is that, and not
// proof that nothing happened in between.
//
// The cost of the check is one more listing per scan, so a scan over a single state is one
// round trip dearer than it was, and a scan whose snapshot is not confirmed pays for both
// passes, forge and cluster reads included. A failed fetch or listing is kept like any other
// answer: every later state then fails its snapshot pass at once, without another connection,
// and is observed live.
//
// A scanGit lives for one scan: each caller builds its own, so claimTarget's second scan, made
// while holding the claim, asks origin again (AGENTS.md principle 2). It is for observing only.
// It holds the wrapped git.Git rather than embedding it, so every method is written out below:
// the ones that write refuse, and a method added to git.Git later does not compile here until
// someone decides which kind it is.
type scanGit struct {
	inner git.Git

	mu      sync.Mutex
	heads   map[remoteKey]listing
	asked   map[remoteKey]map[string]bool // the branches LsRemoteBranch was asked about
	fetched map[fetchKey]fetchResult
}

var _ git.Git = (*scanGit)(nil)

type remoteKey struct{ dir, remote string }

type fetchKey struct{ dir, remote, branch string }

type listing struct {
	heads map[string]string
	err   error
}

type fetchResult struct {
	sha string
	ok  bool
	err error
}

func newScanGit(g git.Git) *scanGit {
	return &scanGit{inner: g, heads: map[remoteKey]listing{}, asked: map[remoteKey]map[string]bool{}, fetched: map[fetchKey]fetchResult{}}
}

// unchanged asks origin for its branches once more and reports whether every answer this scan
// was given still holds: each branch it asked about is where the listing had it (or still
// absent), and each base tip it fetched is where that branch points now. Branches the scan
// never asked about are not compared — someone else's push is not this scan's business. A scan
// that asked nothing has nothing to confirm. An answer that was a failure, or a listing that
// fails now, confirms nothing.
func (s *scanGit) unchanged(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := map[remoteKey]map[string]string{}
	current := func(k remoteKey) (map[string]string, bool) {
		if heads, ok := now[k]; ok {
			return heads, true
		}
		heads, err := s.inner.LsRemoteHeads(ctx, k.dir, k.remote)
		if err != nil {
			return nil, false
		}
		now[k] = heads
		return heads, true
	}
	for k, was := range s.heads {
		if was.err != nil {
			return false
		}
		heads, ok := current(k)
		if !ok {
			return false
		}
		for branch := range s.asked[k] {
			if heads[branch] != was.heads[branch] {
				return false
			}
		}
	}
	for k, was := range s.fetched {
		if was.err != nil {
			return false
		}
		heads, ok := current(remoteKey{k.dir, k.remote})
		// An absent branch reads as "", which is what a fetch that found none recorded.
		if !ok || heads[k.branch] != was.sha {
			return false
		}
	}
	return true
}

// LsRemoteBranch implements git.Git from this scan's one listing of remote's branches.
func (s *scanGit) LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := remoteKey{cloneDir, remote}
	l, ok := s.heads[k]
	if !ok {
		l.heads, l.err = s.inner.LsRemoteHeads(ctx, cloneDir, remote)
		s.heads[k] = l
	}
	if l.err != nil {
		return "", false, l.err
	}
	if s.asked[k] == nil {
		s.asked[k] = map[string]bool{}
	}
	s.asked[k][branch] = true
	sha, found := l.heads[branch]
	return sha, found, nil
}

// FetchBranch implements git.Git, fetching each branch at most once per scan.
func (s *scanGit) FetchBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := fetchKey{dir, remote, branch}
	r, ok := s.fetched[k]
	if !ok {
		r.sha, r.ok, r.err = s.inner.FetchBranch(ctx, dir, remote, branch)
		if r.err != nil || !r.ok {
			r.sha = ""
		}
		s.fetched[k] = r
	}
	if r.err != nil {
		return "", false, r.err
	}
	return r.sha, r.ok, nil
}

// The rest of git.Git's reads go straight to the wrapped implementation.

func (s *scanGit) LsRemoteHeads(ctx context.Context, cloneDir, remote string) (map[string]string, error) {
	return s.inner.LsRemoteHeads(ctx, cloneDir, remote)
}

func (s *scanGit) LsTreeBlob(ctx context.Context, worktreeDir, rev, path string) (string, bool, error) {
	return s.inner.LsTreeBlob(ctx, worktreeDir, rev, path)
}

func (s *scanGit) CatFile(ctx context.Context, worktreeDir, rev, path string) ([]byte, bool, error) {
	return s.inner.CatFile(ctx, worktreeDir, rev, path)
}

func (s *scanGit) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	return s.inner.IsAncestor(ctx, dir, ancestor, descendant)
}

func (s *scanGit) ObjectExists(ctx context.Context, dir, sha string) (bool, error) {
	return s.inner.ObjectExists(ctx, dir, sha)
}

func (s *scanGit) IsShallow(ctx context.Context, dir string) (bool, error) {
	return s.inner.IsShallow(ctx, dir)
}

func (s *scanGit) RevParse(ctx context.Context, worktreeDir, rev string) (string, bool, error) {
	return s.inner.RevParse(ctx, worktreeDir, rev)
}

func (s *scanGit) HashObject(ctx context.Context, worktreeDir string, content []byte) (string, error) {
	return s.inner.HashObject(ctx, worktreeDir, content)
}

func (s *scanGit) DiffNameOnly(ctx context.Context, worktreeDir, fromRev, toRev string) ([]string, error) {
	return s.inner.DiffNameOnly(ctx, worktreeDir, fromRev, toRev)
}

func (s *scanGit) WorktreeBranch(ctx context.Context, cloneDir, worktreeDir string) (string, bool, error) {
	return s.inner.WorktreeBranch(ctx, cloneDir, worktreeDir)
}

func (s *scanGit) LocalBranchExists(ctx context.Context, cloneDir, branch string) (bool, error) {
	return s.inner.LocalBranchExists(ctx, cloneDir, branch)
}

func (s *scanGit) WorktreeDirty(ctx context.Context, worktreeDir string) (bool, error) {
	return s.inner.WorktreeDirty(ctx, worktreeDir)
}

func (s *scanGit) Log(ctx context.Context, worktreeDir, revRange string) ([]string, error) {
	return s.inner.Log(ctx, worktreeDir, revRange)
}

func (s *scanGit) CommitTime(ctx context.Context, dir, sha string) (time.Time, error) {
	return s.inner.CommitTime(ctx, dir, sha)
}

// errScanGitWrite is what every writing method below returns: a scanGit reaching a step's Act
// is a wiring bug, and failing the Act is the loud way to find out.
var errScanGitWrite = errors.New("service: a scan's git snapshot is for observing only and cannot write")

func (s *scanGit) Worktree(context.Context, string, string, string, string) error {
	return errScanGitWrite
}

func (s *scanGit) WorktreeAtRef(context.Context, string, string, string) error {
	return errScanGitWrite
}

func (s *scanGit) RemoveWorktree(context.Context, string, string) error { return errScanGitWrite }

func (s *scanGit) DeleteLocalBranch(context.Context, string, string) (bool, error) {
	return false, errScanGitWrite
}

func (s *scanGit) DeleteLocalBranchAt(context.Context, string, string, string) (bool, error) {
	return false, errScanGitWrite
}

func (s *scanGit) RemoveCleanWorktree(context.Context, string, string) error { return errScanGitWrite }

func (s *scanGit) Commit(context.Context, string, string, []string, time.Duration, func()) (string, error) {
	return "", errScanGitWrite
}

func (s *scanGit) Push(context.Context, string, string, string) error { return errScanGitWrite }

func (s *scanGit) PushHeadTo(context.Context, string, string, string) error { return errScanGitWrite }

func (s *scanGit) DeleteRemoteBranch(context.Context, string, string, string) error {
	return errScanGitWrite
}
