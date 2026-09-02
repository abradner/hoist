package engine

import (
	"path/filepath"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
)

// assertExactlyOne is the resume property's assertion, shared by every kill-point subtest and
// the concurrency test: after however many partial and repeated Drive calls a scenario makes,
// the fake origin must hold exactly one branch ref for this promotion and the fake forge
// exactly one PR — never two, however many times Drive ran.
func assertExactlyOne(t *testing.T, cloneDir, branch string, g git.Git, f *forge.Fake) (commitSHA string, prNumber int) {
	t.Helper()
	sha, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch)
	if err != nil || !ok {
		t.Fatalf("origin should have exactly one branch %s: ok=%v err=%v", branch, ok, err)
	}
	var matching []forge.PR
	for _, pr := range f.PRs() {
		if pr.HeadBranch == branch {
			matching = append(matching, pr)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("forge should hold exactly one PR for %s, got %d: %+v", branch, len(matching), matching)
	}
	return sha, matching[0].Number
}

// TestResumeAfterKillAtEachPoint is the central resume property (AGENTS.md §4.1, invariant 4,
// "What done means"): a process killed (no cleanup handler — SIGKILL, not SIGTERM) at each of
// the four named points, then restarted from a *freshly built* PromotionState (never the one
// the killed run held in memory — newState is called again from scratch each time, exactly as
// a new process invocation would build it from --repo/--from/--to), must converge on exactly
// one branch, one commit, and one PR — for every kill point.
func TestResumeAfterKillAtEachPoint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		killAfterIdx int // Drive runs steps[:killAfterIdx+1] before "the kill"
	}{
		{"after worktree created, before commit", 0},
		{"after commit, before push", 1},
		{"after push, before PR", 2},
		{"after PR opened, before the state file's final write", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			wt := filepath.Join(t.TempDir(), "wt")
			g := git.Exec{}
			f := &forge.Fake{}
			steps := Steps(g, f, nil)

			// The killed run: only the steps up to and including killAfterIdx get to Act.
			// Drive itself is not asked to save (a real killed process's last save may or may
			// not have landed — invariant 4 says the property must hold either way, so this
			// test never persists between the two Drive calls at all, the strongest form of
			// "even if the state file is deleted").
			s1 := newState(fx, wt)
			if err := Drive(ctx(), steps[:tc.killAfterIdx+1], s1, nil); err != nil {
				t.Fatalf("partial run (simulating the killed process) failed: %v", err)
			}

			var preCommitSHA, prePushedSHA string
			var prePR *forge.PR
			preCommitSHA, prePushedSHA, prePR = s1.CommitSHA, s1.PushedSHA, s1.PR

			// The resumed run: a brand new PromotionState, built the same way a fresh `hoist
			// promote` invocation would, driven through all four steps.
			s2 := newState(fx, wt)
			if err := Drive(ctx(), steps, s2, nil); err != nil {
				t.Fatalf("resumed run failed: %v", err)
			}

			finalCommitSHA, prNumber := assertExactlyOne(t, fx.cloneDir, s2.Branch, g, f)

			if preCommitSHA != "" && preCommitSHA != finalCommitSHA {
				t.Errorf("resume created a second commit: killed run had %s, final is %s", preCommitSHA, finalCommitSHA)
			}
			if prePushedSHA != "" && prePushedSHA != finalCommitSHA {
				t.Errorf("resume pushed a different commit than the killed run had already pushed: %s vs %s", prePushedSHA, finalCommitSHA)
			}
			if prePR != nil && prePR.Number != prNumber {
				t.Errorf("resume opened a second PR: killed run had #%d, final is #%d", prePR.Number, prNumber)
			}
			if s2.CommitSHA != finalCommitSHA || s2.PushedSHA != finalCommitSHA {
				t.Errorf("resumed state inconsistent: CommitSHA=%s PushedSHA=%s final=%s", s2.CommitSHA, s2.PushedSHA, finalCommitSHA)
			}
		})
	}
}

// TestResumeWithoutStateFileRediscoversFromNamesAlone is invariant 4's explicit "if the state
// file is deleted" case, made literal: no PromotionState from the first run is reused or
// consulted at all — id, branch, marker and edits are all re-derived from the plan alone, the
// same way DeriveID/BranchName/Marker do for every call, and the engine still finds the
// already-satisfied steps on the remote.
func TestResumeWithoutStateFileRediscoversFromNamesAlone(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}
	f := &forge.Fake{}
	steps := Steps(g, f, nil)

	first := newState(fx, wt)
	if err := Drive(ctx(), steps, first, nil); err != nil {
		t.Fatal(err)
	}

	// Simulate "the state file was deleted": build state purely from the plan, with no
	// reference whatsoever to `first`.
	second := newState(fx, wt)
	if err := Drive(ctx(), steps, second, nil); err != nil {
		t.Fatal(err)
	}
	if second.CommitSHA != first.CommitSHA || second.PR.Number != first.PR.Number {
		t.Fatalf("rediscovery diverged: first CommitSHA=%s PR=#%d, second CommitSHA=%s PR=#%d",
			first.CommitSHA, first.PR.Number, second.CommitSHA, second.PR.Number)
	}
	assertExactlyOne(t, fx.cloneDir, second.Branch, g, f)
}

// TestConcurrentInvocationConvergesOnOne is the named adversary's "a second, concurrent
// invocation of the exact same promotion started while the first is still mid-flight" case:
// two independently-built PromotionStates for the same id/branch/worktree, driven to
// completion in an interleaved (not necessarily simultaneous — see steps.go and pkg/git's own
// worktree/push idempotency, which is what makes this safe at all) order. Whichever one
// commits first, the other's Observe must recognise that commit as already satisfying the
// plan and never create a second one; whichever pushes first, the other's push must be a
// no-op; whichever opens the PR first, the other's FindPR must discover it.
func TestConcurrentInvocationConvergesOnOne(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}
	f := &forge.Fake{}
	steps := Steps(g, f, nil)

	sa := newState(fx, wt)
	sb := newState(fx, wt)

	// A gets as far as committing...
	if err := Drive(ctx(), steps[:2], sa, nil); err != nil {
		t.Fatalf("A (branch+commit): %v", err)
	}
	// ...then B starts from scratch and runs the whole way through, reusing A's worktree and
	// commit rather than creating its own.
	if err := Drive(ctx(), steps, sb, nil); err != nil {
		t.Fatalf("B (full run): %v", err)
	}
	if sb.CommitSHA != sa.CommitSHA {
		t.Fatalf("B created a different commit than A's: %s vs %s", sb.CommitSHA, sa.CommitSHA)
	}
	// A resumes and finishes; it must land on exactly what B already did.
	if err := Drive(ctx(), steps, sa, nil); err != nil {
		t.Fatalf("A (resumed): %v", err)
	}
	if sa.PR == nil || sb.PR == nil || sa.PR.Number != sb.PR.Number {
		t.Fatalf("A and B ended up with different PRs: %+v vs %+v", sa.PR, sb.PR)
	}
	assertExactlyOne(t, fx.cloneDir, sa.Branch, g, f)
}
