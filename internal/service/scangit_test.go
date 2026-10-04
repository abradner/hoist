package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// countingGit counts the calls that open a connection to the remote. Embedding git.Git means
// every other method goes to the wrapped implementation (nil in the unit tests below, which
// never reach one).
type countingGit struct {
	git.Git
	heads, branch, fetch int
	headsErr, fetchErr   error
	fetchMissing         bool
	headsOut             map[string]string
}

func (c *countingGit) LsRemoteHeads(ctx context.Context, cloneDir, remote string) (map[string]string, error) {
	c.heads++
	if c.headsErr != nil {
		return nil, c.headsErr
	}
	if c.headsOut != nil {
		return c.headsOut, nil
	}
	return c.Git.LsRemoteHeads(ctx, cloneDir, remote)
}

func (c *countingGit) LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (string, bool, error) {
	c.branch++
	return c.Git.LsRemoteBranch(ctx, cloneDir, remote, branch)
}

func (c *countingGit) FetchBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	c.fetch++
	if c.fetchErr != nil {
		return "", false, c.fetchErr
	}
	if c.fetchMissing {
		return "", false, nil
	}
	if c.Git == nil {
		return "sha-of-" + branch, true, nil
	}
	return c.Git.FetchBranch(ctx, dir, remote, branch)
}

func TestScanGitAsksTheRemoteOncePerQuestion(t *testing.T) {
	ctx := context.Background()
	inner := &countingGit{headsOut: map[string]string{"main": "aaa", "hoist/env/one": "bbb"}}
	g := newScanGit(inner)

	for _, tc := range []struct {
		branch, sha string
		ok          bool
	}{{"main", "aaa", true}, {"hoist/env/one", "bbb", true}, {"hoist/env/gone", "", false}, {"main", "aaa", true}} {
		sha, ok, err := g.LsRemoteBranch(ctx, "/clone", "origin", tc.branch)
		if err != nil || ok != tc.ok || sha != tc.sha {
			t.Errorf("LsRemoteBranch(%s) = %q, %v, %v; want %q, %v", tc.branch, sha, ok, err, tc.sha, tc.ok)
		}
	}
	if inner.heads != 1 || inner.branch != 0 {
		t.Errorf("four branch lookups cost %d listings and %d per-branch calls, want 1 and 0", inner.heads, inner.branch)
	}

	for i := 0; i < 3; i++ {
		if sha, ok, err := g.FetchBranch(ctx, "/clone", "origin", "main"); err != nil || !ok || sha != "sha-of-main" {
			t.Fatalf("FetchBranch = %q, %v, %v", sha, ok, err)
		}
	}
	if inner.fetch != 1 {
		t.Errorf("three fetches of one branch reached the remote %d times, want 1", inner.fetch)
	}

	// A different clone, remote or branch is a different question.
	if _, _, err := g.FetchBranch(ctx, "/clone", "origin", "dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.FetchBranch(ctx, "/other-clone", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.LsRemoteBranch(ctx, "/other-clone", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if inner.fetch != 3 || inner.heads != 2 {
		t.Errorf("distinct questions: %d fetches and %d listings, want 3 and 2", inner.fetch, inner.heads)
	}
}

// A failure is kept for the scan like any other answer, so an unreachable origin costs one
// failed connection for the whole snapshot pass rather than one per state — each later state
// fails at once and is observed live, which is the pass that reports the error.
func TestScanGitKeepsAFailureForTheScan(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("connection reset")
	inner := &countingGit{headsErr: boom, fetchErr: boom}
	g := newScanGit(inner)

	for i := 0; i < 3; i++ {
		if _, _, err := g.LsRemoteBranch(ctx, "/clone", "origin", "main"); !errors.Is(err, boom) {
			t.Fatalf("want the listing's error, got %v", err)
		}
		if _, _, err := g.FetchBranch(ctx, "/clone", "origin", "main"); !errors.Is(err, boom) {
			t.Fatalf("want the fetch's error, got %v", err)
		}
	}
	if inner.heads != 1 || inner.fetch != 1 {
		t.Errorf("three states against a dead remote made %d listings and %d fetches, want 1 and 1", inner.heads, inner.fetch)
	}
	// And a scan that was answered with a failure is never confirmed, even once origin is back.
	inner.headsErr, inner.fetchErr, inner.headsOut = nil, nil, map[string]string{"main": "aaa"}
	if g.unchanged(ctx) {
		t.Error("a snapshot holding a failure must not be confirmed")
	}
}

