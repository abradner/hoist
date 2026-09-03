package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// runGitHost runs git directly for fixture setup that is not itself part of what's under
// test (seeding the local origin this test promotes against — never a real GitHub repo or
// cluster, per the hard constraints).
func runGitHost(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// newPromoteFixture builds a local bare "origin" and a clone of it shaped like a minimal
// GitOps repo (one family, "app", older in app-production than app-staging) plus a config
// file naming it, and points package-level newGit/newForge at a real local git and a fake
// forge for the duration of the test — never a real GitHub repo, per the hard constraints.
func newPromoteFixture(t *testing.T) (configPath, cloneDir string, f *forge.Fake) {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	const cfgFile = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(gitconfig, []byte(cfgFile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg-cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg-state"))

	seed := t.TempDir()
	runGitHost(t, "", "init", "-q", "-b", "main", seed)
	write := func(rel, content string) {
		p := filepath.Join(seed, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := func(env string) string {
		return "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-" + env + "\n  namespace: argocd\n" +
			"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/" + env + "/app\n" +
			"  destination:\n    server: https://kubernetes.default.svc\n    namespace: " + env + "\n"
	}
	deployment := func(ref string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: " + ref + "\n"
	}
	digestOld := "sha256:" + strings.Repeat("0", 64)
	digestNew := "sha256:" + strings.Repeat("1", 64)
	write("cluster/apps/app-staging-app.yaml", wrapper("app-staging"))
	write("cluster/apps/app-production-app.yaml", wrapper("app-production"))
	write("cluster/apps/app-staging/app/deployment.yaml", deployment("ghcr.io/example/app:v2@"+digestNew))
	write("cluster/apps/app-production/app/deployment.yaml", deployment("ghcr.io/example/app:v1@"+digestOld))
	runGitHost(t, seed, "add", ".")
	runGitHost(t, seed, "commit", "-q", "-m", "seed")

	origin := filepath.Join(t.TempDir(), "origin.git")
	runGitHost(t, "", "init", "-q", "--bare", "-b", "main", origin)
	runGitHost(t, seed, "remote", "add", "origin", origin)
	runGitHost(t, seed, "push", "-q", "origin", "main")

	clone := filepath.Join(t.TempDir(), "clone")
	runGitHost(t, "", "clone", "-q", origin, clone)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	// ci.grace and poll.* are cut to a few milliseconds so a test exercising the full M4
	// pipeline (CIGreen -> Approved -> Merged) against the fake forge's default "no checks
	// reported" converges in-process instead of waiting out the real 3-minute/20-second
	// production defaults; app-production isn't listed under envs.production here, so
	// RepoConfig.Approval defaults it to "auto" and no approval comment is needed either.
	yaml := fmt.Sprintf("repos:\n  - name: gitops\n    path: %s\n    github: example/gitops\n    apps_root: cluster/apps\n    promotable: [ghcr.io/example/]\n    ci:\n      none: green\n      grace: 5ms\n"+
		"poll:\n  ci: 5ms\n  approval: 5ms\n  argo: 5ms\n  rollout: 5ms\n  deadline: 10s\n", clone)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeForge := &forge.Fake{}
	prevGit, prevForge := newGit, newForge
	newGit = git.Exec{}
	newForge = func(string) (forge.Forge, error) { return fakeForge, nil }
	t.Cleanup(func() { newGit, newForge = prevGit, prevForge })

	return cfgPath, clone, fakeForge
}

// commitLine pulls the "  commit: <sha>" line runPromote/runResume print on success.
func commitLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "  commit: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// TestPromoteEndToEndThenResumeIsIdempotent drives a promotion all the way through the M4
// pipeline (CIGreen -> Approved -> Merged, both auto-satisfied by this fixture's config: see
// newPromoteFixture) in one `hoist promote` call, then re-runs the identical command — standing
// in for a resumed/re-invoked process — and confirms it converges on the same commit and PR
// rather than creating a second one of either (AGENTS.md invariant 4).
func TestPromoteEndToEndThenResumeIsIdempotent(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "branch: hoist/app-production/") || !strings.Contains(out.String(), "PR: ") || !strings.Contains(out.String(), "merged: ") {
		t.Fatalf("stdout missing expected fields:\n%s", out.String())
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("expected exactly one PR, got %d", len(f.PRs()))
	}
	firstPR := f.PRs()[0]
	if !firstPR.Merged {
		t.Fatalf("PR should be merged: %+v", firstPR)
	}
	firstCommit := commitLine(out.String())
	if firstCommit == "" {
		t.Fatalf("stdout missing a commit line:\n%s", out.String())
	}

	var g git.Exec
	branchLine := strings.Split(out.String(), "\n")[1]
	branch := strings.TrimSpace(strings.TrimPrefix(branchLine, "  branch:"))
	if _, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", branch); err != nil || ok {
		t.Fatalf("origin should no longer have the merged branch: ok=%v err=%v", ok, err)
	}

	// Simulate what a real GitHub squash-merge would actually do: fast-forward origin's base
	// branch to hold the promoted content. forge.Fake has no access to real git and never
	// touches origin on its own, but MergedStep's Observe now revalidates origin/main's live
	// tip against this promotion's own edits before trusting a historical merge record (M4
	// hardening, finding #1) — without this, the second run below would find the base still at
	// its pre-promotion content and correctly refuse to report success on stale evidence.
	runGitHost(t, clone, "push", "-q", "origin", firstCommit+":refs/heads/main")

	// Re-running the exact same command (simulating a resumed/re-invoked process) must not
	// create a second commit or a second PR.
	out.Reset()
	errOut.Reset()
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("second run exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 1 || f.PRs()[0].Number != firstPR.Number {
		t.Fatalf("second run should reuse PR #%d, got %+v", firstPR.Number, f.PRs())
	}
	if second := commitLine(out.String()); second != firstCommit {
		t.Fatalf("second run produced a different commit: first %s, second %s", firstCommit, second)
	}
}

// TestPromoteRefusesConflictAcquiredAfterTheFirstScan is round-4's regression for the
// scan-then-claim ordering gap: the pre-claim findInFlight scan and engine.ClaimInFlight are not
// one atomic operation, so a state file for a *different* id that only becomes visible after the
// first scan already ran — but before this call proceeds — must still be caught. runPromote now
// re-runs findInFlight a second time immediately after acquiring the claim, closing that window;
// this test can't reproduce the exact multi-process timing (a second real `hoist promote`
// pausing between its own scan and claim), but it does exercise the actual code path added for
// it: a conflicting, non-terminal state file for a different id targeting the same env is
// visible on disk throughout, and the whole `hoist promote` invocation must refuse — proving the
// re-scan's error/conflict handling is wired correctly, not just present.
func TestPromoteRefusesConflictAcquiredAfterTheFirstScan(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)

	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A different id (a distinct digest set would derive its own id naturally; a fixed literal
	// standing in for one here keeps this test independent of the fixture's exact image data),
	// targeting the same env, left mid-flight (PROpened only, never approved) so ObserveAll
	// reports it not-done.
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

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected refusal with a conflicting in-flight promotion for the same env, got exit 0; stdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "still in flight") {
		t.Fatalf("stderr should name the in-flight conflict, got: %s", errOut.String())
	}
	// Exactly one PR: the "other" mid-flight promotion's own, created by this test's own setup
	// (driving it to PROpened) — the actual `run()` call above must refuse before ever getting
	// far enough to open a second one for its own id.
	if len(f.PRs()) != 1 || f.PRs()[0].HeadBranch != other.Branch {
		t.Fatalf("expected exactly the other promotion's own PR and nothing more, got %+v", f.PRs())
	}
}

// TestPromoteRetryNeverStraddlesPolicyAcrossConfigEdits is the sibling regression to
// resume_test.go's TestResumeNeverStraddlesPolicyAcrossConfigEdits, at runPromote's OWN
// in-place-retry path (re-invoking `hoist promote` for an id that already has a state file on
// disk): that path restores History and the CI override from the existing state, but used to
// populate CINone/CIGrace/Approval/Approvers/Collaborators fresh from CURRENT config every time
// — the exact bug runResume was already fixed for, just at a sibling call site that was missed.
// This starts a promotion under `approval: comment`, lets it sit waiting (no comment posted
// yet), edits the config to `approval: auto` for the same env, then re-invokes `hoist promote`
// with the identical --from/--to (same digests, so the same deterministic id, so the same state
// file) and confirms it still enforces the ORIGINAL `comment` policy: without any approval
// comment ever posted, the retry must NOT merge just because current config now says `auto`.
func TestPromoteRetryNeverStraddlesPolicyAcrossConfigEdits(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	original := string(data)

	withApprovers := strings.Replace(original,
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    approvers: [alice]\n    envs:\n      approval:\n        app-production: comment\n",
		1)
	if withApprovers == original {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	withApprovers = strings.Replace(withApprovers, "deadline: 10s", "deadline: 2s", 1)
	if err := os.WriteFile(cfgPath, []byte(withApprovers), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected the first promote to stop waiting on approval (no comment posted yet), got success: %s", out.String())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one promotion state, got %d", len(states))
	}
	s := states[0]
	if s.PR == nil {
		t.Fatalf("expected a PR to already be open: %+v", s)
	}
	if s.Approval != "comment" || len(s.Approvers) != 1 || s.Approvers[0] != "alice" {
		t.Fatalf("expected the original comment/alice policy persisted, got Approval=%q Approvers=%v", s.Approval, s.Approvers)
	}

	// The operator edits the config to a WEAKER policy (auto, no comment needed at all) while
	// the promotion is still in flight.
	changedApproval := strings.Replace(withApprovers, "app-production: comment", "app-production: auto", 1)
	if changedApproval == withApprovers {
		t.Fatal("approval replacement point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(changedApproval), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-invoke the EXACT SAME `hoist promote` command — same --from/--to, same digests, so the
	// same deterministic id and the same existing state file — never `hoist resume`. This is
	// runPromote's own retry path, not runResume's.
	out.Reset()
	errOut.Reset()
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("retry should still be waiting on alice's comment (never posted) under the ORIGINAL comment policy, got success: %s", out.String())
	}
	if strings.Contains(out.String(), "merged:") {
		t.Fatalf("retry must not merge without a comment just because config now says auto: %s", out.String())
	}
	if len(f.PRs()) != 1 || f.PRs()[0].Merged {
		t.Fatalf("the PR must still be unmerged: %+v", f.PRs())
	}
}

// TestPromoteNothingToDoIsANoOp exercises runPromote's own "every edit is already a no-op"
// guard directly: after the first promotion's PR is merged conceptually (here: after the
// content already matches, committed AND PUSHED so the clone agrees with origin/main — a real
// merge advances origin, not just the local checkout, and checkCloneCurrentForBase now compares
// against a freshly-fetched origin/<base> unconditionally, so this simulation must actually push
// to be honest), promoting the same pair again with the same source content must report success
// without touching git or the forge at all.
func TestPromoteNothingToDoIsANoOp(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	// Make app-production already match app-staging directly on the clone (simulating the PR
	// having merged), bypassing the engine entirely. Commit AND push it — the honest no-op case
	// is "already current AND the clone agrees with origin", never an uncommitted local edit
	// (that's TestPromoteNoOpRefusesDirtyClone below) nor an unpushed local-only commit (that
	// would be finding 2's own stale case, TestPromoteRefusesUnpushedLocalCommitAheadOfOrigin).
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "simulate the PR having merged")
	runGitHost(t, clone, "push", "-q", "origin", "main")

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "already current") {
		t.Fatalf("stdout should report the no-op: %s", out.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created for a no-op promotion: %+v", f.PRs())
	}
}

// TestPromoteNoOpRefusesDirtyClone is the counterpart to TestPromoteNothingToDoIsANoOp: the
// plan touches app-production's deployment.yaml, and the clone's on-disk copy is edited to
// already carry the planned ref — but the edit is never committed, so what's actually
// committed at --base (main) still has the old digest. Edit.NoOp is computed against the dirty
// working-tree bytes gitops.Discover read, so the plan looks all-NoOp even though it would not
// be relative to base's real committed content. Nothing on the all-NoOp path ever calls
// gitops.Apply/Verify (no worktree exists yet), so this is the one place that must catch it:
// runPromote must refuse with a clear "dirty" error naming the file, not report success.
func TestPromoteNoOpRefusesDirtyClone(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately not committed: the clone is now dirty for this file relative to main.

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit refusing the dirty clone, got 0; stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must not report false success for a plan that is all-NoOp only against dirty content: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "uncommitted") || !strings.Contains(errOut.String(), "cluster/apps/app-production/app/deployment.yaml") {
		t.Fatalf("stderr should name the dirty file: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created: %+v", f.PRs())
	}
}

// TestPromoteDirectCommitsWithoutPR exercises the CLI's own invocation of direct mode
// end-to-end: newPromoteFixture's config sets no envs.production at all, so app-production
// is not listed there and --direct succeeds — pushing straight to origin/main with no PR and
// no branch of the promotion's own name left behind.
func TestPromoteDirectCommitsWithoutPR(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "pushed straight to main (direct mode, no PR)") {
		t.Fatalf("stdout should report direct mode's own outcome:\n%s", out.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("direct mode must never open a PR, got %+v", f.PRs())
	}

	var g git.Exec
	branchLine := strings.Split(out.String(), "\n")[1]
	branch := strings.TrimSpace(strings.TrimPrefix(branchLine, "  branch:"))
	if _, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", branch); err != nil || ok {
		t.Fatalf("no branch named after the promotion should exist on origin: ok=%v err=%v", ok, err)
	}
	if _, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", "main"); err != nil || !ok {
		t.Fatalf("origin/main should exist: ok=%v err=%v", ok, err)
	}
}

