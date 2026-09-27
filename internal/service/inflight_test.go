package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// This file's fixture (inflightFixture) and its four scenario tests are moved, unchanged in
// substance, from cmd/hoist/findinflight_test.go (which called the package-level findInFlight
// directly) and cmd/hoist/promote_test.go's newPromoteFixture (which this file's own fixture is
// a trimmed copy of, scoped to what FindInFlight/claimTarget need — no CI/poll config, since
// nothing here drives through a CLI command). See AGENTS.md §9 entry 11 for the landed-verdict
// reasoning these scenarios pin.

// gitHostCmd/runGitHost/outGitHost are declared once for the package, in repo_test.go.

// mergeSimulatingForge wraps *forge.Fake so a successful MergePR also does what a real GitHub
// squash-merge actually does to the world beyond the fake's own in-memory bookkeeping — moved
// unchanged from cmd/hoist/promote_test.go; see its own doc comment there for the full
// reasoning (MergedStep's and ArgoSyncedStep's own Observe both revalidate against real,
// only-known-at-merge-time facts a static fixture can't precompute).
type mergeSimulatingForge struct {
	*forge.Fake
	onMerge func(mergeSHA string)
}

func (m *mergeSimulatingForge) MergePR(ctx context.Context, prNumber int, expectedHeadSHA string) (forge.PR, error) {
	pr, err := m.Fake.MergePR(ctx, prNumber, expectedHeadSHA)
	if err == nil && m.onMerge != nil {
		m.onMerge(pr.MergeSHA)
	}
	return pr, err
}

// satisfiedRollout builds a rollout.Fake pre-configured so every Deployment edits touches
// already reports its image matching and its rollout complete. Moved unchanged from
// cmd/hoist/findinflight_test.go.
func satisfiedRollout(namespace string, edits []gitops.Edit) *rollout.Fake {
	f := &rollout.Fake{}
	byName := map[string][]rollout.ContainerImage{}
	for _, e := range edits {
		if e.Kind != "Deployment" {
			continue
		}
		byName[e.Name] = append(byName[e.Name], rollout.ContainerImage{
			Name: e.Container, Init: strings.Contains(e.Path, "initContainers"), Image: e.New.String(),
		})
	}
	for name, imgs := range byName {
		f.SetDeployment(namespace, name, rollout.DeploymentStatus{Namespace: namespace, Name: name, Images: imgs, Complete: true})
	}
	return f
}

// inflightFixture is a local bare "origin" and a clone of it shaped like a minimal GitOps repo
// (one family, "app", older in app-production than app-staging), plus a *Service built with real
// Deps (a real git.Exec, a fake forge, the real FileStore) — never a fake StateStore, since these
// tests (and claimTarget's own race tests) exist specifically to exercise the real atomic claim
// file (engine.ClaimInFlight) and the real state files a future scan reads.
type inflightFixture struct {
	clone string
	f     *forge.Fake
	svc   *Service
}

func newInflightFixture(t *testing.T) inflightFixture {
	return newInflightFixtureWithProduction(t, nil)
}