// finishedPromotion drives the fixture's app-staging -> app-production plan, as it stands in
// the clone right now, all the way to done through the fixture's own forge, Argo and rollout
// fakes, and saves its state file — the shape every earlier promotion into an env has once it is
// finished, for the git/forge core FindInFlight observes and for List's full walk alike.
func finishedPromotion(t *testing.T, fx inflightFixture) *engine.PromotionState {
	t.Helper()
	r, err := gitops.Discover(fx.clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	argoApps, err := engine.ArgoAppNames(r, plan.TargetEnv, plan.Edits)
	if err != nil {
		t.Fatal(err)
	}
	editApps, err := engine.EditApps(r, plan.TargetEnv, plan.Edits)
	if err != nil {
		t.Fatal(err)
	}
	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
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
		ArgoNamespace: "argocd",
		ArgoApps:      argoApps,
		EditApps:      editApps,
	}
	f, err := fx.svc.ForgeFor("example/gitops")
	if err != nil {
		t.Fatal(err)
	}
	a, err := fx.svc.Argo("")
	if err != nil {
		t.Fatal(err)
	}
	ro := satisfiedRollout(plan.TargetEnv, plan.Edits)
	if err := engine.Drive(context.Background(), engine.AllSteps(git.Exec{}, f, a, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v", err)
	}
	saveStateAs(t, s, s.ID)
	return s
}

func saveStateAs(t *testing.T, s *engine.PromotionState, id string) {
	t.Helper()
	cp := *s
	cp.ID = id
	path, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(path, &cp); err != nil {
		t.Fatal(err)
	}
}

// TestFindInFlightAsksOriginOncePerScan is the regression test for the scan's cost: three
// finished promotions into one env used to cost three fetches of the base branch and three
// per-branch ls-remotes — per scan, with claimTarget scanning twice. One scan now makes one
// fetch and two listings (the snapshot, and the check that it still held at the end) however
// many there are, and still reaches the right verdict for each.
func TestFindInFlightAsksOriginOncePerScan(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)
	// Two more state files for the same finished promotion under other ids: FindInFlight
	// observes each state file it lists, and what matters here is how many it observes.
	saveStateAs(t, s, "second-finished")
	saveStateAs(t, s, "third-finished")

	counting := &countingGit{Git: git.Exec{}}
	deps := fx.svc.deps
	deps.Git = func() git.Git { return counting }
	svc := New(fx.svc.settings, deps)

	conflict, status, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("three finished promotions must not conflict: %s stuck at %s: %+v", conflict.ID, status.Step, status.Observation)
	}
	if counting.fetch != 1 || counting.heads != 2 || counting.branch != 0 {
		t.Errorf("one scan over three finished promotions: %d fetches, %d listings, %d per-branch ls-remotes; want 1, 2 (the snapshot and its confirmation), 0",
			counting.fetch, counting.heads, counting.branch)
	}

	if _, _, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id"); err != nil {
		t.Fatal(err)
	}
	if counting.fetch != 2 || counting.heads != 4 {
		t.Errorf("a second scan must ask origin again: %d fetches, %d listings in total; want 2, 4", counting.fetch, counting.heads)
	}
}

// TestFindInFlightSecondScanSeesWhatChangedOnOrigin pins the half of AGENTS.md principle 2 the
// scan's shared snapshot could break: what one scan observed is never the next scan's answer.
// claimTarget's second scan runs while holding the claim precisely to see what appeared since
// the first.
func TestFindInFlightSecondScanSeesWhatChangedOnOrigin(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)

	scan := func() *engine.PromotionState {
		t.Helper()
		conflict, _, err := fx.svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
		if err != nil {
			t.Fatal(err)
		}
		return conflict
	}
	if c := scan(); c != nil {
		t.Fatalf("control: a merged promotion whose branch is gone is finished, got a conflict with %s", c.ID)
	}
	// The promotion's branch reappears on origin: MergedStep no longer reads as done.
	runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Branch)
	if c := scan(); c == nil {
		t.Fatal("the second scan answered from the first scan's view of origin: the branch now exists there again")
	}
}

