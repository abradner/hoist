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
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// landedPRState drives fx's promotion through the merge (branch, commit, push, PR, CI, approval,
// merge, remote branch deleted) with the fixture's merge-simulating forge, so origin's base
// really carries the merge — the state MergedStep.Observe calls landed.
func landedPRState(t *testing.T, fx inflightFixture) *engine.PromotionState {
	t.Helper()
	s := buildPROpenedPromotionsState(t, fx)
	s.Approval = "auto"
	f, err := fx.svc.deps.Forge("example/gitops")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Drive(context.Background(), engine.CoreSteps(git.Exec{}, f, nil), s, nil); err != nil {
		t.Fatalf("driving to merged: %v", err)
	}
	return s
}

// landedDirectState drives fx's promotion straight onto origin's base, as --direct does.
func landedDirectState(t *testing.T, fx inflightFixture) *engine.PromotionState {
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
		ID: id, RepoFullName: "example/gitops", SourceEnv: plan.SourceEnv, TargetEnv: plan.TargetEnv,
		Branch: engine.BranchName(plan.TargetEnv, id), CloneDir: fx.clone, WorktreeDir: wt, Base: "main",
		Direct: true, Edits: plan.Edits, CommitMessage: engine.RenderCommitMessage(id, plan),
	}
	if err := engine.Drive(context.Background(), engine.DirectSteps(git.Exec{}, nil, true, nil), s, nil); err != nil {
		t.Fatalf("driving the direct promotion: %v", err)
	}
	return s
}

// cloneSnapshot is everything about the operator's own clone a cleanup must leave exactly as it
// found it (AGENTS.md §4.6): which branch is checked out and where, the index and working tree,
// every local branch but the promotion's own, and every worktree but the promotion's own.
func cloneSnapshot(t *testing.T, clone string, s *engine.PromotionState) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("head: " + outGitHost(t, clone, "rev-parse", "HEAD"))
	b.WriteString("on: " + outGitHost(t, clone, "symbolic-ref", "HEAD"))
	b.WriteString("status: " + outGitHost(t, clone, "status", "--porcelain"))
	for _, line := range strings.Split(outGitHost(t, clone, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"), "\n") {
		if !strings.HasPrefix(line, "refs/heads/"+s.Branch+" ") {
			b.WriteString("branch: " + line + "\n")
		}
	}
	for _, line := range strings.Split(outGitHost(t, clone, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") && !strings.HasSuffix(line, "/"+s.ID) {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s should still exist: %v", path, err)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should be gone, stat err = %v", path, err)
	}
}

func branchExists(t *testing.T, clone, branch string) bool {
	t.Helper()
	ok, err := (git.Exec{}).LocalBranchExists(context.Background(), clone, branch)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestCleanupLandedRemovesWorktreeAndLocalBranch is the operator's report turned into a test:
// a landed promotion's worktree and its local branch are removed, on both paths, and nothing
// else in the operator's clone moves. A second call is a no-op.
func TestCleanupLandedRemovesWorktreeAndLocalBranch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T, inflightFixture) *engine.PromotionState
	}{
		{"PR path, merged", landedPRState},
		{"direct path, pushed", landedDirectState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInflightFixture(t)
			s := tc.build(t, fx)
			svc := withConfig(fx)
			mustExist(t, s.WorktreeDir)
			if !branchExists(t, fx.clone, s.Branch) {
				t.Fatal("fixture precondition: the promotion's local branch should exist")
			}
			before := cloneSnapshot(t, fx.clone, s)

			lines, err := svc.CleanupLanded(context.Background(), s)
			if err != nil {
				t.Fatalf("CleanupLanded: %v", err)
			}
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "removed worktree ") || !strings.HasPrefix(lines[1], "deleted local branch ") {
				t.Fatalf("lines = %q, want one worktree removal then one branch deletion", lines)
			}
			mustBeGone(t, s.WorktreeDir)
			if branchExists(t, fx.clone, s.Branch) {
				t.Fatalf("local branch %s should be gone", s.Branch)
			}
			if _, registered, err := (git.Exec{}).WorktreeBranch(context.Background(), fx.clone, s.WorktreeDir); err != nil || registered {
				t.Fatalf("the worktree should be deregistered from the clone: registered=%v err=%v", registered, err)
			}
			if after := cloneSnapshot(t, fx.clone, s); after != before {
				t.Fatalf("the operator's clone changed beyond the promotion's own worktree and branch:\nbefore:\n%s\nafter:\n%s", before, after)
			}

			again, err := svc.CleanupLanded(context.Background(), s)
			if err != nil || len(again) != 0 {
				t.Fatalf("a second cleanup must be a no-op: lines=%q err=%v", again, err)
			}
		})
	}
}