// newInflightFixtureWithProduction is newInflightFixture with a caller-named envs.production
// list — start_test.go's own trust-boundary test needs app-production listed there to prove
// StartPromotion's Preflight refuses a direct commit into it before any claim or state exists.
func newInflightFixtureWithProduction(t *testing.T, production []string) inflightFixture {
	t.Helper()
	home := t.TempDir()
	gitconfig := filepath.Join(home, ".gitconfig")
	// The [receive]/[gc]/[maintenance] block keeps git from spawning a detached
	// `maintenance run --auto` on push, which can still be writing under the bare origin's
	// objects/ when t.TempDir cleans up (#123; see internal/engine's noBackgroundGitConfig).
	const cfgFile = "[user]\n\tname = Test\n\temail = test@example.invalid\n[commit]\n\tgpgsign = false\n[init]\n\tdefaultBranch = main\n" +
		"[receive]\n\tautogc = false\n[gc]\n\tauto = 0\n\tautoDetach = false\n[maintenance]\n\tauto = false\n\tautoDetach = false\n"
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

	fakeForge := &forge.Fake{}
	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	fakeArgo := &argo.Fake{}
	fakeRollout := &rollout.Fake{}
	fakeRollout.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Namespace: "app-production",
		Name:      "app",
		Images:    []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@" + digestNew}},
		Complete:  true,
	})
	fakeArgo.SetStatus(app, argo.Status{
		SyncStatus:   "OutOfSync",
		SyncRevision: "before-any-hoist-run",
		HealthStatus: argo.HealthStatusHealthy,
	})
	fakeArgo.OnRefresh = func(a argo.Application) {
		tip, err := gitHostCmd("", "-C", origin, "rev-parse", "refs/heads/main").Output()
		if err != nil {
			return
		}
		fakeArgo.SetStatus(a, argo.Status{
			SyncStatus:   argo.SyncStatusSynced,
			SyncRevision: strings.TrimSpace(string(tip)),
			HealthStatus: argo.HealthStatusHealthy,
			ReconciledAt: time.Now().Add(time.Hour),
		})
	}
	wrappedForge := &mergeSimulatingForge{
		Fake: fakeForge,
		onMerge: func(mergeSHA string) {
			runGitHost(t, origin, "update-ref", "refs/heads/main", mergeSHA)
			fakeArgo.SetStatus(app, argo.Status{
				SyncStatus:   argo.SyncStatusSynced,
				SyncRevision: mergeSHA,
				HealthStatus: argo.HealthStatusHealthy,
				ReconciledAt: time.Now().Add(time.Hour),
			})
		},
	}

	rc := &config.RepoConfig{
		GitHub:     "example/gitops",
		AppsRoot:   "cluster/apps",
		Promotable: []string{"ghcr.io/example/"},
		Envs:       config.EnvsConfig{Production: production},
		CI:         config.CIConfig{None: "green"},
	}
	set := Settings{
		RepoDir:    clone,
		AppsRoot:   "cluster/apps",
		Base:       "main",
		Promotable: []string{"ghcr.io/example/"},
		Repo:       rc,
	}
	svc := New(set, Deps{
		Git:     func() git.Git { return git.Exec{} },
		Forge:   func(string) (forge.Forge, error) { return wrappedForge, nil },
		Argo:    func(string) (argo.Argo, string, error) { return fakeArgo, "test-context", nil },
		Rollout: func(string) (rollout.Rollout, string, error) { return fakeRollout, "test-context", nil },
		Store:   FileStore{},
		Now:     time.Now,
	})

	return inflightFixture{clone: clone, f: fakeForge, svc: svc}
}

func TestFindInFlightRefusesWhenAnotherPromotionIsMidFlight(t *testing.T) {
	fx := newInflightFixture(t)

	r, err := gitops.Discover(fx.clone, "cluster/apps")
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
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      fx.clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "comment",
		Approvers:     []string{"alice"},
		CINone:        "green",
	}

	ro := satisfiedRollout(plan.TargetEnv, plan.Edits)

	if err := engine.Drive(context.Background(), engine.Steps(git.Exec{}, fx.f, nil), s, nil); err != nil {
		t.Fatalf("driving to PROpened: %v", err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	conflict, status, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-different-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict == nil {
		t.Fatal("expected the mid-flight promotion to be reported as in flight")
	}
	if conflict.ID != id {
		t.Fatalf("conflict.ID = %s, want %s", conflict.ID, id)
	}
	if status.Step != engine.StepApproved {
		t.Fatalf("expected to be stuck at %s, got %s", engine.StepApproved, status.Step)
	}

	if conflict, _, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-staging", "a-different-id"); err != nil {
		t.Fatal(err)
	} else if conflict != nil {
		t.Fatalf("a different target env must never conflict: %+v", conflict)
	}

	s.Approval = "auto"
	if err := engine.Drive(context.Background(), engine.AllSteps(git.Exec{}, fx.f, nil, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v", err)
	}
	runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}
	if conflict, status, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-different-id"); err != nil {
		t.Fatal(err)
	} else if conflict != nil {
		t.Fatalf("a fully done (merged, branch deleted) promotion must no longer conflict: stuck at %s: %+v", status.Step, status.Observation)
	}
}