// TestListAndResumeByEnvAskOriginOncePerScan covers the scan's other two callers: `hoist
// promotions` (and the matrix's in-flight pane) through List, and `hoist resume --env` through
// FindInFlightForEnv. Both walk every state file the same way FindInFlight does.
func TestListAndResumeByEnvAskOriginOncePerScan(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)
	saveStateAs(t, s, "second-finished")
	saveStateAs(t, s, "third-finished")

	withCounting := func() (*Service, *countingGit) {
		counting := &countingGit{Git: git.Exec{}}
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return counting }
		return New(base.settings, deps), counting
	}

	// A landed promotion's first listing also tidies up after it (Service.CleanupLanded), and
	// that asks origin about the one promotion, live — deliberately not through the scan's
	// snapshot, since it is about to delete something. It happens once per promotion, so it is
	// paid here, before anything is counted; TestListPaysForCleanupOncePerPromotion pins that
	// the listing counted below, and every one after it, is back to the scan's own cost.
	if _, err := withConfig(fx).List(context.Background(), ListOpts{}); err != nil {
		t.Fatal(err)
	}

	svc, counting := withCounting()
	listed, err := svc.List(context.Background(), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("List = %d entries, want 3", len(listed))
	}
	for _, l := range listed {
		if l.Err != nil || !l.Done {
			t.Fatalf("control: %s must list as done (err=%v, stopped at %s: %+v)", l.State.ID, l.Err, l.Last.Step, l.Last.Observation)
		}
	}
	if counting.fetch != 1 || counting.heads != 2 || counting.branch != 0 {
		t.Errorf("List over three promotions: %d fetches, %d listings, %d per-branch ls-remotes; want 1, 2, 0",
			counting.fetch, counting.heads, counting.branch)
	}

	svc, counting = withCounting()
	var nf *NotFoundError
	if _, err := svc.FindInFlightForEnv(context.Background(), "app-production", Hooks{}); !errors.As(err, &nf) {
		t.Fatalf("three finished promotions leave nothing to resume: got %v", err)
	}
	if counting.fetch != 1 || counting.heads != 2 || counting.branch != 0 {
		t.Errorf("FindInFlightForEnv over three promotions: %d fetches, %d listings, %d per-branch ls-remotes; want 1, 2, 0",
			counting.fetch, counting.heads, counting.branch)
	}
}

// staleFirstFetchGit answers the first FetchBranch with a base tip from before the promotion
// under test merged — what a scan's one shared fetch looks like to a promotion whose merge
// landed after it was taken.
type staleFirstFetchGit struct {
	git.Git
	stale string
	calls int
}

func (g *staleFirstFetchGit) FetchBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	g.calls++
	if g.calls == 1 {
		return g.stale, true, nil
	}
	return g.Git.FetchBranch(ctx, dir, remote, branch)
}

// TestScanDoesNotCallAMergeRevertedOnAStaleBaseTip is the regression test for the snapshot's
// one dangerous property: it is older than the forge read of a state observed later in the
// scan. A promotion merged after the scan's fetch has a merge commit the fetched tip does not
// contain, which MergedStep reads as "the base was reset past this merge" — a finished
// promotion reported as blocking its env, with advice to redo it. The scan's closing check sees
// that origin's base is not where the snapshot had it, and the scan is made again live.
func TestScanDoesNotCallAMergeRevertedOnAStaleBaseTip(t *testing.T) {
	fx := newInflightFixture(t)
	beforeMerge := strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "refs/remotes/origin/main"))
	s := finishedPromotion(t, fx)
	if s.MergeSHA == beforeMerge || s.MergeSHA == "" {
		t.Fatalf("control: the promotion must have merged past %s, got %q", beforeMerge, s.MergeSHA)
	}

	stale := &staleFirstFetchGit{Git: git.Exec{}, stale: beforeMerge}
	deps := fx.svc.deps
	deps.Git = func() git.Git { return stale }
	svc := New(fx.svc.settings, deps)

	conflict, status, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a finished promotion was reported in flight on the strength of a base tip fetched before it merged: %s: %+v", status.Step, status.Observation)
	}
	if stale.calls != 2 {
		t.Errorf("want the stale answer rejected and origin asked again: %d fetches, want 2", stale.calls)
	}
}

// aheadListingGit lists the base branch at a commit the clone has never fetched — what a scan's
// listing looks like when it was taken after the scan's fetch and something landed in between.
type aheadListingGit struct {
	git.Git
	base string
}

func (g *aheadListingGit) LsRemoteHeads(ctx context.Context, cloneDir, remote string) (map[string]string, error) {
	heads, err := g.Git.LsRemoteHeads(ctx, cloneDir, remote)
	if err != nil {
		return nil, err
	}
	heads[g.base] = strings.Repeat("f", 40)
	return heads, nil
}

// directPromotion lands the fixture's plan straight on origin's main in direct mode and saves
// its state file.
func directPromotion(t *testing.T, fx inflightFixture) *engine.PromotionState {
	t.Helper()
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
		t.Fatalf("driving the direct promotion: %v", err)
	}
	saveStateAs(t, s, s.ID)
	return s
}

