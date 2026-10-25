package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
)

// orphanWorktrees creates n orphaned worktrees (hoist's own shape, clean, no state file) — what
// `hoist gc` removes.
func orphanWorktrees(t *testing.T, clone string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("orphan%c%caa", 'a'+i/26, 'a'+i%26)
		dir, err := engine.WorktreeDir(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := (git.Exec{}).Worktree(context.Background(), clone, dir, engine.BranchName("app-staging", id), "main"); err != nil {
			t.Fatal(err)
		}
	}
}

// staleRegistryEntries writes n registry cache entries last used a hundred days ago, past the
// 90-day prune age.
func staleRegistryEntries(t *testing.T, n int) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "hoist", "registry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-100 * 24 * time.Hour)
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("sha256-%064x.json", i))
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, longAgo, longAgo); err != nil {
			t.Fatal(err)
		}
	}
}

// `hoist promotions` points at `hoist gc` once more than gcHintThreshold orphaned worktrees, or
// more than that many registry cache entries past the prune age, have piled up (#298). At the
// threshold exactly, and with nothing collectable, it prints nothing about it. The hint goes to
// stderr (stdout stays the listing), and the command removes nothing itself.
func TestPromotionsHintsAtGCOnlyPastTheThreshold(t *testing.T) {
	const hint = "hoist gc"
	for _, tc := range []struct {
		name      string
		worktrees int
		registry  int
		want      bool
	}{
		{"nothing to collect", 0, 0, false},
		{"worktrees at the threshold", gcHintThreshold, 0, false},
		{"registry entries at the threshold", 0, gcHintThreshold, false},
		{"worktrees past the threshold", gcHintThreshold + 1, 0, true},
		{"registry entries past the threshold", 0, gcHintThreshold + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, clone, _ := newPromoteFixture(t)
			orphanWorktrees(t, clone, tc.worktrees)
			staleRegistryEntries(t, tc.registry)

			var out, errOut strings.Builder
			if got := run([]string{"--config", cfgPath, "promotions"}, &out, &errOut); got != 0 {
				t.Fatalf("exit %d; stderr: %s", got, errOut.String())
			}
			if got := strings.Contains(errOut.String(), hint); got != tc.want {
				t.Errorf("hint on stderr = %v, want %v:\n%s", got, tc.want, errOut.String())
			}
			if strings.Contains(out.String(), hint) {
				t.Errorf("the hint must not be on stdout, which is the listing:\n%s", out.String())
			}
			// promotions removes nothing, whatever it hints.
			for i := 0; i < tc.worktrees; i++ {
				dir, _ := engine.WorktreeDir(fmt.Sprintf("orphan%c%caa", 'a'+i/26, 'a'+i%26))
				if _, err := os.Lstat(dir); err != nil {
					t.Fatalf("promotions removed %s: %v", dir, err)
				}
			}
		})
	}
}
