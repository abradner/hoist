package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

const repoFullName = "example/gitops"

// runHost runs git directly (not through pkg/git) for test setup that isn't itself part of
// what's under test: seeding the fixture repo, and simulating "someone else" pushing a
// conflicting branch from a second clone.
func runHost(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// newFixtureOrigin creates a bare "origin" holding one commit: an Argo GitOps repo shaped
// like testdata/repo, with a single family "app" whose image is older in app-production than
// in app-staging, so BuildPlan(app-staging, app-production) always has exactly one edit to
// make. HOME and the global git config are isolated so no test here can reach the developer's
// own signing configuration (AGENTS.md §9 gotcha on hermetic tests).
func newFixtureOrigin(t *testing.T) (originDir string) {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	const cfg = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(gitconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))

	seed := t.TempDir()
	runHost(t, "", "init", "-q", "-b", "main", seed)

	write := func(rel, content string) {
		p := filepath.Join(seed, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := func(env, family string) string {
		return "" +
			"apiVersion: argoproj.io/v1alpha1\n" +
			"kind: Application\n" +
			"metadata:\n" +
			"  name: " + family + "-" + env + "\n" +
			"  namespace: argocd\n" +
			"spec:\n" +
			"  project: default\n" +
			"  source:\n" +
			"    repoURL: https://git.example.test/example/gitops.git\n" +
			"    targetRevision: main\n" +
			"    path: cluster/apps/" + env + "/" + family + "\n" +
			"  destination:\n" +
			"    server: https://kubernetes.default.svc\n" +
			"    namespace: " + env + "\n"
	}
	deployment := func(env, ref string) string {
		return "" +
			"apiVersion: apps/v1\n" +
			"kind: Deployment\n" +
			"metadata:\n" +
			"  name: app\n" +
			"  namespace: " + env + "\n" +
			"spec:\n" +
			"  template:\n" +
			"    spec:\n" +
			"      containers:\n" +
			"        - name: app\n" +
			"          image: " + ref + "\n"
	}
	digestOld := "sha256:" + strings.Repeat("0", 64)
	digestNew := "sha256:" + strings.Repeat("1", 64)
	write("cluster/apps/app-staging-app.yaml", wrapper("app-staging", "app"))
	write("cluster/apps/app-production-app.yaml", wrapper("app-production", "app"))
	write("cluster/apps/app-staging/app/deployment.yaml", deployment("app-staging", "ghcr.io/example/app:v2@"+digestNew))
	write("cluster/apps/app-production/app/deployment.yaml", deployment("app-production", "ghcr.io/example/app:v1@"+digestOld))

	runHost(t, seed, "add", ".")
	runHost(t, seed, "commit", "-q", "-m", "seed")

	originDir = filepath.Join(t.TempDir(), "origin.git")
	runHost(t, "", "init", "-q", "--bare", "-b", "main", originDir)
	runHost(t, seed, "remote", "add", "origin", originDir)
	runHost(t, seed, "push", "-q", "origin", "main")
	return originDir
}

// fixture is one ready-to-drive promotion: a clone of newFixtureOrigin's origin, and the plan
// BuildPlan produces promoting the "app" family from app-staging to app-production.
type fixture struct {
	cloneDir, originDir string
	plan                gitops.Plan
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	origin := newFixtureOrigin(t)
	clone := filepath.Join(t.TempDir(), "clone")
	runHost(t, "", "clone", "-q", origin, clone)

	repo, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	plan, err := gitops.BuildPlan(repo, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Edits) != 1 || plan.Edits[0].NoOp() {
		t.Fatalf("fixture plan should have exactly one real edit, got %+v", plan.Edits)
	}
	return fixture{cloneDir: clone, originDir: origin, plan: plan}
}

// newState builds a fresh PromotionState from fx, independent of any previously-built state —
// exactly what a restarted `hoist promote` process would build from --repo/--from/--to and the
// same resolved digests, per AGENTS.md invariant 4 ("re-derive names from inputs every time").
// worktreeDir is shared across "kill and restart" calls in the same subtest (it is keyed by id
// in production; tests key it by fx directly for the same reason).
func newState(fx fixture, worktreeDir string) *PromotionState {
	id := DeriveID(repoFullName, fx.plan)
	return &PromotionState{
		ID:            id,
		RepoFullName:  repoFullName,
		SourceEnv:     fx.plan.SourceEnv,
		TargetEnv:     fx.plan.TargetEnv,
		Branch:        BranchName(fx.plan.TargetEnv, id),
		CloneDir:      fx.cloneDir,
		WorktreeDir:   worktreeDir,
		Base:          "main",
		Edits:         fx.plan.Edits,
		CommitMessage: RenderCommitMessage(id, fx.plan),
		PRTitle:       PRTitle(fx.plan),
		PRBody:        RenderPRBody(id, fx.plan),
	}
}

func ctx() context.Context { return context.Background() }

// mergeToBase simulates what a real forge merge actually does to the base branch: fast-forward
// origin's tip of s.Base to s.CommitSHA. forge.Fake models a PR's merge state in memory only and
// never touches real git, so any test that wants MergedStep's Observe to see the base as
// genuinely caught up (past its M4 base-revalidation, finding #1: a historical "merged" record
// alone is never proof the base still holds the promoted content) must call this — it is exactly
// what a real GitHub squash-merge would have done to the same remote this promotion's own git
// operations already point at.
func mergeToBase(t *testing.T, s *PromotionState) {
	t.Helper()
	if s.CommitSHA == "" {
		t.Fatal("mergeToBase: s.CommitSHA is empty — drive to at least CommittedStep first")
	}
	runHost(t, s.CloneDir, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
}

// satisfiedRollout builds a rollout.Fake pre-configured so every Deployment edits touches
// already reports its image matching and its rollout complete — the "nothing left to
// converge" baseline the M4-era convergence tests need now that RolledOutStep is part of
// AllSteps (M5), without any of them needing to know M5's own mechanics. namespace is the
// promotion's TargetEnv, which is also the Deployment's own namespace (gitops.Env's doc
// comment: the destination namespace a family's Application deploys into).
func satisfiedRollout(namespace string, edits []gitops.Edit) *rollout.Fake {
	f := &rollout.Fake{}
	byName := map[string][]rollout.ContainerImage{}
	for _, e := range edits {
		if e.Kind != "Deployment" {
			continue
		}
		byName[e.Name] = append(byName[e.Name], rollout.ContainerImage{
			Name:  e.Container,
			Init:  strings.Contains(e.Path, "initContainers"),
			Image: e.New.String(),
		})
	}
	for name, imgs := range byName {
		f.SetDeployment(namespace, name, rollout.DeploymentStatus{
			Namespace: namespace,
			Name:      name,
			Images:    imgs,
			Complete:  true,
			Detail:    fmt.Sprintf("deployment %q successfully rolled out", name),
		})
	}
	return f
}