// TestCleanupLandedLeavesAnUnlandedPromotionAlone is the other half of the rule: a promotion
// that has not landed keeps its worktree whatever is in it — here an open PR's worktree holding
// an uncommitted file, and a direct promotion that committed but never pushed, whose worktree is
// the only place that commit exists.
func TestCleanupLandedLeavesAnUnlandedPromotionAlone(t *testing.T) {
	t.Run("PR open, uncommitted work in the worktree", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := buildPROpenedPromotionsState(t, fx)
		scratch := filepath.Join(s.WorktreeDir, "uncommitted.txt")
		if err := os.WriteFile(scratch, []byte("not committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		lines, err := withConfig(fx).CleanupLanded(context.Background(), s)
		if err != nil || len(lines) != 0 {
			t.Fatalf("an unlanded promotion must be left alone: lines=%q err=%v", lines, err)
		}
		mustExist(t, scratch)
		if !branchExists(t, fx.clone, s.Branch) {
			t.Fatal("the local branch of an unlanded promotion must not be deleted")
		}
	})
	t.Run("direct, committed but never pushed", func(t *testing.T) {
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
		s := &engine.PromotionState{
			ID: id, RepoFullName: "example/gitops", SourceEnv: plan.SourceEnv, TargetEnv: plan.TargetEnv,
			Branch: engine.BranchName(plan.TargetEnv, id), CloneDir: fx.clone, WorktreeDir: wt, Base: "main",
			Direct: true, Edits: plan.Edits, CommitMessage: engine.RenderCommitMessage(id, plan),
		}
		g := git.Exec{}
		if err := (engine.BranchedStep{Git: g}).Act(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if err := (engine.CommittedStep{Git: g}).Act(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		lines, err := withConfig(fx).CleanupLanded(context.Background(), s)
		if err != nil || len(lines) != 0 {
			t.Fatalf("an unpushed promotion must be left alone: lines=%q err=%v", lines, err)
		}
		mustExist(t, wt)
		if !branchExists(t, fx.clone, s.Branch) {
			t.Fatal("the local branch holding the only copy of an unpushed commit must not be deleted")
		}
	})
}

// TestCleanupRefusesAStateFileAimedOutsideTheCache names the attacker: a state file. Each case
// starts from a promotion that genuinely landed — so nothing but the containment rule stands
// between the state file and a removal — and changes what the file records to aim that removal
// somewhere it must never go. In every case the cleanup is refused, the victim is intact, and
// the operator's clone is untouched.
func TestCleanupRefusesAStateFileAimedOutsideTheCache(t *testing.T) {
	sentinel := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "precious.txt")
		if err := os.WriteFile(f, []byte("do not delete\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, tc := range []struct {
		name string
		// aim rewrites s and returns a path that must survive.
		aim func(t *testing.T, fx inflightFixture, s *engine.PromotionState) (victim string)
	}{
		{
			// The id is a path: <cache>/hoist/worktrees/../../victim is <cache>/victim. Every
			// other field is made to agree with it, as a careful forger would.
			name: "id is a path out of the worktrees directory",
			aim: func(t *testing.T, _ inflightFixture, s *engine.PromotionState) string {
				s.ID = "../../victim"
				dir, err := engine.WorktreeDir(s.ID)
				if err != nil {
					t.Fatal(err)
				}
				s.WorktreeDir = dir
				s.Branch = engine.BranchName(s.TargetEnv, s.ID)
				return sentinel(t, dir)
			},
		},
		{
			name: "recorded worktree is some other directory",
			aim: func(t *testing.T, _ inflightFixture, s *engine.PromotionState) string {
				dir := filepath.Join(t.TempDir(), "documents")
				s.WorktreeDir = dir
				return sentinel(t, dir)
			},
		},
		{
			name: "recorded branch is the operator's own",
			aim: func(_ *testing.T, fx inflightFixture, s *engine.PromotionState) string {
				s.Branch = "main"
				return filepath.Join(fx.clone, ".git", "refs", "heads", "main")
			},
		},
		{
			name: "recorded clone is another repository",
			aim: func(t *testing.T, _ inflightFixture, s *engine.PromotionState) string {
				other := filepath.Join(t.TempDir(), "other-repo")
				runGitHost(t, "", "init", "-q", "-b", "main", other)
				s.CloneDir = other
				return filepath.Join(other, ".git", "HEAD")
			},
		},
		{
			// The cache entry itself is replaced by a link to a directory that is not hoist's.
			name: "worktree path is a symbolic link",
			aim: func(t *testing.T, fx inflightFixture, s *engine.PromotionState) string {
				if err := (git.Exec{}).RemoveWorktree(context.Background(), fx.clone, s.WorktreeDir); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "elsewhere")
				f := sentinel(t, target)
				if err := os.Symlink(target, s.WorktreeDir); err != nil {
					t.Fatal(err)
				}
				return f
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInflightFixture(t)
			s := landedDirectState(t, fx)
			svc := withConfig(fx)
			if landed, _, err := svc.observeLanding(context.Background(), s); err != nil || !landed {
				t.Fatalf("fixture precondition: the promotion must have landed (landed=%v err=%v)", landed, err)
			}
			honest := *s
			victim := tc.aim(t, fx, s)
			before := cloneSnapshot(t, fx.clone, &honest)
			mainBefore := outGitHost(t, fx.clone, "rev-parse", "refs/heads/main")

			lines, err := svc.CleanupLanded(context.Background(), s)
			var refused *engine.CleanupRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("CleanupLanded = %q, %v — want a *engine.CleanupRefusedError", lines, err)
			}
			if len(lines) != 0 {
				t.Fatalf("a refused cleanup must remove nothing, got %q", lines)
			}
			mustExist(t, victim)
			if _, err := os.Lstat(s.WorktreeDir); tc.name == "worktree path is a symbolic link" && err != nil {
				t.Fatalf("the link itself must be left where it is: %v", err)
			}
			if after := cloneSnapshot(t, fx.clone, &honest); after != before {
				t.Fatalf("the operator's clone changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if mainAfter := outGitHost(t, fx.clone, "rev-parse", "refs/heads/main"); mainAfter != mainBefore {
				t.Fatalf("refs/heads/main moved: %s -> %s", mainBefore, mainAfter)
			}
		})
	}
}

// TestCleanupRefusesAWorktreeOnAnotherBranch: the directory is the promotion's own, but someone
// has checked something else out in it. That is no longer the worktree hoist made.
func TestCleanupRefusesAWorktreeOnAnotherBranch(t *testing.T) {
	fx := newInflightFixture(t)
	s := landedDirectState(t, fx)
	runGitHost(t, s.WorktreeDir, "switch", "-q", "-c", "operators-own-work")

	lines, err := withConfig(fx).CleanupLanded(context.Background(), s)
	var refused *engine.CleanupRefusedError
	if !errors.As(err, &refused) || len(lines) != 0 {
		t.Fatalf("CleanupLanded = %q, %v — want it refused with nothing removed", lines, err)
	}
	mustExist(t, s.WorktreeDir)
	if !branchExists(t, fx.clone, "operators-own-work") {
		t.Fatal("the operator's branch must survive")
	}
}

// TestAbandonRemovesWorktreeAndLocalBranch: abandon retires everything the promotion made on
// this machine, not just its PR, remote branch and state file — and says so.
func TestAbandonRemovesWorktreeAndLocalBranch(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	before := cloneSnapshot(t, fx.clone, s)

	lines, err := withConfig(fx).Abandon(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Abandon: %v (lines so far: %v)", err, lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"removed worktree " + s.WorktreeDir, "deleted local branch " + s.Branch} {
		if !strings.Contains(joined, want) {
			t.Errorf("abandon's actions %q do not include %q", lines, want)
		}
	}
	mustBeGone(t, s.WorktreeDir)
	if branchExists(t, fx.clone, s.Branch) {
		t.Fatalf("local branch %s should be gone after abandon", s.Branch)
	}
	if got, err := fileStore.Load(s.ID); err != nil || got != nil {
		t.Fatalf("state file should be deleted after abandon: Load = %v, %v", got, err)
	}
	if after := cloneSnapshot(t, fx.clone, s); after != before {
		t.Fatalf("the operator's clone changed beyond the promotion's own worktree and branch:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestAbandonStillAbandonsWhenCleanupIsRefused: a state file hoist will not follow for a removal
// is still a promotion the operator asked to retire. The PR, the remote branch and the state file
// go; the paths are left, and the action lines say why.
func TestAbandonStillAbandonsWhenCleanupIsRefused(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	realWorktree := s.WorktreeDir
	s.WorktreeDir = filepath.Join(t.TempDir(), "recorded-elsewhere")
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}

	lines, err := withConfig(fx).Abandon(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Abandon: %v (lines so far: %v)", err, lines)
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "left the worktree and local branch in place") {
		t.Fatalf("abandon's actions %q do not report the refused cleanup", lines)
	}
	mustExist(t, realWorktree)
	if got, err := fileStore.Load(s.ID); err != nil || got != nil {
		t.Fatalf("state file should be deleted after abandon: Load = %v, %v", got, err)
	}
}

// TestListCleansUpLandedPromotions: `hoist promotions` and the TUI's in-flight pane both observe
// through List, so that is where a promotion that landed while nobody was driving it gets tidied
// — and where one that has not landed is left exactly as it is.
func TestListCleansUpLandedPromotions(t *testing.T) {
	t.Run("landed", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedPRState(t, fx)
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		listed, err := withConfig(fx).List(context.Background(), ListOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].CleanupErr != nil || len(listed[0].Cleaned) != 2 {
			t.Fatalf("List = %+v, want one entry with its worktree and branch cleaned", listed)
		}
		mustBeGone(t, s.WorktreeDir)
		if branchExists(t, fx.clone, s.Branch) {
			t.Fatal("the landed promotion's local branch should be gone")
		}
	})
	t.Run("not landed", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := buildPROpenedPromotionsState(t, fx)
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		listed, err := withConfig(fx).List(context.Background(), ListOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].Done || len(listed[0].Cleaned) != 0 || listed[0].CleanupErr != nil {
			t.Fatalf("List = %+v, want one in-flight entry, nothing cleaned", listed)
		}
		mustExist(t, s.WorktreeDir)
	})
}

// TestDrivingAPromotionToDoneCleansUpAfterItself: the Drive that Resume (and StartPromotion,
// which builds its Drive the same way) hands back removes the worktree and local branch once the
// promotion it is driving lands — nobody has to list or collect anything afterwards. A second
// Resume of the finished promotion then stays finished and does not rebuild the worktree.
func TestDrivingAPromotionToDoneCleansUpAfterItself(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	svc := withConfig(fx)
	mustExist(t, s.WorktreeDir)

	d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background(), RunHooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mustBeGone(t, s.WorktreeDir)
	if branchExists(t, fx.clone, s.Branch) {
		t.Fatalf("local branch %s should be gone once the promotion is done", s.Branch)
	}

	again, err := svc.Resume(context.Background(), s.ID, ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tick, err := again.Step(context.Background())
	if err != nil || !tick.Done {
		t.Fatalf("resuming a finished promotion: done=%v err=%v", tick.Done, err)
	}
	mustBeGone(t, s.WorktreeDir)
}

// TestSteppingAPromotionCleansUpAtLandingNotAtDone is the TUI's shape of the same thing: a drive
// stepped one poll at a time (session.Controller) removes the worktree on the tick that shows
// the promotion landed, while it is still waiting on the rollout — which is what makes the
// cleanup independent of a rollout that never completes (#168).
func TestSteppingAPromotionCleansUpAtLandingNotAtDone(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	ro, _, err := fx.svc.deps.Rollout("")
	if err != nil {
		t.Fatal(err)
	}
	// The Deployment is running the promoted image but has not finished rolling.
	ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Namespace: "app-production", Name: "app",
		Images: []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
	})

	d, err := withConfig(fx).Resume(context.Background(), s.ID, ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tick, err := d.Step(context.Background())
	if tick.Done || !tick.Waiting {
		t.Fatalf("fixture precondition: the promotion should be waiting on its rollout, got done=%v waiting=%v err=%v", tick.Done, tick.Waiting, err)
	}
	if !engine.Landed(tick.Statuses) {
		t.Fatalf("fixture precondition: the tick should show the merge landed: %+v", tick.Statuses)
	}
	mustBeGone(t, s.WorktreeDir)
	if branchExists(t, fx.clone, s.Branch) {
		t.Fatalf("local branch %s should be gone once the promotion has landed", s.Branch)
	}
}

// TestCleanupLandedRefusesAWorktreeHoldingWorkThatIsNotOnTheRemote: "landed" comes from a forge
// lookup and a state file, and both can be wrong about THIS worktree. Each case makes the landing
// observation say yes while the worktree or its branch holds something that exists nowhere else;
// the cleanup must look at the worktree itself and refuse.
func TestCleanupLandedRefusesAWorktreeHoldingWorkThatIsNotOnTheRemote(t *testing.T) {
	refused := func(t *testing.T, fx inflightFixture, s *engine.PromotionState, mustSurvive string, contains string) {
		t.Helper()
		svc := withConfig(fx)
		if landed, _, err := svc.observeLanding(context.Background(), s); err != nil || !landed {
			t.Fatalf("fixture precondition: the landing observation must say landed, or this proves nothing (landed=%v err=%v)", landed, err)
		}
		for _, dry := range []bool{true, false} {
			lines, err := svc.cleanupLanded(context.Background(), s, dry)
			var r *engine.CleanupRefusedError
			if !errors.As(err, &r) || len(lines) != 0 {
				t.Fatalf("dryRun=%v: cleanupLanded = %q, %v — want it refused with nothing removed", dry, lines, err)
			}
			if !strings.Contains(r.Reason, contains) {
				t.Fatalf("refusal %q does not mention %q", r.Reason, contains)
			}
		}
		mustExist(t, mustSurvive)
		if !branchExists(t, fx.clone, s.Branch) {
			t.Fatalf("local branch %s must survive", s.Branch)
		}
	}

	// The same id run a second time (a rollback to a digest set promoted before): the forge
	// still holds the first run's merged PR under this branch name (#41), and the second run's
	// commit is in the worktree, not yet pushed.
	t.Run("a second run of the same id has committed but not pushed", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedPRState(t, fx)
		secondRun := filepath.Join(s.WorktreeDir, "second-run.txt")
		if err := os.WriteFile(secondRun, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (git.Exec{}).Commit(context.Background(), s.WorktreeDir, "the second run's commit", []string{"second-run.txt"}, time.Minute, nil); err != nil {
			t.Fatal(err)
		}
		refused(t, fx, s, secondRun, "exists nowhere else")
	})

	t.Run("landed, but something uncommitted is in the worktree", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedDirectState(t, fx)
		scratch := filepath.Join(s.WorktreeDir, "uncommitted.txt")
		if err := os.WriteFile(scratch, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refused(t, fx, s, scratch, "uncommitted changes")
	})

	// A state file that names the WRONG repo for a live promotion's id: the worktree directory is
	// repo A's live worktree, the state says it belongs to repo B. B has no such worktree and no
	// such branch, so every question asked of B about them is answered "nothing there".
	t.Run("state file names another configured repo for a live promotion's worktree", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := buildPROpenedPromotionsState(t, fx)
		scratch := filepath.Join(s.WorktreeDir, "uncommitted.txt")
		if err := os.WriteFile(scratch, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		origin := strings.TrimSpace(outGitHost(t, fx.clone, "remote", "get-url", "origin"))
		cloneB := filepath.Join(t.TempDir(), "repo-b")
		runGitHost(t, "", "clone", "-q", origin, cloneB)

		base := withConfig(fx)
		set := base.Settings()
		cfg := *set.Config
		cfg.Repos = append(append([]config.RepoConfig{}, cfg.Repos...), config.RepoConfig{GitHub: "example/other", Dir: cloneB})
		set.Config = &cfg
		svc := New(set, base.deps)

		forged := *s
		forged.RepoFullName = "example/other"
		forged.CloneDir = cloneB
		forged.Direct = true
		forged.CommitSHA = strings.TrimSpace(outGitHost(t, cloneB, "rev-parse", "refs/remotes/origin/main"))
		if landed, _, err := svc.observeLanding(context.Background(), &forged); err != nil || !landed {
			t.Fatalf("fixture precondition: the forged state must read as landed (landed=%v err=%v)", landed, err)
		}
		lines, err := svc.CleanupLanded(context.Background(), &forged)
		var r *engine.CleanupRefusedError
		if !errors.As(err, &r) || len(lines) != 0 {
			t.Fatalf("CleanupLanded = %q, %v — want it refused with nothing removed", lines, err)
		}
		if !strings.Contains(r.Reason, "not a registered worktree") {
			t.Fatalf("refusal %q does not say the directory is not that clone's worktree", r.Reason)
		}
		mustExist(t, scratch)
	})

	// A state file whose base branch is the promotion's own branch: its pushed commit is then
	// "on the base" by definition.
	t.Run("state file records the promotion's own branch as its base", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := buildPROpenedPromotionsState(t, fx)
		forged := *s
		forged.Direct = true
		forged.Base = s.Branch
		svc := withConfig(fx)
		lines, err := svc.CleanupLanded(context.Background(), &forged)
		var r *engine.CleanupRefusedError
		if !errors.As(err, &r) || len(lines) != 0 {
			t.Fatalf("CleanupLanded = %q, %v — want it refused with nothing removed", lines, err)
		}
		mustExist(t, s.WorktreeDir)
		if !branchExists(t, fx.clone, s.Branch) {
			t.Fatal("the live promotion's local branch must survive")
		}
	})

	// A state file for a live, PR-open promotion rewritten to say it is a direct promotion whose
	// commit is the base tip: DirectPushedStep reads that as landed on sight.
	t.Run("state file forged to read as a landed direct promotion", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := buildPROpenedPromotionsState(t, fx)
		tip := strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "refs/remotes/origin/main"))
		forged := *s
		forged.Direct = true
		forged.CommitSHA = tip
		refused(t, fx, &forged, filepath.Join(s.WorktreeDir, ".git"), "exists nowhere else")
	})
}

