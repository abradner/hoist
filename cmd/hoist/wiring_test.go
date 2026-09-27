package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

func loadCfgAndEffForFixture(t *testing.T, cfgPath string) (*config.Config, effective) {
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
	return cfg, eff
}

// buildSvcForFixture builds the same *service.Service runTUI itself would build for cfgPath — the
// long-lived, session-wide Service internal/app/app.go calls StartPromotion/List/Resume/Abandon
// on directly (no cmd/hoist adapter in between since the service-design train's PR F) — and
// loads its current repo view (RepoFromClone: a pure local disk read, exactly what runTUI's
// own RepoFromOrigin falls back to when there is nothing to fetch from, and what every test
// fixture's origin already agrees with anyway) so StartRequest's own nil-Repo/nil-View defaults
// resolve to something, mirroring runTUI's own svc.LoadRepo call at boot. Returns the Service
// alongside the effective value, for tests that also need eff.repo/eff.appsRoot/eff.promotable
// to build their own plan via gitops.Discover+BuildPlan the way runTUI's own plan screen does.
func buildSvcForFixture(t *testing.T, cfgPath string) (*service.Service, effective) {
	t.Helper()
	cfg, eff := loadCfgAndEffForFixture(t, cfgPath)
	set := settingsFor(cfg, eff)
	svc := service.New(set, serviceDeps())
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}
	return svc, eff
}

// startOpts is this test file's own stand-in for the mode a screen would confirm with — the TUI
// no longer has an options struct of its own for this (PR F retired it in favor of service.Mode
// directly: internal/app calls svc.StartPromotion itself now, with no adapter in between), so these
// wiring-level integration tests build the identical service.StartRequest/Hooks shape app.go's
// plan.StartMsg/deploy.StartMsg cases do (see internal/app/app.go's startHooks) directly against
// svc, still proving the exact seam the TUI drives a real promotion through.
type startOpts struct {
	Direct    bool
	Confirmed bool
}

// startForTest issues one svc.StartPromotion call the same shape internal/app/app.go's own
// plan.StartMsg/deploy.StartMsg cases do (Mode, Hooks with progress doubling as onWaiting),
// collapsing the (Drive, error) pair back to the (state, driveFn, err) shape these tests were
// originally written against, back when a cmd/hoist TUI start adapter (removed in the
// service-design train's PR F) produced exactly that shape.
func startForTest(ctx context.Context, svc *service.Service, p gitops.Plan, opts startOpts, progress func(string)) (engine.PromotionState, session.Driver, error) {
	var onWaiting func()
	if progress != nil {
		onWaiting = func() { progress("waiting for signing approval") }
	}
	d, err := svc.StartPromotion(ctx, service.StartRequest{
		Plan: p,
		Mode: service.Mode{Direct: opts.Direct, Confirmed: opts.Confirmed},
	}, service.Hooks{Progress: progress, OnWaiting: onWaiting})
	if err != nil {
		return engine.PromotionState{}, nil, err
	}
	return d.State(), d, nil
}

