package engine

import (
	"errors"
	"os"
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

// TestCIGreenSkippedRequiredCheckNeverSatisfies is the P1 regression for "a skipped check-run
// wrongly counts as green": forge.CheckSummary carries no required-vs-optional distinction, so a
// path-filtered or otherwise-skipped check-run set alongside otherwise-green checks must Block,
// never silently satisfy CIGreenStep — a required check that never ran is exactly the "the
// runbook blocks" exception to AGENTS.md §2 principle 5, not a pass. Mutant-verified: reverting
// CIGreenStep.Observe to fold Skipped into the old total>0&&pending==0&&failure==0 condition (as
// it did before this fix, when Checks itself counted "skipped" conclusions as Success) makes this
// test fail.
func TestCIGreenSkippedRequiredCheckNeverSatisfies(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	f.SetChecks(s.PushedSHA, forge.CheckSummary{Total: 2, Success: 1, Skipped: 1, SkippedNames: []string{"required-integration-test"}})

	obs, err := (CIGreenStep{Forge: f}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("a skipped check-run alongside otherwise-green checks must never satisfy CIGreen, got %+v", obs)
	}
	if obs.Blocked == "" {
		t.Fatalf("a skipped required check should Block (this repo's own runbook-blocks exception), got %+v", obs)
	}
	if !strings.Contains(obs.Blocked, "required-integration-test") {
		t.Fatalf("blocked reason should name the skipped check: %q", obs.Blocked)
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

// TestApprovedExactTimestampTieRejectHasLargerIDWins is the P2 regression for finding #4: an
// approve and a later reject that happen to share the exact same recorded CreatedAt (GitHub's
// comment timestamp precision can collide) must not silently resolve to "approved" just because
// a strict CreatedAt.After comparison can never see the reject as newer. Comment.ID (assigned in
// strictly increasing creation order) is the tiebreaker — the reject here has the larger ID,
// meaning it was truly posted after the approve despite the identical timestamp, and must win.
func TestApprovedExactTimestampTieRejectHasLargerIDWins(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice", "bob"}
	tie := s.PR.CreatedAt.Add(time.Second)
	f.AddComment(s.PR.Number, forge.Comment{ID: 10, Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: tie})
	f.AddComment(s.PR.Number, forge.Comment{ID: 11, Author: "bob", AuthorType: "User", Body: "hoist reject " + s.ID, CreatedAt: tie})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("on an exact CreatedAt tie, the reject with the larger comment ID must win, got %+v", obs)
	}
}

// TestApprovedExactTimestampTieApproveHasLargerIDWins is the mirror direction: same exact
// CreatedAt tie, but this time the approve carries the larger ID (posted after the reject in
// real creation order) — the approve must win, proving the tiebreak isn't just "reject always
// wins ties" but genuinely follows Comment.ID.
func TestApprovedExactTimestampTieApproveHasLargerIDWins(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Approvers = []string{"alice", "bob"}
	tie := s.PR.CreatedAt.Add(time.Second)
	f.AddComment(s.PR.Number, forge.Comment{ID: 20, Author: "bob", AuthorType: "User", Body: "hoist reject " + s.ID, CreatedAt: tie})
	f.AddComment(s.PR.Number, forge.Comment{ID: 21, Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: tie})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("on an exact CreatedAt tie, the approve with the larger comment ID must win, got %+v", obs)
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

// TestApprovedMemoizesIsAllowedAuthorPerAuthor is the minor perf finding's regression: a PR with
// several comments from the same collaborator must call Forge.IsAllowedAuthor at most once for
// that login per Observe call, not once per comment — avoidable rate-limit pressure otherwise.
func TestApprovedMemoizesIsAllowedAuthorPerAuthor(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{Allowed: map[string]bool{"carol": true}}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.Approval = "comment"
	s.Collaborators = true
	// Three comments from carol: two noise, one the real approval — all must still be checked
	// against the same single IsAllowedAuthor result.
	f.AddComment(s.PR.Number, forge.Comment{Author: "carol", AuthorType: "User", Body: "looking at this", CreatedAt: s.PR.CreatedAt.Add(time.Second)})
	f.AddComment(s.PR.Number, forge.Comment{Author: "carol", AuthorType: "User", Body: "still reviewing", CreatedAt: s.PR.CreatedAt.Add(2 * time.Second)})
	f.AddComment(s.PR.Number, forge.Comment{Author: "carol", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: s.PR.CreatedAt.Add(3 * time.Second)})

	obs, err := (ApprovedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied once carol's real approval is reached, got %+v", obs)
	}
	calls := 0
	for _, c := range f.Calls {
		if c == "IsAllowedAuthor carol" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("expected IsAllowedAuthor called exactly once for carol across 3 comments, got %d: %v", calls, f.Calls)
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

// TestMergedRefusesPRWithWrongBase is MergedStep's own belt-and-suspenders half of finding #2's
// regression (PROpenedStep's own check is TestPROpenedStepRefusesPRWithWrongBase in
// steps_test.go): a PR found by head branch name that targets a different base than s.Base must
// never be treated as this promotion's own, however it got here — simulated here by never
// driving through PROpenedStep at all, so MergedStep's own check is exercised in isolation.
func TestMergedRefusesPRWithWrongBase(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	s := newState(fx, wt)
	f := &forge.Fake{}

	if _, err := f.CreatePR(ctx(), forge.PRSpec{Title: "x", Body: "x", Head: s.Branch, Base: "not-" + s.Base}); err != nil {
		t.Fatal(err)
	}

	obs, err := (MergedStep{Forge: f, Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" {
		t.Fatalf("expected Blocked for a PR targeting a different base, got %+v", obs)
	}
	if !strings.Contains(obs.Blocked, s.Base) || !strings.Contains(obs.Blocked, "not-"+s.Base) {
		t.Fatalf("Blocked reason should name both bases, got: %s", obs.Blocked)
	}
	if s.PR != nil {
		t.Fatalf("a wrong-base PR must never be adopted onto s.PR, got %+v", s.PR)
	}
}

// TestMergedRefusesSuccessfulMergeIntoRetargetedBase is round-6's regression: the recovery path
// for a LOST MergePR response already re-checks the freshly observed PR's base before accepting
// it as this promotion's own success (TestMergedRefusesPRWithWrongBase's sibling, one call
// earlier) — but the ordinary SUCCESS path (MergePR returns a PR with no error at all) accepted
// it unconditionally. If another actor retargeted the PR between this step's own Observe (which
// validated the base) and this Act call, GitHub's atomic "merge iff head is X" guard can still
// succeed — the head is unchanged, only the base moved — merging this promotion's content into a
// base hoist was never configured to touch, and (before this fix) Act would proceed to delete
// the branch and report success anyway.
func TestMergedRefusesSuccessfulMergeIntoRetargetedBase(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	step := MergedStep{Forge: f, Git: git.Exec{}}

	// Simulate another actor retargeting the PR (e.g. via the GitHub UI) after this promotion's
	// own Observe already validated its base, but before this Act call reaches MergePR.
	f.SetBase(s.PR.Number, "not-"+s.Base)

	err := step.Act(ctx(), s)
	if err == nil {
		t.Fatal("expected an error when the merge succeeds into a retargeted base, got nil")
	}
	if !strings.Contains(err.Error(), s.Base) || !strings.Contains(err.Error(), "not-"+s.Base) {
		t.Fatalf("error should name both the configured and the actual base, got: %v", err)
	}
	if s.MergeSHA != "" {
		t.Fatalf("must not record a MergeSHA for a merge into the wrong base, got %q", s.MergeSHA)
	}
}

// TestMergedDetectsBaseRevertedAfterMerge is the P1 regression for finding #1: a promotion
// merges successfully (base advances to hold the promoted content, PR.Merged becomes true), but
// later someone resets the base branch directly — outside hoist — backward past this
// promotion's own merge commit (e.g. env E's target content reverted by hand). The deterministic
// id/branch/PR marker are unchanged, so a re-run of the identical promotion finds the exact same,
// already-merged PR. Before this fix, MergedStep.Observe trusted pr.Merged alone and would report
// Satisfied on that stale evidence — this proves it now revalidates that the merge commit is
// still part of s.Base's live history (mergeWasReverted, an ancestry check — see its own doc
// comment for why ancestry rather than content is the correct test) and Blocks instead, naming
// the base, rather than silently reporting success while nothing is actually re-applied.
//
// The revert here is a genuine ref reset backward (force-pushing origin's base back to its exact
// pre-merge tip), not merely a content change at the promoted path: priorSHA is captured before
// the merge and is an ancestor of the merge commit, so resetting back to it makes the merge
// commit itself unreachable from the new tip — precisely the condition mergeWasReverted checks
// for, and precisely what distinguishes a real revert from TestMergedSupersededByLaterPromotionIsSatisfiedNotBlocked's
// forward-superseding case right below.
func TestMergedDetectsBaseRevertedAfterMerge(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	g := git.Exec{}
	step := MergedStep{Forge: f, Git: g}

	priorSHA, ok, err := g.LsRemoteBranch(ctx(), s.CloneDir, "origin", s.Base)
	if err != nil || !ok {
		t.Fatalf("expected origin's %s to exist before the merge: ok=%v err=%v", s.Base, ok, err)
	}

	// A real merge: the base actually advances to hold the promoted content (mergeToBase
	// simulates what a real GitHub squash-merge does, since forge.Fake never touches real git),
	// then hoist's own Act completes it (PR.Merged, branch deleted).
	mergeToBase(t, s)
	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("merging: %v", err)
	}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied right after a real merge with the base genuinely advanced: %+v", obs)
	}

	// Someone resets env E's target content directly, outside hoist: force-move origin's base
	// branch backward past this promotion's own merge commit, all the way to its pre-promotion
	// tip — a genuine history rewrite, not just a changed file.
	runHost(t, s.CloneDir, "push", "--force", "origin", priorSHA+":refs/heads/"+s.Base)

	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("must not report satisfied once the base has been reset past this promotion's merge: %+v", obs)
	}
	if obs.Blocked == "" {
		t.Fatalf("expected a clear Blocked signal, not a silent re-drive, got %+v", obs)
	}
	if !strings.Contains(obs.Blocked, s.Base) {
		t.Fatalf("Blocked reason should name the reverted base, got: %s", obs.Blocked)
	}
}

// TestMergedSupersededByLaterPromotionIsSatisfiedNotBlocked is the new regression for finding #2
// (round 3): a promotion (s) merges successfully, and then a second, later, entirely legitimate
// promotion re-touches the very same manifest path — an ordinary "promote to this env again"
// operation — advancing the base's tip *forward*, past s's own merge commit, never resetting it.
// s's own merge commit is still genuinely part of the base's history; a re-observe of s must
// report Satisfied, not Blocked. Under the old (round 2) blob-comparison implementation this
// test fails: the base's current content at the promoted path no longer matches what s's own
// merge wrote, and the old code read that as "reverted" even though nothing was ever reset —
// exactly the regression this fix closes. See TestMergedDetectsBaseRevertedAfterMerge just above
// for the genuine-revert case this must still Block on.
func TestMergedSupersededByLaterPromotionIsSatisfiedNotBlocked(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	g := git.Exec{}
	step := MergedStep{Forge: f, Git: g}

	mergeToBase(t, s)
	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("merging: %v", err)
	}
	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied right after a real merge with the base genuinely advanced: %+v", obs)
	}
	if len(s.Edits) == 0 {
		t.Fatal("fixture should have at least one edit")
	}

	// A second, later, legitimate promotion: a fresh clone of the very same origin (exactly what
	// an independent `hoist promote` worktree would have), re-touching the exact path s's own
	// merge wrote, and pushing that as a new commit on top of the base's current tip — forward,
	// never a reset. This is the ordinary shape of promoting to the same env more than once.
	second := filepath.Join(t.TempDir(), "second-clone")
	runHost(t, "", "clone", "-q", fx.originDir, second)
	runHost(t, second, "checkout", "-q", s.Base)
	editedPath := s.Edits[0].File
	full := filepath.Join(second, editedPath)
	before, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	after := append(append([]byte{}, before...), []byte("\n# a later, legitimate promotion touched this file too\n")...)
	if err := os.WriteFile(full, after, 0o644); err != nil {
		t.Fatal(err)
	}
	runHost(t, second, "add", "--", editedPath)
	runHost(t, second, "commit", "-q", "-m", "a later, legitimate promotion of the same path")
	runHost(t, second, "push", "-q", "origin", s.Base)

	obs, err = step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked != "" {
		t.Fatalf("a later, forward, legitimate re-promotion of the same path must not read as a revert: %+v", obs)
	}
	if !obs.Satisfied {
		t.Fatalf("expected satisfied: this promotion's own merge commit is still part of %s's history, just superseded by a later one: %+v", s.Base, obs)
	}
}

// TestMergedUnresolvableMergeSHABlocksCleanly is round-4's regression: the forge can report a
// merge sha this clone never fetched and cannot resolve locally (the base was reset far enough
// past it that the object was pruned, or this clone simply never had it), which the old code
// let bubble up from git.Git.IsAncestor's raw exit-128 "not a valid object name" error — a
// confusing plumbing failure instead of the clean, actionable "reverted" signal every other
// revert path already gives. An unresolvable merge sha is itself proof the merge is not part of
// the base's current reachable history either way, so it must Block the same way a confirmed
// revert does, not error.
func TestMergedUnresolvableMergeSHABlocksCleanly(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	g := git.Exec{}
	step := MergedStep{Forge: f, Git: g}

	// A merge sha no git object anywhere backs (never pushed, never fetched) — the fake's own
	// MergePR records whatever sha it's given verbatim when no HeadSHA was set to compare
	// against (see fake.go's MergePR), so this never touches real git at all.
	const bogusMergeSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := f.MergePR(ctx(), s.PR.Number, bogusMergeSHA); err != nil {
		t.Fatalf("seeding the fake merge: %v", err)
	}

	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatalf("expected a clean Blocked signal for an unresolvable merge sha, not an error: %v", err)
	}
	if obs.Satisfied {
		t.Fatalf("must not report satisfied for a merge sha this clone cannot resolve: %+v", obs)
	}
	if obs.Blocked == "" {
		t.Fatalf("expected a clear Blocked signal, not a silent re-drive, got %+v", obs)
	}
	if !strings.Contains(obs.Blocked, bogusMergeSHA) {
		t.Fatalf("Blocked reason should name the unresolvable merge sha, got: %s", obs.Blocked)
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

// TestMergedRecoveryRejectsMergedPRWithUnexpectedHead is finding #5's regression: Act's
// lost-response recovery path (MergePR errors, but a fresh FindPR shows the PR merged anyway)
// must not accept ANY observed-as-merged PR as this promotion's own success — only one whose
// HeadSHA still matches what this promotion expected. This simulates another actor changing the
// PR's head and merging something else in the gap between this promotion's own Observe and its
// own MergePR call: the fake's real merge lands against a DIFFERENT head sha than this
// promotion ever pushed, so Act's recovery must surface a genuine error, never silently accept
// that unrelated merge as its own.
func TestMergedRecoveryRejectsMergedPRWithUnexpectedHead(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)

	expected := s.PushedSHA
	if expected == "" {
		expected = s.CommitSHA
	}
	if expected == "" {
		t.Fatal("fixture should have set PushedSHA or CommitSHA")
	}

	// Simulate a different actor moving the PR's head and merging THAT, not what this promotion
	// pushed — the fake has no expectedHeadSHA check to bypass since this direct call passes "".
	f.SetHeadSHA(s.PR.Number, "impostor-head-sha-not-this-promotions")
	if _, err := f.MergePR(ctx(), s.PR.Number, ""); err != nil {
		t.Fatal(err)
	}

	// s.PR (this process's own copy) still thinks it's unmerged and still expects its own head,
	// so Act's own MergePR call fails (the fake's PR is already merged) and falls into the
	// lost-response recovery path — which must now refuse to adopt the impostor merge.
	step := MergedStep{Forge: f, Git: git.Exec{}}
	err := step.Act(ctx(), s)
	if err == nil {
		t.Fatal("Act must not silently accept a merged PR whose head does not match what this promotion expected")
	}
	if s.PR.Merged {
		t.Fatalf("s.PR must not have been overwritten with the impostor merge: %+v", s.PR)
	}
}

func TestMergedDeletesBranchAndResumesIfKilledBeforeDelete(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	g := git.Exec{}
	step := MergedStep{Forge: f, Git: g}

	// Simulate what a real GitHub squash-merge would actually do to the base branch: forge.Fake
	// has no access to real git and never touches origin on its own, but MergedStep's Observe
	// now revalidates origin/main's live tip against this promotion's own edits before trusting
	// a historical merge record (M4 hardening, finding #1) — without this, every Observe call
	// below would correctly refuse rather than report satisfied.
	mergeToBase(t, s)

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
