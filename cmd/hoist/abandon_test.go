package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// buildPROpenedState drives a promotion through exactly the four PR-opening steps
// (engine.Steps: branch, commit, push, open the PR) — NOT CIGreen/Approved/Merged, so the
// result is a real PR that genuinely has not landed, the shape abandonPromotion's own tests
// need and newPromoteFixture's auto-converging config (ci.none=green, approval=auto) makes
// otherwise hard to stop short of.
func buildPROpenedState(t *testing.T, clone string) *engine.PromotionState {
	t.Helper()
	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID: id, RepoFullName: "example/gitops", SourceEnv: plan.SourceEnv, TargetEnv: plan.TargetEnv,
		Branch: engine.BranchName(plan.TargetEnv, id), CloneDir: clone, WorktreeDir: wt, Base: "main",
		Edits: plan.Edits, CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle: engine.PRTitle(plan), PRBody: engine.RenderPRBody(id, plan),
		Approval: "auto", CINone: "green", // matches newPromoteFixture's own repo config (ci.none: green)
	}
	return s
}

// TestAbandonRefusesALandedPromotion drives a promotion all the way through `hoist promote`
// (this fixture's config auto-converges CI/approval, so one call lands it) and confirms
// abandonPromotion refuses it outright — abandoning is not a rollback.
func TestAbandonRefusesALandedPromotion(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut strings.Builder
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("fixture precondition: expected exactly one state file, got %d", len(states))
	}
	id := states[0].ID

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = abandonPromotion(context.Background(), cfg, id)
	if err == nil {
		t.Fatal("abandonPromotion accepted a landed promotion")
	}
	if !strings.Contains(err.Error(), "already landed") || !strings.Contains(err.Error(), "not a rollback") {
		t.Errorf("err = %v, want it to say the promotion already landed and abandoning is not a rollback", err)
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := engine.LoadState(statePath); err != nil || st == nil {
		t.Errorf("refused abandon must leave the state file in place: LoadState = %v, %v", st, err)
	}
}

// TestAbandonClosesPRAndDeletesBranchAndState drives a promotion only as far as PR-opened
// (never merged), then abandons it: the PR must close, the remote branch must go away, and the
// state file must be deleted.
func TestAbandonClosesPRAndDeletesBranchAndState(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	if s.PR == nil {
		t.Fatal("fixture precondition: a PR should be open")
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := abandonPromotion(context.Background(), cfg, s.ID)
	if err != nil {
		t.Fatalf("abandonPromotion: %v (lines so far: %v)", err, lines)
	}

	pr, ok, err := f.FindPR(context.Background(), s.Branch, engine.Marker(s.ID))
	if err != nil || !ok {
		t.Fatalf("finding the PR after abandon: ok=%v err=%v", ok, err)
	}
	if !pr.Closed {
		t.Errorf("PR #%d should be closed after abandon: %+v", pr.Number, pr)
	}
	if _, exists, err := newGit.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Branch); err != nil || exists {
		t.Errorf("branch %s should be gone from origin after abandon: exists=%v err=%v", s.Branch, exists, err)
	}
	if st, err := engine.LoadState(statePath); err != nil || st != nil {
		t.Errorf("state file should be deleted after abandon: LoadState = %v, %v", st, err)
	}
}

// TestAbandonDirectModeNeverTouchesGitAndDeletesState is direct mode's own shape: no PR, no
// branch pushed to origin at all — abandon must not attempt either, only delete the state file.
func TestAbandonDirectModeNeverTouchesGitAndDeletesState(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	s.Direct = true
	s.PR = nil
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := abandonPromotion(context.Background(), cfg, s.ID)
	if err != nil {
		t.Fatalf("abandonPromotion: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("direct mode should touch neither a PR nor a branch, got %v", lines)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "ClosePR") {
			t.Errorf("direct mode has no PR to close, but ClosePR was called: %v", f.Calls)
		}
	}
	if st, err := engine.LoadState(statePath); err != nil || st != nil {
		t.Errorf("state file should be deleted after abandon: LoadState = %v, %v", st, err)
	}
}

