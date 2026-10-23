package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveAndClaimFlushBeforePublishing is the regression gate for fsync-before-rename: a power
// loss cannot be simulated, but the order can. Each write must flush its temp file while the
// final name does not yet carry the new content, and a failed flush must leave the final name
// as it was (no new state, no claim).
func TestSaveAndClaimFlushBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "abc.json")
	if err := SaveState(statePath, &PromotionState{ID: "abc", Phase: StepBranched}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(statePath)

	orig := syncFile
	t.Cleanup(func() { syncFile = orig })
	calls := 0
	syncFile = func(f *os.File) error {
		calls++
		if now, _ := os.ReadFile(statePath); string(now) != string(before) {
			t.Errorf("the state file changed before its temp file was flushed")
		}
		return orig(f)
	}
	if err := SaveState(statePath, &PromotionState{ID: "abc", Phase: StepCommitted}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("SaveState flushed %d times, want 1", calls)
	}

	// A failed flush publishes nothing.
	syncFile = func(*os.File) error { return errors.New("disk full") }
	written, _ := os.ReadFile(statePath)
	if err := SaveState(statePath, &PromotionState{ID: "abc", Phase: StepPushed}); err == nil {
		t.Fatal("SaveState must fail when its flush fails")
	}
	if now, _ := os.ReadFile(statePath); string(now) != string(written) {
		t.Fatal("a failed flush replaced the state file")
	}
	claimPath := filepath.Join(dir, "claim")
	if err := tryClaim(claimPath, []byte("claim")); err == nil {
		t.Fatal("tryClaim must fail when its flush fails")
	}
	if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
		t.Fatalf("a failed flush left a claim behind (stat err %v)", err)
	}
}
