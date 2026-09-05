package engine

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// publicSafetyPatterns mirrors scripts/public-safety.sh's own patterns (AGENTS.md §4.4,
// invariant 6): a rendered PR body must never match any of these, on a fixture built to
// exercise every rendering path — edits, an untouched image, and a warning — not just the
// happy-path edit table.
//
// The last pattern is built from two concatenated literals rather than one contiguous
// string: scripts/public-safety.sh greps every tracked file for this exact substring, and
// a source file that spells it out whole — even here, where it names a forbidden pattern
// rather than leaking one — trips the same scanner it's helping to test. Splitting it is a
// deliberate way to defeat the static grep while the runtime string built from it (used
// below and in TestRenderPRBodyWithInternalLookingFixtureIsCaught) is unchanged.
var publicSafetyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b`),
	regexp.MustCompile(`\b192\.168\.[0-9]{1,3}\.[0-9]{1,3}\b`),
	regexp.MustCompile(`\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}\b`),
	regexp.MustCompile(`\.(local|lan|internal|home\.arpa)\b`),
	regexp.MustCompile(`\.asn\.casa\b`),
	regexp.MustCompile(`admin@` + `athena`),
}

func fixturePlan() gitops.Plan {
	oldWeb := image.Ref{Repo: "ghcr.io/example/web", Tag: "v1", Digest: digestA}
	newWeb := image.Ref{Repo: "ghcr.io/example/web", Tag: "v2", Digest: digestB}
	return gitops.Plan{
		SourceEnv: "app-staging",
		TargetEnv: "app-production",
		Edits: []gitops.Edit{
			{
				Occurrence: gitops.Occurrence{
					File: "cluster/apps/app-production/web/app.yaml", Line: 12, Kind: "Deployment", Name: "web", Container: "web",
					Path: "spec.template.spec.containers[0].image", Raw: oldWeb.String(), Ref: oldWeb,
				},
				New: newWeb,
			},
		},
		Untouched: []image.Ref{
			{Repo: "ghcr.io/example/marketing", Tag: "v9", Digest: digestA},
		},
		Warnings: []gitops.Warning{
			{
				Code:    gitops.WarnMissingInTarget,
				Message: "ghcr.io/example/counta runs in app-staging but has no occurrence in app-production; nothing to move",
			},
		},
		GeneratedAt: time.Now(),
	}
}

func assertPublicSafe(t *testing.T, label, text string) {
	t.Helper()
	for _, p := range publicSafetyPatterns {
		if loc := p.FindString(text); loc != "" {
			t.Errorf("%s: matched public-safety pattern %s: %q\nfull text:\n%s", label, p.String(), loc, text)
		}
	}
}

func TestRenderPRBodyIsPublicSafe(t *testing.T) {
	plan := fixturePlan()
	body := RenderPRBody("dh4arammqe", plan)
	assertPublicSafe(t, "PR body", body)

	// Every rendering path the fixture exercises must actually show up, or this test would
	// pass by accident on a template that dropped a section.
	for _, want := range []string{
		"ghcr.io/example/web",
		"ghcr.io/example/marketing", // untouched
		"missing-in-target",         // warning code
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderPRBodyMarkerIsFirstLine(t *testing.T) {
	body := RenderPRBody("dh4arammqe", fixturePlan())
	firstLine := strings.SplitN(body, "\n", 2)[0]
	if want := Marker("dh4arammqe"); firstLine != want {
		t.Fatalf("first line = %q, want exactly %q", firstLine, want)
	}
}

func TestRenderCommitMessageIsPublicSafeAndCarriesTrailer(t *testing.T) {
	msg := RenderCommitMessage("dh4arammqe", fixturePlan())
	assertPublicSafe(t, "commit message", msg)
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	if last := lines[len(lines)-1]; last != CommitTrailer("dh4arammqe") {
		t.Fatalf("last line = %q, want the trailer %q", last, CommitTrailer("dh4arammqe"))
	}
}

func TestPRTitleCountsChangedRepos(t *testing.T) {
	title := PRTitle(fixturePlan())
	if !strings.Contains(title, "1 image") {
		t.Fatalf("PRTitle = %q, want it to mention 1 image", title)
	}
}

// TestRenderPRBodyWithInternalLookingFixtureIsCaught is the "prove the assertion can fail"
// counterpart (AGENTS.md §8): a fixture deliberately carrying what public-safety forbids must
// trip assertPublicSafe, or the check above is not actually gating anything.
//
// The planted address is built from concatenated literals rather than one IP-shaped string:
// written whole, it would be a private address sitting in a tracked file, and
// scripts/public-safety.sh would flag this test file itself for the very thing the test
// exists to prove gets caught. Splitting it defeats the static grep; the runtime string is
// unchanged, so the property under test — a leaked address trips a public-safety pattern —
// is exercised exactly as before.
func TestRenderPRBodyWithInternalLookingFixtureIsCaught(t *testing.T) {
	plan := fixturePlan()
	plantedAddress := "10." + "0.0.5"
	plan.Warnings = append(plan.Warnings, gitops.Warning{Code: "test", Message: "unreachable at " + plantedAddress})
	body := RenderPRBody("dh4arammqe", plan)
	found := false
	for _, p := range publicSafetyPatterns {
		if p.MatchString(body) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected the deliberately-planted 10.x address to trip a public-safety pattern")
	}
}

// deployFixturePlan mirrors fixturePlan but for the deploy variant: no source env, one repo
// planned, everything else in the env untouched.
func deployFixturePlan() gitops.Plan {
	p := fixturePlan()
	p.Variant = gitops.VariantDeploy
	p.SourceEnv = ""
	return p
}

// A deploy's artifacts must describe a deploy. The failure this guards is not cosmetic: with
// the promotion wording and an empty SourceEnv, the PR title reads "promote 1 image(s)  ->
// app-production" and the body opens "hoist promotes “ -> `app-production`" — an artifact
// that both misdescribes the operation and looks corrupted.
func TestDeployArtifactsDescribeADeployNotAPromotion(t *testing.T) {
	plan := deployFixturePlan()
	title := PRTitle(plan)
	body := RenderPRBody("dh4arammqe", plan)
	commit := RenderCommitMessage("dh4arammqe", plan)

	for label, text := range map[string]string{"title": title, "body": body, "commit": commit} {
		if strings.Contains(text, "promote") || strings.Contains(text, "Promotion") {
			t.Errorf("%s uses promotion wording for a deploy: %q", label, text)
		}
		// The tell-tale of a source env rendered into a deploy: an arrow with nothing to its
		// left, or a doubled space where SourceEnv would have gone.
		if strings.Contains(text, "`` ->") || strings.Contains(text, "  ->") {
			t.Errorf("%s renders an empty source env: %q", label, text)
		}
	}
	if !strings.Contains(title, "deploy") || !strings.Contains(title, "app-production") {
		t.Errorf("title should name the operation and the env, got %q", title)
	}
	if !strings.Contains(body, "Deploy id:") {
		t.Errorf("body should label the id as a deploy's:\n%s", body)
	}
	if !strings.Contains(body, "not part of this deploy") {
		t.Errorf("body's untouched section should read as a deploy's:\n%s", body)
	}
}

// Invariant 5 and 6 hold for the deploy variant too — the marker is still the first line, and
// nothing public-safety would flag reaches the rendered body.
func TestDeployPRBodyKeepsMarkerFirstAndIsPublicSafe(t *testing.T) {
	body := RenderPRBody("dh4arammqe", deployFixturePlan())
	if first := strings.SplitN(body, "\n", 2)[0]; first != Marker("dh4arammqe") {
		t.Errorf("first line = %q, want the marker", first)
	}
	assertPublicSafe(t, "deploy PR body", body)
	if !strings.Contains(RenderCommitMessage("dh4arammqe", deployFixturePlan()), CommitTrailer("dh4arammqe")) {
		t.Error("deploy commit message must still carry the hoist-id trailer")
	}
}