// TestRunAbandonRequiresConfirmationMatchingID is the CLI's own gate: --confirm-abandon must
// repeat the id exactly, and a mismatch (or omission) touches nothing.
func TestRunAbandonRequiresConfirmationMatchingID(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	got := run([]string{"--config", cfgPath, "abandon", s.ID, "--confirm-abandon", "not-the-id"}, &out, &errOut)
	if got == 0 {
		t.Fatalf("exit 0 with a mismatched confirmation, want a refusal; stdout=%s stderr=%s", out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "nothing was touched") {
		t.Errorf("stderr should say nothing was touched: %s", errOut.String())
	}
	if st, err := engine.LoadState(statePath); err != nil || st == nil {
		t.Errorf("a refused abandon must leave the state file in place: LoadState = %v, %v", st, err)
	}

	out.Reset()
	errOut.Reset()
	got = run([]string{"--config", cfgPath, "abandon", s.ID, "--confirm-abandon", s.ID}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d with a correct confirmation, want 0; stderr: %s", got, errOut.String())
	}
	if st, err := engine.LoadState(statePath); err != nil || st != nil {
		t.Errorf("state file should be deleted after a confirmed abandon: LoadState = %v, %v", st, err)
	}
}

// TestReorderConfirmAbandonFirst is the regression test for the ordering pitfall
// TestRunAbandonRequiresConfirmationMatchingID above found while writing it: flag.Parse stops
// scanning at the first non-flag token, so `abandon <id> --confirm-abandon <id>` — arguably the
// more natural reading order — left --confirm-abandon completely unparsed before this fix.
func TestReorderConfirmAbandonFirst(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flag already first, --confirm-abandon=", []string{"--confirm-abandon=x", "id"}, []string{"--confirm-abandon=x", "id"}},
		{"id first, --confirm-abandon=", []string{"id", "--confirm-abandon=x"}, []string{"--confirm-abandon=x", "id"}},
		{"id first, space-separated value", []string{"id", "--confirm-abandon", "x"}, []string{"--confirm-abandon", "x", "id"}},
		{"no confirm flag at all", []string{"id"}, []string{"id"}},
	}
	for _, tc := range cases {
		got := reorderConfirmAbandonFirst(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: reorderConfirmAbandonFirst(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestAbandonRefusesAMergedPromotionEvenWithBranchStillPresent is the round-2 adversarial
// review's own regression test: MergedStep.Observe reports Satisfied:false — "merged as X;
// branch not yet deleted" — whenever the squash-merge itself landed but the branch-delete
// cleanup hasn't finished (AGENTS.md §6.1 gotcha 7: `gh pr merge --delete-branch` can fail in a
// worktree AFTER a real merge already succeeded). An earlier version trusted ObserveAll's bare
// `done` here and would have "abandoned" — deleted the state file, discarded the History — a
// promotion that had genuinely merged, possibly to production. Simulates the gotcha by
// recreating the branch ref on origin right after a real merge, exactly the shape a failed
// `--delete-branch` leaves behind.
func TestAbandonRefusesAMergedPromotionEvenWithBranchStillPresent(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.CoreSteps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to merged: %v", err)
	}
	if s.MergeSHA == "" {
		t.Fatal("fixture precondition: the promotion should have merged")
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	originURL := strings.TrimSpace(outGitHost(t, clone, "remote", "get-url", "origin"))
	runGitHost(t, originURL, "update-ref", "refs/heads/"+s.Branch, "refs/heads/main")
	if _, exists, err := newGit.LsRemoteBranch(context.Background(), clone, "origin", s.Branch); err != nil || !exists {
		t.Fatalf("fixture precondition: branch %s should still be on origin: exists=%v err=%v", s.Branch, exists, err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = abandonPromotion(context.Background(), cfg, s.ID)
	if err == nil {
		t.Fatal("abandonPromotion accepted a merged promotion whose branch happened to still be present")
	}
	if !strings.Contains(err.Error(), "already landed") || !strings.Contains(err.Error(), "not a rollback") {
		t.Errorf("err = %v, want it to say the promotion already landed and abandoning is not a rollback", err)
	}
	if st, err := engine.LoadState(statePath); err != nil || st == nil {
		t.Errorf("refused abandon must leave the state file (and its History of the real merge) in place: LoadState = %v, %v", st, err)
	}
}

// branchDeleteFailingGit wraps the real git.Git and fails only DeleteRemoteBranch — simulating
// exactly the AGENTS.md §6.1 gotcha 7 shape (the merge itself succeeds; only the branch-delete
// cleanup fails) without needing a real permissions failure.
type branchDeleteFailingGit struct{ git.Git }

func (branchDeleteFailingGit) DeleteRemoteBranch(context.Context, string, string, string) error {
	return fmt.Errorf("simulated: git push --delete failed")
}

// TestAbandonRetryAfterBranchDeleteFailureDoesNotRecloseThePR was raised by a round-2 review as
// a possible gap: a first abandonPromotion call whose ClosePR succeeds but whose
// DeleteRemoteBranch then fails never re-saves s, so the on-disk state's own PR.Closed stays
// stale for a retry. Traced rather than assumed (AGENTS.md's own "a finding is a claim, not a
// verdict"): ObserveAll's own MergedStep probe (engine.go's phaseIndex short-circuit) always
// re-fetches the live PR via findOwnPR and assigns it back to s.PR before abandonPromotion's own
// ClosePR guard ever runs, regardless of merged/closed state — so the guard is never actually
// looking at stale data. This proves that property holds, not a fix for a bug that traced out.
func TestAbandonRetryAfterBranchDeleteFailureDoesNotRecloseThePR(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	prevGit := newGit
	newGit = branchDeleteFailingGit{Git: prevGit}
	t.Cleanup(func() { newGit = prevGit })

	if _, err := abandonPromotion(context.Background(), cfg, s.ID); err == nil || !strings.Contains(err.Error(), "deleting branch") {
		t.Fatalf("first abandonPromotion = %v, want a branch-delete failure", err)
	}
	pr, ok, err := f.GetPR(context.Background(), s.PR.Number)
	if err != nil || !ok || !pr.Closed {
		t.Fatalf("fixture precondition: PR #%d should already be closed after the first (partial) abandon: ok=%v pr=%+v err=%v", s.PR.Number, ok, pr, err)
	}
	closeCallsAfterFirst := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "ClosePR") {
			closeCallsAfterFirst++
		}
	}

	newGit = prevGit // the branch-delete succeeds this time
	if _, err := abandonPromotion(context.Background(), cfg, s.ID); err != nil {
		t.Fatalf("second abandonPromotion: %v", err)
	}
	closeCallsAfterSecond := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "ClosePR") {
			closeCallsAfterSecond++
		}
	}
	if closeCallsAfterSecond != closeCallsAfterFirst {
		t.Errorf("ClosePR was called again on retry: %d calls before, %d after, want unchanged", closeCallsAfterFirst, closeCallsAfterSecond)
	}
	if st, err := engine.LoadState(statePath); err != nil || st != nil {
		t.Errorf("state file should be deleted after the second, fully-successful abandon: LoadState = %v, %v", st, err)
	}
}
