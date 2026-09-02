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

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
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
	yaml := fmt.Sprintf("repos:\n  - name: gitops\n    path: %s\n    github: example/gitops\n    apps_root: cluster/apps\n    promotable: [ghcr.io/example/]\n", clone)
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

func TestPromoteEndToEndThenResumeIsIdempotent(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "branch: hoist/app-production/") || !strings.Contains(out.String(), "PR: ") {
		t.Fatalf("stdout missing expected fields:\n%s", out.String())
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("expected exactly one PR, got %d", len(f.PRs()))
	}
	firstPR := f.PRs()[0].Number

	var g git.Exec
	branchLine := strings.Split(out.String(), "\n")[1]
	branch := strings.TrimSpace(strings.TrimPrefix(branchLine, "  branch:"))
	sha1, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", branch)
	if err != nil || !ok {
		t.Fatalf("origin should have the branch: ok=%v err=%v", ok, err)
	}

	// Re-running the exact same command (simulating a resumed/re-invoked process) must not
	// create a second branch, commit or PR.
	out.Reset()
	errOut.Reset()
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("second run exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 1 || f.PRs()[0].Number != firstPR {
		t.Fatalf("second run should reuse PR #%d, got %+v", firstPR, f.PRs())
	}
	sha2, ok, err := g.LsRemoteBranch(context.Background(), clone, "origin", branch)
	if err != nil || !ok || sha2 != sha1 {
		t.Fatalf("second run should not move the branch: %s -> %s (ok=%v err=%v)", sha1, sha2, ok, err)
	}
}

// TestPromoteNothingToDoIsANoOp exercises runPromote's own "every edit is already a no-op"
// guard directly: after the first promotion's PR is merged conceptually (here: after the
// content already matches, committed so the clone is clean relative to --base), promoting the
// same pair again with the same source content must report success without touching git or
// the forge at all.
func TestPromoteNothingToDoIsANoOp(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	// Make app-production already match app-staging directly on the clone (simulating the PR
	// having merged), bypassing the engine entirely. Commit it — the honest no-op case is
	// "already current AND the clone is clean", never an uncommitted local edit (that's
	// TestPromoteNoOpRefusesDirtyClone below).
	digestNew := "sha256:" + strings.Repeat("1", 64)
	prodFile := filepath.Join(clone, "cluster/apps/app-production/app/deployment.yaml")
	content := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2@" + digestNew + "\n"
	if err := os.WriteFile(prodFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, clone, "add", ".")
	runGitHost(t, clone, "commit", "-q", "-m", "simulate the PR having merged")

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
