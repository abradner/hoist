package gitops

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// A deploy rewrites every occurrence of the named repo in the named env, and nothing else.
// app-production carries three web containers (web, worker, queue) in one file, so this also
// pins that a deploy is repo-scoped rather than container- or family-scoped.
func TestBuildDeployPlanRewritesEveryOccurrenceOfTheRepo(t *testing.T) {
	p, err := BuildDeployPlan(discoverFixture(t), "app-production", mustRef(t, webNew), promotable)
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ File, Container, New string }
	var got []key
	for _, e := range p.Edits {
		got = append(got, key{e.File, e.Container, e.New.String()})
	}
	want := []key{
		{"cluster/apps/app-production/web/app.yaml", "web", webNew},
		{"cluster/apps/app-production/web/app.yaml", "worker", webNew},
		{"cluster/apps/app-production/web/app.yaml", "queue", webNew},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("edits (path order): %s", diff)
	}
	if !p.IsDeploy() || p.Variant != VariantDeploy {
		t.Errorf("Variant = %q, want the deploy variant", p.Variant)
	}
	if p.SourceEnv != "" {
		t.Errorf("SourceEnv = %q, want empty: a deploy has no source env", p.SourceEnv)
	}
	if p.TargetEnv != "app-production" || p.GeneratedAt.IsZero() {
		t.Errorf("plan header = -> %q at %v", p.TargetEnv, p.GeneratedAt)
	}
}

// Untouched is every other distinct ref in the env — exactly one repo is ever planned, so a
// deploy's Untouched is the larger list, and it must include first-party repos the deploy
// simply isn't about as well as third-party ones.
func TestBuildDeployPlanUntouchedListsEveryOtherRefInTheEnv(t *testing.T) {
	p, err := BuildDeployPlan(discoverFixture(t), "app-production", mustRef(t, webNew), promotable)
	if err != nil {
		t.Fatal(err)
	}
	var firstParty, thirdParty int
	for _, u := range p.Untouched {
		if u.Repo == "ghcr.io/example/web" {
			t.Errorf("Untouched names the deployed repo: %s", u)
		}
		if strings.HasPrefix(u.Repo, "ghcr.io/example/") {
			firstParty++
		} else {
			thirdParty++
		}
	}
	if firstParty == 0 {
		t.Error("want first-party repos this deploy isn't about listed as untouched")
	}
	if thirdParty == 0 {
		t.Error("want third-party repos listed as untouched")
	}
}

// Invariant 1: nothing hoist writes is a bare tag. A promotion can downgrade this to a warning
// when the target has nothing to write; a deploy cannot — the ref is the whole request.
func TestBuildDeployPlanRefusesAnUnwritableRef(t *testing.T) {
	for name, ref := range map[string]string{
		"bare tag":       "ghcr.io/example/web:v1",
		"digest, no tag": "ghcr.io/example/web@" + digestA,
	} {
		_, err := BuildDeployPlan(discoverFixture(t), "app-production", mustRef(t, ref), promotable)
		if err == nil {
			t.Errorf("%s: accepted %s, want a refusal", name, ref)
		}
	}
}

// No occurrence in the env means there is nothing to deploy into. A promotion warns
// (WarnMissingInTarget) because its other repos still proceed; a deploy has no other repos, so
// reporting success would claim a write that never happened.
func TestBuildDeployPlanRefusesARepoAbsentFromTheEnv(t *testing.T) {
	// dbwait appears in app-production only, so deploying it into app-staging has nowhere to
	// land — a real asymmetry in the fixture rather than an invented repo name.
	_, err := BuildDeployPlan(discoverFixture(t), "app-staging",
		mustRef(t, "ghcr.io/example/dbwait:v1@"+digestC), promotable)
	if err == nil {
		t.Fatal("accepted a repo with no occurrence in the env")
	}
	if !strings.Contains(err.Error(), "nothing to deploy into") {
		t.Errorf("error should say there is nothing to deploy into, got: %v", err)
	}
}

func TestBuildDeployPlanRefusesANonPromotableRepo(t *testing.T) {
	_, err := BuildDeployPlan(discoverFixture(t), "app-production",
		mustRef(t, "docker.io/library/redis:7@"+digestB), promotable)
	if err == nil {
		t.Fatal("accepted a repo outside the promotable prefixes")
	}
	if !strings.Contains(err.Error(), "not a promotable repo") {
		t.Errorf("error should name the promotable rule, got: %v", err)
	}
}

func TestBuildDeployPlanRefusesAnUnknownEnv(t *testing.T) {
	_, err := BuildDeployPlan(discoverFixture(t), "no-such-env", mustRef(t, webNew), promotable)
	if err == nil {
		t.Fatal("accepted an env that isn't in the repo")
	}
}
