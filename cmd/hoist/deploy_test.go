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
func rolloutFor(t *testing.T, env, image string) {
	t.Helper()
	f := &rollout.Fake{}
	f.SetDeployment(env, "app", rollout.DeploymentStatus{
		Namespace: env,
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
	rolloutFor(t, "app-production", "ghcr.io/example/app:v3@"+digestThird)
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
	rolloutFor(t, "app-production", "ghcr.io/example/app:v3@"+digestThird)
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
