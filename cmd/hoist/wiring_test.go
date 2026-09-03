package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// buildEffForFixture loads cfgPath (built by newPromoteFixture) and resolves it into the
// effective value runTUI itself would build for the TUI, standing in for
// gitops.Discover+selectRepo's own real work without duplicating main.go's flag-parsing.
func buildEffForFixture(t *testing.T, cfgPath string) effective {
	t.Helper()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	eff, err := selectRepo(cfg, selection{given: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if eff.cfg == nil {
		t.Fatal("fixture config should resolve to exactly one repo")
	}
	return eff
}

// driveToDone runs driveFn repeatedly (mirroring what the flight screen's own tick loop does,
// minus the real terminal) until done or it errors terminally, bounded by maxIters so a bug that
// never converges fails the test instead of hanging it. A transient err (a plumbing hiccup on a
// retryable step, or MergedStep's own Blocked "not yet caught up" reading before the base-push
// simulation below lands) is tolerated and retried, exactly like driveToCompletion's own retry
// loop — this test only fails on an error that persists past the iteration cap.
//
// clone stands in for what a real GitHub squash-merge does to the base branch the instant a
// commit sha exists on this promotion: forge.Fake's own MergePR never touches real git (it only
// flips an in-memory Merged flag), so MergedStep's own Observe — re-run by this driveFn's own
// engine.Status call every tick, per flight.DriveFunc's contract, not only once like
// driveToCompletion's own loop — would otherwise see origin's base branch never caught up and
// misreport a genuine revert (M4 hardening finding #1; internal/engine/fixture_test.go's own
// mergeToBase helper does the identical push for that package's tests). Doing the push as soon
// as CommitSHA is known, rather than waiting for MergeSHA, covers the case where CIGreenStep's
// own grace period hasn't elapsed yet on the very first call that reaches it (so the whole
// pipeline can complete branch/commit/push/PR-open and the merge itself within one later call,
// with no separate opportunity to react in between).
func driveToDone(t *testing.T, clone string, driveFn func(ctx context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error), start engine.PromotionState, maxIters int) engine.PromotionState {
	t.Helper()
	cur := start
	var lastErr error
	pushed := false
	for i := 0; i < maxIters; i++ {
		next, done, _, err := driveFn(context.Background(), cur)
		cur = next
		lastErr = err
		if !pushed && cur.CommitSHA != "" {
			runGitHost(t, clone, "push", "-q", "origin", cur.CommitSHA+":refs/heads/"+cur.Base)
			pushed = true
		}
		if done {
			return cur
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("driveFn did not converge within %d iterations; last error: %v; last state: %+v", maxIters, lastErr, cur)
	return cur
}

// TestTUIStartPromotionDrivesRealPromotionEndToEnd is the TUI-path sibling of
// promote_test.go's TestPromoteEndToEndThenResumeIsIdempotent: it proves that
// buildStartPromotion's app.StartPromotionFunc — the adaptor cmd/hoist/main.go's runTUI wires
// into internal/app.New, which internal/app/app.go calls from its plan.StartMsg case — drives a
// real engine.PromotionState through engine.Drive for real, against the same local git remote +
// fake forge fixture the CLI test uses, ending in an actual branch/commit/PR/merge rather than
// the M4-wiring-brief's pre-fix nil DriveFunc stub.
func TestTUIStartPromotionDrivesRealPromotionEndToEnd(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	eff := buildEffForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := buildStartPromotion(eff, newGit, f, nil)
	state, driveFn, err := start(context.Background(), plan)
	if err != nil {
		t.Fatalf("startPromotion: %v", err)
	}
	if state.ID == "" {
		t.Fatal("startPromotion returned an empty ID")
	}
	if driveFn == nil {
		t.Fatal("startPromotion returned a nil DriveFunc for a real, driveable plan")
	}

	final := driveToDone(t, clone, driveFn, state, 500)
	if final.CommitSHA == "" {
		t.Errorf("expected a real commit sha, got none: %+v", final)
	}
	if final.PR == nil {
		t.Fatalf("expected a real PR, got none: %+v", final)
	}
	if final.MergeSHA == "" {
		t.Errorf("expected a real merge sha, got none: %+v", final)
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("expected exactly one PR opened against the fake forge, got %d", len(f.PRs()))
	}
	if !f.PRs()[0].Merged {
		t.Fatalf("PR should be merged: %+v", f.PRs()[0])
	}
	if !strings.HasPrefix(final.Branch, "hoist/app-production/") {
		t.Errorf("unexpected branch name: %q", final.Branch)
	}

	// MergedStep's own Act deletes the branch on origin once merged — the same real-git
	// assertion promote_test.go's CLI-path test makes, proving this path actually drove Act
	// calls against origin rather than only updating in-memory fields.
	var g git.Exec
	if _, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", final.Branch); err != nil || ok {
		t.Fatalf("origin should no longer have the merged branch: ok=%v err=%v", ok, err)
	}

	// The state file this path saved is discoverable exactly like a CLI-driven promotion's
	// would be — proof the TUI path shares the same durable state, not a parallel copy.
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range states {
		if s.ID == final.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %s among ListStates, got %+v", final.ID, states)
	}
}

// TestTUIStartPromotionRefusesConflictingInFlight is the TUI-path sibling of
// promote_test.go's TestPromoteRefusesConflictAcquiredAfterTheFirstScan: buildStartPromotion
// must refuse exactly the way runPromote does — via the same shared
// buildPromotionForConfirm — when another promotion targeting the same env is already in
// flight, rather than silently opening a second branch/PR for it.
func TestTUIStartPromotionRefusesConflictingInFlight(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	eff := buildEffForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	const otherID = "other-in-flight-promotion"
	wt, err := engine.WorktreeDir(otherID)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := engine.StatePath(otherID)
	if err != nil {
		t.Fatal(err)
	}
	other := &engine.PromotionState{
		ID:            otherID,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, otherID),
		CloneDir:      clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(otherID, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(otherID, plan),
		Approval:      "comment",
		Approvers:     []string{"alice"},
		CINone:        "green",
	}
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), other, nil); err != nil {
		t.Fatalf("driving the other promotion to PROpened: %v", err)
	}
	if err := engine.SaveState(statePath, other); err != nil {
		t.Fatal(err)
	}

	start := buildStartPromotion(eff, newGit, f, nil)
	_, driveFn, err := start(context.Background(), plan)
	if err == nil {
		t.Fatal("expected startPromotion to refuse a conflicting in-flight promotion for the same env")
	}
	if !strings.Contains(err.Error(), "still in flight") {
		t.Errorf("error should name the in-flight conflict, got: %v", err)
	}
	if driveFn != nil {
		t.Error("expected a nil DriveFunc alongside the refusal")
	}
	// Exactly one PR: the "other" mid-flight promotion's own, created by this test's own
	// setup — startPromotion above must refuse before ever getting far enough to open a
	// second one for its own id.
	if len(f.PRs()) != 1 || f.PRs()[0].HeadBranch != other.Branch {
		t.Fatalf("expected exactly the other promotion's own PR and nothing more, got %+v", f.PRs())
	}
}

// TestTUIStartPromotionRequiresGitHubConfig is the TUI-path sibling of
// TestPromoteRequiresGitHubConfig: a repo with no github: owner/name configured must refuse
// with the same message runPromote uses, rather than panicking on a nil-pointer RepoConfig
// field or on a nil forge.
func TestTUIStartPromotionRequiresGitHubConfig(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	eff := buildEffForFixture(t, cfgPath)
	eff.cfg.GitHub = ""

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := buildStartPromotion(eff, newGit, f, nil)
	_, driveFn, err := start(context.Background(), plan)
	if err == nil {
		t.Fatal("expected a refusal with no github configured")
	}
	if !strings.Contains(err.Error(), "github") {
		t.Errorf("error should mention the missing github config, got: %v", err)
	}
	if driveFn != nil {
		t.Error("expected a nil DriveFunc alongside the refusal")
	}
}

// TestBrowserCommandPerOS pins browserCommand's per-platform choice — the pure seam
// defaultOpenBrowser calls, and the one wiring_test.go itself can exercise without ever calling
// exec.Command (no test in this repo launches a real browser or process it doesn't own).
func TestBrowserCommandPerOS(t *testing.T) {
	const url = "https://example.invalid/pr/1"
	cases := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{"darwin", "open", []string{url}},
		{"windows", "cmd", []string{"/c", "start", "", url}},
		{"linux", "xdg-open", []string{url}},
		{"freebsd", "xdg-open", []string{url}}, // unlisted GOOS falls back to the Unix convention
	}
	for _, tc := range cases {
		name, args := browserCommand(tc.goos, url)
		if name != tc.wantName || len(args) != len(tc.wantArgs) {
			t.Fatalf("%s: browserCommand = %q, %v; want %q, %v", tc.goos, name, args, tc.wantName, tc.wantArgs)
		}
		for i := range args {
			if args[i] != tc.wantArgs[i] {
				t.Errorf("%s: arg %d = %q, want %q", tc.goos, i, args[i], tc.wantArgs[i])
			}
		}
	}
}