// TestCleanupLandedDoesNotInferLandingFromAStateWithNothingToJudgeBy is the service-level face of
// engine's own test of the same name: a direct state recording a commit but no edits or blobs
// makes DirectPushedStep's content check vacuous. observeLanding must not ask it.
func TestCleanupLandedDoesNotInferLandingFromAStateWithNothingToJudgeBy(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	forged := *s
	forged.Direct = true
	forged.CommitSHA = strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "HEAD"))
	forged.Edits = nil
	forged.ExpectedBlobs = nil
	landed, _, err := withConfig(fx).observeLanding(context.Background(), &forged)
	if err != nil || landed {
		t.Fatalf("observeLanding = %v, %v — a state with nothing to judge a landing by has not landed", landed, err)
	}
}

// TestCleanupConvergesAfterTheWorktreeWasDeletedByHand: the operator clears the cache directory
// with rm. The worktree is still registered in the clone, so git will not delete the branch
// until it is deregistered — which cleanup must do even though there is no directory left, and
// even when the cache path runs through a symlink (as the fixture's does on macOS, and as this
// test arranges explicitly everywhere else).
func TestCleanupConvergesAfterTheWorktreeWasDeletedByHand(t *testing.T) {
	fx := newInflightFixture(t)
	realCache := filepath.Join(t.TempDir(), "real-cache")
	if err := os.Mkdir(realCache, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cache-link")
	if err := os.Symlink(realCache, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", link)

	s := landedDirectState(t, fx)
	if err := os.RemoveAll(s.WorktreeDir); err != nil {
		t.Fatal(err)
	}
	svc := withConfig(fx)
	lines, err := svc.CleanupLanded(context.Background(), s)
	if err != nil {
		t.Fatalf("CleanupLanded: %v", err)
	}
	if linesWith(lines, "deleted local branch "+s.Branch) != 1 {
		t.Fatalf("lines = %q, want the local branch deleted", lines)
	}
	if branchExists(t, fx.clone, s.Branch) {
		t.Fatal("the local branch should be gone")
	}
	if again, err := svc.CleanupLanded(context.Background(), s); err != nil || len(again) != 0 {
		t.Fatalf("a second cleanup must have nothing left to do: %q, %v", again, err)
	}
}

func linesWith(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// branchCheckCountingGit counts the calls cleanup's own "is there anything left" check makes.
type branchCheckCountingGit struct {
	git.Git
	branchChecks *int
}

func (c branchCheckCountingGit) LocalBranchExists(ctx context.Context, cloneDir, branch string) (bool, error) {
	*c.branchChecks++
	return c.Git.LocalBranchExists(ctx, cloneDir, branch)
}

// TestSteppedDriveCleansUpOnce: a drive that waits on its rollout is stepped every poll for as
// long as that takes. Once it has cleaned up, later ticks must not go back and look again.
func TestSteppedDriveCleansUpOnce(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	ro, _, err := fx.svc.deps.Rollout("")
	if err != nil {
		t.Fatal(err)
	}
	ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Namespace: "app-production", Name: "app",
		Images: []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
	})
	checks := 0
	base := withConfig(fx)
	deps := base.deps
	deps.Git = func() git.Git { return branchCheckCountingGit{Git: git.Exec{}, branchChecks: &checks} }
	svc := New(base.Settings(), deps)

	d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if tick, _ := d.Step(context.Background()); !tick.Waiting || !engine.Landed(tick.Statuses) {
		t.Fatalf("fixture precondition: the first tick should land and wait on the rollout: %+v", tick)
	}
	mustBeGone(t, s.WorktreeDir)
	// The first cleanup found the worktree directory and so never had to ask whether the branch
	// alone was left; a later look, with the directory gone, would have to.
	after := checks
	for i := 0; i < 3; i++ {
		if tick, _ := d.Step(context.Background()); !tick.Waiting {
			t.Fatalf("tick %d: expected to keep waiting on the rollout: %+v", i, tick)
		}
	}
	if checks != after {
		t.Fatalf("cleanup looked again on later ticks: %d branch checks after the first cleanup, %d after three more ticks", after, checks)
	}
}

// TestTipIsOnTheRemoteAcceptsTheCommitTheMergedPRMerged: a squash merge puts a NEW commit on the
// base, so the promotion's own commit is never in the base's history — the evidence that it is
// safe to delete is the forge naming it as the commit the merged PR merged. The state here has
// its commit on its branch and not on the base (an open PR is exactly that shape), so only the
// merged-head comparison can say yes.
func TestTipIsOnTheRemoteAcceptsTheCommitTheMergedPRMerged(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	svc := withConfig(fx)
	tip := strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "refs/heads/"+s.Branch))

	if err := svc.tipIsOnTheRemote(context.Background(), s, tip, tip); err != nil {
		t.Fatalf("the branch tip is the commit the merged PR merged; nothing on it is unlanded: %v", err)
	}
	for _, head := range []string{"", strings.Repeat("0", 40)} {
		err := svc.tipIsOnTheRemote(context.Background(), s, tip, head)
		var refused *engine.CleanupRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("mergedHead=%q: err = %v, want a refusal — the tip is neither that commit nor on the base", head, err)
		}
	}
}

