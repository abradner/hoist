package gitops

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/pkg/image"
)

func TestBuildPlanOneEditPerTargetOccurrence(t *testing.T) {
	p := planFixture(t)
	type key struct{ File, Container, New string }
	var got []key
	for _, e := range p.Edits {
		if !e.New.Pinned() {
			t.Errorf("edit writes an unpinned ref: %+v", e)
		}
		got = append(got, key{e.File, e.Container, e.New.String()})
	}
	want := []key{
		{"cluster/apps/app-production/counta/app.yaml", "counta", countaNew},
		{"cluster/apps/app-production/counta/purge-cronjob.yaml", "purge", countaNew},
		{"cluster/apps/app-production/marketing/app.yaml", "marketing", mktNew},
		{"cluster/apps/app-production/web/app.yaml", "web", webNew},
		{"cluster/apps/app-production/web/app.yaml", "worker", webNew},
		{"cluster/apps/app-production/web/app.yaml", "queue", webNew},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("edits (path order; production has three web containers to staging's two): %s", diff)
	}
	if p.SourceEnv != "app-staging" || p.TargetEnv != "app-production" || p.GeneratedAt.IsZero() {
		t.Errorf("plan header = %q -> %q at %v", p.SourceEnv, p.TargetEnv, p.GeneratedAt)
	}
}

func TestBuildPlanUntouchedListsTargetOnlyAndThirdParty(t *testing.T) {
	p := planFixture(t)
	var got []string
	for _, r := range p.Untouched {
		got = append(got, r.String())
	}
	want := []string{
		"docker.io/temporalio/server:1.31.2",
		"docker.io/temporalio/ui:2.34.0",
		"ghcr.io/example/dbwait:v202601010101@sha256:dbfa1e01dbfa1e01dbfa1e01dbfa1e01dbfa1e01dbfa1e01dbfa1e01dbfa1e01",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Untouched: %s", diff)
	}
	for _, e := range p.Edits {
		if strings.HasPrefix(e.Ref.Repo, "docker.io/") {
			t.Errorf("third-party image planned: %+v", e)
		}
	}
}

func TestBuildPlanDisagreeingSourceWarns(t *testing.T) {
	p := planFixture(t)
	if len(p.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one", p.Warnings)
	}
	w := p.Warnings[0]
	if w.Code != WarnSourceDisagrees || len(w.Occurrences) != 2 {
		t.Fatalf("warning = %+v", w)
	}
	for _, o := range w.Occurrences {
		if o.File != "cluster/apps/app-staging/web/app.yaml" || !strings.Contains(w.Message, o.Ref.String()) {
			t.Errorf("warning does not name occurrence %s %s: %s", o.Container, o.Ref, w.Message)
		}
	}
	if !strings.Contains(w.Message, webNew) || !strings.Contains(w.Message, "only pinned") {
		t.Errorf("warning does not state the choice and its rule: %s", w.Message)
	}
}

func TestBuildPlanDigestOverrideWins(t *testing.T) {
	r := discoverFixture(t)
	override := image.Ref{Tag: "v9", Digest: digestC}
	p, err := BuildPlan(r, "app-staging", "app-production", promotable, map[string]image.Ref{"ghcr.io/example/web": override})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range editsFor(p, "cluster/apps/app-production/web/app.yaml") {
		if e.New.String() != "ghcr.io/example/web:v9@"+digestC {
			t.Errorf("override ignored: %s", e.New)
		}
	}
	if _, err := BuildPlan(r, "app-staging", "app-production", promotable, map[string]image.Ref{"ghcr.io/example/web": {Tag: "v9"}}); err == nil {
		t.Error("unpinned override accepted")
	}
}

// An override is the one ref that never went through image.Parse, so a malformed digest
// ("sha256:DEADBEEF" passes Pinned()) must be refused when the plan is built, with an error
// that names the repo — not written verbatim, and not first noticed at write time.
func TestBuildPlanRefusesMalformedOverrideDigest(t *testing.T) {
	r := discoverFixture(t)
	for name, ov := range map[string]image.Ref{
		"short uppercase": {Tag: "v9", Digest: "sha256:DEADBEEF"},
		"uppercase hex":   {Tag: "v9", Digest: "sha256:" + strings.Repeat("A", 64)},
		"wrong algorithm": {Tag: "v9", Digest: "sha512:" + strings.Repeat("a", 64)},
		"missing sha256:": {Tag: "v9", Digest: strings.Repeat("a", 64)},
		"malformed tag":   {Tag: "-v9", Digest: digestC},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildPlan(r, "app-staging", "app-production", promotable, map[string]image.Ref{"ghcr.io/example/web": ov})
			if err == nil {
				t.Fatalf("malformed override %+v accepted", ov)
			}
			if !strings.Contains(err.Error(), "digest override for ghcr.io/example/web") {
				t.Errorf("error does not name the repo at the override layer: %v", err)
			}
		})
	}
}

