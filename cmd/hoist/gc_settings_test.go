package main

import (
	"testing"

	"github.com/abradner/hoist/internal/config"
)

// TestGCKeepsTheRootRepoClonesView (Codex on #347): on flags alone, `hoist --repo <clone> gc`
// must keep that clone's repo view, as the guide says. gc's keep set is the config's clones plus
// Settings.RepoDir, so the root --repo has to reach it.
func TestGCKeepsTheRootRepoClonesView(t *testing.T) {
	if got := gcSettings(&config.Config{}, selection{repo: "/clones/app"}).RepoDir; got != "/clones/app" {
		t.Fatalf("gcSettings RepoDir = %q, want the root --repo", got)
	}
	if got := gcSettings(&config.Config{}, selection{}).RepoDir; got != "" {
		t.Fatalf("no --repo: RepoDir = %q, want none", got)
	}
}