func TestFindInFlightDoesNotBlockNewPromotionAfterPriorOneFullyMerged(t *testing.T) {
	fx := newInflightFixture(t)

	r, err := gitops.Discover(fx.clone, "cluster/apps")
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
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      fx.clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "auto",
		CINone:        "green",
	}

	ro := satisfiedRollout(plan.TargetEnv, plan.Edits)

	if err := engine.Drive(context.Background(), engine.AllSteps(git.Exec{}, fx.f, nil, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v", err)
	}
	runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	conflict, status, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a fully completed (merged, branch deleted) promotion must not block a new one for the same env: reported stuck at %s: %+v", status.Step, status.Observation)
	}
}

func TestFindInFlightDoesNotBlockAfterMergeWithRolloutPending(t *testing.T) {
	fx := newInflightFixture(t)

	r, err := gitops.Discover(fx.clone, "cluster/apps")
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
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      fx.clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "auto",
		CINone:        "green",
	}

	ro := &rollout.Fake{}

	err = engine.Drive(context.Background(), engine.AllSteps(git.Exec{}, fx.f, nil, ro, nil), s, nil)
	if !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("expected Drive to stop waiting on the rollout, got %v", err)
	}
	if s.MergeSHA == "" {
		t.Fatal("expected the promotion to have actually merged before Drive stopped at the rollout")
	}
	if s.Phase != engine.StepRolledOut {
		t.Fatalf("expected Drive to be stuck at %s, got %s", engine.StepRolledOut, s.Phase)
	}
	runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	conflict, status, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a merged promotion must not block a new one for the same env just because its Argo/rollout convergence is still pending: reported stuck at %s: %+v", status.Step, status.Observation)
	}
}

func TestFindInFlightDoesNotBlockAfterASupersededDirectDeploy(t *testing.T) {
	fx := newInflightFixture(t)

	r, err := gitops.Discover(fx.clone, "cluster/apps")
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
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      fx.clone,
		WorktreeDir:   wt,
		Base:          "main",
		Direct:        true,
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
	}
	if err := engine.Drive(context.Background(), engine.DirectSteps(git.Exec{}, nil, true, nil), s, nil); err != nil {
		t.Fatalf("driving the direct deploy: %v", err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	origin := strings.TrimSpace(outGitHost(t, fx.clone, "remote", "get-url", "origin"))
	other := filepath.Join(t.TempDir(), "later-deploy")
	runGitHost(t, "", "clone", "-q", origin, other)
	p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	newer := "ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64)
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.Contains(line, "image: ghcr.io/") {
			lines[i] = "          image: " + newer
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, other, "commit", "-q", "-a", "-m", "a later deploy into the same env")
	runGitHost(t, other, "push", "-q", "origin", "main")

	conflict, status, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a direct deploy superseded by a later one has landed and is finished; it must not refuse every later promotion into %s forever — reported stuck at %s: %+v",
			"app-production", status.Step, status.Observation)
	}
}

// claimThenWriteConflictStore wraps a real StateStore (the FileStore an inflightFixture already
// uses, per AGENTS.md's own design doc: a claim/race test must use the real, atomic filesystem
// Claim) so that the moment its embedded Claim returns successfully, a conflicting state file for
// a DIFFERENT id targeting the same env is written to disk — simulating another `hoist promote`
// process winning the claim race and saving its own state in the window between claimTarget's
// first scan and its rescan. This is what makes
// TestClaimTargetRefusesConflictAcquiredAfterTheFirstScan actually exercise the rescan: without
// it, the conflicting file already existed before the first scan ran, so the first scan alone
// would have caught it, and the test would pass even if claimTarget's second FindInFlight call
// were deleted entirely (t1-review.md P1 #3).
type claimThenWriteConflictStore struct {
	StateStore
	conflict *engine.PromotionState
	written  bool
}