// A listing that names a base tip the scan's fetch never brought in makes DirectPushedStep read
// a tree the clone does not have. That error belongs to the snapshot, not to the promotion, and
// must not become the scan's answer.
func TestScanDoesNotFailOnAListingAheadOfItsFetch(t *testing.T) {
	fx := newInflightFixture(t)
	directPromotion(t, fx)
	// A later commit on main, so the direct promotion's own commit is no longer the tip and its
	// Observe has to read the tip's tree rather than match it by sha.
	runGitHost(t, fx.clone, "fetch", "-q", "origin", "main")
	runGitHost(t, fx.clone, "merge", "-q", "--ff-only", "origin/main")
	runGitHost(t, fx.clone, "commit", "-q", "--allow-empty", "-m", "unrelated later commit")
	runGitHost(t, fx.clone, "push", "-q", "origin", "main")

	deps := fx.svc.deps
	deps.Git = func() git.Git { return &aheadListingGit{Git: git.Exec{}, base: "main"} }
	svc := New(fx.svc.settings, deps)

	conflict, status, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatalf("the snapshot's own inconsistency became the scan's error: %v", err)
	}
	if conflict != nil {
		t.Fatalf("a landed direct promotion was reported in flight: %s: %+v", status.Step, status.Observation)
	}
}

// TestScanOverDirectAndPRPromotionsAsksOriginOnce puts both kinds of finished promotion — two
// distinct ones, with their own commits — in one scan: the direct one is superseded by the PR
// one that followed it, and both read origin's base branch through the same snapshot.
func TestScanOverDirectAndPRPromotionsAsksOriginOnce(t *testing.T) {
	fx := newInflightFixture(t)
	direct := directPromotion(t, fx)

	// A newer build reaches staging, and the clone catches up with origin (the direct push never
	// moves the clone's own branch), so the next plan promotes it over the direct one's.
	runGitHost(t, fx.clone, "fetch", "-q", "origin", "main")
	runGitHost(t, fx.clone, "merge", "-q", "--ff-only", "origin/main")
	p := filepath.Join(fx.clone, "cluster/apps/app-staging/app/deployment.yaml")
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
	runGitHost(t, fx.clone, "commit", "-q", "-a", "-m", "staging runs v3")
	runGitHost(t, fx.clone, "push", "-q", "origin", "main")
	viaPR := finishedPromotion(t, fx)
	if viaPR.ID == direct.ID {
		t.Fatal("control: the two promotions must be distinct")
	}

	counting := &countingGit{Git: git.Exec{}}
	deps := fx.svc.deps
	deps.Git = func() git.Git { return counting }
	svc := New(fx.svc.settings, deps)

	conflict, status, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a superseded direct promotion and a merged one are both finished: %s stuck at %s: %+v", conflict.ID, status.Step, status.Observation)
	}
	if counting.fetch != 1 || counting.heads != 2 || counting.branch != 0 {
		t.Errorf("one scan over a direct and a PR promotion: %d fetches, %d listings, %d per-branch ls-remotes; want 1, 2, 0",
			counting.fetch, counting.heads, counting.branch)
	}
}

// TestListPaysForCleanupOncePerPromotion: cleaning up after a landed promotion asks origin about
// it live — one promotion's worth of the per-state traffic the shared scan exists to remove. That
// is the price of not deleting anything on a snapshot's say-so, and it must be a one-off: the
// listing that cleans pays it, and the next listing over the same promotions is exactly the
// scan's own cost again (one fetch, two listings, no per-branch ls-remote). Covers the two
// shapes that pile up: finished, and landed but never satisfied at rolled-out (#168).
func TestListPaysForCleanupOncePerPromotion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		superseded bool
	}{{"finished", false}, {"landed, never rolled out", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInflightFixture(t)
			s := finishedPromotion(t, fx)
			if tc.superseded {
				ro, err := fx.svc.Rollout("")
				if err != nil {
					t.Fatal(err)
				}
				ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
					Namespace: "app-production", Name: "app", Complete: true,
					Images: []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v9@sha256:" + strings.Repeat("9", 64)}},
				})
			}
			list := func() ([]Listed, *countingGit) {
				counting := &countingGit{Git: git.Exec{}}
				base := withConfig(fx)
				deps := base.deps
				deps.Git = func() git.Git { return counting }
				listed, err := New(base.settings, deps).List(context.Background(), ListOpts{})
				if err != nil || len(listed) != 1 {
					t.Fatalf("List = %v, %v", listed, err)
				}
				return listed, counting
			}

			first, c1 := list()
			if len(first[0].Cleaned) != 2 || first[0].CleanupErr != nil {
				t.Fatalf("control: the first listing must clean up the landed promotion (worktree and branch): %+v", first[0])
			}
			if first[0].Done == tc.superseded {
				t.Fatalf("control: done = %v, want %v", first[0].Done, !tc.superseded)
			}
			if _, err := os.Lstat(s.WorktreeDir); !os.IsNotExist(err) {
				t.Fatalf("the worktree should be gone: %v", err)
			}
			// The scan's own 1 fetch and 2 listings, plus the cleanup's live look at this one
			// promotion: a fetch and a per-branch ls-remote to re-observe the merge, and a fetch
			// to place the branch tip on the base (the fake forge reports no head sha to compare).
			if c1.fetch != 3 || c1.heads != 2 || c1.branch != 1 {
				t.Errorf("the listing that cleans up: %d fetches, %d listings, %d per-branch ls-remotes; want 3, 2, 1", c1.fetch, c1.heads, c1.branch)
			}

			second, c2 := list()
			if len(second[0].Cleaned) != 0 || second[0].CleanupErr != nil {
				t.Fatalf("there is nothing left to clean on the second listing: %+v", second[0])
			}
			if c2.fetch != 1 || c2.heads != 2 || c2.branch != 0 {
				t.Errorf("the listing after cleanup: %d fetches, %d listings, %d per-branch ls-remotes; want the scan's own 1, 2, 0", c2.fetch, c2.heads, c2.branch)
			}
		})
	}
}

