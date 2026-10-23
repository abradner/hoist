package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSaveStateLeavesAnIdenticalFileAlone: a drive saves after every step on every poll, mostly
// with nothing changed, and every write is a full flush to disk. An identical file is not
// rewritten; a changed one is.
func TestSaveStateLeavesAnIdenticalFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "promotions", "abc.json")
	s := &PromotionState{ID: "abc", TargetEnv: "app-staging"}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("an unchanged state was rewritten (mtime %v, err %v)", info.ModTime(), err)
	}

	// Control: a change is written.
	s.Phase = StepBranched
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.ModTime().Equal(old) {
		t.Fatalf("a changed state was not written (err %v)", err)
	}
	got, err := LoadState(path)
	if err != nil || got.Phase != StepBranched {
		t.Fatalf("reloaded phase %q, err %v", got.Phase, err)
	}
}
