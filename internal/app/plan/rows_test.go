package plan

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/resolve"
)

const fixtureRoot = "../../../testdata/repo"

func ref(s string) image.Ref {
	r, err := image.Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

func occ(file string, r image.Ref) gitops.Occurrence {
	return gitops.Occurrence{File: file, Line: 1, Kind: "Deployment", Name: "app", Container: "app", Ref: r}
}

func edit(file string, old, newRef image.Ref) gitops.Edit {
	return gitops.Edit{Occurrence: occ(file, old), New: newRef}
}

// TestDeriveRows tables an in-memory Plan against a resolve.Resolution map: ticked/disabled
// rows, labels and warning attachment, per rows.go's contract with model.go.
func TestDeriveRows(t *testing.T) {
	webOld := ref("ghcr.io/example/web:v1@sha256:" + strings.Repeat("a", 64))
	webNew := ref("ghcr.io/example/web:v2@sha256:" + strings.Repeat("b", 64))
	dbOld := ref("ghcr.io/example/db:v1@sha256:" + strings.Repeat("c", 64))
	dbNew := ref("ghcr.io/example/db:v1@sha256:" + strings.Repeat("c", 64)) // NoOp

	pl := gitops.Plan{
		SourceEnv: "app-staging",
		TargetEnv: "app-production",
		Edits: []gitops.Edit{
			edit("a.yaml", webOld, webNew),
			edit("b.yaml", webOld, webNew),
			edit("db.yaml", dbOld, dbNew),
		},
		Warnings: []gitops.Warning{
			{Code: "source-disagrees", Message: "web disagrees", Occurrences: []gitops.Occurrence{occ("a.yaml", webOld)}},
		},
	}
	res := map[string]resolve.Resolution{
		"ghcr.io/example/web": {Repo: "ghcr.io/example/web", Ref: webNew, Source: resolve.SourcePods, Detail: "1 running container agrees"},
		"ghcr.io/example/db":  {Repo: "ghcr.io/example/db", Warnings: []gitops.Warning{{Code: resolve.WarnUnresolved, Message: "db: no digest source", Occurrences: []gitops.Occurrence{occ("db.yaml", dbOld)}}}},
	}

	rows := DeriveRows(pl, res)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// Rows are sorted by repo: db before web.
	if rows[0].Repo != "ghcr.io/example/db" || rows[1].Repo != "ghcr.io/example/web" {
		t.Fatalf("rows not sorted by repo: %+v", rows)
	}
	db, web := rows[0], rows[1]

	if !db.Disabled {
		t.Error("db: want Disabled (unresolved), got not disabled")
	}
	if db.Reason == "" {
		t.Error("db: want a Reason")
	}
	if db.Count != 1 {
		t.Errorf("db.Count = %d, want 1", db.Count)
	}

	if web.Disabled {
		t.Error("web: want not Disabled (resolved via pods)")
	}
	if web.Source != string(resolve.SourcePods) {
		t.Errorf("web.Source = %q, want %q", web.Source, resolve.SourcePods)
	}
	if web.Count != 2 {
		t.Errorf("web.Count = %d, want 2", web.Count)
	}
	if len(web.Warnings) != 1 {
		t.Errorf("web.Warnings = %d, want 1 (source-disagrees)", len(web.Warnings))
	}
	if !strings.Contains(web.Label(), "!") {
		t.Errorf("web.Label() = %q, want the warning marker", web.Label())
	}
	if strings.Contains(db.Label(), "!") {
		t.Errorf("db.Label() = %q, want no warning marker (it has none)", db.Label())
	}
	if !strings.Contains(web.Label(), "v1") || !strings.Contains(web.Label(), "v2") {
		t.Errorf("web.Label() = %q, want old and new tags", web.Label())
	}
	if !strings.Contains(web.Label(), "(2 occurrences)") {
		t.Errorf("web.Label() = %q, want the occurrence count", web.Label())
	}

	sel := Selectable(rows)
	if len(sel) != 1 || sel[0].Repo != "ghcr.io/example/web" {
		t.Errorf("Selectable(rows) = %+v, want just web", sel)
	}
	dis := Disabled(rows)
	if len(dis) != 1 || dis[0].Repo != "ghcr.io/example/db" {
		t.Errorf("Disabled(rows) = %+v, want just db", dis)
	}
}

// TestDeriveRowsNoResolution is "digest sources: none": every row reads Source "manifest"
// and none are disabled, exactly as BuildPlan alone (M1) would have planned.
func TestDeriveRowsNoResolution(t *testing.T) {
	old := ref("ghcr.io/example/web:v1@sha256:" + strings.Repeat("a", 64))
	newRef := ref("ghcr.io/example/web:v2@sha256:" + strings.Repeat("b", 64))
	pl := gitops.Plan{Edits: []gitops.Edit{edit("a.yaml", old, newRef)}}

	rows := DeriveRows(pl, nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Disabled {
		t.Error("want not disabled with no resolution at all")
	}
	if rows[0].Source != "manifest" {
		t.Errorf("Source = %q, want manifest", rows[0].Source)
	}
}

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

func TestIsProduction(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"app-production"}}
	if !IsProduction("app-production", envs) {
		t.Error("app-production should be production")
	}
	if IsProduction("app-staging", envs) {
		t.Error("app-staging should not be production")
	}
}

func TestSkippedStaging(t *testing.T) {
	envs := config.EnvsConfig{
		Production: []string{"app-production", "app-production-2"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	if _, skip := SkippedStaging("app-staging", "app-production", envs); skip {
		t.Error("promoting to the configured pair must not warn")
	}
	if staging, skip := SkippedStaging("app-staging", "app-production-2", envs); !skip {
		t.Error("promoting straight to an unpaired production env should warn")
	} else if staging != "app-production" {
		t.Errorf("staging = %q, want app-production", staging)
	}
	if _, skip := SkippedStaging("app-staging", "app-staging-2", envs); skip {
		t.Error("a non-production target must never warn")
	}
	// A source with no configured pair has nothing to report as skipped.
	if _, skip := SkippedStaging("app-dev", "app-production", envs); skip {
		t.Error("no configured pair for the source means nothing to skip")
	}
}

func TestTargetsFor(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	got := TargetsFor(r, "app-staging")
	if len(got) != 1 || got[0] != "app-production" {
		t.Errorf("TargetsFor(app-staging) = %v, want [app-production]", got)
	}
}

// Codex P2 (draft #29 pass): the CLI distinguishes "registry not consulted" from
// "consulted; every auth source failed" (cmd/hoist's resolutionReport.print); the TUI's
// Summary must make the same distinction rather than reporting both as "not consulted",
// which reads as "the registry was never asked" when it was asked and simply failed.
func TestSummaryDistinguishesNotConsultedFromAllFailed(t *testing.T) {
	notConsulted := Summary(ResolveOutcome{})
	if !containsLine(notConsulted, "registry not consulted") {
		t.Errorf("no attempt at all: got %v, want a \"registry not consulted\" line", notConsulted)
	}
	allFailed := Summary(ResolveOutcome{RegistryConsulted: true, RegistryAuthTried: []string{"env", "keychain"}})
	want := "registry: consulted; all auth sources failed (env, keychain)"
	if !containsLine(allFailed, want) {
		t.Errorf("consulted, all failed: got %v, want a line %q", allFailed, want)
	}
	won := Summary(ResolveOutcome{RegistryConsulted: true, RegistryAuth: "cluster"})
	if !containsLine(won, "registry auth: cluster") {
		t.Errorf("consulted, cluster won: got %v, want \"registry auth: cluster\"", won)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