// TestListDoesNotAskOriginAgainAboutAPromotionItIsLeavingInPlace: a landed promotion whose
// worktree and branch are NOT removed is listed again on every poll, with something still
// pending every time. It must not cost a live look at origin every time: the TUI lists every
// poll.approval, and a promotion left in place can stay that way for good.
func TestListDoesNotAskOriginAgainAboutAPromotionItIsLeavingInPlace(t *testing.T) {
	scanOnly := func(t *testing.T, c *countingGit, what string) {
		t.Helper()
		if c.fetch != 1 || c.heads != 2 || c.branch != 0 {
			t.Errorf("%s: %d fetches, %d listings, %d per-branch ls-remotes; want the scan's own 1, 2, 0", what, c.fetch, c.heads, c.branch)
		}
	}
	// One Service across the listings, as the TUI has: what it remembers is per process.
	service := func(fx inflightFixture) (*Service, *countingGit) {
		counting := &countingGit{Git: git.Exec{}}
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return counting }
		return New(base.settings, deps), counting
	}
	reset := func(c *countingGit) { c.fetch, c.heads, c.branch = 0, 0, 0 }

	// Refused by a local check: origin is never asked on the cleanup's behalf, not even once.
	t.Run("uncommitted file in the worktree", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		if err := os.WriteFile(filepath.Join(s.WorktreeDir, "left-behind.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svc, c := service(fx)
		for i := 1; i <= 3; i++ {
			reset(c)
			listed, err := svc.List(context.Background(), ListOpts{})
			if err != nil || len(listed) != 1 || listed[0].CleanupErr == nil || len(listed[0].Cleaned) != 0 {
				t.Fatalf("listing %d: want the promotion left in place with the reason: %+v, %v", i, listed, err)
			}
			scanOnly(t, c, fmt.Sprintf("listing %d", i))
		}
		if _, err := os.Lstat(s.WorktreeDir); err != nil {
			t.Fatalf("the worktree must be left: %v", err)
		}
	})

	// Refused only after asking origin (the branch tip is a commit that is nowhere on the
	// remote — the same id run a second time, #41): the first listing pays for the look, and
	// the ones after it do not, until the branch moves.
	t.Run("branch tip is not on the remote", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		commit := func(name string) {
			if err := os.WriteFile(filepath.Join(s.WorktreeDir, name), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := (git.Exec{}).Commit(context.Background(), s.WorktreeDir, "a second run's commit", []string{name}, time.Minute, nil); err != nil {
				t.Fatal(err)
			}
		}
		commit("second-run.txt")
		svc, c := service(fx)

		listed, err := svc.List(context.Background(), ListOpts{})
		if err != nil || len(listed) != 1 || listed[0].CleanupErr == nil {
			t.Fatalf("first listing: want the promotion left in place: %+v, %v", listed, err)
		}
		if c.fetch <= 1 {
			t.Fatalf("control: the first listing must have asked origin on the cleanup's behalf (%d fetches), or the listings below prove nothing", c.fetch)
		}
		for i := 2; i <= 3; i++ {
			reset(c)
			listed, err := svc.List(context.Background(), ListOpts{})
			if err != nil || len(listed) != 1 || listed[0].CleanupErr == nil {
				t.Fatalf("listing %d: the reason must still be reported: %+v, %v", i, listed, err)
			}
			scanOnly(t, c, fmt.Sprintf("listing %d", i))
		}

		// The branch moves: what was remembered was about the old tip, so origin is asked again.
		commit("third-run.txt")
		reset(c)
		if _, err := svc.List(context.Background(), ListOpts{}); err != nil {
			t.Fatal(err)
		}
		if c.fetch <= 1 {
			t.Errorf("after the branch moved the cleanup must look again, got %d fetches", c.fetch)
		}
	})
}