// driveToDone runs driveFn repeatedly (mirroring what the flight screen's own tick loop does,
// minus the real terminal) until done or it errors terminally, bounded by maxIters so a bug that
// never converges fails the test instead of hanging it. A transient err (a plumbing hiccup on a
// retryable step, or MergedStep's own Blocked "not yet caught up" reading before the base-push
// simulation below lands) is tolerated and retried, exactly like internal/service.Driver.Run's
// own retry loop — this test only fails on an error that persists past the iteration cap.
//
// clone stands in for what a real GitHub squash-merge does to the base branch the instant a
// commit sha exists on this promotion: forge.Fake's own MergePR never touches real git (it only
// flips an in-memory Merged flag), so MergedStep's own Observe — re-run by this driveFn's own
// engine.DriveStatus walk every tick, per session.Driver's contract, not only once like
// internal/service.Driver.Run's own loop — would otherwise see origin's base branch never caught up and
// misreport a genuine revert (M4 hardening finding #1; internal/engine/fixture_test.go's own
// mergeToBase helper does the identical push for that package's tests). Doing the push as soon
// as CommitSHA is known, rather than waiting for MergeSHA, covers the case where CIGreenStep's
// own grace period hasn't elapsed yet on the very first call that reaches it (so the whole
// pipeline can complete branch/commit/push/PR-open and the merge itself within one later call,
// with no separate opportunity to react in between).
func driveToDone(t *testing.T, clone string, driveFn session.Driver, start engine.PromotionState, maxIters int) engine.PromotionState {
	t.Helper()
	cur := start
	var lastErr error
	pushed := false
	for i := 0; i < maxIters; i++ {
		tick, err := driveFn.Step(context.Background())
		cur = tick.State
		lastErr = err
		if !pushed && cur.CommitSHA != "" {
			runGitHost(t, clone, "push", "-q", "origin", cur.CommitSHA+":refs/heads/"+cur.Base)
			pushed = true
		}
		if tick.Done {
			return cur
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("driveFn did not converge within %d iterations; last error: %v; last state: %+v", maxIters, lastErr, cur)
	return cur
}

// TestTUIStartPromotionDrivesRealPromotionEndToEnd is the TUI-path sibling of
// promote_test.go's TestPromoteEndToEndThenResumeIsIdempotent: it proves that svc.StartPromotion
// — called directly by internal/app/app.go's plan.StartMsg case since the service-design train's
// PR F — drives a real engine.PromotionState through engine.Drive for real, against the same local git remote +
// fake forge fixture the CLI test uses, ending in an actual branch/commit/PR/merge rather than
// the M4-wiring-brief's pre-fix nil DriveFunc stub.
func TestTUIStartPromotionDrivesRealPromotionEndToEnd(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	state, driveFn, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
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

// TestDriveFuncForCallsProgressThroughoutARealDrive is a regression test for a P1 an
// adversarial review found (and internal/app's own fix corrected — see AdoptBuilt's and app.go's doc
// comments): driveFuncFor's own wrapped save reuses the SAME progress callback the preflight
// used for engine.Drive's per-step history hook (defect B/C: a long single Act streams into
// the log as it happens, not only once the whole Drive call returns), across the WHOLE
// promotion, not only its first step. This test is the wiring-level half of that coverage: it
// proves progress is actually called from real drive steps — branch, commit, push, PR-open,
// merge — reached through a REAL svc.StartPromotion-produced Drive driving a full,
// real promotion (the same fixture and driveToDone helper
// TestTUIStartPromotionDrivesRealPromotionEndToEnd uses), not a stub that never touches
// progress at all. The other half — that internal/app/app.go's own channel, reused across
// its preflight-then-adopt-then-drive lifecycle, is never closed while anything can still
// send on it (the actual panic this bug produced: a send on a closed channel panics
// unconditionally in Go, select/default only guards a full buffer, never a closed one) — is
// covered where that lifecycle actually lives, internal/app/app_test.go's
// TestProgressSurvivesFromPreflightThroughDrive.
func TestDriveFuncForCallsProgressThroughoutARealDrive(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	progress := func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}
	state, driveFn, err := startForTest(context.Background(), svc, plan, startOpts{}, progress)
	if err != nil {
		t.Fatalf("startPromotion: %v", err)
	}
	if driveFn == nil {
		t.Fatal("startPromotion returned a nil DriveFunc for a real, driveable plan")
	}
	mu.Lock()
	preflightLines := len(lines)
	mu.Unlock()
	if preflightLines == 0 {
		t.Fatal("progress was never called during preflight")
	}

	driveToDone(t, clone, driveFn, state, 500)

	mu.Lock()
	defer mu.Unlock()
	if len(lines) <= preflightLines {
		t.Fatalf("progress was never called during the drive itself: %d lines after preflight, still %d after a full promotion", preflightLines, len(lines))
	}
}

// TestTUIStartPromotionRefusesConflictingInFlight is the TUI-path sibling of
// promote_test.go's TestPromoteRefusesConflictAcquiredAfterTheFirstScan: svc.StartPromotion
// must refuse exactly the way runPromote does — via the same shared
// claim-then-rescan check — when another promotion targeting the same env is already in
// flight, rather than silently opening a second branch/PR for it.
func TestTUIStartPromotionRefusesConflictingInFlight(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)

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

	_, driveFn, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
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
	cfgPath, _, _ := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)
	eff.cfg.GitHub = ""

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, driveFn, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
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

// TestTUIStartPromotionSkipsAllNoOpPlan is the TUI-path sibling of
// promote_test.go's TestPromoteNothingToDoIsANoOp (PR #50 review finding #9): the all-NoOp
// fast-path guard (service.AnyRealEdit and its own no-op-against-base check) lives inside
// svc.StartPromotion itself now (internal/service/start.go), applied before ever claiming or
// building a worktree — otherwise confirming an already-current plan would still claim, build a
// worktree and save a real state file before
// the commit step ever rejected the empty change, potentially blocking a real future promotion
// to the same target env. This mirrors TestPromoteNothingToDoIsANoOp's own fixture mutation
// (simulate the PR having already merged) and confirms startPromotion reports "already current"
// with no PR opened and no state file left behind.
func TestTUIStartPromotionSkipsAllNoOpPlan(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)

	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "simulate the PR having merged")
	// Push it too: a merged PR lands on origin, not only in someone's local clone. Without
	// this the fixture leaves the clone ahead of origin/main, which is exactly the stale-clone
	// state checkCloneCurrentForBase (M6) now refuses before any plan is trusted — so the test
	// would stop at that refusal and never reach the all-no-op path it exists to check.
	runGitHost(t, clone, "push", "-q", "origin", "main")

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, driveFn, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
	if err == nil {
		t.Fatal("expected startPromotion to refuse an all-NoOp plan")
	}
	if !strings.Contains(err.Error(), "already current") {
		t.Errorf("error should report the plan as already current, got: %v", err)
	}
	if driveFn != nil {
		t.Error("expected a nil DriveFunc alongside the already-current refusal")
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created for a no-op plan: %+v", f.PRs())
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Errorf("no state file should have been written for a no-op plan: %+v", states)
	}
}

