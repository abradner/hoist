package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promotions", "abc.json")
	s := &PromotionState{
		ID: "abc", RepoFullName: "example/gitops", SourceEnv: "app-staging", TargetEnv: "app-production",
		Branch: "hoist/app-production/abc", GeneratedAt: time.Now().Truncate(time.Second),
		History: []HistoryEntry{{Step: StepBranched, At: time.Now().Truncate(time.Second), Detail: "acted"}},
	}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID || got.Branch != s.Branch || len(got.History) != 1 {
		t.Fatalf("LoadState = %+v, want %+v", got, s)
	}
}

func TestLoadStateMissingFileIsNilNil(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestSaveStateIsAtomicNoPartialFileVisible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abc.json")
	s := &PromotionState{ID: "abc"}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	// A concurrent writer's temp file must never collide with or be mistaken for the real
	// state file: only one file matching the promotion's name should exist afterward, and no
	// leftover .tmp file from this save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "abc.json" {
		t.Fatalf("directory contents = %v, want exactly [abc.json]", names)
	}
}

func TestSaveStatePermissionsAreExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abc.json")
	if err := SaveState(path, &PromotionState{ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600", perm)
	}
}

func TestSaveStateOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "abc.json")
	if err := SaveState(path, &PromotionState{ID: "abc", Phase: StepBranched}); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, &PromotionState{ID: "abc", Phase: StepPushed}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != StepPushed {
		t.Fatalf("Phase = %q, want %q", got.Phase, StepPushed)
	}
}

func TestStateDirAndCacheDirRespectXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg-state")
	t.Setenv("XDG_CACHE_HOME", "/xdg-cache")
	if got, err := StateDir(); err != nil || got != "/xdg-state/hoist" {
		t.Fatalf("StateDir() = %q, %v", got, err)
	}
	if got, err := CacheDir(); err != nil || got != "/xdg-cache/hoist" {
		t.Fatalf("CacheDir() = %q, %v", got, err)
	}
	if got, err := WorktreeDir("abc123"); err != nil || got != "/xdg-cache/hoist/worktrees/abc123" {
		t.Fatalf("WorktreeDir() = %q, %v", got, err)
	}
	if got, err := StatePath("abc123"); err != nil || got != "/xdg-state/hoist/promotions/abc123.json" {
		t.Fatalf("StatePath() = %q, %v", got, err)
	}
}

func TestStateDirNeverFallsBackToLibrary(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Library") {
		t.Fatalf("StateDir() = %q, must never use ~/Library (AGENTS.md rule for XDG paths)", got)
	}
	if !strings.Contains(got, filepath.Join(".local", "state", "hoist")) {
		t.Fatalf("StateDir() = %q, want it to end in .local/state/hoist", got)
	}
}
