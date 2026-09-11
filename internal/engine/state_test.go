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

// TestArchiveStateMovesOutOfListStates: ListStates' own entries.IsDir() check already skips
// ArchiveDir as a subdirectory of the live promotions dir — this proves that holds for a real
// archived file, not just by reading the code: an archived promotion is invisible to
// ListStates() (and therefore to findInFlight, or anything else walking it) purely because it
// physically isn't under promotions/ anymore, never a second, separately-maintained exclusion.
func TestArchiveStateMovesOutOfListStates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := StatePath("abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, &PromotionState{ID: "abc", TargetEnv: "app-production"}); err != nil {
		t.Fatal(err)
	}
	if states, err := ListStates(); err != nil || len(states) != 1 {
		t.Fatalf("fixture precondition: ListStates() = %v, %v, want exactly one", states, err)
	}

	if err := ArchiveState("abc"); err != nil {
		t.Fatal(err)
	}

	states, err := ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("ListStates() after archiving = %v, want empty", states)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("live state file should be gone after archiving: stat err = %v", err)
	}

	archived, err := ListArchivedStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != "abc" {
		t.Fatalf("ListArchivedStates() = %v, want exactly [abc]", archived)
	}
}

// TestArchiveStateIsIdempotent mirrors DeleteState/DeleteRemoteBranch's own convention: an
// already-archived (or never-existed) id is success, not an error — a retry (or two concurrent
// `hoist promotions` runs) must not fail on it.
func TestArchiveStateIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := StatePath("abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(path, &PromotionState{ID: "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveState("abc"); err != nil {
		t.Fatalf("first ArchiveState: %v", err)
	}
	if err := ArchiveState("abc"); err != nil {
		t.Fatalf("second ArchiveState (already archived) should be a no-op success: %v", err)
	}
	if err := ArchiveState("never-existed"); err != nil {
		t.Fatalf("ArchiveState on an id with no state file should be a no-op success: %v", err)
	}
}

// TestLastActivityPrefersTheLatestHistoryEntryOverGeneratedAt: History's own most recent entry
// is a closer proxy for "when did anything last actually happen" than construction time, which
// never advances again once a promotion starts converging.
func TestLastActivityPrefersTheLatestHistoryEntryOverGeneratedAt(t *testing.T) {
	generated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s := &PromotionState{
		GeneratedAt: generated,
		History: []HistoryEntry{
			{Step: StepBranched, At: generated},
			{Step: StepRolledOut, At: latest},
			{Step: StepMerged, At: generated.AddDate(0, 1, 0)},
		},
	}
	if got := s.LastActivity(); !got.Equal(latest) {
		t.Errorf("LastActivity() = %v, want the latest History entry %v", got, latest)
	}

	empty := &PromotionState{GeneratedAt: generated}
	if got := empty.LastActivity(); !got.Equal(generated) {
		t.Errorf("LastActivity() with no History = %v, want GeneratedAt %v", got, generated)
	}
}
