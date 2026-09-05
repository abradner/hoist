package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/rollout"
)

// digestThird is a reference neither env in the fixture runs — a genuinely new build, which is
// what a deploy is for. digestOld/digestNew both already appear in the fixture, so using either
// would let a deploy pass by accidentally matching a promotion's outcome.
const digestThird = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// rolloutFor repoints newRollout at a fake that reports the deployed image as live, so
// RolledOutStep can converge. newPromoteFixture configures its own rollout fake for the image
// a *promotion* would land (v2); a deploy names its own image, so the expectation has to move
// with it — otherwise the pipeline runs correctly all the way to rolled-out and then waits out
// the deadline for an image the fake will never report.
func rolloutFor(t *testing.T, image string) {
	t.Helper()
	// Every deploy test targets app-production — the fixture's only env with an Argo
	// Application wired up. Named here rather than passed so the call sites stay short.
	const deployEnv = "app-production"
	f := &rollout.Fake{}
	f.SetDeployment(deployEnv, "app", rollout.DeploymentStatus{
		Namespace: deployEnv,
		Name:      "app",
		Images:    []rollout.ContainerImage{{Name: "app", Image: image}},
		Complete:  true,
	})
	prev := newRollout
	newRollout = func(string) (rollout.Rollout, string, error) { return f, "test-context", nil }
	t.Cleanup(func() { newRollout = prev })
}

// The end-to-end shape: a deploy drives the same pipeline a promotion does — worktree, commit,
// push, PR, CI, approval, merge, Argo, rollout — but for an image named on the command line
// rather than one read out of a source env.
func TestDeployEndToEndDrivesTheSamePipeline(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	args := []string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
	}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	for _, want := range []string{"branch: hoist/app-production/", "PR: ", "merged: "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("expected exactly one PR, got %d", len(f.PRs()))
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one state file, got %d", len(states))
	}

	// The artifacts must describe a deploy. This is the whole reason the variant exists: the
	// workaround it replaces (promote --from <some env> --digest ...) produced a PR that said
	// "promote A -> B" for a build that was in neither env. PRTitle/PRBody on the state are
	// exactly the strings handed to CreatePR, so asserting here needs no new fake accessor.
	if title := states[0].PRTitle; strings.Contains(title, "promote") ||
		!strings.Contains(title, "deploy") || !strings.Contains(title, "app-production") {
		t.Errorf("PR title should describe a deploy into the env, got %q", title)
	}
	if body := states[0].PRBody; strings.Contains(body, "hoist promotes") {
		t.Errorf("PR body describes a promotion:\n%s", body)
	}
	if msg := states[0].CommitMessage; strings.Contains(msg, "promote") {
		t.Errorf("commit message describes a promotion: %q", msg)
	}
	if got := states[0].SourceEnv; got != "" {
		t.Errorf("SourceEnv = %q, want empty: a deploy has no source env", got)
	}
	if states[0].TargetEnv != "app-production" {
		t.Errorf("TargetEnv = %q", states[0].TargetEnv)
	}
}

// Re-running the same deploy resumes rather than starting a second one — the deterministic id
// (§4.1) keyed on target env plus resulting refs, which a deploy produces just as a promotion
// does.
func TestDeployIsIdempotentOnRerun(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	args := []string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
	}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("first run: exit %d; stderr: %s", got, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("second run: exit %d; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 1 {
		t.Errorf("a re-run must not open a second PR, got %d", len(f.PRs()))
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Errorf("a re-run must not create a second promotion state, got %d", len(states))
	}
}

// An unpinned --image is refused rather than resolved. A promotion can fall back to the source
// env's pods/manifest/registry for a digest; a deploy has no source, so invariant 1 leaves
// nothing to do but refuse.
func TestDeployRefusesAnUnpinnedImage(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production", "--image", "ghcr.io/example/app:v3"}, &out, &errOut)
	if got == 0 {
		t.Fatalf("accepted a bare tag: %s", out.String())
	}
	if len(f.PRs()) != 0 {
		t.Errorf("nothing should have been opened: %+v", f.PRs())
	}
}

// --dry-run prints the diff and touches nothing, matching `hoist plan --dry-run`.
func TestDeployDryRunWritesNothing(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
		"--dry-run"}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "hoist deploy:") {
		t.Errorf("dry run should print a deploy header:\n%s", out.String())
	}
	if len(f.PRs()) != 0 {
		t.Errorf("a dry run must not open a PR: %+v", f.PRs())
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Errorf("a dry run must not write state, got %d", len(states))
	}
}