// A scanGit that reached a step's Act would hide the Act's own effect from the next Observe;
// it refuses to write at all, so that wiring mistake fails the first time it runs.
func TestScanGitRefusesToWrite(t *testing.T) {
	ctx := context.Background()
	g := newScanGit(&countingGit{})
	_, commitErr := g.Commit(ctx, "/wt", "msg", nil, 0, nil)
	_, deleteLocalErr := g.DeleteLocalBranch(ctx, "/clone", "b")
	for name, err := range map[string]error{
		"Worktree":           g.Worktree(ctx, "/clone", "/wt", "b", "main"),
		"WorktreeAtRef":      g.WorktreeAtRef(ctx, "/clone", "/wt", "main"),
		"RemoveWorktree":     g.RemoveWorktree(ctx, "/clone", "/wt"),
		"Commit":             commitErr,
		"Push":               g.Push(ctx, "/wt", "origin", "b"),
		"PushHeadTo":         g.PushHeadTo(ctx, "/wt", "origin", "main"),
		"DeleteRemoteBranch": g.DeleteRemoteBranch(ctx, "/clone", "origin", "b"),
		"DeleteLocalBranch":  deleteLocalErr,
	} {
		if !errors.Is(err, errScanGitWrite) {
			t.Errorf("%s: want the read-only refusal, got %v", name, err)
		}
	}
}

// TestScanGitUnchangedComparesEveryAnswerItGave pins the check every scan's verdicts wait on:
// each branch the scan asked about, and each base tip it fetched, must be where the snapshot
// had it. A branch the scan never asked about is not its business.
func TestScanGitUnchangedComparesEveryAnswerItGave(t *testing.T) {
	ctx := context.Background()
	// A scan that asked about main, one promotion branch that exists and one that is gone.
	scanned := func() (*scanGit, *countingGit) {
		inner := &countingGit{headsOut: map[string]string{"main": "aaa", "hoist/env/one": "bbb", "renovate/x": "rrr"}}
		g := newScanGit(inner)
		for _, branch := range []string{"main", "hoist/env/one", "hoist/env/gone"} {
			if _, _, err := g.LsRemoteBranch(ctx, "/clone", "origin", branch); err != nil {
				t.Fatal(err)
			}
		}
		return g, inner
	}

	untouched := newScanGit(&countingGit{headsErr: errors.New("must not be asked")})
	if !untouched.unchanged(ctx) {
		t.Error("a scan that asked origin nothing has nothing to confirm")
	}

	g, inner := scanned()
	if !g.unchanged(ctx) || inner.heads != 2 {
		t.Errorf("an identical listing must confirm the snapshot, with one more round trip (listings: %d)", inner.heads)
	}

	for name, tc := range map[string]struct {
		now  map[string]string
		want bool
	}{
		"an asked branch moved":         {map[string]string{"main": "aaa", "hoist/env/one": "ddd", "renovate/x": "rrr"}, false},
		"an asked branch disappeared":   {map[string]string{"main": "aaa", "renovate/x": "rrr"}, false},
		"an absent branch appeared":     {map[string]string{"main": "aaa", "hoist/env/one": "bbb", "hoist/env/gone": "ggg", "renovate/x": "rrr"}, false},
		"the base moved":                {map[string]string{"main": "eee", "hoist/env/one": "bbb", "renovate/x": "rrr"}, false},
		"an unrelated branch moved":     {map[string]string{"main": "aaa", "hoist/env/one": "bbb", "renovate/x": "sss"}, true},
		"an unrelated branch appeared":  {map[string]string{"main": "aaa", "hoist/env/one": "bbb", "renovate/x": "rrr", "feature/y": "yyy"}, true},
		"an unrelated branch went away": {map[string]string{"main": "aaa", "hoist/env/one": "bbb"}, true},
	} {
		g, inner := scanned()
		inner.headsOut = tc.now
		if got := g.unchanged(ctx); got != tc.want {
			t.Errorf("%s: unchanged = %v, want %v", name, got, tc.want)
		}
	}

	g, inner = scanned()
	inner.headsErr = errors.New("connection reset")
	if g.unchanged(ctx) {
		t.Error("a listing that fails confirms nothing")
	}

	// A fetched tip is checked against the listing too, including for a scan that only fetched.
	fetchOnly := func(missing bool, now map[string]string) bool {
		inner := &countingGit{headsOut: now, fetchMissing: missing}
		g := newScanGit(inner)
		if _, _, err := g.FetchBranch(ctx, "/clone", "origin", "main"); err != nil {
			t.Fatal(err)
		}
		return g.unchanged(ctx)
	}
	if !fetchOnly(false, map[string]string{"main": "sha-of-main"}) {
		t.Error("a fetched tip the listing agrees with must be confirmed")
	}
	if fetchOnly(false, map[string]string{"main": "somewhere-else"}) {
		t.Error("a fetched tip the base has since left must not be confirmed")
	}
	if fetchOnly(false, map[string]string{}) {
		t.Error("a fetched branch that no longer exists must not be confirmed")
	}
	if !fetchOnly(true, map[string]string{}) {
		t.Error("a branch the fetch found missing, and that is still missing, must be confirmed")
	}
	if fetchOnly(true, map[string]string{"main": "now-it-exists"}) {
		t.Error("a branch the fetch found missing that has since appeared must not be confirmed")
	}
}

