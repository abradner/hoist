package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
)

// gcWorktree registers a worktree of fx's clone at <cache>/worktrees/<name> on branch — what a
// promotion's BranchedStep leaves behind — with no state file naming it.
func gcWorktree(t *testing.T, fx inflightFixture, name, branch string) string {
	t.Helper()
	dir, err := engine.WorktreeDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := (git.Exec{}).Worktree(context.Background(), fx.clone, dir, branch, "main"); err != nil {
		t.Fatalf("creating worktree %s: %v", name, err)
	}
	return dir
}

// diskSnapshot is every path under the test's XDG cache and state homes, plus the clone's local
// branches and registered worktrees — what a dry run must leave exactly as it found it.
func diskSnapshot(t *testing.T, fx inflightFixture) string {
	t.Helper()
	var paths []string
	for _, root := range []string{os.Getenv("XDG_CACHE_HOME"), os.Getenv("XDG_STATE_HOME")} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			// Two levels below hoist/ is enough to see every worktree, state file and cache entry
			// without walking each worktree's own checkout.
			if d.IsDir() && strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			paths = append(paths, p)
			return nil
		})
	}
	sort.Strings(paths)
	return strings.Join(paths, "\n") +
		"\n--branches--\n" + outGitHost(t, fx.clone, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads") +
		"--worktrees--\n" + outGitHost(t, fx.clone, "worktree", "list", "--porcelain")
}

