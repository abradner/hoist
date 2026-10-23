package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

// productionConfig rewrites the promote fixture's config so app-production is listed under
// envs.production, as the production-gate tests do.
func productionConfig(t *testing.T, cfgPath string) {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withProd := strings.Replace(string(data), "    promotable: [ghcr.io/example/]\n", "    promotable: [ghcr.io/example/]\n    envs:\n      production: [app-production]\n", 1)
	if withProd == string(data) {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(withProd), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The direct-commit gate is the first thing a --direct run into production meets: before digests
// are resolved or a plan is built, and (for a deploy --dry-run, which never reaches
// StartPromotion) the ONLY refusal. Both are invisible to the older tests, which pass whichever
// layer refuses first: they assert a non-zero exit and a substring. These assert nothing was
// resolved, planned or printed.
func TestDirectProductionIsRefusedBeforeAnythingIsResolvedOrPlanned(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	productionConfig(t, cfgPath)

	var out, errOut bytes.Buffer
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}
	if got := run(args, &out, &errOut); got != exitFailure {
		t.Fatalf("promote: exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if strings.Contains(errOut.String(), "resolving") {
		t.Errorf("promote: a production direct commit was refused only after digests were resolved:\n%s", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	args = []string{"--config", cfgPath, "deploy", "--env", "app-production", "--image", "ghcr.io/example/app:v3@" + digestThird, "--direct", "--confirm-direct=app-production", "--dry-run"}
	if got := run(args, &out, &errOut); got != exitFailure {
		t.Fatalf("deploy --dry-run: exit %d, want %d; stdout: %s stderr: %s", got, exitFailure, out.String(), errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("deploy --dry-run printed a plan for a refused production direct commit:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "envs.production") {
		t.Errorf("deploy --dry-run: stderr should cite envs.production:\n%s", errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Errorf("no PR may exist: %+v", f.PRs())
	}
}

// Production restart: the acknowledgement is checked before the cluster is asked anything, a
// --dry-run included. Counting the rollout client requests is what shows the order; the existing
// test only sees that nothing was patched.
func TestRestartProductionIsRefusedBeforeAnyClusterWork(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	productionConfig(t, cfgPath)
	restartFake(t, rolled("app-production"))
	asked := 0
	inner := adaptors.Rollout
	adaptors.Rollout = func(kc string) (rollout.Rollout, string, error) { asked++; return inner(kc) }

	for _, extra := range [][]string{nil, {"--dry-run"}} {
		asked = 0
		var out, errOut bytes.Buffer
		args := append([]string{"--config", cfgPath, "restart", "--env", "app-production"}, extra...)
		if got := run(args, &out, &errOut); got != exitUsage {
			t.Fatalf("%v: exit %d, want %d; stderr: %s", extra, got, exitUsage, errOut.String())
		}
		if !strings.Contains(errOut.String(), "--confirm-production=app-production") {
			t.Errorf("%v: refused for some other reason than the missing acknowledgement:\n%s", extra, errOut.String())
		}
		if asked != 0 || out.Len() != 0 {
			t.Errorf("%v: refused only after %d rollout client request(s) and stdout %q", extra, asked, out.String())
		}
	}
}

// markerGit is a git.Git value distinct from git.Exec{}, so a snapshot of the real one cannot
// compare equal to it.
type markerGit struct{ git.Git }

// serviceDeps hands the Service functions that read adaptors when they are CALLED. A snapshot
// taken when serviceDeps runs would pass every other test that swaps a fake before building its
// Service; this swaps after, field by field.
func TestServiceDepsReadTheAdaptorsAtCallTime(t *testing.T) {
	saved := adaptors
	t.Cleanup(func() { adaptors = saved })
	d := serviceDeps()

	var gotGit git.Git = markerGit{}
	wantErr := errors.New("swapped")
	adaptors.Git = func() git.Git { return gotGit }
	adaptors.Forge = func(string) (forge.Forge, error) { return nil, wantErr }
	adaptors.Argo = func(string) (argo.Argo, string, error) { return nil, "swapped-argo", wantErr }
	adaptors.Rollout = func(string) (rollout.Rollout, string, error) { return nil, "swapped-rollout", wantErr }
	adaptors.Cluster = func(string) (k8s.Cluster, string, error) { return nil, "swapped-cluster", wantErr }
	adaptors.Registry = func(registry.AuthConfig) (registry.Registry, error) { return nil, wantErr }

	if d.Git() != gotGit {
		t.Error("Git was read when serviceDeps ran, not when called")
	}
	if _, err := d.Forge("o/r"); !errors.Is(err, wantErr) {
		t.Errorf("Forge: err = %v, want the swapped constructor's", err)
	}
	if _, name, err := d.Argo(""); !errors.Is(err, wantErr) || name != "swapped-argo" {
		t.Errorf("Argo: %q, %v", name, err)
	}
	if _, name, err := d.Rollout(""); !errors.Is(err, wantErr) || name != "swapped-rollout" {
		t.Errorf("Rollout: %q, %v", name, err)
	}
	if _, name, err := d.Cluster(""); !errors.Is(err, wantErr) || name != "swapped-cluster" {
		t.Errorf("Cluster: %q, %v", name, err)
	}
	if _, err := d.Registry(registry.AuthConfig{}); !errors.Is(err, wantErr) {
		t.Errorf("Registry: err = %v", err)
	}
}

// gc prints what it removes and keeps on stdout, and those lines carry paths from the machine
// (the worktree under the cache directory, the clone). With a registered value inside one, stdout
// must not carry it; the unredacted line is the positive control that the sweep did find the
// orphan. The state-directory test covers the error path only.
func TestGCRedactsItsStdoutLines(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	const secret = "tok-3b9a62-cache"
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), secret))
	redact.Register(secret)

	const id = "orphan3333"
	dir, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dir, secret) {
		t.Fatalf("precondition: worktree dir %q should sit under the cache directory", dir)
	}
	if err := (git.Exec{}).Worktree(context.Background(), clone, dir, engine.BranchName("app-staging", id), "main"); err != nil {
		t.Fatal(err)
	}
	// A second orphan with an uncommitted file is kept, and the line saying so names its directory.
	const dirtyID = "orphan4444"
	dirty, err := engine.WorktreeDir(dirtyID)
	if err != nil {
		t.Fatal(err)
	}
	if err := (git.Exec{}).Worktree(context.Background(), clone, dirty, engine.BranchName("app-staging", dirtyID), "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirty, "uncommitted.txt"), []byte("work in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "gc", "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("gc --dry-run: exit %d; stderr: %s", got, errOut.String())
	}
	var kept int
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "kept") || strings.Contains(line, "not clean") || strings.Contains(line, dirtyID) {
			kept++
			if strings.Contains(line, secret) {
				t.Errorf("a Kept line carries the registered value:\n%s", line)
			}
		}
	}
	if kept == 0 {
		t.Fatalf("no Kept line for the dirty orphan (positive control):\n%s", out.String())
	}
	if !strings.Contains(out.String(), "would remove worktree") {
		t.Fatalf("the sweep did not find the orphan (positive control):\n%s", out.String())
	}
	if strings.Contains(out.String(), secret) {
		t.Errorf("gc printed a registered value on stdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), redact.Redacted) {
		t.Errorf("gc's output should show where the value was scrubbed:\n%s", out.String())
	}
}

// buildTagsFunc maps service.TagSource onto the picker's BuildFunc contract: a registry that
// cannot be built leaves the registry list reporting the real error and offers no git tags or
// metadata; an unmapped repo has no git tags; a mapped one has them and says so.
func TestBuildTagsFuncHonoursThePickersContract(t *testing.T) {
	const imageRepo = "ghcr.io/example/app"
	saved := adaptors
	t.Cleanup(func() { adaptors = saved })
	adaptors.Forge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	regErr := errors.New("no credentials")
	adaptors.Registry = func(registry.AuthConfig) (registry.Registry, error) {
		return &registry.Fake{TagLists: map[string][]string{imageRepo: {"v1"}}}, nil
	}
	build := func(apps map[string]string) tags.BuildFunc {
		return buildTagsFunc(service.New(service.Settings{Repo: &config.RepoConfig{Apps: apps}}, serviceDeps()))
	}

	mapped, reg, git, meta := build(map[string]string{imageRepo: "example/app"})(imageRepo)
	if !mapped || git == nil || meta == nil {
		t.Errorf("mapped repo: mapped=%v git=%v meta=%v, want a mapped source with git tags and metadata", mapped, git != nil, meta != nil)
	}
	if got, err := reg(context.Background()); err != nil || len(got) != 1 {
		t.Errorf("registry tags = %v, %v", got, err)
	}

	mapped, _, git, meta = build(nil)(imageRepo)
	if mapped || git != nil || meta == nil {
		t.Errorf("unmapped repo: mapped=%v git=%v meta=%v, want unmapped, no git tags, metadata available", mapped, git != nil, meta != nil)
	}

	adaptors.Registry = func(registry.AuthConfig) (registry.Registry, error) { return nil, regErr }
	mapped, reg, git, meta = build(map[string]string{imageRepo: "example/app"})(imageRepo)
	if mapped || git != nil || meta != nil {
		t.Errorf("registry failure: mapped=%v git=%v meta=%v, want unmapped and neither git tags nor metadata", mapped, git != nil, meta != nil)
	}
	if _, err := reg(context.Background()); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("registry failure: the registry list should report the real error, got %v", err)
	}
}
