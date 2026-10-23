package writepath

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
)

const fixtureRoot = "../../../testdata/repo"

func TestRenderDiff(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]bool{}
	for _, e := range pl.Edits {
		all[e.Ref.Repo] = true
	}
	diff, err := RenderDiff(r.Root, pl.Edits, all)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "ghcr.io/example/web") {
		t.Errorf("diff missing an edited repo:\n%s", diff)
	}

	// Ticking nothing yields no diff at all: filtering by repo, not just an empty ticked set
	// producing every edit by accident.
	empty, err := RenderDiff(r.Root, pl.Edits, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Errorf("empty ticked set produced a diff:\n%s", empty)
	}

	// Ticking one repo shows only that repo's hunks.
	oneRepo := map[string]bool{}
	for repo := range all {
		oneRepo[repo] = true
		break
	}
	one, err := RenderDiff(r.Root, pl.Edits, oneRepo)
	if err != nil {
		t.Fatal(err)
	}
	// A repo not ticked must not contribute a hunk: ticking just one of several repos
	// produces a strictly smaller diff than ticking every repo, rather than the filter being
	// a no-op that always returns everything.
	if len(one) >= len(diff) {
		t.Errorf("ticking one repo produced as much diff as ticking every repo")
	}
}