func (c *claimThenWriteConflictStore) Claim(repoFullName, targetEnv, id string) (func(), error) {
	release, err := c.StateStore.Claim(repoFullName, targetEnv, id)
	if err != nil {
		return nil, err
	}
	if !c.written {
		c.written = true
		if err := (FileStore{}).Save(c.conflict); err != nil {
			return nil, err
		}
	}
	return release, nil
}

// TestClaimTargetRefusesConflictAcquiredAfterTheFirstScan is round-4's regression for the
// scan-then-claim ordering gap, moved to exercise claimTarget directly (rather than through the
// whole CLI, as cmd/hoist/promote_test.go's TestPromoteRefusesConflictAcquiredAfterTheFirstScan
// still does at the integration level): a conflicting, non-terminal state file for a different id
// targeting the same env, written to disk ONLY after the real claim succeeds (via
// claimThenWriteConflictStore, so the first scan cannot see it), must still be refused by
// claimTarget's own rescan while holding the claim.
func TestClaimTargetRefusesConflictAcquiredAfterTheFirstScan(t *testing.T) {
	fx := newInflightFixture(t)

	r, err := gitops.Discover(fx.clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const otherID = "other-in-flight-promotion"
	wt, err := engine.WorktreeDir(otherID)
	if err != nil {
		t.Fatal(err)
	}
	other := &engine.PromotionState{
		ID:            otherID,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, otherID),
		CloneDir:      fx.clone,
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
	if err := engine.Drive(context.Background(), engine.Steps(git.Exec{}, fx.f, nil), other, nil); err != nil {
		t.Fatalf("driving the other promotion to PROpened: %v", err)
	}
	// other is NOT saved here — it is written by claimThenWriteConflictStore, from inside Claim,
	// after the real claim for "a-brand-new-id" has already succeeded. The first FindInFlight
	// scan in claimTarget therefore sees nothing; only the rescan after Claim can catch it.
	fx.svc.deps.Store = &claimThenWriteConflictStore{StateStore: fx.svc.deps.Store, conflict: other}

	_, err = fx.svc.claimTarget(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err == nil {
		t.Fatal("expected claimTarget to refuse a conflicting in-flight promotion acquired after the first scan")
	}
	var conflictErr *InFlightConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected an *InFlightConflictError, got %T: %v", err, err)
	}
	if conflictErr.Conflict.ID != otherID {
		t.Fatalf("conflict.ID = %s, want %s", conflictErr.Conflict.ID, otherID)
	}
}

// TestClaimTargetReleaseIsIdempotentAndAtomic pins claimTarget's own contract against the REAL
// FileStore claim file (never a fake — AGENTS.md's own design doc for this train is explicit
// that a claim race test must use the real atomic filesystem claim, engine.ClaimInFlight): a
// second claimTarget call for the same repo/env, made before the first's release runs, must be
// refused, and once released a third call for the same repo/env must succeed.
func TestClaimTargetReleaseIsIdempotentAndAtomic(t *testing.T) {
	fx := newInflightFixture(t)

	release, err := fx.svc.claimTarget(context.Background(), "example/gitops", "app-production", "id-1")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if release == nil {
		t.Fatal("expected a non-nil release func")
	}

	// A second, distinct id targeting the same env must be refused while the first claim is
	// still held — this is engine.ClaimInFlight's own atomic file create, not a scan (no state
	// file exists yet for either id), so this proves the claim itself, not FindInFlight's scan.
	if _, err := fx.svc.deps.Store.Claim("example/gitops", "app-production", "id-2"); err == nil {
		t.Fatal("expected a second claim for the same repo/env to be refused while the first is held")
	}

	// release is idempotent: calling it twice must not panic or double-release someone else's
	// later claim.
	release()
	release()

	// Once released, a fresh claim for the same repo/env succeeds.
	release2, err := fx.svc.deps.Store.Claim("example/gitops", "app-production", "id-3")
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	release2()
}