// changedAfterTheCheck is a git.Git that answers the cleanup's own checks as they were a moment
// ago and lets everything else through: the worktree was clean and the branch was at the commit
// that landed when the cleanup looked, and are not any more by the time it removes them.
type changedAfterTheCheck struct {
	git.Git
	cleanWhenChecked bool   // WorktreeDirty answers false whatever is there now
	tipWhenChecked   string // RevParse of the promotion's branch answers this, if set
	branchRef        string
}

func (c changedAfterTheCheck) WorktreeDirty(ctx context.Context, dir string) (bool, error) {
	if c.cleanWhenChecked {
		return false, nil
	}
	return c.Git.WorktreeDirty(ctx, dir)
}

func (c changedAfterTheCheck) RevParse(ctx context.Context, dir, rev string) (string, bool, error) {
	if c.tipWhenChecked != "" && rev == c.branchRef {
		return c.tipWhenChecked, true, nil
	}
	return c.Git.RevParse(ctx, dir, rev)
}

// TestAutomaticCleanupDoesNotRemoveWhatChangedAfterItLooked: the checks and the removal are
// separate moments. A file written into the worktree, or a commit made on the branch, in
// between must be refused by the removal itself — git's own non-force worktree removal, and a
// branch deletion that only succeeds at the commit that was checked — not lost.
func TestAutomaticCleanupDoesNotRemoveWhatChangedAfterItLooked(t *testing.T) {
	withGit := func(fx inflightFixture, g git.Git) *Service {
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return g }
		return New(base.Settings(), deps)
	}

	t.Run("a file appears in the worktree", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedDirectState(t, fx)
		late := filepath.Join(s.WorktreeDir, "written-after-the-check.txt")
		if err := os.WriteFile(late, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		svc := withGit(fx, changedAfterTheCheck{Git: git.Exec{}, cleanWhenChecked: true})
		lines, err := svc.CleanupLanded(context.Background(), s)
		if err == nil || len(lines) != 0 {
			t.Fatalf("CleanupLanded = %q, %v — the removal must fail rather than delete the file", lines, err)
		}
		mustExist(t, late)
		if !branchExists(t, fx.clone, s.Branch) {
			t.Fatal("the branch must not be deleted when its worktree could not be removed")
		}
	})

	t.Run("a commit lands on the branch", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedDirectState(t, fx)
		checked := strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "refs/heads/"+s.Branch))
		if err := os.WriteFile(filepath.Join(s.WorktreeDir, "late.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		late, err := (git.Exec{}).Commit(context.Background(), s.WorktreeDir, "made after the check", []string{"late.txt"}, time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		svc := withGit(fx, changedAfterTheCheck{Git: git.Exec{}, tipWhenChecked: checked, branchRef: "refs/heads/" + s.Branch})
		_, err = svc.CleanupLanded(context.Background(), s)
		if err == nil {
			t.Fatal("the branch moved after it was checked; deleting it must fail")
		}
		if now := strings.TrimSpace(outGitHost(t, fx.clone, "rev-parse", "refs/heads/"+s.Branch)); now != late {
			t.Fatalf("the later commit must still be on the branch: %s, want %s", now, late)
		}
	})
}