// afterFirstListingGit runs hook once, right after the scan's snapshot of origin's branches is
// taken — the moment from which anything that changes on origin is invisible to that snapshot.
type afterFirstListingGit struct {
	git.Git
	hook  func()
	fired bool
}

func (g *afterFirstListingGit) LsRemoteHeads(ctx context.Context, cloneDir, remote string) (map[string]string, error) {
	heads, err := g.Git.LsRemoteHeads(ctx, cloneDir, remote)
	if err == nil && !g.fired {
		g.fired = true
		g.hook()
	}
	return heads, err
}

// TestScanDoesNotCallAPromotionFinishedOnAStaleSnapshot is the dangerous direction: a snapshot
// that says "finished" about a promotion origin no longer agrees is finished would let a second
// promotion into the env. Here the promotion's branch comes back on origin after the snapshot
// was taken; the snapshot still lists it as gone.
func TestScanDoesNotCallAPromotionFinishedOnAStaleSnapshot(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)

	hooked := &afterFirstListingGit{Git: git.Exec{}, hook: func() {
		runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Branch)
	}}
	deps := fx.svc.deps
	deps.Git = func() git.Git { return hooked }
	svc := New(fx.svc.settings, deps)

	conflict, _, err := svc.FindInFlight(context.Background(), "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if !hooked.fired {
		t.Fatal("control: the scan never took a listing, so nothing was tested")
	}
	if conflict == nil {
		t.Fatal("the scan called a promotion finished from a snapshot origin had already moved past")
	}

	// The same for the listing: nothing is reported done, and nothing archived, on that snapshot.
	hooked.fired = false
	runGitHost(t, fx.clone, "push", "-q", "origin", ":refs/heads/"+s.Branch)
	base := withConfig(fx)
	ldeps := base.deps
	ldeps.Git = func() git.Git { return hooked }
	listed, err := New(base.settings, ldeps).List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Done || listed[0].Archived {
		t.Fatalf("List reported done=%v archived=%v on a stale snapshot: %+v", listed[0].Done, listed[0].Archived, listed[0].Last)
	}
}

// TestListSharesOriginAcrossPromotionsThatNeverGoTerminal covers the states that actually pile
// up: merged, branch deleted, but never satisfied at rolled-out (#168). They are not finished,
// so their verdict has to come from the shared snapshot too, or the listing's cost grows with
// them exactly as before.
func TestListSharesOriginAcrossPromotionsThatNeverGoTerminal(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)
	saveStateAs(t, s, "second-superseded")
	saveStateAs(t, s, "third-superseded")
	// The env now runs something newer than any of them promoted.
	ro, err := fx.svc.Rollout("")
	if err != nil {
		t.Fatal(err)
	}
	ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Namespace: "app-production",
		Name:      "app",
		Images:    []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v9@sha256:" + strings.Repeat("9", 64)}},
		Complete:  true,
	})

	// A landed promotion's first listing also tidies up after it (Service.CleanupLanded), and
	// that asks origin about the one promotion, live — deliberately not through the scan's
	// snapshot, since it is about to delete something. It happens once per promotion, so it is
	// paid here, before anything is counted; TestListPaysForCleanupOncePerPromotion pins that
	// the listing counted below, and every one after it, is back to the scan's own cost.
	if _, err := withConfig(fx).List(context.Background(), ListOpts{}); err != nil {
		t.Fatal(err)
	}

	counting := &countingGit{Git: git.Exec{}}
	base := withConfig(fx)
	deps := base.deps
	deps.Git = func() git.Git { return counting }
	listed, err := New(base.settings, deps).List(context.Background(), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("List = %d entries, want 3", len(listed))
	}
	for _, l := range listed {
		if l.Err != nil || l.Done || l.Last.Step != engine.StepRolledOut {
			t.Fatalf("control: %s must stop at rolled-out, not done (err=%v, done=%v, at %s)", l.State.ID, l.Err, l.Done, l.Last.Step)
		}
	}
	if counting.fetch != 1 || counting.heads != 2 || counting.branch != 0 {
		t.Errorf("List over three never-terminal promotions: %d fetches, %d listings, %d per-branch ls-remotes; want 1, 2, 0",
			counting.fetch, counting.heads, counting.branch)
	}
}