// TestTUIStartPromotionReleasesClaimWithoutDriving is PR #50 review finding #6 (Copilot +
// Codex): buildPromotionForConfirm's own doc comment requires release to run once the
// returned state's first successful save lands — that must not depend on engine.Drive ever
// actually being called and reaching its own first per-step save; if the operator backs out
// (Esc, abort, quit) before that happens, the claim must still be gone, not stuck on disk.
// This deliberately never calls the first call's returned driveFn at all — standing in for
// exactly that "backed out early" gap — then confirms a second confirm of the identical plan
// still succeeds: proof the first call already released its claim, rather than leaving it for
// engine.Drive's own save to release (which, per engine.Drive's own code, never even runs when
// the very first step's Observe returns a plain error before Act is ever reached).
func TestTUIStartPromotionReleasesClaimWithoutDriving(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	state1, driveFn1, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
	if err != nil {
		t.Fatalf("first startPromotion call: %v", err)
	}
	if driveFn1 == nil {
		t.Fatal("expected a non-nil DriveFunc for a real, driveable plan")
	}
	_ = driveFn1 // deliberately never called — see the test's own doc comment

	state2, driveFn2, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
	if err != nil {
		t.Fatalf("second startPromotion call failed — the first call's claim was not released before it ever returned driveFn: %v", err)
	}
	if state2.ID != state1.ID {
		t.Errorf("second call's id = %q, want the same id %q (identical plan)", state2.ID, state1.ID)
	}
	_ = driveFn2
}