// failingRemovalGit fails the removal itself, as a locked index or a full disk would.
type failingRemovalGit struct{ git.Git }

func (failingRemovalGit) RemoveCleanWorktree(context.Context, string, string) error {
	return errors.New("unable to remove: device busy")
}

// TestListDoesNotArchiveAPromotionWhoseCleanupFailed: nothing lists an archived state, so a
// landed promotion archived on the same pass its cleanup FAILED would never be retried and its
// worktree would stay for good. One whose cleanup was refused is not archived either: it stays
// listed with the reason rather than becoming an orphan for the gc sweep.
func TestListDoesNotArchiveAPromotionWhoseCleanupFailed(t *testing.T) {
	old := func(s *engine.PromotionState) {
		at := time.Now().Add(-48 * time.Hour)
		s.GeneratedAt = at
		for i := range s.History {
			s.History[i].At = at
		}
	}
	t.Run("cleanup failed", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		old(s)
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		base := withConfig(fx)
		deps := base.deps
		deps.Git = func() git.Git { return failingRemovalGit{git.Exec{}} }
		listed, err := New(base.Settings(), deps).List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Hour})
		if err != nil || len(listed) != 1 {
			t.Fatalf("List = %+v, %v", listed, err)
		}
		if !listed[0].Done || listed[0].CleanupErr == nil {
			t.Fatalf("control: the promotion must be done with a failed cleanup: %+v", listed[0])
		}
		if listed[0].Archived {
			t.Fatal("a promotion whose cleanup failed was archived; nothing would ever retry it")
		}
		if st, err := fileStore.Load(s.ID); err != nil || st == nil {
			t.Fatalf("the state file must still be live: %v, %v", st, err)
		}
	})
	t.Run("control: cleanup succeeded", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		old(s)
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		listed, err := withConfig(fx).List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Hour})
		if err != nil || len(listed) != 1 || listed[0].CleanupErr != nil || !listed[0].Archived {
			t.Fatalf("a finished, cleaned, old promotion is archived as before: %+v, %v", listed, err)
		}
	})
	t.Run("cleanup refused", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := finishedPromotion(t, fx)
		old(s)
		if err := os.WriteFile(filepath.Join(s.WorktreeDir, "uncommitted.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		listed, err := withConfig(fx).List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Hour})
		if err != nil || len(listed) != 1 || listed[0].CleanupErr == nil {
			t.Fatalf("control: want one entry with a refused cleanup: %+v, %v", listed, err)
		}
		if listed[0].Archived {
			t.Fatal("a promotion whose worktree was deliberately left must stay listed with the reason; archived, it becomes an orphan the gc sweep judges by a weaker rule")
		}
	})
}