// The same two guards, through the scan's other callers: `resume --env` must not answer from a
// snapshot origin has moved past, nor report a snapshot's own inconsistency as a promotion it
// could not confirm; and List must not report that inconsistency as a promotion's error.
func TestResumeByEnvAndListRedoLiveWhenTheSnapshotCannotBeTrusted(t *testing.T) {
	t.Run("FindInFlightForEnv: origin moved under the snapshot", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		hooked := &afterFirstListingGit{Git: git.Exec{}, hook: func() {
			runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Branch)
		}}
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return hooked }
		st, err := New(base.settings, deps).FindInFlightForEnv(context.Background(), "app-production", Hooks{})
		if err != nil || st == nil || st.ID != s.ID {
			t.Fatalf("the promotion whose branch came back is the one in flight; got %v, %v", st, err)
		}
	})
	t.Run("FindInFlightForEnv: the snapshot pass failed", func(t *testing.T) {
		fx := newInflightFixture(t)
		directPromotion(t, fx)
		runGitHost(t, fx.clone, "fetch", "-q", "origin", "main")
		runGitHost(t, fx.clone, "merge", "-q", "--ff-only", "origin/main")
		runGitHost(t, fx.clone, "commit", "-q", "--allow-empty", "-m", "unrelated later commit")
		runGitHost(t, fx.clone, "push", "-q", "origin", "main")
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return &aheadListingGit{Git: git.Exec{}, base: "main"} }
		var nf *NotFoundError
		if _, err := New(base.settings, deps).FindInFlightForEnv(context.Background(), "app-production", Hooks{}); !errors.As(err, &nf) {
			t.Fatalf("a landed direct promotion leaves nothing to resume; the snapshot's own error must not be reported: %v", err)
		}
	})
	t.Run("List: the snapshot pass failed", func(t *testing.T) {
		fx := newInflightFixture(t)
		directPromotion(t, fx)
		runGitHost(t, fx.clone, "fetch", "-q", "origin", "main")
		runGitHost(t, fx.clone, "merge", "-q", "--ff-only", "origin/main")
		runGitHost(t, fx.clone, "commit", "-q", "--allow-empty", "-m", "unrelated later commit")
		runGitHost(t, fx.clone, "push", "-q", "origin", "main")
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return &aheadListingGit{Git: git.Exec{}, base: "main"} }
		listed, err := New(base.settings, deps).List(context.Background(), ListOpts{})
		if err != nil || len(listed) != 1 {
			t.Fatalf("List = %v, %v", listed, err)
		}
		if listed[0].Err != nil {
			t.Fatalf("the snapshot's own inconsistency was reported as the promotion's error: %v", listed[0].Err)
		}
	})
}

// An unreachable origin costs one failed snapshot pass on top of what it cost before, not a
// second failed connection per state.
func TestListAgainstAnUnreachableOriginFailsEachStateOnce(t *testing.T) {
	fx := newInflightFixture(t)
	s := finishedPromotion(t, fx)
	saveStateAs(t, s, "second-finished")
	saveStateAs(t, s, "third-finished")

	// A finished promotion's first listing also cleans up after it; this test is about every
	// listing after that, with the worktrees gone — the steady state, and the one in which the
	// steps before the merge have nothing local to be satisfied by and so depend on the walk
	// not asking origin about the landing a second time (engine's
	// TestWalkersDoNotAskAgainAfterTheirOwnProbe).
	if _, err := withConfig(fx).List(context.Background(), ListOpts{}); err != nil {
		t.Fatal(err)
	}
	dead := &countingGit{Git: git.Exec{}, fetchErr: errors.New("could not read from remote"), headsErr: errors.New("could not read from remote")}
	base := withConfig(fx)
	deps := base.deps
	deps.Git = func() git.Git { return dead }
	listed, err := New(base.settings, deps).List(context.Background(), ListOpts{})
	if err != nil || len(listed) != 3 {
		t.Fatalf("List = %d entries, %v", len(listed), err)
	}
	for _, l := range listed {
		if l.Err == nil {
			t.Fatalf("control: %s must report the unreachable origin", l.State.ID)
		}
	}
	// One fetch for the snapshot pass, shared; one per state for the live pass that reports it.
	if dead.fetch != 4 {
		t.Errorf("three states against a dead origin made %d fetch attempts, want 4", dead.fetch)
	}
}