// TestRunLauncherSurfacesNonZeroExit is Copilot's PR #50 review finding: defaultOpenBrowser's
// original Start-and-reap shape only ever reported an error when the launcher binary itself
// couldn't be found — a launcher that started but then failed at runtime (no browser installed,
// a bad DISPLAY, xdg-open's own failure) reported nil, so flight.OpenPRMsg's handler showed no
// notice at all even though nothing actually opened. Rather than launching a real browser (no
// test in this repo launches a real browser or a process it doesn't own — see
// newPromoteFixture's own comment), this uses the standard os/exec "helper process" idiom:
// re-exec this same test binary as a subprocess it fully owns and controls, this time made to
// exit non-zero deliberately, and confirms runLauncher's own error reflects that exit rather
// than reporting success.
func TestRunLauncherSurfacesNonZeroExit(t *testing.T) {
	if os.Getenv("HOIST_WIRING_TEST_HELPER_PROCESS") == "1" {
		// Acts as a launcher that started fine but failed at runtime.
		os.Exit(7)
	}
	// Set only after the check above, and only for this process going forward — runLauncher's
	// re-exec'd child inherits it (exec.Cmd's default Env, nil, means "the current process's
	// environment" at Start time), while this same check at the TOP of this very function
	// already ran and returned false before this line, so the parent invocation is unaffected.
	t.Setenv("HOIST_WIRING_TEST_HELPER_PROCESS", "1")
	err := runLauncher(2*time.Second, os.Args[0], "-test.run=^TestRunLauncherSurfacesNonZeroExit$")
	if err == nil {
		t.Fatal("runLauncher = nil, want the helper process's non-zero exit surfaced as an error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runLauncher error = %v (%T), want an *exec.ExitError", err, err)
	}
	if exitErr.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7 (the helper process's own deliberate exit)", exitErr.ExitCode())
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
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", url}},
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

// TestTUIStartPromotionRecordsDirectBeforeTheFirstSave is Copilot's PR #72 finding: the TUI
// chose direct mode only for the step list, so the state it saved and handed to the flight
// screen never carried Direct at all. Two things read the mode off the state rather than off
// the step list — flight.OrderFor, which would draw the PR path's ten steps for a run that
// only has six, and `hoist resume`, which for a state saved before DirectPushedStep ever ran
// would drive the promotion as a PR: opening a branch and a PR for a change the operator
// explicitly asked to push straight to base.
func TestTUIStartPromotionRecordsDirectBeforeTheFirstSave(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)
	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}

	state, _, err := startForTest(context.Background(), svc, plan, startOpts{Direct: true, Confirmed: true}, nil)
	if err != nil {
		t.Fatalf("startPromotion: %v", err)
	}
	if !state.Direct {
		t.Error("the returned state must carry Direct: the flight screen picks its step order from it")
	}
	// And on disk, which is what resume reads — the state is saved before this function ever
	// returns, so a quit here must not leave a file resume drives as a PR promotion.
	saved, err := engine.LoadState(mustStatePath(t, state.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Direct {
		t.Error("the SAVED state must carry Direct: resume reads the mode from the file, not from this process")
	}

	// The asymmetry: a PR-mode start must not set it, or the assertion above passes on a
	// field that is simply always true.
	prState, _, err := startForTest(context.Background(), svc, plan, startOpts{}, nil)
	if err == nil && prState.Direct {
		t.Error("a PR-mode start must not record Direct")
	}
}

func mustStatePath(t *testing.T, id string) string {
	t.Helper()
	p, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// An already-current plan says so from the TUI even when the forge could not be built: the
// CLI's promote and deploy both check the all-no-op fast path before newForge, and the TUI
// must not disagree with them about when a GitHub login is needed (issue #55).
func TestTUIStartPromotionAllNoOpBeatsForgeError(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	svc, eff := buildSvcForFixture(t, cfgPath)
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "simulate the PR having merged")
	runGitHost(t, clone, "push", "-q", "origin", "main")
	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	forgeErr := errors.New("gh: not logged in")
	prevForge := newForge
	newForge = func(string) (forge.Forge, error) { return nil, forgeErr }
	t.Cleanup(func() { newForge = prevForge })
	_, _, err = startForTest(context.Background(), svc, plan, startOpts{}, nil)
	if err == nil || !strings.Contains(err.Error(), "already current") {
		t.Fatalf("err = %v, want the already-current refusal ahead of the forge error", err)
	}
	if errors.Is(err, forgeErr) {
		t.Errorf("the forge error reached the operator for a plan that never needed the forge: %v", err)
	}
}
