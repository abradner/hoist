package plan

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/resolve"
)

// TestSpaceTicks is the positive control for keys.HuhKeyMap's own MultiSelect.Toggle == space
// (T3-09, UX-H4): a real keypress, never a write to the bound field (§9 entry 6).
func TestSpaceTicks(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	before := append([]string(nil), m.ticked...)
	m, _ = m.Update(uitest.Key("space"))
	if equalSets(before, m.ticked) {
		t.Fatalf("space did not toggle the hovered row: ticked = %v", m.ticked)
	}
}

// TestXUnbound is space's own retirement test (T3-09, UX-H4: x toggle repo → space toggle
// repo): x must change nothing at all now that keys.HuhKeyMap strips it from
// MultiSelect.Toggle, not merely "also" toggle via some other path.
func TestXUnbound(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	before := m.View()
	m2, cmd := m.Update(uitest.Key("x"))
	if cmd != nil {
		t.Errorf("x produced a command: %v", cmd())
	}
	if !equalSets(m.ticked, m2.ticked) {
		t.Errorf("x changed the ticked set: before=%v after=%v", m.ticked, m2.ticked)
	}
	if got := m2.View(); got != before {
		t.Errorf("x changed the screen:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestEOpensOverride is the digest-override gesture's own retirement test (T3-09, was o):
// e — not o — opens the override dialog. override_test.go's own fixtures already exercise the
// gesture end-to-end through "e"; this is the narrow, named regression the design calls for.
func TestEOpensOverride(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	m2, _ := m.Update(uitest.Key("o"))
	if m2.overriding {
		t.Fatal("o must not open the override dialog any more")
	}
	m3, _ := m.Update(uitest.Key("e"))
	if !m3.overriding {
		t.Fatal("e did not open the override dialog")
	}
}

// noOpFixture builds a ready plan screen with one ordinary row (web) and one already-current,
// resolved row (worker) — Row.NoOp true, Row.Disabled false — so DeriveRows' own guard
// ("!row.Disabled") is exercised by a row that could otherwise be confused with an unresolved
// one. Styled and sized so leftBody/View render real text (§9 entry 9: an unstyled/unsized
// fixture would pass this test whether or not the styling call happened at all).
func noOpFixture(t *testing.T) Model {
	t.Helper()
	r := discoverFixture(t)
	webOld := ref("ghcr.io/example/web:v1@sha256:" + strings.Repeat("a", 64))
	webNew := ref("ghcr.io/example/web:v2@sha256:" + strings.Repeat("b", 64))
	workerRef := ref("ghcr.io/example/worker:v3@sha256:" + strings.Repeat("c", 64))
	pl := gitops.Plan{
		SourceEnv: "app-staging", TargetEnv: "app-production",
		Edits: []gitops.Edit{
			edit("cluster/apps/app-production/web/app.yaml", webOld, webNew),
			edit("cluster/apps/app-production/worker/app.yaml", workerRef, workerRef),
		},
	}
	res := map[string]resolve.Resolution{
		"ghcr.io/example/web":    {Repo: "ghcr.io/example/web", Ref: webNew, Source: resolve.SourcePods, Detail: "resolved"},
		"ghcr.io/example/worker": {Repo: "ghcr.io/example/worker", Ref: workerRef, Source: resolve.SourcePods, Detail: "resolved"},
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", noneFunc([]string{"ghcr.io/"}), history.Funcs{})
	m.state = stateReady
	m.plan = pl
	m.rows = DeriveRows(pl, res)
	m.prefix = CommonPrefix(m.rows)
	m.buildMultiSelect()
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = m.refreshRight()
	return m
}

// TestNoOpRowGreyedAndExplained: a row whose edits already match the target (Row.NoOp) is
// listed, greyed, with the reason — but is neither tickable nor counted toward the totals
// (T3-09, UX-M17, the plan-confirm mockup's own "· worker · already current").
func TestNoOpRowGreyedAndExplained(t *testing.T) {
	m := noOpFixture(t)
	if len(Selectable(m.rows)) != 1 || Selectable(m.rows)[0].Repo != "ghcr.io/example/web" {
		t.Fatalf("Selectable = %+v, want only web", Selectable(m.rows))
	}
	noops := NoOps(m.rows)
	if len(noops) != 1 || noops[0].Repo != "ghcr.io/example/worker" {
		t.Fatalf("NoOps = %+v, want only worker", noops)
	}
	body := ansi.Strip(m.leftBody())
	if !strings.Contains(body, "· worker · already current") {
		t.Errorf("leftBody lacks the greyed no-op row:\n%s", body)
	}
	if has(m.ticked, "ghcr.io/example/worker") {
		t.Errorf("worker must never be ticked: ticked = %v", m.ticked)
	}
	if strings.Contains(ansi.Strip(m.totalsSection()), "2 repos") {
		t.Errorf("totals must not count the no-op row: %s", m.totalsSection())
	}
}

// TestWarningsAreSentences: nothing this screen renders leads a line with a bare "!" any more
// (T3-09, UX-M17) — checked over every surface that used to prefix one: a repo's own resolution
// warnings (impactBody) and the production-skip notice (notes).
func TestWarningsAreSentences(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{Production: []string{"app-production"}, Pairs: map[string]string{"app-staging": "app-staging-2"}})
	var withWarnings Row
	found := false
	for _, r := range m.rows {
		if len(r.Warnings) > 0 {
			withWarnings, found = r, true
			break
		}
	}
	if !found {
		t.Fatal("setup: fixture has no row with warnings to check")
	}
	for i, r := range Selectable(m.rows) {
		if r.Repo == withWarnings.Repo {
			m.moveCursorTo(i)
			break
		}
	}
	body := ansi.Strip(m.impactBody())
	// Positive control: the warning's own text must actually be on screen, or the "no bare !"
	// assertion below would pass whether or not this row's warnings were ever rendered at all.
	if !strings.Contains(body, "pinned ref") {
		t.Fatalf("setup: impactBody for %s lacks its own warning text:\n%s", withWarnings.Repo, body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "!") {
			t.Errorf("impactBody line still leads with a bare \"!\": %q", line)
		}
	}
	n := ansi.Strip(m.notes())
	// Positive control: the fixture's own env config (production target, source paired
	// elsewhere) must actually reach the skip-notice branch, or the "no bare !" assertion above
	// would pass whether or not that branch was ever exercised.
	if !strings.Contains(n, "skipping app-staging-2") {
		t.Fatalf("setup: notes() = %q, want the production-skip sentence (SkippedStaging)", n)
	}
	if strings.HasPrefix(strings.TrimSpace(n), "!") {
		t.Errorf("notes still leads with a bare \"!\": %q", n)
	}
}