// A deploy has no source env, so the shared success line must not render the promotion's
// "A -> B" with a hole where the source would be. Same defect class the templates and
// printPlan were reworked for; this is the most visible line hoist prints.
func TestDeploySuccessLineHasNoEmptySourceEnv(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	first := strings.SplitN(out.String(), "\n", 2)[0]
	// No arrow at all, not merely no double space. An earlier version of this test looked for
	// the whitespace and passed on "hoist deploy: -> app-production", which still renders the
	// hole this test exists to catch: a deploy is not a movement between two places, so there
	// is nothing for an arrow to point away from.
	if strings.Contains(first, "->") {
		t.Errorf("a deploy's success line has no source env, so it has no arrow: %q", first)
	}
	if !strings.Contains(first, "app-production") {
		t.Errorf("success line should name the target env: %q", first)
	}
	// A promotion's does, and must keep it — the fix must not flatten both shapes into one.
	if got := promoteSuccessLine(t); !strings.Contains(got, "->") {
		t.Errorf("a promotion still moves between two envs and keeps its arrow: %q", got)
	}
}

// Direct mode converges through Argo and rollout, not just to the push. The design always said
// so ("Pushed -> ArgoRefreshed -> ..."), but every M5 step gated on MergeSHA — which a direct
// push never produces — so DirectSteps stopped at the push and nothing ever told Argo the commit
// had landed (issue #66). PromotionState.LandedSHA now resolves to PushedSHA for a direct
// promotion and MergeSHA for a PR one, which is what let the same three steps serve both.
func TestDeployDirectConvergesThroughArgoAndRollout(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withEnvs := strings.Replace(string(data),
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    envs:\n      production: [somewhere-else]\n",
		1)
	if withEnvs == string(data) {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(withEnvs), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
		"--direct", "--confirm-direct", "app-production"}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("direct mode must not open a PR: %+v", f.PRs())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected one state, got %d", len(states))
	}
	s := states[0]
	if !s.Direct {
		t.Error("DirectPushedStep should have recorded Direct on the state")
	}
	if s.MergeSHA != "" {
		t.Errorf("a direct promotion never merges, got MergeSHA %q", s.MergeSHA)
	}
	// The heart of it: the landing anchor resolves, and the run reached the last M5 step
	// rather than stopping at the push.
	if s.LandedSHA() == "" || s.LandedSHA() != s.PushedSHA {
		t.Errorf("LandedSHA should be the pushed commit for a direct promotion: Landed=%q Pushed=%q",
			s.LandedSHA(), s.PushedSHA)
	}
	if s.Phase != engine.StepRolledOut {
		t.Errorf("Phase = %q, want %q: direct mode must converge through Argo and rollout, not stop at the push",
			s.Phase, engine.StepRolledOut)
	}
}

// DeriveID deliberately gives a deploy and a promotion the same id when they land identical
// refs in the same env (identity.go): same end state, same promotion, so re-running either
// resumes the other. The hazard the self-review found is narrower — the rendered artifacts. A
// promote re-run against a deploy's id used to re-render CommitMessage/PRTitle/PRBody from its
// own promotion-shaped plan and overwrite the state file, leaving it narrating a promotion
// while the commit and PR that actually exist still said deploy.
func TestPromoteDoesNotRewriteADeploysArtifactsOnIdCollision(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	// The same ref app-staging runs in the fixture, so a deploy of it into app-production
	// lands exactly what a staging->production promotion would — which is what makes the ids
	// collide (newPromoteFixture builds this value the same way).
	digestNew := "sha256:" + strings.Repeat("1", 64)
	rolloutFor(t, "ghcr.io/example/app:v2@"+digestNew)

	// A deploy of exactly what a staging->production promotion would land.
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v2@" + digestNew}, &out, &errOut); got != 0 {
		t.Fatalf("deploy: exit %d; stderr: %s", got, errOut.String())
	}
	before, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("expected one state, got %d", len(before))
	}
	if !strings.Contains(before[0].PRTitle, "deploy") {
		t.Fatalf("precondition: the deploy's own title should say deploy, got %q", before[0].PRTitle)
	}

	// The promotion that collides on id.
	out.Reset()
	errOut.Reset()
	run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}, &out, &errOut)

	after, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("the collision must not create a second state, got %d", len(after))
	}
	if after[0].ID != before[0].ID {
		t.Fatalf("precondition: ids should collide; got %q then %q", before[0].ID, after[0].ID)
	}
	if after[0].PRTitle != before[0].PRTitle {
		t.Errorf("the promote re-run rewrote the deploy's PR title:\n before: %q\n after:  %q",
			before[0].PRTitle, after[0].PRTitle)
	}
	if after[0].CommitMessage != before[0].CommitMessage {
		t.Errorf("the promote re-run rewrote the deploy's commit message")
	}
}

