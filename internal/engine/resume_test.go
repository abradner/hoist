package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
)

// assertExactlyOne is the resume property's assertion, shared by every kill-point subtest and
// the concurrency test: after however many partial and repeated Drive calls a scenario makes,
// the fake origin must hold exactly one branch ref for this promotion, exactly one commit on
// that branch past "main" and the fake forge exactly one PR — never two, however many times
// Drive ran. A ref's tip alone cannot prove "exactly one commit": a regression that produced a
// second commit on the branch (a double-Act race, or a bug reintroducing what Findings A/C
// guard against) would still leave the ref with a single tip, so countCommits below walks the
// actual range rather than trusting LsRemoteBranch's ok=true/sha alone.
func assertExactlyOne(t *testing.T, cloneDir, branch string, g git.Git, f *forge.Fake) (commitSHA string, prNumber int) {
	t.Helper()
	sha, ok, err := g.LsRemoteBranch(ctx(), cloneDir, "origin", branch)
	if err != nil || !ok {
		t.Fatalf("origin should have exactly one branch %s: ok=%v err=%v", branch, ok, err)
	}
	if n := countCommits(t, cloneDir, "main", branch, g); n != 1 {
		t.Fatalf("branch %s should hold exactly one commit past main, got %d", branch, n)
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

// countCommits reports how many commits are in base..branch. Factored out of assertExactlyOne
// so TestCountCommitsCatchesADoubleCommit below can exercise it directly without needing
// assertExactlyOne itself to fail (t.Fatalf can't be caught mid-test).
func countCommits(t *testing.T, cloneDir, base, branch string, g git.Git) int {
	t.Helper()
	shas, err := g.Log(ctx(), cloneDir, base+".."+branch)
	if err != nil {
		t.Fatalf("git log %s..%s: %v", base, branch, err)
	}
	return len(shas)
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

// TestCountCommitsCatchesADoubleCommit is Finding E, made literal: the old assertion (the
// branch's remote ref exists and has a tip) cannot see a regression that produces a second
// commit on the promotion branch — a ref only ever has one tip regardless of how many commits
// sit behind it. countCommits must actually discriminate one commit from two, not just report
// "the ref resolves". This drives a promotion to completion (one real commit), then simulates
// the shape of the regression Finding A/C guard against — a second commit landing on the same
// branch outside Act's own idempotency checks entirely — and confirms countCommits's answer
// changes from 1 to 2.
func TestCountCommitsCatchesADoubleCommit(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}
	f := &forge.Fake{}
	steps := Steps(g, f, nil)

	s := newState(fx, wt)
	if err := Drive(ctx(), steps, s, nil); err != nil {
		t.Fatal(err)
	}
	if n := countCommits(t, fx.cloneDir, "main", s.Branch, g); n != 1 {
		t.Fatalf("a real, correct promotion should produce exactly one commit, got %d", n)
	}

	// Simulate the regression directly against the worktree (standing in for a double-Act
	// race or any bug that reintroduces a second commit on the branch), then push it — the
	// ref still has exactly one tip, which is exactly what the old ref-only assertion checked.
	if err := os.WriteFile(filepath.Join(wt, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runHost(t, wt, "add", "extra.txt")
	runHost(t, wt, "commit", "-q", "-m", "second commit (simulated regression)")
	runHost(t, wt, "push", "-q", "origin", s.Branch)

	if _, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", s.Branch); err != nil || !ok {
		t.Fatalf("sanity check: branch ref should still resolve after the second commit: ok=%v err=%v", ok, err)
	}
	if n := countCommits(t, fx.cloneDir, "main", s.Branch, g); n != 2 {
		t.Fatalf("countCommits should have caught the second commit (want 2, the old ref-only check would have missed this entirely), got %d", n)
	}
}
