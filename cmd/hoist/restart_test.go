package main

import (
	"bytes"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// restartRolloutFor repoints newRollout at a fake reporting the fixture's existing image as
// live and complete. A restart changes no image, so — unlike a deploy — what RolledOutStep is
// waiting for is exactly what was already running.
func restartRolloutFor(t *testing.T) {
	t.Helper()
	const deployEnv = "app-production"
	f := &rollout.Fake{}
	f.SetDeployment(deployEnv, "app", rollout.DeploymentStatus{
		Namespace: deployEnv,
		Name:      "app",
		Images:    []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v1@" + strings.Repeat("0", 64)}},
		Complete:  true,
	})
	prev := newRollout
	newRollout = func(string) (rollout.Rollout, string, error) { return f, "test-context", nil }
	t.Cleanup(func() { newRollout = prev })
}

// The dry run writes nothing and says what it would do — including, for a Deployment hoist has
// never restarted, that there is no previous stamp to supersede.
func TestRestartDryRunWritesNothing(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart",
		"--env", "app-production", "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	for _, want := range []string{"restart app-production", "Deployment app", gitops.RestartAnnotation, "never restarted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run missing %q:\n%s", want, out.String())
		}
	}
	// A restart is not an image change, and the dry run must not pretend otherwise.
	if strings.Contains(out.String(), "sha256:") {
		t.Errorf("a restart's dry run should show no image at all:\n%s", out.String())
	}
	if st := gitStatusPorcelain(t, clone); st != "" {
		t.Errorf("a dry run must not touch the checkout:\n%s", st)
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Errorf("a dry run must not write state, got %d", len(states))
	}
}

// The end-to-end shape: a restart drives the same pipeline a promotion does, and lands a commit
// whose only change is the pod-template annotation.
func TestRestartEndToEndDrivesTheSamePipeline(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	restartRolloutFor(t)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
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
	s := states[0]

	// The artifacts must describe a restart. Rendered from the same templates, so this is the
	// variant reaching them, not a second set of strings.
	if !strings.Contains(s.PRTitle, "restart") || strings.Contains(s.PRTitle, "promote") {
		t.Errorf("PR title should describe a restart, got %q", s.PRTitle)
	}
	if !strings.Contains(s.PRBody, gitops.RestartAnnotation) {
		t.Errorf("the PR body should say what it writes:\n%s", s.PRBody)
	}
	if strings.Contains(s.CommitMessage, "promote") {
		t.Errorf("commit message describes a promotion: %q", s.CommitMessage)
	}
	if s.SourceEnv != "" {
		t.Errorf("SourceEnv = %q, want empty: a restart has no source env", s.SourceEnv)
	}
	if len(s.Edits) != 0 {
		t.Errorf("a restart changes no image, got %d edits", len(s.Edits))
	}
	if len(s.Restarts) == 0 {
		t.Fatal("the state should carry the planned restarts")
	}

	// And what actually landed: the annotation, and nothing else.
	runGitHost(t, clone, "pull", "-q", "--ff-only")
	got := gitShowFile(t, clone, "HEAD", s.Restarts[0].File)
	if !strings.Contains(got, gitops.RestartAnnotation) {
		t.Errorf("the merged manifest has no restart annotation:\n%s", got)
	}
	if !strings.Contains(got, "ghcr.io/example/app:v1@") {
		t.Errorf("the image should be untouched by a restart:\n%s", got)
	}
}