// digestFourth is a second genuinely-new build, for the follow-up deploy the first must not
// have locked out.
const digestFourth = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

// TestDirectRunDoesNotLockOutLaterPromotionsForTheEnv is the regression test for the worst bug
// in direct mode's convergence work: every caller that asks "is another promotion still running
// for this env?" observed a fixed PR-path step list, which a direct state can never satisfy — it
// pushes to the base branch, so there is no promotion branch on origin, no PR, and no merge, for
// ever. One completed direct deploy therefore made findInFlight refuse EVERY later promotion
// into that env permanently, and left the finished run listed as in flight by `hoist promotions`.
//
// Asserted through the CLI rather than against findInFlight directly, because the bug was in
// which step list three separate callers chose, not in the scan itself.
func TestDirectRunDoesNotLockOutLaterPromotionsForTheEnv(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withEnvs := strings.Replace(string(data),
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    envs:\n      production: [somewhere-else]\n",
		1)
	if withEnvs == string(data) {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(withEnvs), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
		"--direct", "--confirm-direct", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("first (direct) deploy: exit %d; stderr: %s", got, errOut.String())
	}
	// What the operator does after a direct push: bring their own clone up to the base branch
	// it just moved. Without this the next run is refused for clone staleness — correctly, and
	// for a different reason than the one under test, which would mask it.
	runGitHost(t, clone, "pull", "-q", "--ff-only")

	// `hoist promotions` re-observes every state file. The direct run finished; it must not
	// still be listed as in flight.
	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "promotions"}, &out, &errOut); got != 0 {
		t.Fatalf("promotions: exit %d; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "done") {
		t.Errorf("a converged direct promotion must list as done:\n%s", out.String())
	}

	// And a genuinely different promotion into the same env must be allowed to start.
	rolloutFor(t, "ghcr.io/example/app:v4@"+digestFourth)
	out.Reset()
	errOut.Reset()
	got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v4@" + digestFourth}, &out, &errOut)
	if got != 0 {
		t.Fatalf("a later deploy into the same env was refused after a direct run: exit %d\nstdout: %s\nstderr: %s",
			got, out.String(), errOut.String())
	}
	if strings.Contains(errOut.String(), "in flight") {
		t.Errorf("the finished direct run was treated as in flight:\n%s", errOut.String())
	}
}

// promoteSuccessLine drives a plain promotion through the same fixture and returns its first
// stdout line, so the deploy assertion above can be stated as an asymmetry rather than as an
// absence — an absence alone would also pass if the line stopped rendering entirely.
func promoteSuccessLine(t *testing.T) string {
	t.Helper()
	cfgPath, _, _ := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "promote",
		"--from", "app-staging", "--to", "app-production",
		"--digest-sources", "none"}, &out, &errOut); got != 0 {
		t.Fatalf("promote: exit %d; stderr: %s", got, errOut.String())
	}
	return strings.SplitN(out.String(), "\n", 2)[0]
}

// TestDeployDryRunNeedsNoForgeConfig: --dry-run's promise is that it touches nothing, and a
// non-direct dry run opens no PR and derives no promotion id — so demanding repos[].github
// refused a read-only command for a reason that could not apply to it, while `hoist plan
// --dry-run` (the same operation for a promotion) has never demanded one (Copilot, PR #70).
//
// --direct keeps the whole gate even under --dry-run: a dry run of something that would be
// refused outright should say so, which the second half asserts.
func TestDeployDryRunNeedsNoForgeConfig(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	noGitHub := strings.Replace(string(data), "    github: example/gitops\n", "", 1)
	if noGitHub == string(data) {
		t.Fatal("fixture config shape changed; github line not found")
	}
	if err := os.WriteFile(cfgPath, []byte(noGitHub), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
		"--dry-run"}, &out, &errOut)
	if got != 0 {
		t.Fatalf("a read-only dry run must not require a forge identity: exit %d; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "v3") {
		t.Errorf("the dry run should still print the plan:\n%s", out.String())
	}

	// The direct gate is not relaxed with it.
	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "deploy",
		"--env", "app-production",
		"--image", "ghcr.io/example/app:v3@" + digestThird,
		"--dry-run", "--direct", "--confirm-direct", "app-production"}, &out, &errOut); got == 0 {
		t.Errorf("a direct dry run with no forge identity must still be refused:\n%s", out.String())
	}
}