func twoEnvRepo(t *testing.T, staging, production string) *Repo {
	t.Helper()
	root := writeRepo(t, map[string]string{
		"cluster/apps/s.yaml":                  wrapper("api-s", "cluster/apps/staging/api", "staging"),
		"cluster/apps/p.yaml":                  wrapper("api-p", "cluster/apps/production/api", "production"),
		"cluster/apps/staging/api/app.yaml":    staging,
		"cluster/apps/production/api/app.yaml": production,
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildPlanRefusesBareTagSource(t *testing.T) {
	r := twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api:v2"),
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA))
	_, err := BuildPlan(r, "staging", "production", promotable, nil)
	if err == nil || !strings.Contains(err.Error(), "bare tag") {
		t.Fatalf("err = %v, want a bare-tag refusal", err)
	}
	// Implicit latest is a bare tag too.
	r = twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api"),
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA))
	if _, err := BuildPlan(r, "staging", "production", promotable, nil); err == nil {
		t.Fatal("implicit latest accepted as a source ref")
	}
	// A caller-supplied digest rescues it.
	p, err := BuildPlan(r, "staging", "production", promotable, map[string]image.Ref{"ghcr.io/example/api": {Tag: "v2", Digest: digestB}})
	if err != nil || len(p.Edits) != 1 || p.Edits[0].New.Digest != digestB {
		t.Fatalf("plan with override: %+v, %v", p.Edits, err)
	}
}

// A source occurrence in pod-imageID form (repo@sha256:…) is pinned but tagless. Promoting it
// verbatim would write image: repo@sha256:… — invariant 1 requires <repo>:<tag>@sha256:<digest>
// — and borrowing the target's tag would make that tag lie about the digest. The plan fails
// naming the repo; an override carrying both tag and digest is the way through.
func TestBuildPlanRefusesTaglessPinnedSource(t *testing.T) {
	r := twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api@"+digestA),
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestC))
	_, err := BuildPlan(r, "staging", "production", promotable, nil)
	if err == nil {
		t.Fatal("tagless pinned source ref accepted")
	}
	if !strings.HasPrefix(err.Error(), "ghcr.io/example/api:") || !strings.Contains(err.Error(), "a tag is required") {
		t.Errorf("error must name the repo and say a tag is required: %v", err)
	}
	// With nothing in the target to write the same ref is a warning, not a failure:
	// TestBuildPlanSourceOnlyUnwritableWarns.
	// A tagless override is refused the same way; one carrying both parts satisfies it.
	r = twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api@"+digestA),
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestC))
	if _, err := BuildPlan(r, "staging", "production", promotable, map[string]image.Ref{"ghcr.io/example/api": {Digest: digestB}}); err == nil || !strings.Contains(err.Error(), "a tag is required") {
		t.Errorf("tagless override: err = %v", err)
	}
	p, err := BuildPlan(r, "staging", "production", promotable, map[string]image.Ref{"ghcr.io/example/api": {Tag: "v2", Digest: digestA}})
	if err != nil || len(p.Edits) != 1 || p.Edits[0].New.String() != "ghcr.io/example/api:v2@"+digestA {
		t.Fatalf("plan with tag+digest override: %+v, %v", p.Edits, err)
	}
}