// gitShowFile returns rev:path's blob content via `git show`, read-only — never touches dir's
// own checkout, branch, working tree or index (AGENTS.md §4.6), so it is safe to call against a
// clone under test without disturbing what the test is trying to observe.
func gitShowFile(t *testing.T, dir, rev, path string) string {
	t.Helper()
	cmd := exec.Command("git", "show", rev+":"+path)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git show %s:%s: %v", rev, path, err)
	}
	return string(out)
}

// TestPromoteSecondDirectPromotionSeesFirstsPushedContent is finding 1's own regression test
// (round 2 — round 1's fix, adding git.Git.FetchBranch after a direct push, only refreshed
// cloneDir's refs/remotes/origin/main; it never made cloneDir's own disk content — what
// gitops.Discover actually reads — reflect what direct mode had just pushed). After one direct
// promotion lands, cloneDir's own disk is never touched by it (AGENTS.md §4.6: direct mode only
// ever advances refs/remotes/origin/<base>) — a second, independent promotion (a new digest
// arriving on staging) touching the SAME production file must not silently plan against the
// pre-promotion content still sitting on the clone's disk. Here the two promotions target
// different digests, so the resulting plan looks like an entirely ordinary edit rather than a
// no-op — the harder of the finding's two named failure modes, since nothing before
// gitops.Apply/Verify (three steps into the engine) would otherwise have caught the mismatch at
// all. runPromote must refuse clearly, before ever deriving an id, creating a worktree or
// attempting a commit.
func TestPromoteSecondDirectPromotionSeesFirstsPushedContent(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	args := func() []string {
		return []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	}

	var out, errOut bytes.Buffer
	if got := run(args(), &out, &errOut); got != 0 {
		t.Fatalf("first direct promotion: exit %d, want 0; stderr: %s", got, errOut.String())
	}

	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodPath := "cluster/apps/app-production/app/deployment.yaml"
	// Ground the test's own premise: origin/main's production file really did move to the
	// first promotion's digest, pushed straight there with no PR (direct mode) — and never
	// through cloneDir's own checkout at all.
	if got := gitShowFile(t, clone, "origin/main", prodPath); !strings.Contains(got, digestNew) {
		t.Fatalf("origin/main's production file should already carry the first promotion's digest %s:\n%s", digestNew, got)
	}

	// A new image lands on staging — a third digest, distinct from both the fixture's original
	// ("old", still what cloneDir's own disk shows for production — direct mode never rewrites
	// it there) and "new" (what the first promotion already pushed to origin/main).
	digestThird := "sha256:" + strings.Repeat("2", 64)
	stagingFile := filepath.Join(clone, "cluster/apps/app-staging/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v3@" + digestThird + "\n"
	if err := os.WriteFile(stagingFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "staging now runs a third digest")

	out.Reset()
	errOut.Reset()
	got := run(args(), &out, &errOut)
	if got == 0 {
		t.Fatalf("second, independent direct promotion should refuse planning against a stale clone, got exit 0; stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must never report false success against stale content: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "disagrees with") || !strings.Contains(errOut.String(), prodPath) {
		t.Fatalf("stderr should name the stale file and explain why: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should ever be involved in direct mode: %+v", f.PRs())
	}
}

// originURL reads the "origin" remote's URL out of a git dir — used by tests that need to reach
// origin directly (a second, independent clone) without hoist itself ever touching it.
func originURL(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git remote get-url origin: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestPromoteFetchesOriginFreshBeforeTrustingClone is round 5's finding 1 regression test:
// checkCloneCurrentForBase used to trust cloneDir's own refs/remotes/origin/<base> exactly as it
// stood since the initial `git clone` — never fetching it fresh itself — so origin advancing via
// any route OTHER than a prior hoist-driven promotion (which happened to fetch as a side effect
// of its own direct-mode push) went entirely undetected. Here a second, independent clone of the
// SAME origin pushes directly — no hoist command, no fetch, ever touches the primary clone in
// between — and the primary clone's own cached remote-tracking ref is left exactly as stale as
// the day it was cloned. Only this function's own fresh fetch can catch it.
//
// Mutant-verified: removing the g.FetchBranch call at the top of checkCloneCurrentForBase makes
// this test fail — the promotion proceeds (wrongly reporting a branch/PR) instead of refusing,
// because the cached ref still agrees with the clone's own untouched local content.
func TestPromoteFetchesOriginFreshBeforeTrustingClone(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	origin := originURL(t, clone)

	// A second clone — standing in for a colleague, or any other route to origin that isn't
	// this hoist invocation — pushes a change to the very file this promotion will touch,
	// entirely independent of, and unobserved by, the primary clone.
	second := filepath.Join(t.TempDir(), "second-clone")
	runGitHost(t, "", "clone", "-q", origin, second)
	digestAdvanced := "sha256:" + strings.Repeat("3", 64)
	prodPath := "cluster/apps/app-production/app/deployment.yaml"
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v9@" + digestAdvanced + "\n"
	if err := os.WriteFile(filepath.Join(second, prodPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, second, "add", ".")
	runGitHost(t, second, "commit", "-q", "-m", "someone else advances origin directly")
	runGitHost(t, second, "push", "-q", "origin", "main")

	// The primary clone's own cached refs/remotes/origin/main is untouched — nothing here has
	// fetched it since newPromoteFixture's own initial clone.
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit refusing content the fresh fetch should have caught, got 0; stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must never report false success against content only a fresh fetch reveals: %s", out.String())
	}
	if !strings.Contains(errOut.String(), prodPath) {
		t.Fatalf("stderr should name the file the second clone advanced: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created: %+v", f.PRs())
	}
}

// TestPromoteDirectRefusesWhenOriginHasAnOccurrenceLocalDoesNotKnow is
// checkNoMissingOccurrenceAtFreshBase's own regression test (round-N finding,
// "base-advanced-with-new-occurrence"): origin/main gains an entirely new family — a second
// occurrence of a promotable image repo — via a route that never touches the primary clone at
// all (a second, independent clone standing in for another operator's own direct-mode run, or
// any other route to origin that bypasses this checkout entirely). The primary clone's own
// local disk has no idea this family exists at all: gitops.Discover reading it never sees the
// file, so plan.Edits never mentions it, and checkCloneCurrentForBase — which only validates
// files plan.Edits already names — has nothing to compare it against either. Without this
// check, direct mode would silently promote only the family it already knew about and leave the
// new one on its old image, violating AGENTS.md principle 3's "every occurrence" contract. With
// it, the whole run refuses outright, naming the file, rather than silently under-promoting.
//
// Mutant-verified: removing the checkNoMissingOccurrenceAtFreshBase call in runPromote makes
// this test fail — the promotion proceeds and reports success while app2 is never touched.
func TestPromoteDirectRefusesWhenOriginHasAnOccurrenceLocalDoesNotKnow(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	origin := originURL(t, clone)

	// A second, independent clone adds a brand-new family under the SAME promotable prefix and
	// pushes it straight to origin — never through the primary clone.
	second := filepath.Join(t.TempDir(), "second-clone")
	runGitHost(t, "", "clone", "-q", origin, second)
	digestApp2Staging := "sha256:" + strings.Repeat("9", 64)
	digestApp2Prod := "sha256:" + strings.Repeat("8", 64)
	wrapper := func(env string) string {
		return "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app2-" + env + "\n  namespace: argocd\n" +
			"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/" + env + "/app2\n" +
			"  destination:\n    server: https://kubernetes.default.svc\n    namespace: " + env + "\n"
	}
	deployment := func(ref string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app2\nspec:\n  template:\n    spec:\n      containers:\n        - name: app2\n          image: " + ref + "\n"
	}
	write := func(rel, content string) {
		p := filepath.Join(second, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-staging-app2.yaml", wrapper("app-staging"))
	write("cluster/apps/app-production-app2.yaml", wrapper("app-production"))
	write("cluster/apps/app-staging/app2/deployment.yaml", deployment("ghcr.io/example/app2:v2@"+digestApp2Staging))
	write("cluster/apps/app-production/app2/deployment.yaml", deployment("ghcr.io/example/app2:v1@"+digestApp2Prod))
	runGitHost(t, second, "add", ".")
	runGitHost(t, second, "commit", "-q", "-m", "add app2, straight to origin, bypassing the primary clone")
	runGitHost(t, second, "push", "-q", "origin", "main")

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected refusal — origin has an occurrence (app2) the primary clone's local disk never knew about; got exit 0, stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must never report false success while silently missing an occurrence: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "app2") {
		t.Fatalf("stderr should name the missing occurrence's file (app2): %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should ever be involved in direct mode: %+v", f.PRs())
	}
}

// TestPromoteRefusesUnpushedLocalCommitAheadOfOrigin is round 5's finding 2 regression test: the
// clone's local main branch carries a commit changing the production file that origin does NOT
// have (an ordinary unpushed local commit — no direct-mode promotion involved at all). The
// previous checkCloneCurrentForBase trusted local's bytes whenever local was ahead of (or equal
// to) origin, unconditionally — but pkg/git.Exec.Worktree's own resolveBase prefers origin/<base>
// over the local branch whenever that ref exists, regardless of which side is ahead, so the
// worktree a real promotion would actually commit into is seeded from origin — which does not
// have this unpushed change. Planning from local's ahead-of-origin bytes is exactly the mismatch
// this check exists to catch.
//
// Mutant-verified: reintroducing the old "local is ahead/caught up, trust its bytes
// unconditionally" branch makes this test fail — the promotion proceeds (or fails later with a
// confusing gitops.Apply mismatch) instead of refusing clearly up front.
func TestPromoteRefusesUnpushedLocalCommitAheadOfOrigin(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	prodPath := "cluster/apps/app-production/app/deployment.yaml"
	digestLocalAhead := "sha256:" + strings.Repeat("4", 64)
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v9@" + digestLocalAhead + "\n"
	if err := os.WriteFile(filepath.Join(clone, prodPath), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "an ordinary unpushed local commit")
	// Deliberately never pushed: origin/main still shows the fixture's original digest.

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit refusing to plan from an unpushed local-ahead commit, got 0; stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must never report false success while local is ahead of origin on the planned file: %s", out.String())
	}
	// The refusal must come from checkCloneCurrentForBase itself, up front — not from a worktree
	// ever being created and gitops.Apply discovering the mismatch three steps later. The two
	// are distinguishable: only checkCloneCurrentForBase's own message names the file alongside
	// "disagrees with ... worktree is actually built from"; engine.CommittedStep's Apply failure
	// instead says "the file changed after the plan was built" and no branch/worktree name ever
	// should have been derived at all.
	if !strings.Contains(errOut.String(), prodPath) || !strings.Contains(errOut.String(), "disagrees with") {
		t.Fatalf("stderr should be checkCloneCurrentForBase's own clear, up-front refusal naming the file: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), "changed after the plan was built") {
		t.Fatalf("must refuse before ever reaching gitops.Apply, not rely on its downstream mismatch error: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created: %+v", f.PRs())
	}
}

// TestPromoteDirectResumeRecognizesOwnPriorPush is the resume-safety carve-out's own regression
// test at the cmd/hoist level (the carve-out's mechanism moved with round 5's redesign, but the
// property it protects — a killed-and-resumed direct-mode promotion recognizing its own prior
// success rather than refusing itself as foreign drift — must still hold): running the identical
// direct-mode promotion twice must succeed both times and never push a second, redundant commit.
// cloneDir's own disk is never advanced by direct mode (it only ever moves origin/<base>), so the
// second run's plan is built from the same "stale" disk content as the first — the carve-out is
// exactly what recognizes that applying this promotion's own edits to that content reproduces
// origin's current tip exactly, rather than mistaking it for someone else's drift.
func TestPromoteDirectResumeRecognizesOwnPriorPush(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("first direct promotion: exit %d, want 0; stderr: %s", got, errOut.String())
	}

	var g git.Exec
	sha1, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", "main")
	if err != nil || !ok {
		t.Fatalf("origin/main should exist after the first push: ok=%v err=%v", ok, err)
	}

	// Re-running the identical promotion — simulating a killed-and-resumed process, or simply a
	// re-invocation after the fact — must recognize origin/main already carries exactly this
	// promotion's own content, not refuse it as foreign drift, and must not push a second,
	// redundant commit.
	out.Reset()
	errOut.Reset()
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("resumed direct promotion: exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "pushed straight to main (direct mode, no PR)") {
		t.Fatalf("resumed run should report the same direct-mode outcome: %s", out.String())
	}
	sha2, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", "main")
	if err != nil || !ok || sha2 != sha1 {
		t.Fatalf("resumed run should not move origin/main again: %s -> %s (ok=%v err=%v)", sha1, sha2, ok, err)
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("direct mode must never open a PR, got %+v", f.PRs())
	}
}

// TestPromoteDirectRefusedForConfiguredProductionEnv is the CLI-level counterpart to
// internal/engine's own mandatory adversarial test: with envs.production naming
// app-production, --direct --confirm-direct must still be refused — the flags alone are not
// the gate, internal/engine.DirectCommitGateStep is.
func TestPromoteDirectRefusedForConfiguredProductionEnv(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withProd := strings.Replace(string(data), "    promotable: [ghcr.io/example/]\n", "    promotable: [ghcr.io/example/]\n    envs:\n      production: [app-production]\n", 1)
	if err := os.WriteFile(cfgPath, []byte(withProd), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit refusing production, got 0; stdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "app-production") || !strings.Contains(errOut.String(), "envs.production") {
		t.Fatalf("stderr should name the env and cite envs.production: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created either: %+v", f.PRs())
	}
}

// TestPromoteDirectRefusedForConfiguredProductionEnvEvenWhenNoOp is finding 7's own regression
// test (round N, Codex P2): the production gate used to be constructed only after BuildPlan and
// the all-no-op fast path, so a --direct run against a production env whose plan happened to
// already be current (TestPromoteNothingToDoIsANoOp's own setup: app-production's committed
// content already matches app-staging's, pushed to origin) exited 0 claiming the no-op success
// message without ever being refused. Direct mode must refuse a production env outright
// (AGENTS.md §4.5) regardless of whether there would have been anything left to write.
func TestPromoteDirectRefusedForConfiguredProductionEnvEvenWhenNoOp(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withProd := strings.Replace(string(data), "    promotable: [ghcr.io/example/]\n", "    promotable: [ghcr.io/example/]\n    envs:\n      production: [app-production]\n", 1)
	if err := os.WriteFile(cfgPath, []byte(withProd), 0o644); err != nil {
		t.Fatal(err)
	}

	// Make app-production already match app-staging, committed AND pushed — exactly
	// TestPromoteNothingToDoIsANoOp's own no-op setup — so the plan built from it is all-NoOp.
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "simulate the PR having merged")
	runGitHost(t, clone, "push", "-q", "origin", "main")

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit refusing production even though the plan is a no-op, got 0; stdout: %s", out.String())
	}
	if strings.Contains(out.String(), "already current") {
		t.Fatalf("must not report the no-op success message for a refused production target: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "app-production") || !strings.Contains(errOut.String(), "envs.production") {
		t.Fatalf("stderr should name the env and cite envs.production: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("no PR should have been created either: %+v", f.PRs())
	}
}

// TestPromoteDirectRequiresConfirmFlag: --direct alone, without --confirm-direct, must be
// refused as a usage error rather than silently proceeding.
func TestPromoteDirectRequiresConfirmFlag(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct"}
	var errOut bytes.Buffer
	got := run(args, io.Discard, &errOut)
	if got != exitUsage {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "--confirm-direct") {
		t.Errorf("stderr should name the missing flag: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("nothing should have run at all: %+v", f.PRs())
	}
}

// TestPromoteDirectRequiresConfirmValueToMatchTo is finding 8's own mandatory test: a
// --confirm-direct value that doesn't equal --to exactly must be refused, even though --direct
// itself would otherwise be allowed (newPromoteFixture's config sets no envs.production).
func TestPromoteDirectRequiresConfirmValueToMatchTo(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-staging"}
	var errOut bytes.Buffer
	got := run(args, io.Discard, &errOut)
	if got != exitUsage {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "app-staging") || !strings.Contains(errOut.String(), "app-production") {
		t.Errorf("stderr should name both the mismatched value and --to's real value: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("nothing should have run at all: %+v", f.PRs())
	}
}

func TestPromoteRequiresGitHubConfig(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	noGitHub := strings.Replace(string(data), "    github: example/gitops\n", "", 1)
	if err := os.WriteFile(cfgPath, []byte(noGitHub), 0o644); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}, io.Discard, &errOut)
	if got != exitUsage {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "github") {
		t.Errorf("stderr should mention the missing github config: %s", errOut.String())
	}
}

// TestPromoteDirectRequiresGitHubConfig is finding 1's own mandatory test: a repo configured
// without github: must be refused for --direct too, not only for the non-direct/PR path above
// — engine.DeriveID hashes eff.cfg.GitHub into the promotion's id, which names the state path,
// branch and worktree directory; two repos both configured without github: that promote the
// same env+digest set would otherwise derive the identical id and collide.
func TestPromoteDirectRequiresGitHubConfig(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	noGitHub := strings.Replace(string(data), "    github: example/gitops\n", "", 1)
	if err := os.WriteFile(cfgPath, []byte(noGitHub), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	var errOut bytes.Buffer
	got := run(args, io.Discard, &errOut)
	if got != exitUsage {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "github") {
		t.Errorf("stderr should mention the missing github config: %s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("nothing should have run at all: %+v", f.PRs())
	}
}

// secretLeakGit wraps a real git.Git, failing Worktree with an error that embeds a value the
// caller has registered with pkg/redact — standing in for a git hook or the 1Password signing
// helper echoing a registered credential to stderr, which pkg/git's own error wrapping folds
// into the error text verbatim.
type secretLeakGit struct {
	git.Git
	secret string
}

func (g secretLeakGit) Worktree(_ context.Context, _, _, _, _ string) error {
	return fmt.Errorf("git worktree add: exit status 1: hook printed %s to stderr", g.secret)
}

// TestPromoteRedactsRegisteredSecretInDriverError is Finding B's CLI-side sink: a step's Act
// error that embeds a value already registered with pkg/redact must never reach the terminal
// unscrubbed, even though pkg/git already does its own best-effort scrubbing where it can (this
// is the final boundary, same pattern as internal/app/plan/model.go's View()).
func TestPromoteRedactsRegisteredSecretInDriverError(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	const secret = "sekrit-finding-b-cli-token-value"
	redact.Register(secret)
	newGit = secretLeakGit{Git: git.Exec{}, secret: secret}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	got := run(args, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected a non-zero exit for a failing driver step, got 0; stdout: %s", out.String())
	}
	if strings.Contains(errOut.String(), secret) {
		t.Fatalf("stderr leaked the registered secret verbatim: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), redact.Redacted) {
		t.Fatalf("stderr should carry the redaction marker, got: %s", errOut.String())
	}
}