// TestAbandonLeavesADirectoryThatIsNotItsClonesWorktree: an abandon's confirmation is for the
// promotion. A directory at the promotion's path that the state's clone does not have registered
// as a worktree — another repo's live worktree for the same id, named by a state file with the
// repo wrong — is not what was confirmed, and RemoveWorktree would end in os.RemoveAll.
func TestAbandonLeavesADirectoryThatIsNotItsClonesWorktree(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	scratch := filepath.Join(s.WorktreeDir, "live-work.txt")
	if err := os.WriteFile(scratch, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origin := strings.TrimSpace(outGitHost(t, fx.clone, "remote", "get-url", "origin"))
	cloneB := filepath.Join(t.TempDir(), "repo-b")
	runGitHost(t, "", "clone", "-q", origin, cloneB)
	base := withConfig(fx)
	set := base.Settings()
	cfg := *set.Config
	cfg.Repos = append(append([]config.RepoConfig{}, cfg.Repos...), config.RepoConfig{GitHub: "example/other", Dir: cloneB})
	set.Config = &cfg
	svc := New(set, base.deps)

	forged := *s
	forged.RepoFullName = "example/other"
	forged.CloneDir = cloneB
	forged.PR = nil
	if err := fileStore.Save(&forged); err != nil {
		t.Fatal(err)
	}
	lines, err := svc.Abandon(context.Background(), forged.ID)
	if err != nil {
		t.Fatalf("Abandon: %v (%v)", err, lines)
	}
	if linesWith(lines, "left the worktree and local branch in place") != 1 || linesWith(lines, "removed worktree") != 0 {
		t.Fatalf("abandon's actions %q: want the directory left, and said so", lines)
	}
	mustExist(t, scratch)
}

// flakyForge fails its PR lookups a set number of times, then answers.
type flakyForge struct {
	forge.Forge
	failures *int
}

func (f flakyForge) down() error {
	if *f.failures > 0 {
		*f.failures--
		return errors.New("502 Bad Gateway")
	}
	return nil
}

func (f flakyForge) GetPR(ctx context.Context, number int) (forge.PR, bool, error) {
	if err := f.down(); err != nil {
		return forge.PR{}, false, err
	}
	return f.Forge.GetPR(ctx, number)
}

func (f flakyForge) FindPR(ctx context.Context, headBranch, marker string) (forge.PR, bool, error) {
	if err := f.down(); err != nil {
		return forge.PR{}, false, err
	}
	return f.Forge.FindPR(ctx, headBranch, marker)
}

// flakyRemoteGit fails its per-branch ls-remote a set number of times, then answers.
type flakyRemoteGit struct {
	git.Git
	failures *int
}

func (g flakyRemoteGit) LsRemoteBranch(ctx context.Context, cloneDir, remote, branch string) (string, bool, error) {
	if *g.failures > 0 {
		*g.failures--
		return "", false, errors.New("could not read from remote repository")
	}
	return g.Git.LsRemoteBranch(ctx, cloneDir, remote, branch)
}

// TestALandedCleanedPromotionDoesNotRedoItselfWhenOneLookupFails is the whole thing end to end,
// on both paths: land, clean up, then one poll of the wait on the rollout in which the landing
// cannot be confirmed. Nothing may be rebuilt, re-committed or re-pushed on that poll; the drive
// reports a retryable error; and the next poll carries on as if nothing had happened.
func TestALandedCleanedPromotionDoesNotRedoItselfWhenOneLookupFails(t *testing.T) {
	rolloutPending := func(t *testing.T, fx inflightFixture) {
		t.Helper()
		ro, _, err := fx.svc.deps.Rollout("")
		if err != nil {
			t.Fatal(err)
		}
		ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
			Namespace: "app-production", Name: "app",
			Images: []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		})
	}
	untouched := func(t *testing.T, fx inflightFixture, s engine.PromotionState) {
		t.Helper()
		mustBeGone(t, s.WorktreeDir)
		if branchExists(t, fx.clone, s.Branch) {
			t.Fatalf("the local branch %s was recreated", s.Branch)
		}
		if _, exists, err := (git.Exec{}).LsRemoteBranch(context.Background(), fx.clone, "origin", s.Branch); err != nil || exists {
			t.Fatalf("the promotion's branch was pushed to origin again: exists=%v err=%v", exists, err)
		}
	}

	t.Run("PR path, forge lookup fails", func(t *testing.T) {
		fx := newInflightFixture(t)
		rolloutPending(t, fx)
		s := buildPROpenedPromotionsState(t, fx)
		fx.f.SetHeadSHA(s.PR.Number, s.CommitSHA)
		if err := fileStore.Save(s); err != nil {
			t.Fatal(err)
		}
		failures := 0
		base := withConfig(fx)
		deps := base.deps
		inner := deps.Forge
		deps.Forge = func(repo string) (forge.Forge, error) {
			f, err := inner(repo)
			return flakyForge{Forge: f, failures: &failures}, err
		}
		deps.NoCache = true
		svc := New(base.Settings(), deps)

		d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if tick, err := d.Step(context.Background()); err != nil || !tick.Waiting || !engine.Landed(tick.Statuses) {
			t.Fatalf("fixture precondition: land and wait on the rollout: %+v, %v", tick, err)
		}
		landed := d.State()
		untouched(t, fx, landed)

		failures = 100 // the forge is down for the whole of the next poll
		tick, err := d.Step(context.Background())
		failures = 0
		if err == nil {
			t.Fatalf("the poll could not confirm the landing and must say so: %+v", tick)
		}
		if !engine.Retryable(err) || !tick.Retry {
			t.Fatalf("err = %v (retry=%v), want a retryable error so the wait goes on", err, tick.Retry)
		}
		untouched(t, fx, landed)

		if tick, err := d.Step(context.Background()); err != nil || !tick.Waiting {
			t.Fatalf("the next poll, with the forge back, carries on waiting on the rollout: %+v, %v", tick, err)
		}
		untouched(t, fx, landed)
	})

	t.Run("direct path, origin lookup fails", func(t *testing.T) {
		fx := newInflightFixture(t)
		s := landedDirectState(t, fx)
		if lines, err := withConfig(fx).CleanupLanded(context.Background(), s); err != nil || len(lines) != 2 {
			t.Fatalf("fixture precondition: clean up the landed promotion: %q, %v", lines, err)
		}
		failures := 100
		steps := engine.DirectSteps(flakyRemoteGit{Git: git.Exec{}, failures: &failures}, nil, true, nil)
		before := len(s.History)
		err := engine.Drive(context.Background(), steps, s, nil)
		if err == nil || !engine.Retryable(err) {
			t.Fatalf("err = %v, want a retryable error and nothing redone", err)
		}
		untouched(t, fx, *s)
		for _, h := range s.History[before:] {
			if strings.Contains(h.Detail, "acted") {
				t.Fatalf("a step acted on the failed poll: %+v", h)
			}
		}

		failures = 0
		if err := engine.Drive(context.Background(), steps, s, nil); err != nil {
			t.Fatalf("with origin back the landed promotion is simply done: %v", err)
		}
		untouched(t, fx, *s)
	})
}
