package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleanupState is a state file's worth of fields for a promotion whose worktree would live under
// the test's own XDG_CACHE_HOME — every field CleanupTarget reads, agreeing with each other, so
// each case below changes exactly one thing.
func cleanupState(t *testing.T, id string) *PromotionState {
	t.Helper()
	dir, err := WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	return &PromotionState{ID: id, TargetEnv: "app-production", Branch: BranchName("app-production", id), WorktreeDir: dir, Base: "main"}
}

func wantRefused(t *testing.T, err error, contains string) {
	t.Helper()
	var refused *CleanupRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v (%T), want a *CleanupRefusedError", err, err)
	}
	if !strings.Contains(refused.Reason, contains) {
		t.Fatalf("refusal %q does not mention %q", refused.Reason, contains)
	}
}

func TestCleanupTargetIsDerivedFromTheID(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	const id = "abcdefgh23"
	dir, branch, err := CleanupTarget(cleanupState(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, "hoist", "worktrees", id); dir != want {
		t.Fatalf("dir = %s, want %s", dir, want)
	}
	if want := "hoist/app-production/" + id; branch != want {
		t.Fatalf("branch = %s, want %s", branch, want)
	}
}

// TestPromotionWorktreeRefusesAnIDThatIsNotOne is the containment test for the only place a
// string from outside is joined onto the cache directory. The attacker is a state file (or a
// directory entry) whose id is a path: "../../victim" joins to <cache>/victim, outside
// <cache>/hoist/worktrees entirely, and an absolute or dotted id does the same by other routes.
func TestPromotionWorktreeRefusesAnIDThatIsNotOne(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, id := range []string{
		"", ".", "..", "../../victim", "/etc", "abc/defgh2", `abc\defgh2`, "ABCDEFGH23",
		"abcdefgh2", "abcdefgh234", "abcdefgh1!", "abcdefgh2\n", "archive",
	} {
		if dir, err := PromotionWorktree(id); err == nil {
			t.Errorf("PromotionWorktree(%q) = %s, want it refused", id, dir)
		} else {
			wantRefusedf(t, id, err)
		}
	}
}

func wantRefusedf(t *testing.T, id string, err error) {
	t.Helper()
	var refused *CleanupRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("PromotionWorktree(%q) err = %v (%T), want a *CleanupRefusedError", id, err, err)
	}
}

// TestPromotionWorktreeRefusesARelativeCacheDirectory: a relative $XDG_CACHE_HOME makes the
// worktree path mean a different directory from every working directory — including, run from
// inside the operator's clone, one inside that clone, where git.RemoveWorktree's own guard cannot
// relate a relative path to an absolute one and so cannot refuse it.
func TestPromotionWorktreeRefusesARelativeCacheDirectory(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "relative/cache")
	_, err := PromotionWorktree("abcdefgh23")
	wantRefused(t, err, "not an absolute path")
}

func TestPromotionWorktreeRefusesAnythingButARealDirectory(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	root := filepath.Join(cache, "hoist", "worktrees")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()

	if err := os.Symlink(elsewhere, filepath.Join(root, "linkaaaaaa")); err != nil {
		t.Fatal(err)
	}
	_, err := PromotionWorktree("linkaaaaaa")
	wantRefused(t, err, "symbolic link")

	if err := os.WriteFile(filepath.Join(root, "fileaaaaaa"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = PromotionWorktree("fileaaaaaa")
	wantRefused(t, err, "not a directory")

	// Controls: a real directory, and nothing there at all, are both fine.
	if err := os.Mkdir(filepath.Join(root, "realaaaaaa"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"realaaaaaa", "goneaaaaaa"} {
		if _, err := PromotionWorktree(id); err != nil {
			t.Errorf("PromotionWorktree(%q) = %v, want it accepted", id, err)
		}
	}
}

// TestCleanupTargetRefusesAStateFileThatDisagreesWithItsOwnID: the recorded worktree and branch
// are never the delete target, so a state file cannot aim a removal by lying in them — but one
// that does lie is not a file to act on at all.
func TestCleanupTargetRefusesAStateFileThatDisagreesWithItsOwnID(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const id = "abcdefgh23"
	for _, tc := range []struct {
		name     string
		mutate   func(*PromotionState)
		contains string
	}{
		{"worktree recorded elsewhere", func(s *PromotionState) { s.WorktreeDir = "/home/someone/Documents" }, "records the worktree"},
		{"worktree recorded as a sibling id", func(s *PromotionState) { s.WorktreeDir = filepath.Join(filepath.Dir(s.WorktreeDir), "zzzzzzzz22") }, "records the worktree"},
		{"branch recorded as main", func(s *PromotionState) { s.Branch = "main" }, "records the branch"},
		{"branch recorded for another env", func(s *PromotionState) { s.Branch = BranchName("app-staging", id) }, "records the branch"},
		{"env is a path", func(s *PromotionState) { s.TargetEnv = "../x"; s.Branch = BranchName("../x", id) }, "not an env name"},
		{"env has a separator", func(s *PromotionState) { s.TargetEnv = "a/b"; s.Branch = BranchName("a/b", id) }, "not an env name"},
		{"base is the promotion's own branch", func(s *PromotionState) { s.Base = s.Branch }, "not a base a promotion lands on"},
		{"base is empty", func(s *PromotionState) { s.Base = "" }, "not a base a promotion lands on"},
		{"env is empty", func(s *PromotionState) { s.TargetEnv = ""; s.Branch = BranchName("", id) }, "not an env name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cleanupState(t, id)
			tc.mutate(s)
			_, _, err := CleanupTarget(s)
			wantRefused(t, err, tc.contains)
		})
	}
}

func TestLanded(t *testing.T) {
	sat := Observation{Satisfied: true}
	for _, tc := range []struct {
		name string
		in   []StepStatus
		want bool
	}{
		{"merged", []StepStatus{{Step: StepMerged, Observation: sat}}, true},
		{"direct pushed", []StepStatus{{Step: StepBranched, Observation: sat}, {Step: StepDirectPushed, Observation: sat}}, true},
		{"merged but waiting on the branch delete", []StepStatus{{Step: StepMerged, Observation: Observation{Detail: "merged; branch not yet deleted"}}}, false},
		{"pushed to its own branch is not landed", []StepStatus{{Step: StepPushed, Observation: sat}, {Step: StepPROpened, Observation: sat}}, false},
		{"blocked at the merge", []StepStatus{{Step: StepMerged, Observation: Observation{Satisfied: true, Blocked: "x"}}}, false},
		{"nothing observed", nil, false},
	} {
		if got := Landed(tc.in); got != tc.want {
			t.Errorf("%s: Landed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
