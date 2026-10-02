package service

import (
	"context"
	"errors"
	"maps"
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
// and after the claim). The forge is still asked once per state; only the git round trips are
// shared.
//
// The listing and the fetch are two reads of the remote at two instants, replayed to every state
// that asks, while each state's forge read is live. The steps' Observe methods were written for
// live calls in a fixed order and can misread that: a promotion merged mid-scan looks reverted
// against a base tip fetched before its merge, and a base reset mid-scan leaves a reverted
// promotion looking finished. So nothing a scan learned through a scanGit is used until
// unchanged has asked origin again, at the end, and found every branch where the snapshot had
// it. Then the remote the scan saw is the remote as it stood for the whole scan, and each verdict
// is the one live calls would have reached. If anything moved — or the pass itself failed — the
// scan is made again through the live git.Git, as if no snapshot existed.
//
// A scanGit lives for one scan: each caller builds its own, so claimTarget's second scan, made
// while holding the claim, asks origin again (AGENTS.md principle 2). It is for observing only.
// It holds the wrapped git.Git rather than embedding it, so every method is written out below:
// the ones that write refuse, and a method added to git.Git later does not compile here until
// someone decides which kind it is.
//
// Only successful answers are kept. A failed fetch or listing is asked again by the next state.
type scanGit struct {
	inner git.Git

	mu      sync.Mutex
	heads   map[remoteKey]map[string]string
	fetched map[fetchKey]fetchResult
}

var _ git.Git = (*scanGit)(nil)

type remoteKey struct{ dir, remote string }

type fetchKey struct{ dir, remote, branch string }

type fetchResult struct {
	sha string
	ok  bool
}

func newScanGit(g git.Git) *scanGit {
	return &scanGit{inner: g, heads: map[remoteKey]map[string]string{}, fetched: map[fetchKey]fetchResult{}}
}

// unchanged asks origin for its branches once more and reports whether every answer this scan
// was given still holds: each listing it took is identical now, and each base tip it fetched is
// where that branch still points. A scan that asked nothing has nothing to confirm. A listing
// that fails now confirms nothing, and reads as changed.
func (s *scanGit) unchanged(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := map[remoteKey]map[string]string{}
	listing := func(k remoteKey) (map[string]string, bool) {
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
		heads, ok := listing(k)
		if !ok || !maps.Equal(was, heads) {
			return false
		}
	}
	for k, was := range s.fetched {
		heads, ok := listing(remoteKey{k.dir, k.remote})
		if !ok {
			return false
		}
		if sha, there := heads[k.branch]; there != was.ok || sha != was.sha {
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
	heads, ok := s.heads[k]
	if !ok {
		var err error
		if heads, err = s.inner.LsRemoteHeads(ctx, cloneDir, remote); err != nil {
			return "", false, err
		}
		s.heads[k] = heads
	}
	sha, found := heads[branch]
	return sha, found, nil
}

// FetchBranch implements git.Git, fetching each branch at most once per scan.
func (s *scanGit) FetchBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := fetchKey{dir, remote, branch}
	if r, ok := s.fetched[k]; ok {
		return r.sha, r.ok, nil
	}
	sha, ok, err := s.inner.FetchBranch(ctx, dir, remote, branch)
	if err != nil {
		return "", false, err
	}
	s.fetched[k] = fetchResult{sha, ok}
	return sha, ok, nil
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

func (s *scanGit) Commit(context.Context, string, string, []string, time.Duration, func()) (string, error) {
	return "", errScanGitWrite
}

func (s *scanGit) Push(context.Context, string, string, string) error { return errScanGitWrite }

func (s *scanGit) PushHeadTo(context.Context, string, string, string) error { return errScanGitWrite }

func (s *scanGit) DeleteRemoteBranch(context.Context, string, string, string) error {
	return errScanGitWrite
}
