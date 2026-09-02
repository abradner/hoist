package engine

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
)

// driveToPR drives fx's fixture through the four M3 steps against the fake forge f, returning
// a PromotionState with s.PR/s.CommitSHA/s.PushedSHA populated — the starting point every M4
// step test below needs (CIGreen/Approved/Merged all assume a PR already exists).
func driveToPR(t *testing.T, fx fixture, wt string, f *forge.Fake) *PromotionState {
	t.Helper()
	s := newState(fx, wt)
	if err := Drive(ctx(), Steps(git.Exec{}, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PROpened: %v", err)
	}
	if s.PR == nil {
		t.Fatal("expected a PR after the four M3 steps")
	}
	return s
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// --- CIGreenStep ---------------------------------------------------------

func TestCIGreenSatisfiedWhenChecksSucceed(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 2, Success: 2})

	obs, err := (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied, got %+v", obs)
	}
}

func TestCIGreenWaitingWhilePending(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 2, Pending: 1, Success: 1})

	obs, err := (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied || !obs.Waiting {
		t.Fatalf("expected waiting, got %+v", obs)
	}

	// Checks complete: a later Observe (simulating the CLI's next poll) must now be satisfied.
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 2, Success: 2})
	obs, err = (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied once pending clears, got %+v", obs)
	}
}

func TestCIGreenBlockedOnFailureNamesCheck(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 2, Success: 1, Failure: 1, FailedNames: []string{"unit-tests"}})

	obs, err := (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("expected blocked, got %+v", obs)
	}
	if !strings.Contains(obs.Blocked, "unit-tests") {
		t.Fatalf("blocked reason should name the failed check: %q", obs.Blocked)
	}
}

func TestCIGreenNoneGraceThenGreen(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "green"
	s.CIGrace = time.Minute

	step := CIGreenStep{Forge: f, Now: fixedNow(s.PR.CreatedAt.Add(30 * time.Second))}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Waiting {
		t.Fatalf("within grace with zero checks should be Waiting, got %+v", obs)
	}

	step.Now = fixedNow(s.PR.CreatedAt.Add(2 * time.Minute))
	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("past grace with ci.none=green should satisfy, got %+v", obs)
	}
}

func TestCIGreenNonePromptRequiresOverride(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "prompt"
	s.CIGrace = time.Minute

	step := CIGreenStep{Forge: f, Now: fixedNow(s.PR.CreatedAt.Add(2 * time.Minute))}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("ci.none=prompt past grace without an override should Block, got %+v", obs)
	}

	s.CINoneOverride = true
	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("ci.none=prompt with CINoneOverride should satisfy, got %+v", obs)
	}
}

func TestCIGreenNoneBlockHasNoOverride(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "block"
	s.CIGrace = time.Minute
	s.CINoneOverride = true // must not help — block has no override path at all

	step := CIGreenStep{Forge: f, Now: fixedNow(s.PR.CreatedAt.Add(2 * time.Minute))}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("ci.none=block must stay Blocked even with CINoneOverride set, got %+v", obs)
	}
}

// TestCIGreenChecksErrorIsRetryableNotAbsence is Known bug classes' "a 404 on Checks/Comments
// must be retried, never treated as authoritative absence": Checks returning an error must
// propagate as an error (so the caller retries), never silently read as CheckSummary{} (which
// would eventually satisfy ci.none=green — the exact conflation this test guards against).
func TestCIGreenChecksErrorIsRetryableNotAbsence(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{ChecksErr: errors.New("simulated 404")}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "green"

	_, err := (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err == nil {
		t.Fatal("expected Checks' error to propagate, not be swallowed as zero checks")
	}
}

// --- ApprovedStep ---------------------------------------------------------

func TestApprovedAutoSkipsComment(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "auto"

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("auto approval should satisfy with no comment at all, got %+v", obs)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Comments ") {
			t.Fatalf("auto approval should never even list comments: %v", f.Calls)
		}
	}
}

