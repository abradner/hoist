package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

func TestBranchedStepCreatesThenObservesSatisfied(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	step := BranchedStep{Git: git.Exec{}}

	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatal("should not be satisfied before the worktree exists")
	}
	if err := step.Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatal("should be satisfied once the worktree exists")
	}
}

func TestCommittedStepAppliesAndCommitsThenIsIdempotent(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	g := git.Exec{}
	if err := (BranchedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}

	step := CommittedStep{Git: g}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatal("should not be satisfied before any commit exists")
	}
	if len(s.ExpectedBlobs) == 0 {
		t.Fatal("Observe should have precomputed ExpectedBlobs")
	}
	if err := step.Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	firstSHA := s.CommitSHA
	if firstSHA == "" {
		t.Fatal("Act did not set CommitSHA")
	}

	// Re-observe: the exact same commit must be reported satisfied, never recreated.
	s.CommitSHA = "" // simulate a fresh process that hasn't rev-parsed yet
	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("should be satisfied once HEAD matches: %+v", obs)
	}
	if s.CommitSHA != firstSHA {
		t.Fatalf("CommitSHA changed on re-observe: %s vs %s", s.CommitSHA, firstSHA)
	}
}

// TestCommittedStepActResumesAfterApplyWithoutCommit reproduces a process killed after
// gitops.Apply wrote the "after" bytes to the worktree but before git commit returned
// (AGENTS.md §4.6: 1Password's SSH-sign approval can hang or the process can just die).
// gitops.Apply is intentionally non-idempotent, so a naive resume that unconditionally
// re-calls Apply fails with "the file changed after the plan was built" — this asserts the
// resumed run instead recognises the file is already at its expected content and proceeds
// straight to commit.
func TestCommittedStepActResumesAfterApplyWithoutCommit(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := git.Exec{}

	// The killed run: worktree created, Apply already ran directly against it (standing in
	// for CommittedStep.Act having reached that point before dying), but nothing was ever
	// committed.
	killed := newState(fx, wt)
	if err := (BranchedStep{Git: g}).Act(ctx(), killed); err != nil {
		t.Fatal(err)
	}
	changed, err := gitops.Apply(killed.WorktreeDir, killed.Edits)
	if err != nil {
		t.Fatalf("simulating the killed run's Apply: %v", err)
	}
	if len(changed) == 0 {
		t.Fatal("fixture plan should produce a real edit for Apply to have made")
	}
	sha, ok, err := g.RevParse(ctx(), killed.WorktreeDir, "HEAD")
	if err != nil || !ok {
		t.Fatalf("worktree should still have its branched HEAD: ok=%v err=%v", ok, err)
	}
	_ = sha // no commit exists yet; this is just confirming the worktree itself is usable

	// The resumed run: a brand-new PromotionState, exactly as a fresh `hoist promote`
	// invocation would build it, driven through Committed's Observe then Act.
	resumed := newState(fx, wt)
	step := CommittedStep{Git: g}
	obs, err := step.Observe(ctx(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatal("should not be satisfied before any commit exists, even though Apply already ran")
	}
	if err := step.Act(ctx(), resumed); err != nil {
		t.Fatalf("resume after Apply-without-commit must succeed, got: %v", err)
	}
	if resumed.CommitSHA == "" {
		t.Fatal("Act did not set CommitSHA")
	}

	obs, err = step.Observe(ctx(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected Committed satisfied after resume, got %+v", obs)
	}
}

func TestPushedStepBlocksOnGenuineConflict(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	g := git.Exec{}
	if err := (BranchedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if err := (CommittedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}

	// Someone/something else pushes a different, unrelated commit to the exact same branch
	// name on origin before we get to push ours.
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "checkout", "-q", "-b", s.Branch)
	runHost(t, other, "commit", "-q", "--allow-empty", "-m", "someone else entirely")
	runHost(t, other, "push", "-q", "origin", s.Branch)

	step := PushedStep{Git: g}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("expected Blocked, got %+v", obs)
	}
}

func TestPROpenedStepFindsByMarkerAcrossBranchRename(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	f := &forge.Fake{}
	step := PROpenedStep{Forge: f}

	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatal("should not be satisfied before a PR exists")
	}
	if err := step.Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if s.PR == nil {
		t.Fatal("Act did not set PR")
	}
	firstNumber := s.PR.Number

	// A fresh state (branch renamed/unknown) must still find the same PR by its body marker.
	s2 := newState(fx, wt)
	s2.Branch = "totally-different-branch-name"
	obs, err = step.Observe(ctx(), s2)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied || s2.PR == nil || s2.PR.Number != firstNumber {
		t.Fatalf("expected to find PR #%d by marker, got %+v", firstNumber, obs)
	}
}

func TestPushedStepRetryableNetworkBlip(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	g := git.Exec{}
	if err := (BranchedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if err := (CommittedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}

	flaky := &blipGit{Git: g, failsLeft: 1}
	step := PushedStep{Git: flaky}
	if err := step.Act(ctx(), s); err == nil {
		t.Fatal("expected the first push to fail")
	}
	if s.PushedSHA != "" {
		t.Fatal("PushedSHA must not be set after a failed push")
	}
	// Retry: the same Act call, no --force, must now succeed.
	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	remoteSHA, ok, err := g.LsRemoteBranch(ctx(), fx.cloneDir, "origin", s.Branch)
	if err != nil || !ok || remoteSHA != s.CommitSHA {
		t.Fatalf("origin branch = %s ok=%v err=%v, want %s", remoteSHA, ok, err, s.CommitSHA)
	}
}

// blipGit wraps a real git.Git, failing Push exactly failsLeft times with a plain (non-
// timeout, non-conflict) error before delegating — a stand-in for a transient network
// failure, distinct from the signing-timeout and genuine-conflict cases covered elsewhere.
type blipGit struct {
	git.Git
	failsLeft int
}

func (b *blipGit) Push(ctx context.Context, worktreeDir, remote, branch string) error {
	if b.failsLeft > 0 {
		b.failsLeft--
		return errors.New("simulated network blip: connection reset")
	}
	return b.Git.Push(ctx, worktreeDir, remote, branch)
}