func linesContaining(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// TestGCRemovesLandedAndOrphanedAndKeepsEverythingElse builds one of everything GC can meet
// under the worktrees directory and checks each lands on the right side — and that --dry-run
// lists exactly what a real run then removes while touching nothing.
func TestGCRemovesLandedAndOrphanedAndKeepsEverythingElse(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	root, err := engine.WorktreeDir("x")
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root)

	// Removed: a landed promotion that still has its worktree (it landed before cleanup existed).
	landed := landedPRState(t, fx)
	if err := fileStore.Save(landed); err != nil {
		t.Fatal(err)
	}
	// Removed: an orphan — hoist's own worktree shape, clean, no state file. It carries a commit
	// that exists nowhere else, as an abandoned promotion's does.
	orphan := gcWorktree(t, fx, "orphan2222", "hoist/app-staging/orphan2222")
	if err := os.WriteFile(filepath.Join(orphan, "edit.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (git.Exec{}).Commit(context.Background(), orphan, "an abandoned promotion's commit", []string{"edit.txt"}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	// Kept: an orphan with something uncommitted in it.
	dirty := gcWorktree(t, fx, "dirty22222", "hoist/app-staging/dirty22222")
	if err := os.WriteFile(filepath.Join(dirty, "uncommitted.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Kept: an id-shaped worktree that is on somebody's own branch.
	wrongBranch := gcWorktree(t, fx, "wrongbr222", "feature/operators-own")
	// Kept: an id-shaped plain directory git has never heard of.
	plain := filepath.Join(root, "plain22222")
	// Kept: a directory whose name is not a promotion id at all.
	notes := filepath.Join(root, "notes")
	for _, d := range []string{plain, notes} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "keep.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Removed: a registry cache entry nothing has read for a hundred days. Kept: a recent one.
	regDir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "hoist", "registry")
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleBlob := filepath.Join(regDir, "sha256-"+strings.Repeat("a", 64)+".json")
	freshBlob := filepath.Join(regDir, "sha256-"+strings.Repeat("b", 64)+".json")
	for _, p := range []string{staleBlob, freshBlob} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	longAgo := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(staleBlob, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}

	before := diskSnapshot(t, fx)
	dry, err := svc.GC(context.Background(), GCOpts{DryRun: true})
	if err != nil {
		t.Fatalf("GC --dry-run: %v", err)
	}
	if after := diskSnapshot(t, fx); after != before {
		t.Fatalf("a dry run changed something on disk:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, want := range []string{
		"would remove worktree " + landed.WorktreeDir, "would delete local branch " + landed.Branch,
		"would remove worktree " + orphan, "would delete local branch hoist/app-staging/orphan2222",
		"would remove registry cache entry " + staleBlob,
	} {
		if linesContaining(dry.Removed, want) != 1 {
			t.Errorf("dry run does not list %q exactly once:\n%s", want, strings.Join(dry.Removed, "\n"))
		}
	}
	if len(dry.Removed) != 5 {
		t.Errorf("dry run lists %d removals, want 5:\n%s", len(dry.Removed), strings.Join(dry.Removed, "\n"))
	}
	for path, reason := range map[string]string{
		dirty:       "uncommitted changes",
		wrongBranch: `"feature/operators-own"`,
		plain:       "not a registered worktree",
		notes:       "not a promotion id",
	} {
		found := false
		for _, l := range dry.Kept {
			if strings.Contains(l, path) && strings.Contains(l, reason) {
				found = true
			}
		}
		if !found {
			t.Errorf("dry run does not report keeping %s because %s:\n%s", path, reason, strings.Join(dry.Kept, "\n"))
		}
	}

	rep, err := svc.GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	// What the dry run listed is what the real run removed, line for line.
	said := strings.NewReplacer("would remove ", "removed ", "would delete ", "deleted ").Replace(strings.Join(dry.Removed, "\n"))
	if got := strings.Join(rep.Removed, "\n"); got != said {
		t.Fatalf("the real run did not do what the dry run said:\ndry run (reworded):\n%s\nreal run:\n%s", said, got)
	}
	for _, p := range []string{landed.WorktreeDir, orphan, staleBlob} {
		mustBeGone(t, p)
	}
	for _, p := range []string{dirty, filepath.Join(dirty, "uncommitted.txt"), wrongBranch, filepath.Join(plain, "keep.txt"), filepath.Join(notes, "keep.txt"), freshBlob} {
		mustExist(t, p)
	}
	for branch, want := range map[string]bool{
		landed.Branch:                  false,
		"hoist/app-staging/orphan2222": false,
		"hoist/app-staging/dirty22222": true,
		"feature/operators-own":        true,
		"main":                         true,
	} {
		if got := branchExists(t, fx.clone, branch); got != want {
			t.Errorf("local branch %s exists = %v, want %v", branch, got, want)
		}
	}
	// The landed promotion's state file is not GC's to remove: retention archives it.
	if st, err := fileStore.Load(landed.ID); err != nil || st == nil {
		t.Errorf("GC must leave state files alone: Load = %v, %v", st, err)
	}

	again, err := svc.GC(context.Background(), GCOpts{})
	if err != nil || len(again.Removed) != 0 {
		t.Fatalf("a second GC has nothing left to remove: %q, %v", again.Removed, err)
	}
}

// TestGCLeavesAnInFlightPromotionAlone: a promotion with a live state file that has not landed
// is neither cleaned up nor mistaken for an orphan, and is not worth a "kept" line either — it
// is simply in flight.
func TestGCLeavesAnInFlightPromotionAlone(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(s.WorktreeDir, "uncommitted.txt")
	if err := os.WriteFile(scratch, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		rep, err := withConfig(fx).GC(context.Background(), GCOpts{DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Removed) != 0 || len(rep.Kept) != 0 {
			t.Fatalf("dryRun=%v: GC = %+v, want nothing removed and nothing reported", dry, rep)
		}
	}
	mustExist(t, scratch)
	if !branchExists(t, fx.clone, s.Branch) {
		t.Fatal("an in-flight promotion's local branch must survive GC")
	}
}

func TestIsPromotionBranch(t *testing.T) {
	const id = "abcdefgh23"
	for branch, want := range map[string]bool{
		"hoist/app-production/" + id:  true,
		"hoist/" + id:                 false,
		"hoist//" + id:                false,
		"hoist/a/b/" + id:             false,
		"main":                        false,
		"feature/" + id:               false,
		"hoist/app-production/other":  false,
		"xhoist/app-production/" + id: false,
	} {
		if got := isPromotionBranch(branch, id); got != want {
			t.Errorf("isPromotionBranch(%q) = %v, want %v", branch, got, want)
		}
	}
}

// startsDuringList is a StateStore whose List returns the real listing and then, before handing
// it back, has a new promotion appear: its state file saved and its worktree created, in that
// order, exactly as StartPromotion does. It is the promotion that starts while GC is running.
type startsDuringList struct {
	StateStore
	start func()
}

func (s startsDuringList) List() ([]*engine.PromotionState, error) {
	list, err := s.StateStore.List()
	if s.start != nil {
		s.start()
	}
	return list, err
}

// TestGCDoesNotMistakeAPromotionStartingMidSweepForAnOrphan pins the ordering GC's doc comment
// states: the worktrees directory is read before the state files are listed. A promotion saves
// its state file before it creates its worktree, so anything in the directory read already has a
// state file by the time the listing happens. Read the other way round, a promotion that starts
// between the two reads has a worktree and — in GC's stale listing — no state file: an orphan,
// clean, on its own branch, and removed out from under the run that just created it.
func TestGCDoesNotMistakeAPromotionStartingMidSweepForAnOrphan(t *testing.T) {
	fx := newInflightFixture(t)
	base := withConfig(fx)
	const id = "racer22222"
	dir, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	branch := engine.BranchName("app-staging", id)
	deps := base.deps
	deps.Store = startsDuringList{StateStore: FileStore{}, start: func() {
		st := &engine.PromotionState{ID: id, RepoFullName: "example/gitops", TargetEnv: "app-staging", Branch: branch, CloneDir: fx.clone, WorktreeDir: dir, Base: "main"}
		if err := fileStore.Save(st); err != nil {
			t.Error(err)
		}
		if err := (git.Exec{}).Worktree(context.Background(), fx.clone, dir, branch, "main"); err != nil {
			t.Error(err)
		}
	}}
	svc := New(base.Settings(), deps)

	rep, err := svc.GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 {
		t.Fatalf("GC removed something belonging to a promotion that started while it ran: %q", rep.Removed)
	}
	mustExist(t, dir)
	if !branchExists(t, fx.clone, branch) {
		t.Fatal("the starting promotion's branch must survive")
	}
}

// TestGCReportsWhatItCouldNotCheckAsFailedNotKept: "kept" is GC's decision; an observation that
// errored is not a decision. The two must not share a list, because the second is what makes
// `hoist gc` exit non-zero.
func TestGCReportsWhatItCouldNotCheckAsFailedNotKept(t *testing.T) {
	fx := newInflightFixture(t)
	s := landedPRState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	base := withConfig(fx)
	deps := base.deps
	deps.Forge = func(string) (forge.Forge, error) { return nil, errors.New("gh: not logged in") }
	deps.NoCache = true
	svc := New(base.Settings(), deps)

	rep, err := svc.GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0], s.ID) || !strings.Contains(rep.Failed[0], "not logged in") {
		t.Fatalf("Failed = %q, want one line naming %s and the forge error", rep.Failed, s.ID)
	}
	if len(rep.Kept) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("an unobservable promotion is neither kept nor removed: %+v", rep)
	}
	mustExist(t, s.WorktreeDir)
}

// TestGCAsksEveryConfiguredCloneEvenWhenOneCannotBeAsked: a repos[] entry whose checkout has
// moved or been deleted must not stop an orphan being matched to the clone it does belong to.
func TestGCAsksEveryConfiguredCloneEvenWhenOneCannotBeAsked(t *testing.T) {
	fx := newInflightFixture(t)
	base := withConfig(fx)
	set := base.Settings()
	cfg := *set.Config
	// Sorts before any temp-dir path, so it is the first clone asked.
	cfg.Repos = append([]config.RepoConfig{{GitHub: "someone/gone", Dir: "/aaa-hoist-test-does-not-exist"}}, cfg.Repos...)
	set.Config = &cfg
	svc := New(set, base.deps)
	if clones := svc.configuredClones(); len(clones) != 2 || clones[0] != "/aaa-hoist-test-does-not-exist" {
		t.Fatalf("fixture precondition: the missing checkout should be asked first, got %q", clones)
	}

	orphan := gcWorktree(t, fx, "orphan2222", "hoist/app-staging/orphan2222")
	rep, err := svc.GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 0 || linesContaining(rep.Removed, "removed worktree "+orphan) != 1 {
		t.Fatalf("GC = %+v, want the orphan removed and nothing failed", rep)
	}
	mustBeGone(t, orphan)
}

// TestGCReportsAnOrphanNoCloneCouldBeAskedAboutAsFailed: when no configured checkout could be
// asked whether a directory is its worktree, ownership was never decided. That is a failure to
// check, not "kept because nobody owns it".
func TestGCReportsAnOrphanNoCloneCouldBeAskedAboutAsFailed(t *testing.T) {
	fx := newInflightFixture(t)
	base := withConfig(fx)
	set := base.Settings()
	set.RepoDir = ""
	set.Repo = nil
	set.Config = &config.Config{Repos: []config.RepoConfig{{GitHub: "someone/gone", Dir: "/aaa-hoist-test-does-not-exist"}}}
	svc := New(set, base.deps)

	orphan := gcWorktree(t, fx, "orphan2222", "hoist/app-staging/orphan2222")
	rep, err := svc.GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Failed) != 1 || !strings.Contains(rep.Failed[0], orphan) || len(rep.Kept) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("GC = %+v, want the orphan under Failed and nothing else", rep)
	}
	mustExist(t, orphan)
}

// appearsOnLoad is a StateStore whose List does not show a state that Load then finds: the
// promotion re-run under an orphan's id after the sweep listed the state files.
type appearsOnLoad struct {
	StateStore
	id string
}

func (a appearsOnLoad) Load(id string) (*engine.PromotionState, error) {
	if id == a.id {
		return &engine.PromotionState{ID: id}, nil
	}
	return a.StateStore.Load(id)
}

func TestGCLooksAgainForAStateFileBeforeRemovingAnOrphan(t *testing.T) {
	fx := newInflightFixture(t)
	base := withConfig(fx)
	orphan := gcWorktree(t, fx, "orphan2222", "hoist/app-staging/orphan2222")
	deps := base.deps
	deps.Store = appearsOnLoad{StateStore: FileStore{}, id: "orphan2222"}
	rep, err := New(base.Settings(), deps).GC(context.Background(), GCOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 || linesContaining(rep.Kept, "has started since the sweep began") != 1 {
		t.Fatalf("GC = %+v, want the worktree kept because its id is live again", rep)
	}
	mustExist(t, orphan)
}