// A promotable repo that runs only in the source env, as a ref hoist could never write,
// must not abort the plan for every other repo: nothing would be written for it, so
// invariant 1 is not at stake (issue #13; AGENTS.md principle 5). It is reported as
// source-only-unpinned, naming the repo, why it could not be planned and its occurrences,
// and the other repos' edits are unaffected.
func TestBuildPlanSourceOnlyUnwritableWarns(t *testing.T) {
	target := deployment("api", "api", "ghcr.io/example/api:v0@"+digestC)
	for name, tc := range map[string]struct{ ref, why string }{
		"bare tag":        {"ghcr.io/example/side:v2", "bare tag"},
		"implicit latest": {"ghcr.io/example/side", "bare tag"},
		"tagless digest":  {"ghcr.io/example/side@" + digestB, "a tag is required"},
	} {
		t.Run(name, func(t *testing.T) {
			r := twoEnvRepo(t,
				deployment("api", "api", "ghcr.io/example/api:v1@"+digestA, "side", tc.ref),
				target)
			p, err := BuildPlan(r, "staging", "production", promotable, nil)
			if err != nil {
				t.Fatalf("source-only unwritable ref failed the whole plan: %v", err)
			}
			if len(p.Edits) != 1 || p.Edits[0].Container != "api" || p.Edits[0].New.String() != "ghcr.io/example/api:v1@"+digestA {
				t.Errorf("edits = %+v, want the one api edit intact", p.Edits)
			}
			if len(p.Warnings) != 1 {
				t.Fatalf("warnings = %+v, want exactly one", p.Warnings)
			}
			w := p.Warnings[0]
			if w.Code != WarnSourceOnlyUnpinned {
				t.Errorf("warning code = %q, want %q", w.Code, WarnSourceOnlyUnpinned)
			}
			if len(w.Occurrences) != 1 || w.Occurrences[0].Container != "side" || w.Occurrences[0].Ref.String() != mustRef(t, tc.ref).String() {
				t.Errorf("warning occurrences = %+v, want side's one occurrence", w.Occurrences)
			}
			for _, want := range []string{"ghcr.io/example/side", tc.why, "no occurrence in production", "cluster/apps/staging/api/app.yaml:12 Deployment/api container=side"} {
				if !strings.Contains(w.Message, want) {
					t.Errorf("warning lacks %q: %s", want, w.Message)
				}
			}
			if len(p.Untouched) != 0 {
				t.Errorf("Untouched = %+v, want none", p.Untouched)
			}
		})
	}
}

// The positive control for the case above: the same source ref with an occurrence in the
// target would need a write, and the plan still fails exactly as before.
func TestBuildPlanSourceUnwritableWithTargetStillRefused(t *testing.T) {
	for name, tc := range map[string]struct{ ref, want string }{
		"bare tag":       {"ghcr.io/example/side:v2", "ghcr.io/example/side: ghcr.io/example/side:v2 runs as side in staging, a bare tag with no digest; nothing hoist writes is a bare tag (AGENTS.md §4.2) — supply a digest for this repo"},
		"tagless digest": {"ghcr.io/example/side@" + digestB, "ghcr.io/example/side: ghcr.io/example/side@" + digestB + " runs as side in staging, a digest with no tag; hoist writes <repo>:<tag>@sha256:<digest> so a tag is required — supply a digest override for this repo carrying both the tag and the digest"},
	} {
		t.Run(name, func(t *testing.T) {
			r := twoEnvRepo(t,
				deployment("api", "api", "ghcr.io/example/api:v1@"+digestA, "side", tc.ref),
				deployment("api", "api", "ghcr.io/example/api:v0@"+digestC, "side", "ghcr.io/example/side:v1@"+digestC))
			_, err := BuildPlan(r, "staging", "production", promotable, nil)
			if err == nil {
				t.Fatal("unwritable source ref with a target occurrence accepted")
			}
			if err.Error() != tc.want {
				t.Errorf("err = %q\nwant %q", err, tc.want)
			}
		})
	}
}

// A digest override still wins for a source-only repo: the ref becomes writable, so the
// warning is the ordinary missing-in-target one and there is still nothing to edit.
func TestBuildPlanSourceOnlyOverrideSilencesWarning(t *testing.T) {
	r := twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA, "side", "ghcr.io/example/side:v2"),
		deployment("api", "api", "ghcr.io/example/api:v0@"+digestC))
	p, err := BuildPlan(r, "staging", "production", promotable, map[string]image.Ref{"ghcr.io/example/side": {Tag: "v2", Digest: digestB}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edits) != 1 || p.Edits[0].Container != "api" {
		t.Errorf("edits = %+v, want only the api edit", p.Edits)
	}
	if len(p.Warnings) != 1 || p.Warnings[0].Code != WarnMissingInTarget || !strings.Contains(p.Warnings[0].Message, "ghcr.io/example/side") {
		t.Errorf("warnings = %+v, want one %s for side and no %s", p.Warnings, WarnMissingInTarget, WarnSourceOnlyUnpinned)
	}
}