func TestApprovedRequiresCommentFromApprover(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice"}

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Waiting {
		t.Fatalf("no comment yet should be Waiting, got %+v", obs)
	}

	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})
	obs, err = (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("an approver's exact command should satisfy, got %+v", obs)
	}
}

// TestApprovedIgnoresNonApprover is the named-attacker test AGENTS.md §8 requires for an
// authorization check: a login that is neither in Approvers nor (Collaborators is false here)
// checked against the forge's collaborator API must never satisfy, however correctly it types
// the command.
func TestApprovedIgnoresNonApprover(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice"}
	f.AddComment(s.PR.Number, forge.Comment{Author: "mallory", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("a non-approver's comment must never satisfy: %+v", obs)
	}
}

func TestApprovedIgnoresBotComment(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"some-bot"}
	f.AddComment(s.PR.Number, forge.Comment{Author: "some-bot", AuthorType: "Bot", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("a Bot-typed account must never satisfy even if listed in Approvers: %+v", obs)
	}
}

// TestApprovedIgnoresCommentBeforeHeadCommit is the re-anchor trap named in the brief: a
// comment that predates the PR (and therefore the one commit it could be about) must never
// satisfy, however correctly it types the command.
func TestApprovedIgnoresCommentBeforeHeadCommit(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice"}
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(-time.Hour)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("a comment predating the PR's head commit must never satisfy: %+v", obs)
	}
}

// TestApprovedAnchorsOnCommitTimeNotPRCreatedAt proves the anchor is the head commit's own
// committer date (git.Git.CommitTime), not forge.PR.CreatedAt: it manufactures a PR.CreatedAt
// earlier than the commit's real time (the shape of the bug this repo's other steps happen to
// make unreachable in production, per steps_m4.go's doc comment, but which ApprovedStep must
// not lean on) and shows a comment sitting between the two anchors is ignored, while the same
// comment posted after the real commit time satisfies.
func TestApprovedAnchorsOnCommitTimeNotPRCreatedAt(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice"}

	commitTime, err := (git.Exec{}).CommitTime(ctx(), s.CloneDir, s.CommitSHA)
	if err != nil {
		t.Fatalf("CommitTime: %v", err)
	}
	// Simulate a PR whose CreatedAt is earlier than its own head commit (e.g. it was opened,
	// then the branch was force-pushed to a later commit) — PR.CreatedAt alone is not a safe
	// anchor; only the commit's real time is.
	s.PR.CreatedAt = commitTime.Add(-time.Hour)

	between := commitTime.Add(-30 * time.Minute)
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: between})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("a comment before the commit's real time must not satisfy, even though it is after the (manufactured, earlier) PR.CreatedAt: %+v", obs)
	}

	after := commitTime.Add(time.Minute)
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: after})

	obs, err = (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("a comment after the commit's real time should satisfy: %+v", obs)
	}
}

