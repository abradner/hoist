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
	// A configured repo's NAME maps to its clone dir, as selectRepo maps it: taken as a path,
	// git ran in a dir called "my-gitops" and every orphan check failed (delta read of #353).
	cfg := &config.Config{Repos: []config.RepoConfig{{Name: "my-gitops", Path: "~/src/gitops", Dir: "/home/me/src/gitops"}}}
	for _, repo := range []string{"my-gitops", "~/src/gitops", "/home/me/src/gitops"} {
		if got := gcSettings(cfg, selection{repo: repo}).RepoDir; got != "/home/me/src/gitops" {
			t.Errorf("--repo %s: RepoDir = %q, want the clone dir", repo, got)
		}
	}
}
