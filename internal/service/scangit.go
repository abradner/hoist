package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/abradner/hoist/internal/engine"
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
// that asks — so a state observed late in the scan can see a listing or a base tip older than
// its own forge read. The steps' Observe methods were written for live calls in a fixed order
// and can misread that: a promotion merged mid-scan looks reverted against a base tip fetched
// before its merge. observeThroughScan is therefore the only way a scanGit is used, and it
// accepts exactly one answer from this pass — "finished". Anything else is observed again
// through the live git.Git.
//
// A scanGit lives for one scan: each caller builds its own with newScanGit, so claimTarget's
// second scan, made while holding the claim, asks origin again (AGENTS.md principle 2). It is
// for observing only, and refuses every method that writes — a step that Acts must see its own
// effect on the next Observe, which a snapshot cannot show it.
//
// Only successful answers are kept. A failed fetch or listing is asked again by the next state.
type scanGit struct {
	git.Git

	mu      sync.Mutex
	heads   map[remoteKey]map[string]string
	fetched map[fetchKey]fetchResult
}

type remoteKey struct{ dir, remote string }

type fetchKey struct{ dir, remote, branch string }

type fetchResult struct {
	sha string
	ok  bool
}

func newScanGit(g git.Git) *scanGit {
	return &scanGit{Git: g, heads: map[remoteKey]map[string]string{}, fetched: map[fetchKey]fetchResult{}}
}

// observeThroughScan observes one state for a scan: first through scan's shared snapshot, on a
// copy of st, and — unless that pass finds the promotion finished — again through live, on st
// itself, exactly as if no snapshot existed. So the snapshot can only ever make a finished
// promotion cheap to recognise; every in-flight, blocked or failed verdict a scan reports was
// reached by live calls in the order the steps expect.
//
// The copy is what keeps a discarded first pass from leaking into the second: Observe records
// what it found on the state (PR, MergeSHA, PushedSHA), always by assigning a field, never by
// writing through a slice or map the copy shares.
func observeThroughScan[T any](st *engine.PromotionState, scan *scanGit, live git.Git, observe func(g git.Git, st *engine.PromotionState) (bool, T, error)) (bool, T, error) {
	probe := *st
	if done, out, err := observe(scan, &probe); err == nil && done {
		*st = probe
		return true, out, nil
	}
	return observe(live, st)
}

// LsRemoteBranch implements git.Git from this scan's one listing of remote's branches.
func (s *scanGit) LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := remoteKey{cloneDir, remote}
	heads, ok := s.heads[k]
	if !ok {
		var err error
		if heads, err = s.LsRemoteHeads(ctx, cloneDir, remote); err != nil {
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
	sha, ok, err := s.Git.FetchBranch(ctx, dir, remote, branch)
	if err != nil {
		return "", false, err
	}
	s.fetched[k] = fetchResult{sha, ok}
	return sha, ok, nil
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