// TestApprovedRejectAfterApproveWins is the named adversary: a correctly-typed reject after a
// correctly-typed approve, both from allowed authors, must Block — last one wins,
// chronologically, by comment creation time.
func TestApprovedRejectAfterApproveWins(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice", "bob"}
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})
	f.AddComment(s.PR.Number, forge.Comment{Author: "bob", AuthorType: "User", Body: "hoist reject " + s.ID, CreatedAt: s.PR.CreatedAt.Add(2 * time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("a newer reject must win over an older approve, got %+v", obs)
	}
}

// TestApprovedTypoRejectDoesNotBlockRealApproval is the named adversary's other half: a typo'd
// reject ("hoft reject <id>") must not match the exact pattern at all, so a real approve right
// after it must still satisfy.
func TestApprovedTypoRejectDoesNotBlockRealApproval(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice", "bob"}
	f.AddComment(s.PR.Number, forge.Comment{Author: "bob", AuthorType: "User", Body: "hoft reject " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(2 * time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("a typo'd reject must not match, so the real approve should satisfy: %+v", obs)
	}
}

func TestApprovedCollaboratorViaForge(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{Allowed: map[string]bool{"carol": true}}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Collaborators = true
	f.AddComment(s.PR.Number, forge.Comment{Author: "carol", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("a write-collaborator should satisfy when Collaborators is opted in: %+v", obs)
	}
}

// TestApprovedIsAllowedAuthorErrorSurfaces is Known bug classes' "don't silently deny on 403":
// a permission-scope error from IsAllowedAuthor must reach the caller as an error, never fold
// into "not allowed" silently.
func TestApprovedIsAllowedAuthorErrorSurfaces(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{AllowedErr: errors.New("simulated 403: missing repo scope")}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Collaborators = true
	f.AddComment(s.PR.Number, forge.Comment{Author: "carol", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(time.Second)})

	_, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err == nil {
		t.Fatal("expected IsAllowedAuthor's error to surface, not be swallowed")
	}
}

// --- MergedStep ------------------------------------------------------------

func TestMergedRefusesStaleHead(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	// Simulate "something else moved the branch since this promotion last observed it pushed".
	f.SetHeadSHA(s.PR.Number, "someone-elses-sha")

	obs, err := (MergedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("a stale head must Block at Observe, before ever calling MergePR: %+v", obs)
	}

	// Act must also refuse via the forge's own atomic check (never a client-side race) — Fake
	// simulates that by comparing HeadSHA to the expected sha MergePR is given.
	step := MergedStep{Forge: f, Git: git.Exec{}}
	if err := step.Act(ctx(), s); err == nil || !errors.Is(err, forge.ErrStaleHead) {
		t.Fatalf("Act should refuse a stale head via forge.ErrStaleHead, got %v", err)
	}
}

// TestMergedResumesAfterKilledMergeCall is the named adversary: MergePR errors (the client
// never saw a response, but the merge may have landed server-side) — Act must re-check FindPR
// before treating it as a real failure, never re-issue a second merge call blindly once it
// finds the PR already merged.
func TestMergedResumesAfterKilledMergeCall(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)

	// Merge it "for real" once (simulating the merge having landed server-side, e.g. from a
	// process invocation whose own MergePR call this test doesn't model directly)...
	if _, err := f.MergePR(ctx(), s.PR.Number, ""); err != nil {
		t.Fatal(err)
	}
	// ...but s.PR (this process's own copy) still thinks it's unmerged, as it would if the
	// client never saw that call's response. Act's own MergePR call must therefore fail
	// (Fake.MergePR errors on an already-merged PR, mirroring a real 405) — Act must recover via
	// FindPR rather than surface that as a real failure.
	step := MergedStep{Forge: f, Git: git.Exec{}}
	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("Act should recover via FindPR rather than error: %v", err)
	}
	if !s.PR.Merged || s.MergeSHA == "" {
		t.Fatalf("Act should have recovered the merged state: PR=%+v MergeSHA=%q", s.PR, s.MergeSHA)
	}
	calls := 0
	for _, c := range f.Calls {
		if c == "MergePR "+strconv.Itoa(s.PR.Number) {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("expected MergePR called twice (the pre-seeded direct call plus Act's own, which fails and falls back to FindPR), got %d: %v", calls, f.Calls)
	}
}

func TestMergedDeletesBranchAndResumesIfKilledBeforeDelete(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	g := git.Exec{}
	step := MergedStep{Forge: f, Git: g}

	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("merging: %v", err)
	}
	if _, ok, err := g.LsRemoteBranch(ctx(), s.CloneDir, "origin", s.Branch); err != nil || ok {
		t.Fatalf("branch should be deleted after Act: ok=%v err=%v", ok, err)
	}

	// Simulate "killed after merge, before delete landed": recreate the branch out of band and
	// confirm Observe reports not-done (so a resumed Drive would call Act again) rather than
	// declaring victory just because the PR itself is merged.
	runHost(t, s.CloneDir, "push", "origin", s.Branch)
	obs, err := (MergedStep{Forge: f, Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("merged-but-branch-still-there must not report satisfied: %+v", obs)
	}
	if err := (MergedStep{Forge: f, Git: g}).Act(ctx(), s); err != nil {
		t.Fatalf("resumed Act should just delete the branch, not re-merge: %v", err)
	}
	obs, err = (MergedStep{Forge: f, Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied after the branch is actually gone: %+v", obs)
	}
}

// --- resume across CIGreen/Approved via AllSteps ---------------------------

// TestResumeAfterKillDuringCIGreenConvergesWithoutDuplicating is the issue's own "done when",
// made literal for CI: kill (never call Act again — a fresh Drive call standing in for the
// restarted process) while CIGreen is Waiting on a pending check, then the check succeeds, and
// resuming must converge to Merged with exactly one PR/commit, no duplicate action.
func TestResumeAfterKillDuringCIGreenConvergesWithoutDuplicating(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	f := &forge.Fake{}
	g := git.Exec{}
	all := AllSteps(g, f, nil)

	s := newState(fx, wt)
	s.CINone = "green"
	s.CIGrace = time.Minute
	s.Approval = "auto"

	if err := Drive(ctx(), all, s, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("expected ErrWaiting (CI pending), got %v", err)
	}
	if s.Phase != StepCIGreen {
		t.Fatalf("expected to stop at %s, stopped at %s", StepCIGreen, s.Phase)
	}
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 1, Pending: 1})

	// "Kill" — a brand new state built the same way, never reusing s in memory — then resume.
	s2 := newState(fx, wt)
	s2.CINone, s2.CIGrace, s2.Approval = "green", time.Minute, "auto"
	if err := Drive(ctx(), all, s2, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("resumed run should still be waiting on the pending check: %v", err)
	}

	f.SetChecks(s2.PushedSHA, forge.CheckSummary{Total: 1, Success: 1})
	s3 := newState(fx, wt)
	s3.CINone, s3.CIGrace, s3.Approval = "green", time.Minute, "auto"
	if err := Drive(ctx(), all, s3, nil); err != nil {
		t.Fatalf("resumed run should complete once CI is green: %v", err)
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("expected exactly one PR across every attempt, got %d", len(f.PRs()))
	}
	if !f.PRs()[0].Merged {
		t.Fatalf("expected the PR to be merged: %+v", f.PRs()[0])
	}
}

// TestResumeAfterKillDuringApprovalConvergesWithoutDuplicating is the issue's own "done when",
// made literal for approval: kill while Approved is Waiting, post the approval comment after
// the restart, and resuming must converge without duplicating the PR or commit.
func TestResumeAfterKillDuringApprovalConvergesWithoutDuplicating(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	f := &forge.Fake{}
	g := git.Exec{}
	all := AllSteps(g, f, nil)

	s := newState(fx, wt)
	s.CINone, s.CIGrace = "green", time.Nanosecond
	s.Approval = "comment"
	s.Approvers = []string{"alice"}

	if err := Drive(ctx(), all, s, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("expected ErrWaiting (no approval comment yet), got %v", err)
	}
	if s.Phase != StepApproved {
		t.Fatalf("expected to stop at %s, stopped at %s", StepApproved, s.Phase)
	}

	// The approval comment arrives only after the "kill" — posted with a fresh timestamp so it
	// is provably after the PR's head commit either way.
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: time.Now()})

	s2 := newState(fx, wt)
	s2.CINone, s2.CIGrace = "green", time.Nanosecond
	s2.Approval, s2.Approvers = "comment", []string{"alice"}
	if err := Drive(ctx(), all, s2, nil); err != nil {
		t.Fatalf("resumed run should complete once approved: %v", err)
	}
	if len(f.PRs()) != 1 || !f.PRs()[0].Merged {
		t.Fatalf("expected exactly one, merged PR: %+v", f.PRs())
	}
}