func TestBuildPlanChoiceRules(t *testing.T) {
	a, b, c := "ghcr.io/example/api:v1@"+digestA, "ghcr.io/example/api:v2@"+digestB, "ghcr.io/example/api:v3@"+digestC
	target := deployment("api", "api", "ghcr.io/example/api:v0@"+digestC)
	cases := []struct {
		name   string
		source string
		want   string
		reason string
	}{
		{"unique pinned wins over bare majority", deployment("api", "x", "ghcr.io/example/api:v5", "y", "ghcr.io/example/api:v5", "z", b), b, "only pinned"},
		{"most frequent among pinned", deployment("api", "x", a, "y", b, "z", b), b, "most frequent"},
		{"tie breaks to first in path order", deployment("api", "x", c, "y", a, "z", b), c, "first in path order"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := twoEnvRepo(t, tc.source, target)
			p, err := BuildPlan(r, "staging", "production", promotable, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Edits) != 1 || p.Edits[0].New.String() != tc.want {
				t.Errorf("edits = %+v, want New %s", p.Edits, tc.want)
			}
			if len(p.Warnings) != 1 || p.Warnings[0].Code != WarnSourceDisagrees || !strings.Contains(p.Warnings[0].Message, tc.reason) {
				t.Errorf("warnings = %+v, want one %s naming %q", p.Warnings, WarnSourceDisagrees, tc.reason)
			}
		})
	}
}

func TestBuildPlanMissingInTargetWarns(t *testing.T) {
	r := twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA, "side", "ghcr.io/example/side:v1@"+digestB),
		deployment("api", "api", "ghcr.io/example/api:v0@"+digestC))
	p, err := BuildPlan(r, "staging", "production", promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edits) != 1 || len(p.Warnings) != 1 || p.Warnings[0].Code != WarnMissingInTarget || !strings.Contains(p.Warnings[0].Message, "ghcr.io/example/side") {
		t.Errorf("edits=%+v warnings=%+v", p.Edits, p.Warnings)
	}
}

func TestBuildPlanRefusesBlockScalarTarget(t *testing.T) {
	block := "kind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: |\n            ghcr.io/example/api:v0@" + digestC + "\n"
	r := twoEnvRepo(t, deployment("api", "api", "ghcr.io/example/api:v1@"+digestA), block)
	if _, err := BuildPlan(r, "staging", "production", promotable, nil); err == nil || !strings.Contains(err.Error(), "block scalar") {
		t.Fatalf("err = %v, want block-scalar refusal", err)
	}
}

func TestBuildPlanArgumentErrors(t *testing.T) {
	r := discoverFixture(t)
	if _, err := BuildPlan(r, "app-staging", "app-staging", promotable, nil); err == nil {
		t.Error("same env accepted")
	}
	if _, err := BuildPlan(r, "nope", "app-production", promotable, nil); err == nil {
		t.Error("unknown source env accepted")
	}
	if _, err := BuildPlan(r, "app-staging", "nope", promotable, nil); err == nil {
		t.Error("unknown target env accepted")
	}
	if _, err := BuildPlan(r, "app-staging", "app-production", nil, nil); err == nil {
		t.Error("empty promotable list accepted")
	}
	if _, err := BuildPlan(nil, "a", "b", promotable, nil); err == nil {
		t.Error("nil repo accepted")
	}
}

func TestEditNoOp(t *testing.T) {
	r := twoEnvRepo(t,
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		deployment("api", "api", "ghcr.io/example/api:v1@"+digestA))
	p, err := BuildPlan(r, "staging", "production", promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edits) != 1 || !p.Edits[0].NoOp() {
		t.Fatalf("edits = %+v, want one no-op edit", p.Edits)
	}
	before := []byte(deployment("api", "api", "ghcr.io/example/api:v1@"+digestA))
	after, err := ApplyBytes(before, p.Edits)
	if err != nil || string(after) != string(before) {
		t.Errorf("no-op edit changed bytes or failed: %v", err)
	}
}