// Two restarts of the same env are two different promotions. A promotion's identity is the end
// state it lands, so re-running one resumes it; a restart's whole point is to happen again, so
// re-running one must not be mistaken for the first (engine.DeriveID).
func TestTwoRestartsAreDifferentPromotions(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	first, err := gitops.BuildRestartPlan(r, "app-production", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gitops.BuildRestartPlan(r, "app-production", nil, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	a := engine.DeriveID("example/gitops", first)
	b := engine.DeriveID("example/gitops", second)
	if a == b {
		t.Fatalf("two restarts a minute apart derived the same id %q: re-running would resume the first instead of restarting again", a)
	}
	// Same instant, same id — the determinism every other variant relies on for resume is
	// unchanged, so a killed restart still resumes rather than starting a second one.
	again, err := gitops.BuildRestartPlan(r, "app-production", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine.DeriveID("example/gitops", again); got != a {
		t.Errorf("the same restart derived two ids, %q and %q: it would not resume", a, got)
	}
}

// A restart's id must not collide with a promotion's into the same env, whatever that env is
// called — the discriminator is separated by a byte no env name can contain.
func TestRestartIdDoesNotCollideWithAPromotion(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	restart, err := gitops.BuildRestartPlan(r, "app-production", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	promote, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if engine.DeriveID("example/gitops", restart) == engine.DeriveID("example/gitops", promote) {
		t.Error("a restart and a promotion into the same env derived the same id")
	}
}

// --family narrows what is restarted, and naming one that does not exist is refused rather
// than silently restarting everything.
func TestRestartFamilySelection(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	all, err := gitops.BuildRestartPlan(r, "app-production", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Restarts) < 2 {
		t.Skipf("fixture has %d Deployments in app-production; need 2+ to prove narrowing", len(all.Restarts))
	}
	one, err := gitops.BuildRestartPlan(r, "app-production", []string{all.Restarts[0].Name}, time.Now())
	if err != nil {
		// The family name and the Deployment name need not match; fall back to a real family.
		var fam string
		for name := range r.Envs["app-production"].Families {
			fam = name
			break
		}
		if one, err = gitops.BuildRestartPlan(r, "app-production", []string{fam}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if len(one.Restarts) >= len(all.Restarts) {
		t.Errorf("--family should narrow the plan: %d of %d", len(one.Restarts), len(all.Restarts))
	}

	if _, err := gitops.BuildRestartPlan(r, "app-production", []string{"no-such-family"}, time.Now()); err == nil {
		t.Error("naming a family that does not exist must be refused, not silently ignored")
	}
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitHostCmd(dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestRestartWatchesItsOwnRollout is Copilot's PR #79 finding, and it was the worst one in the
// stack: RolledOutStep builds its workload set from s.Edits, a restart has none, so the step
// found nothing to check and reported complete without asking the cluster a single question.
// `hoist restart` would print success for a rollout nobody watched — on the one command whose
// entire purpose is the rollout.
func TestRestartWatchesItsOwnRollout(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)

	// A Deployment that never finishes rolling. If the step observes it at all, the run cannot
	// report success; if it does not, it will.
	f := &rollout.Fake{}
	f.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Namespace: "app-production",
		Name:      "app",
		Images:    []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v1@" + strings.Repeat("0", 64)}},
		Complete:  false,
		Detail:    "1 of 2 updated replicas are available",
	})
	prev := newRollout
	newRollout = func(string) (rollout.Rollout, string, error) { return f, "test-context", nil }
	t.Cleanup(func() { newRollout = prev })

	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut)
	if got == 0 {
		t.Fatalf("a restart whose Deployment never completes must not report success:\nstdout: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "updated replicas") {
		t.Errorf("the failure should name what it was still waiting for:\n%s", errOut.String())
	}
}

// An interrupted restart is resumed by id, not by re-running: re-running writes a new timestamp
// and therefore derives a new promotion, starting a second restart rather than picking the
// first back up. The advice has to say so (Copilot, PR #79).
func TestInterruptedRestartIsResumedById(t *testing.T) {
	s := &engine.PromotionState{ID: "abc123", Restarts: []gitops.RestartEdit{{File: "x.yaml", Name: "web"}}}
	if got := resumeAdvice(s); !strings.Contains(got, "hoist resume abc123") {
		t.Errorf("a restart must be resumed by id, got %q", got)
	}
	// A promotion's identity is the end state it lands, so re-running really does resume it.
	if got := resumeAdvice(&engine.PromotionState{ID: "abc123"}); got != "re-run to resume" {
		t.Errorf("a promotion resumes by re-running, got %q", got)
	}
}

// Two restarts in the same second with different --family selections are different operations
// and must not derive the same id — the timestamp alone has second resolution, so the workload
// set is part of the identity too (Copilot, PR #79).
func TestRestartsOfDifferentFamiliesInOneSecondDiffer(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	all, err := gitops.BuildRestartPlan(r, "app-production", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	var families []string
	for name := range r.Envs["app-production"].Families {
		families = append(families, name)
	}
	sort.Strings(families)
	var ids []string
	for _, fam := range families {
		pl, err := gitops.BuildRestartPlan(r, "app-production", []string{fam}, at)
		if err != nil {
			continue // a family with no Deployment has nothing to restart
		}
		ids = append(ids, engine.DeriveID("example/gitops", pl))
	}
	if len(ids) < 2 {
		t.Skipf("fixture has %d restartable families; need 2+", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("two families restarted in the same second derived the same id %q", id)
		}
		seen[id] = true
	}
	if seen[engine.DeriveID("example/gitops", all)] {
		t.Error("restarting one family derived the same id as restarting the whole env")
	}
}
