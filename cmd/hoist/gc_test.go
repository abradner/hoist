package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/git"
)

// TestPromoteLeavesNoWorktreeOrLocalBranchBehind is the operator's report at the CLI: a
// promotion driven to done by `hoist promote` leaves neither its worktree under the cache
// directory nor its hoist/<env>/<id> branch in the clone. The state file stays — that is
// retention's to archive, not cleanup's to delete.
func TestPromoteLeavesNoWorktreeOrLocalBranchBehind(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	var out, errOut strings.Builder
	if got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	states, err := engine.ListStates()
	if err != nil || len(states) != 1 {
		t.Fatalf("expected exactly one state file, got %d (%v)", len(states), err)
	}
	s := states[0]
	if _, err := os.Lstat(s.WorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("worktree %s should be gone after the promotion landed, stat err = %v", s.WorktreeDir, err)
	}
	if ok, err := (git.Exec{}).LocalBranchExists(context.Background(), clone, s.Branch); err != nil || ok {
		t.Fatalf("local branch %s should be gone after the promotion landed: exists=%v err=%v", s.Branch, ok, err)
	}
}

// TestGCDryRunListsThenRealRunRemoves drives `hoist gc` itself against an orphaned worktree —
// hoist's own shape, no state file — which is what the pre-cleanup versions left behind for
// every abandoned promotion.
func TestGCDryRunListsThenRealRunRemoves(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	const id = "orphan2222"
	dir, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	branch := engine.BranchName("app-staging", id)
	if err := (git.Exec{}).Worktree(context.Background(), clone, dir, branch, "main"); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if got := run([]string{"--config", cfgPath, "gc", "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("gc --dry-run: exit %d; stderr: %s", got, errOut.String())
	}
	for _, want := range []string{"would remove worktree " + dir, "would delete local branch " + branch, "2 to remove — nothing was touched"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("gc --dry-run output does not contain %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("gc --dry-run must not remove anything: %v", err)
	}
	if ok, err := (git.Exec{}).LocalBranchExists(context.Background(), clone, branch); err != nil || !ok {
		t.Fatalf("gc --dry-run must not delete the branch: exists=%v err=%v", ok, err)
	}

	out.Reset()
	if got := run([]string{"--config", cfgPath, "gc"}, &out, &errOut); got != 0 {
		t.Fatalf("gc: exit %d; stderr: %s", got, errOut.String())
	}
	for _, want := range []string{"removed worktree " + dir, "deleted local branch " + branch, "2 removed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("gc output does not contain %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("gc should have removed %s, stat err = %v", dir, err)
	}
	if ok, err := (git.Exec{}).LocalBranchExists(context.Background(), clone, branch); err != nil || ok {
		t.Fatalf("gc should have deleted %s: exists=%v err=%v", branch, ok, err)
	}

	out.Reset()
	if got := run([]string{"--config", cfgPath, "gc"}, &out, &errOut); got != 0 || !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("a second gc: exit %d, output %q", got, out.String())
	}
}

func TestGCTakesNoArguments(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	var out, errOut strings.Builder
	if got := run([]string{"--config", cfgPath, "gc", "everything"}, &out, &errOut); got != exitUsage {
		t.Fatalf("exit %d, want %d (usage); stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), `unexpected argument "everything"`) || !strings.Contains(errOut.String(), "takes flags only") {
		t.Errorf("stderr %q does not refuse the stray argument", errOut.String())
	}
}

// TestGCSummaryDoesNotClaimADecisionItDidNotMake: with everything it looked at unreachable, the
// sweep removed nothing because it could not decide, which is not "nothing to remove".
func TestGCSummaryDoesNotClaimADecisionItDidNotMake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rep    service.GCReport
		dryRun bool
		want   string
	}{
		{"nothing there", service.GCReport{}, false, "nothing to remove"},
		{"only kept", service.GCReport{Kept: []string{"k"}}, false, "nothing to remove"},
		{"only failed", service.GCReport{Failed: []string{"f", "g"}}, false, "sweep incomplete — 2 could not be checked; nothing was removed"},
		{"removed", service.GCReport{Removed: []string{"r"}}, false, "1 removed"},
		{"removed and failed", service.GCReport{Removed: []string{"r"}, Failed: []string{"f"}}, false, "1 removed; sweep incomplete — 1 could not be checked"},
		{"dry run", service.GCReport{Removed: []string{"r", "s"}}, true, "2 to remove — nothing was touched; run `hoist gc` to remove them"},
		{"dry run and failed", service.GCReport{Removed: []string{"r"}, Failed: []string{"f"}}, true, "1 to remove — nothing was touched; run `hoist gc` to remove them; sweep incomplete — 1 could not be checked"},
	} {
		if got := gcSummary(tc.rep, tc.dryRun); got != tc.want {
			t.Errorf("%s: gcSummary = %q, want %q", tc.name, got, tc.want)
		}
	}
}
