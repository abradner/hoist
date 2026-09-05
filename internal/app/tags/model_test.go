package tags

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/registry"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

const goldenDir = "../../../testdata/golden"

// drain runs m through every tea.Cmd Init/Update produces, one message at a time (batches
// unpacked recursively), exactly as internal/app's root would deliver them — no tea.Program
// involved. Mirrors plan/model_test.go's runInit, generalized to keep draining after Update
// (this screen's onListLoaded/onMetaLoaded return further batched fetch cmds of their own).
func drain(m Model, cmd tea.Cmd) Model {
	for cmd != nil {
		msg := cmd()
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			if _, isTick := msg.(spinner.TickMsg); isTick {
				// spinner.Update's own returned cmd re-fires forever, to keep the animation
				// going for as long as the real program runs — draining it here would loop
				// forever. Tests don't need the animation itself, only what a tick's sibling
				// cmds in the same batch produce (handled below).
				return m
			}
			m, cmd = m.Update(msg)
			continue
		}
		var next tea.Cmd
		for _, c := range batch {
			if c == nil {
				continue
			}
			sub := c()
			if _, isTick := sub.(spinner.TickMsg); isTick {
				continue
			}
			var mc tea.Cmd
			m, mc = m.Update(sub)
			next = tea.Batch(next, mc)
		}
		cmd = next
	}
	return m
}

// fixedMetas builds a MetaFunc over a fixed table — deterministic, synchronous, no real
// registry — keyed by tag.
func fixedMetas(table map[string]registry.ImageMeta) MetaFunc {
	return func(_ context.Context, tag string) (registry.ImageMeta, error) {
		m, ok := table[tag]
		if !ok {
			return registry.ImageMeta{}, errors.New("no fixture metadata for " + tag)
		}
		return m, nil
	}
}

func readyModel(t *testing.T, target string, mapped, production bool) Model {
	t.Helper()
	regTags := []string{"v3", "v2", "v1"}
	gitTags := []forge.GitTag{
		{Name: "v1", Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "v2", Date: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "v3", Date: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	}
	metas := map[string]registry.ImageMeta{
		"v1": {Digest: "sha256:" + strings.Repeat("1", 64), Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		"v2": {Digest: "sha256:" + strings.Repeat("2", 64), Created: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
		"v3": {Digest: "sha256:" + strings.Repeat("3", 64), Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	}
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		if !mapped {
			return regTags, nil, false, nil
		}
		return regTags, gitTags, true, nil
	}
	m := New("ghcr.io/example/app", target, mapped, production, "app-staging", []string{"v1"}, target == "app-production", listFn, fixedMetas(metas))
	m = m.SetSize(100, 30)
	m = m.SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	return m
}

func TestInitListsAndLoadsVisibleMetadata(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(m.rows))
	}
	for _, r := range m.rows {
		if !r.MetaLoaded {
			t.Errorf("row %s: MetaLoaded = false, want true (small list, all visible)", r.Tag)
		}
	}
	// Mapped + git tags present: sorted by git date, newest first.
	if m.rows[0].Tag != "v3" || m.rows[1].Tag != "v2" || m.rows[2].Tag != "v1" {
		t.Fatalf("order = %v, want v3, v2, v1", tagsOf(m.rows))
	}
}

// TestUnmappedRepoNeverSetsHasGitDate exercises readyModel's mapped=false path end to end
// (through New/Init, not just DeriveRows/Reorder in isolation — rows_test.go already covers
// those pure functions): with no app-repo mapping, the listFn never reports git tags at all,
// and no row ever carries HasGitDate.
func TestUnmappedRepoNeverSetsHasGitDate(t *testing.T) {
	m := readyModel(t, "app-staging", false, false)
	for _, r := range m.rows {
		if r.HasGitDate {
			t.Fatalf("unmapped repo must never set HasGitDate, got %+v", r)
		}
	}
}

// TestMappedRepoFallsBackToCreatedWhenForgeLookupFailsAtRuntime is finding 3's own regression
// test: a repo the config genuinely maps to an app repo (New's own mapped=true, exactly what
// cmd/hoist's buildTagsFunc would pass from a real RepoConfig.Apps entry), but whose forge tag
// lookup fails once the picker actually asks for it — ListFunc's own runtime mapped return is
// false (cmd/hoist's own degrade-not-fail shape). DeriveRows/Reorder must fall back to
// Created-based ordering (invariant 3's fallback), not leave rows in the registry's own
// arbitrary order while mislabeling every row "mapped but unmatched" — the bug: cmd/hoist's
// ListFunc left New's constructor-time mapped=true in effect even after the runtime failure,
// so DeriveRows/Reorder never engaged the fallback at all.
func TestMappedRepoFallsBackToCreatedWhenForgeLookupFailsAtRuntime(t *testing.T) {
	// Registry order deliberately differs from Created order, so a test that merely didn't
	// crash couldn't pass by accident — only an actual Created-descending sort produces v1,
	// v3, v2 from this input.
	regTags := []string{"v1", "v2", "v3"}
	metas := map[string]registry.ImageMeta{
		"v1": {Digest: "sha256:" + strings.Repeat("1", 64), Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		"v2": {Digest: "sha256:" + strings.Repeat("2", 64), Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		"v3": {Digest: "sha256:" + strings.Repeat("3", 64), Created: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
	}
	// Mirrors cmd/hoist's buildTagsFunc: the forge lookup itself fails at runtime, so this
	// call's own mapped return is false even though the config-known mapped param below is
	// true (a real RepoConfig.Apps entry exists for this image repo).
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return regTags, nil, false, nil
	}
	m := New("ghcr.io/example/app", "app-staging", true /* config says mapped */, false, "", nil, false, listFn, fixedMetas(metas))
	m = m.SetSize(100, 30)
	m = m.SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	if m.mapped {
		t.Fatal("m.mapped must be overwritten false once the runtime forge lookup fails, not left at New's constructor-time true")
	}
	for _, r := range m.rows {
		if r.HasGitDate {
			t.Fatalf("a runtime forge failure must never leave a row's HasGitDate set: %+v", r)
		}
	}
	got := make([]string, len(m.rows))
	for i, r := range m.rows {
		got[i] = r.Tag
	}
	want := []string{"v1", "v3", "v2"} // Created descending, NOT regTags' own [v1,v2,v3] order.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows should have fallen back to Created-based ordering: got %v, want %v", got, want)
	}
	if strings.Contains(m.View(), "unordered (no matching git tag)") {
		t.Fatalf("the mapped-but-unmatched divider must not render once mapped has fallen back to false:\n%s", m.View())
	}
}

func TestCursorMoveSelectsNextRow(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	if m.selectedTag != "v3" {
		t.Fatalf("initial selection = %q, want v3", m.selectedTag)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.selectedTag != "v2" {
		t.Fatalf("after down: selection = %q, want v2", m.selectedTag)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.selectedTag != "v3" {
		t.Fatalf("after up: selection = %q, want v3", m.selectedTag)
	}
}

// TestViewWindowsAroundCursorPastFirstPage is finding 5's own regression test: the tag list has
// no viewport if every row renders unconditionally, so the cursor can move off-screen. Builds a
// mapped, deterministically-ordered (by git tag date, so Reorder never reshuffles it) list of
// 30 tags at a small height (pageSize = max(10-4,5) = 6), confirming the initial view is
// windowed (the last row is off-screen) and that moving the cursor to the last row scrolls the
// window to bring it back into view while staying bounded (the first row scrolls away).
func TestViewWindowsAroundCursorPastFirstPage(t *testing.T) {
	const n = 30
	regTags := make([]string, n)
	gitTags := make([]forge.GitTag, n)
	metas := map[string]registry.ImageMeta{}
	for i := 0; i < n; i++ {
		tag := fmt.Sprintf("v%02d", i)
		regTags[i] = tag
		// Date increases with i, so DeriveRows (newest git-tag date first) sorts the rows as
		// v29, v28, ..., v00 — a fixed order Reorder never touches (mapped=true is a no-op for
		// it), so cursor movement by index is fully predictable regardless of meta-load timing.
		date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		gitTags[i] = forge.GitTag{Name: tag, Date: date}
		metas[tag] = registry.ImageMeta{Digest: "sha256:" + strings.Repeat("1", 64), Created: date}
	}
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) { return regTags, gitTags, true, nil }
	m := New("ghcr.io/example/app", "app-staging", true, false, "", nil, false, listFn, fixedMetas(metas))
	m = m.SetSize(100, 10) // pageSize = max(10-4, 5) = 6
	m = m.SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	if m.selectedTag != "v29" {
		t.Fatalf("initial selection = %q, want v29 (newest git-tag date first)", m.selectedTag)
	}

	got := ansi.Strip(m.View())
	if strings.Contains(got, "v00") {
		t.Fatalf("initial view should be windowed to the viewport, but shows the last (oldest) row:\n%s", got)
	}

	// Move the cursor all the way to the last row — far past the first page.
	for i := 0; i < n-1; i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.selectedTag != "v00" {
		t.Fatalf("cursor should be on the last row after %d downs, got %q", n-1, m.selectedTag)
	}
	got = ansi.Strip(m.View())
	if !strings.Contains(got, "v00") {
		t.Fatalf("scrolling to the last row should bring it back into view:\n%s", got)
	}
	if strings.Contains(got, "v29") {
		t.Fatalf("window should have scrolled away from the first row once the cursor left it:\n%s", got)
	}
}

// TestUnmappedLazyOrderingMarksUnevaluatedRows is round 5's finding 4 regression test: an
// unmapped repo's Created-based ordering (invariant 3's fallback) is only ever established among
// rows fetchVisible has actually loaded — a genuinely newer tag sitting outside every window the
// cursor has visited is never fetched, and so can never be sorted to the top. This builds an
// unmapped, 30-tag list at a small height (pageSize = max(10-4,5) = 6, so only the first 6 rows
// ever get fetched at cursor v00) where the very LAST tag (v29, off past the fetched window) is
// deliberately the actual newest — proving both halves: the limitation itself (v29 is never
// promoted to the top) and this round's chosen fix (the view honestly marks how many rows
// outside the window remain unevaluated, rather than silently claiming a complete sort).
func TestUnmappedLazyOrderingMarksUnevaluatedRows(t *testing.T) {
	const n = 30
	regTags := make([]string, n)
	metas := map[string]registry.ImageMeta{}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		tag := fmt.Sprintf("v%02d", i)
		regTags[i] = tag
		// Every tag except the cursor's own (v00, below) gets an OLD date. This keeps the
		// fetch window from cascading past its initial page: once v00's own window-mates
		// (v01..v05) load, they all sort BEHIND v00 (older), so v00's own index — and
		// therefore the window fetchVisible computes around it — never moves. Without this,
		// a naively increasing date-by-index would make every row progressively newer than
		// the cursor's, and the resulting cascade of re-fetches would eventually load the
		// entire list, defeating what this test needs to demonstrate.
		metas[tag] = registry.ImageMeta{Digest: "sha256:" + strings.Repeat("1", 64), Created: base.AddDate(0, 0, i)}
	}
	// v00 (the initially-selected cursor, and the first tag fetchVisible's first window
	// includes) is deliberately newer than every one of its initial window-mates, so it stays
	// at the front once they all load.
	metas["v00"] = registry.ImageMeta{Digest: "sha256:" + strings.Repeat("0", 64), Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// v29 is actually the newest tag of all — but it sits at the far end of the list, well
	// outside the window that ever gets fetched, so Reorder never learns its date at all.
	metas["v29"] = registry.ImageMeta{Digest: "sha256:" + strings.Repeat("9", 64), Created: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) { return regTags, nil, false, nil }
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false, listFn, fixedMetas(metas))
	m = m.SetSize(100, 10) // pageSize = max(10-4, 5) = 6
	m = m.SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	if m.selectedTag != "v00" {
		t.Fatalf("initial selection = %q, want v00 (unmapped: DeriveRows never reorders before anything loads)", m.selectedTag)
	}
	loaded := 0
	for _, r := range m.rows {
		if r.MetaLoaded {
			loaded++
		}
		if r.Tag == "v29" && r.MetaLoaded {
			t.Fatal("v29 sits far outside the visible window and must not have been fetched — otherwise this test isn't exercising the lazy-fetch gap at all")
		}
	}
	if loaded != 6 {
		t.Fatalf("expected exactly the 6-row initial window to have been fetched, got %d loaded rows — this test's own fixture no longer keeps the fetch window bounded", loaded)
	}

	got := ansi.Strip(m.View())
	if strings.Contains(got, "v29") {
		t.Fatalf("v29 was never evaluated and must not have been promoted into the visible top rows:\n%s", got)
	}
	if !strings.Contains(got, "haven't been evaluated yet") {
		t.Fatalf("view should honestly mark that rows outside the window remain unevaluated, rather than silently claim a complete Created-sort:\n%s", got)
	}
	if !strings.Contains(got, "24 tag(s)") {
		t.Fatalf("24 of the 30 rows (everything outside the 6-row window) should be counted as unevaluated:\n%s", got)
	}
}

func TestEnterEmitsSelectedMsgOnceMetaLoaded(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg, ok := cmd().(SelectedMsg)
	if !ok {
		t.Fatalf("got %T, want SelectedMsg", cmd())
	}
	if msg.Tag != "v3" || msg.ImageRepo != "ghcr.io/example/app" || msg.Digest == "" {
		t.Fatalf("SelectedMsg = %+v", msg)
	}
}

// TestFailedMetaFetchIsNotRetriedForever is finding 4's own regression test: a tag whose
// Config call fails must be treated as settled, not rescheduled every time fetchVisible runs
// again (cursor movement, a filter change, another row's metadata landing and reordering the
// list) — without this, a missing manifest, an unsupported platform, or a temporarily
// unavailable registry creates an unbounded, indefinite request loop for as long as the
// picker stays open and the row remains visible.
func TestFailedMetaFetchIsNotRetriedForever(t *testing.T) {
	regTags := []string{"v1", "v2", "v3"}
	var calls int
	metaFn := func(_ context.Context, tag string) (registry.ImageMeta, error) {
		calls++
		if tag == "v3" {
			return registry.ImageMeta{}, errors.New("manifest not found")
		}
		return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("1", 64)}, nil
	}
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return regTags, nil, false, nil
	}
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false, listFn, metaFn)
	m = m.SetSize(100, 30)
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	i := IndexOf(m.rows, "v3")
	if i < 0 {
		t.Fatal("fixture precondition: v3 must be a row")
	}
	if m.rows[i].MetaErr == nil {
		t.Fatal("fixture precondition: v3's Config call must have failed")
	}
	if m.rows[i].MetaLoading {
		t.Fatal("a failed fetch must clear MetaLoading, not leave it stuck true")
	}
	callsAfterFirstSettle := calls

	// Trigger fetchVisible again several times over, exactly as cursor movement, a filter
	// change, or another row's own metadata landing would in the real picker — none of these
	// may re-request v3's already-failed metadata.
	for n := 0; n < 5; n++ {
		var cmd tea.Cmd
		m, cmd = m.fetchVisible()
		m = drain(m, cmd)
	}
	if calls != callsAfterFirstSettle {
		t.Fatalf("Config was called %d more time(s) for an already-failed row after 5 more fetchVisible passes — want exactly the original %d calls, no retries", calls-callsAfterFirstSettle, callsAfterFirstSettle)
	}
	if m.rows[IndexOf(m.rows, "v3")].MetaErr == nil {
		t.Fatal("v3 should still carry its recorded error")
	}
}

// TestSelectCurrentDistinguishesFailedFromStillLoading is Copilot's own round-N finding: a row
// whose metadata permanently failed (MetaErr set, never retried — TestFailedMetaFetchIsNotRetriedForever
// above) is indistinguishable from a row still in flight under a bare "!MetaLoaded" check —
// selectCurrent used to report "still loading... try again in a moment" for both, which never
// self-corrects for the failed case: fetchVisible will never retry it, so the row is
// permanently unselectable with no operator-visible explanation of why. Enter must instead say
// the fetch failed and won't be retried, and must still emit no message (a selection without a
// resolved digest is refused regardless — AGENTS.md principle 3).
func TestSelectCurrentDistinguishesFailedFromStillLoading(t *testing.T) {
	regTags := []string{"v1", "v2"}
	metaFn := func(_ context.Context, tag string) (registry.ImageMeta, error) {
		if tag == "v1" {
			return registry.ImageMeta{}, errors.New("manifest not found")
		}
		return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("2", 64)}, nil
	}
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return regTags, nil, false, nil
	}
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false, listFn, metaFn)
	m = m.SetSize(100, 30)
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	if m.selectedTag != "v1" {
		t.Fatalf("fixture precondition: v1 should be selected by default (unmapped, incoming order), got %q", m.selectedTag)
	}
	i := IndexOf(m.rows, "v1")
	if i < 0 || m.rows[i].MetaErr == nil {
		t.Fatal("fixture precondition: v1's Config call must have failed")
	}

	m2, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("Enter on a permanently-failed row must not emit a message (no digest to promote), got a command: %v", cmd())
	}
	if strings.Contains(m2.notice, "still loading") {
		t.Fatalf("notice = %q: must not claim this row is still loading — it already failed and fetchVisible will never retry it", m2.notice)
	}
	if !strings.Contains(m2.notice, "fail") {
		t.Fatalf("notice = %q, want it to say the metadata fetch failed", m2.notice)
	}
}

func TestFilterNarrowsRowsAndResetsCursor(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !m.filtering {
		t.Fatal("expected filtering mode after /")
	}
	for _, r := range "v1" {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.filterQuery != "v1" {
		t.Fatalf("filterQuery = %q, want v1", m.filterQuery)
	}
	if len(m.filtered()) != 1 || m.filtered()[0].Tag != "v1" {
		t.Fatalf("filtered = %v, want just v1", tagsOf(m.filtered()))
	}
	if m.selectedTag != "v1" {
		t.Fatalf("selection should have followed the filter to v1, got %q", m.selectedTag)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.filtering || m.filterQuery != "" {
		t.Fatalf("esc in filter mode should clear it: filtering=%v query=%q", m.filtering, m.filterQuery)
	}
}

// TestDirectKeyNotOfferedForProduction is the UI-side half of invariant 5: pressing D on a
// production target must never open the confirm dialog or emit DirectRequestedMsg — it is a
// politeness notice only, since internal/engine.DirectCommitGateStep is what actually enforces
// this (see DirectRequestedMsg's own doc comment).
// TestStagingMismatchNoteDoesNotClaimLiveState is finding 5's own regression test: the paired
// staging env's own tag shown here comes from the gitops repo's committed manifest occurrence
// (rows.StagingMismatch), never any live cluster/Argo read — this package has no such
// connection wired in at all. The rendered note must say so honestly ("committed manifest
// tag") rather than claim the staging env is "currently running" that tag, which would be a
// false claim about live state whenever Argo hasn't synced yet, a rollout is incomplete, or the
// live workload otherwise differs from what's committed.
func TestStagingMismatchNoteDoesNotClaimLiveState(t *testing.T) {
	m := readyModel(t, "app-production", true, true)
	if !m.hasStagingMismatch {
		t.Fatal("fixture precondition: readyModel's own target==app-production shape must set hasStagingMismatch")
	}
	v := m.View()
	if strings.Contains(v, "currently running") {
		t.Fatalf("the staging note must never claim live state (\"currently running\"):\n%s", v)
	}
	if !strings.Contains(v, "committed manifest tag") {
		t.Fatalf("the staging note should honestly describe a committed manifest value:\n%s", v)
	}
	if !strings.Contains(v, m.stagingEnv) || !strings.Contains(v, m.stagingTags[0]) {
		t.Fatalf("the staging note should still name the env and tag (%q, %q):\n%s", m.stagingEnv, m.stagingTags, v)
	}
}

// TestStagingNoteRendersDisagreementAcrossMultipleTags is finding 3's own rendering
// regression test: when StagingMismatch reports more than one distinct tag (the staging env
// genuinely disagrees with itself across families/occurrences), the note must say so
// explicitly and name every distinct tag — never fall back to the singular "committed
// manifest tag is %s" phrasing, which would silently hide the disagreement behind whichever
// one happened to be first.
func TestStagingNoteRendersDisagreementAcrossMultipleTags(t *testing.T) {
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return []string{"v1"}, nil, false, nil
	}
	metaFn := fixedMetas(map[string]registry.ImageMeta{
		"v1": {Digest: "sha256:" + strings.Repeat("1", 64)},
	})
	m := New("ghcr.io/example/app", "app-production", false, true, "app-staging", []string{"v1", "v2"}, true, listFn, metaFn)
	m = m.SetSize(300, 30)
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	v := m.View()
	if !strings.Contains(v, "disagrees") {
		t.Fatalf("expected the staging note to say the tags disagree:\n%s", v)
	}
	if !strings.Contains(v, "v1") || !strings.Contains(v, "v2") {
		t.Fatalf("expected both distinct tags to be named:\n%s", v)
	}
	if strings.Contains(v, "committed manifest tag is") {
		t.Fatalf("must not render the singular-tag phrasing when there's more than one distinct tag:\n%s", v)
	}
}

// TestStagingNoteComparesCursorTagAgainstStaging is the note's reason for existing: the
// operator's actual question on this screen is "has the build under my cursor been anywhere
// first?", and until now the screen printed staging's tags and left them to check by eye.
// The verdict is appended to — never substituted for — the honest description of what was
// read, so both halves must survive together.
func TestStagingNoteComparesCursorTagAgainstStaging(t *testing.T) {
	// readyModel's registry tags are v3/v2/v1 newest-first and its paired staging env carries
	// only v1, so the cursor lands on a tag staging has never had.
	m := readyModel(t, "app-production", true, true)
	if m.cursorTag() != "v3" {
		t.Fatalf("fixture precondition: cursor should start on the newest tag, got %q", m.cursorTag())
	}
	v := m.View()
	if !strings.Contains(v, "warning: v3 (under the cursor) is not committed there") {
		t.Fatalf("a tag staging has never carried must be called out as such:\n%s", v)
	}
	if !strings.Contains(v, "committed manifest tag is v1") {
		t.Fatalf("the verdict must not displace what was actually read:\n%s", v)
	}

	// Move the cursor down to v1, which staging does carry: same note, opposite verdict, and
	// no warning left over from the row above.
	m2 := m
	for i := 0; i < 2; i++ {
		m2, _ = m2.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	if m2.cursorTag() != "v1" {
		t.Fatalf("expected the cursor on v1 after two j presses, got %q", m2.cursorTag())
	}
	v2 := m2.View()
	if !strings.Contains(v2, "v1 is the tag committed there") {
		t.Fatalf("a tag staging does carry must read as such:\n%s", v2)
	}
	if strings.Contains(v2, "warning:") {
		t.Fatalf("no warning should survive moving onto a tag staging carries:\n%s", v2)
	}
}

func TestDirectKeyNotOfferedForProduction(t *testing.T) {
	m := readyModel(t, "app-production", true, true)
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if cmd != nil {
		t.Fatalf("D on a production target produced a command: %v", cmd())
	}
	if m2.confirming {
		t.Fatal("D on a production target must not open the confirm dialog")
	}
	if !strings.Contains(m2.notice, "production") {
		t.Fatalf("expected a notice explaining why, got %q", m2.notice)
	}
}

// TestDirectKeyRequiresConfirmBeforeEmitting is invariant 5's keypress-then-confirm shape:
// the keypress alone must not emit DirectRequestedMsg — only accepting the huh.Confirm does.
func TestDirectKeyRequiresConfirmBeforeEmitting(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if cmd != nil {
		if _, ok := cmd().(DirectRequestedMsg); ok {
			t.Fatal("the keypress alone must not emit DirectRequestedMsg")
		}
	}
	if !m.confirming {
		t.Fatal("D on a non-production target should open the confirm dialog")
	}

	// Answered through the widget's own key, not by assigning the bool behind it: the field
	// huh writes to lives on a superseded Model copy, so setting it here proved nothing about
	// what an operator can actually do (Copilot, PR #72).
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command after confirming")
	}
	msg, ok := cmd().(DirectRequestedMsg)
	if !ok {
		t.Fatalf("got %T, want DirectRequestedMsg", cmd())
	}
	if msg.Tag != "v3" || msg.ImageRepo != "ghcr.io/example/app" {
		t.Fatalf("DirectRequestedMsg = %+v", msg)
	}
}

// TestDirectConfirmDeclineEmitsNothing: accepting the dialog after answering no (the
// operator declined) must not emit DirectRequestedMsg.
func TestDirectConfirmDeclineEmitsNothing(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("declining should emit nothing, got %v", cmd())
	}
	if m.confirming {
		t.Fatal("confirming should be cleared after enter, decline or not")
	}
}

// TestEscDuringConfirmEmitsBackMsg is round-6's regression: the status bar advertises "esc
// back" even while the direct-mode confirm dialog is open, but updateConfirm only ever
// special-cased Enter — Esc fell straight into huh's own widget update and was silently
// swallowed, trapping the operator in the confirm dialog with no way out via the advertised
// key.
func TestEscDuringConfirmEmitsBackMsg(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if !m.confirming {
		t.Fatal("D on a non-production target should open the confirm dialog")
	}
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("expected a command for Esc while confirming")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("got %T, want BackMsg", cmd())
	}
	if m.confirming {
		t.Fatal("confirming should be cleared once Esc is handled")
	}
}

func TestEscEmitsBackMsg(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("expected a command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("got %T, want BackMsg", cmd())
	}
}

// TestStaleListResultFromDifferentRepoIsDiscarded is finding 4's own regression test: a picker
// left mid-flight for one image repo (its own listFn command captured but never delivered)
// must not have that result land in a picker for a DIFFERENT image repo — internal/app's root
// dispatches a message to whatever screen is on top of its stack by type alone, not by which
// Model instance's command produced it, so without imageRepo-scoping a stale listLoadedMsg
// would silently overwrite the new picker's own rows.
func TestStaleListResultFromDifferentRepoIsDiscarded(t *testing.T) {
	left := New("ghcr.io/example/other", "app-staging", false, false, "", nil, false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) {
			return []string{"stale"}, nil, false, nil
		},
		fixedMetas(nil))
	staleMsg := left.loadCmd()()

	current := readyModel(t, "app-staging", true, false)
	beforeRows := append([]Row(nil), current.rows...)
	beforeState := current.state

	after, cmd := current.Update(staleMsg)
	if cmd != nil {
		t.Fatalf("a stale cross-repo result should be discarded silently, got a command: %v", cmd())
	}
	if !reflect.DeepEqual(after.rows, beforeRows) {
		t.Fatalf("stale result from a different image repo mutated rows: got %+v, want unchanged %+v", after.rows, beforeRows)
	}
	if after.state != beforeState {
		t.Fatalf("stale result changed state: got %v, want unchanged %v", after.state, beforeState)
	}
}

// TestStaleMetaResultFromDifferentRepoIsDiscarded is metaLoadedMsg's own half of finding 4: a
// stale per-tag metadata result tagged with a different imageRepo must not overwrite a row's
// already-loaded metadata.
func TestStaleMetaResultFromDifferentRepoIsDiscarded(t *testing.T) {
	current := readyModel(t, "app-staging", true, false)
	before := append([]Row(nil), current.rows...)

	staleMsg := metaLoadedMsg{
		imageRepo: "ghcr.io/example/other",
		tag:       "v3",
		meta:      registry.ImageMeta{Digest: "sha256:" + strings.Repeat("9", 64)},
	}
	after, cmd := current.Update(staleMsg)
	if cmd != nil {
		t.Fatalf("a stale cross-repo meta result should be discarded silently, got a command: %v", cmd())
	}
	if !reflect.DeepEqual(after.rows, before) {
		t.Fatalf("stale meta result from a different image repo mutated rows: got %+v, want unchanged %+v", after.rows, before)
	}
}

// TestStaleListResultFromClosedAndReopenedSameRepoIsDiscarded is finding 2's own regression test
// (round 2 — round 1's fix scoped stale results by imageRepo alone, which cannot distinguish a
// closed-and-reopened picker for the SAME repo from the picker it replaced, since the reopened
// instance's imageRepo is by definition identical). The operator opens a picker for repo X,
// its list command is still in flight when they back out, and they immediately reopen a NEW
// picker for the identical repo X. The FIRST instance's stale result must never land on the
// second, even though msg.imageRepo == current.imageRepo trivially holds for both.
func TestStaleListResultFromClosedAndReopenedSameRepoIsDiscarded(t *testing.T) {
	const repo = "ghcr.io/example/app"
	first := New(repo, "app-staging", true, false, "", nil, false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) {
			return []string{"stale-from-first"}, nil, false, nil
		},
		fixedMetas(nil))
	staleMsg := first.loadCmd()() // captured, never delivered — the operator backs out first.

	// Reopen: a brand new Model for the identical repo, exactly as internal/app's root would
	// construct on a fresh 'd' keypress.
	second := readyModel(t, "app-staging", true, false)
	if first.imageRepo != second.imageRepo {
		t.Fatalf("fixture precondition: both instances must share imageRepo %q, got %q and %q", repo, first.imageRepo, second.imageRepo)
	}
	if first.generation == second.generation {
		t.Fatalf("two distinct Model instances must never share a generation, got %d for both", first.generation)
	}
	beforeRows := append([]Row(nil), second.rows...)
	beforeState := second.state

	after, cmd := second.Update(staleMsg)
	if cmd != nil {
		t.Fatalf("a stale same-repo result from a closed-and-reopened picker should be discarded silently, got a command: %v", cmd())
	}
	if !reflect.DeepEqual(after.rows, beforeRows) {
		t.Fatalf("stale result from the first (closed) instance mutated the reopened picker's rows: got %+v, want unchanged %+v", after.rows, beforeRows)
	}
	if after.state != beforeState {
		t.Fatalf("stale result changed state: got %v, want unchanged %v", after.state, beforeState)
	}
}

// TestStaleMetaResultFromClosedAndReopenedSameRepoIsDiscarded is metaLoadedMsg's own half of the
// same regression: a stale per-tag metadata result from a closed-and-reopened same-repo instance
// must not overwrite the reopened picker's already-loaded (or loading) row metadata.
func TestStaleMetaResultFromClosedAndReopenedSameRepoIsDiscarded(t *testing.T) {
	const repo = "ghcr.io/example/app"
	first := New(repo, "app-staging", true, false, "", nil, false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) { return []string{"v1"}, nil, false, nil },
		fixedMetas(nil))
	staleMsg := first.fetchCmd("v1")() // captured while first was still in flight, never delivered.

	second := readyModel(t, "app-staging", true, false)
	if first.generation == second.generation {
		t.Fatalf("two distinct Model instances must never share a generation, got %d for both", first.generation)
	}
	before := append([]Row(nil), second.rows...)

	after, cmd := second.Update(staleMsg)
	if cmd != nil {
		t.Fatalf("a stale same-repo meta result should be discarded silently, got a command: %v", cmd())
	}
	if !reflect.DeepEqual(after.rows, before) {
		t.Fatalf("stale meta result from the first (closed) instance mutated the reopened picker's rows: got %+v, want unchanged %+v", after.rows, before)
	}
}

func TestListErrorRendersInsteadOfHanging(t *testing.T) {
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) {
			return nil, nil, false, errors.New("registry unreachable")
		},
		nil)
	m = drain(m, m.Init())
	if m.err == nil {
		t.Fatal("expected the list error to be recorded")
	}
	if !strings.Contains(m.View(), "registry unreachable") {
		t.Fatalf("View() should show the error:\n%s", m.View())
	}
}

// TestNilListFuncErrorNamesRepoAndConfigKnob is finding 4's own regression test (round N,
// Copilot): a nil listFn used to report "no registry configured for this repo" — a hardcoded
// placeholder naming neither the actual image repo nor how to fix it. The error must name the
// real image repo and point at the registries[] config knob that supplies listFn.
func TestNilListFuncErrorNamesRepoAndConfigKnob(t *testing.T) {
	m := New("ghcr.io/example/nilcase", "app-staging", false, false, "", nil, false, nil, nil)
	m = drain(m, m.Init())
	if m.err == nil {
		t.Fatal("expected an error state for a nil listFn")
	}
	msg := m.err.Error()
	if !strings.Contains(msg, "ghcr.io/example/nilcase") {
		t.Fatalf("error should name the actual image repo, got %q", msg)
	}
	if !strings.Contains(msg, "registries[]") {
		t.Fatalf("error should point at the registries[] config knob, got %q", msg)
	}
}

// TestNilMetaFuncErrorNamesRepoTagAndConfigKnob is finding 5's own regression test (round N,
// Copilot): a nil metaFn used to report a bare "no registry configured" — no image repo, no
// tag, no pointer to how to fix it. The row's own recorded error must name the image repo and
// the tag this fetch was for, and point at the same registries[] config knob.
func TestNilMetaFuncErrorNamesRepoTagAndConfigKnob(t *testing.T) {
	listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return []string{"v1"}, nil, false, nil
	}
	m := New("ghcr.io/example/nilmeta", "app-staging", false, false, "", nil, false, listFn, nil)
	m = m.SetSize(100, 30)
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady (err=%v)", m.state, m.err)
	}
	i := IndexOf(m.rows, "v1")
	if i < 0 || m.rows[i].MetaErr == nil {
		t.Fatal("fixture precondition: v1's Config fetch must have failed for a nil metaFn")
	}
	msg := m.rows[i].MetaErr.Error()
	if !strings.Contains(msg, "ghcr.io/example/nilmeta") || !strings.Contains(msg, "v1") {
		t.Fatalf("error should name both the image repo and the tag, got %q", msg)
	}
	if !strings.Contains(msg, "registries[]") {
		t.Fatalf("error should point at the registries[] config knob, got %q", msg)
	}
}

// TestLoadCmdUsesModelsCancellableContext and TestFetchCmdUsesModelsCancellableContext are
// finding 8's own wiring regression tests (round N, Codex P2, "cancel tag loads when leaving
// the picker"): loadCmd/fetchCmd used to close over context.Background(), which nothing can
// ever cancel — the fix is a context.Context/CancelFunc pair stored on Model (New's own doc
// comment) and threaded through both. Cancelling the model's own context before invoking the
// command must be visible to listFn/metaFn, proving the wiring, not just that a cancel field
// exists somewhere unused.
func TestLoadCmdUsesModelsCancellableContext(t *testing.T) {
	var gotCtx context.Context
	listFn := func(ctx context.Context) ([]string, []forge.GitTag, bool, error) {
		gotCtx = ctx
		return nil, nil, false, nil
	}
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false, listFn, nil)
	m.cancel()
	m.loadCmd()()
	if gotCtx == nil {
		t.Fatal("listFn was never called")
	}
	if gotCtx.Err() != context.Canceled {
		t.Fatalf("loadCmd must pass the model's own cancellable context to listFn, not context.Background(): Err()=%v", gotCtx.Err())
	}
}

func TestFetchCmdUsesModelsCancellableContext(t *testing.T) {
	var gotCtx context.Context
	metaFn := func(ctx context.Context, _ string) (registry.ImageMeta, error) {
		gotCtx = ctx
		return registry.ImageMeta{}, nil
	}
	m := New("ghcr.io/example/app", "app-staging", false, false, "", nil, false, nil, metaFn)
	m.cancel()
	m.fetchCmd("v1")()
	if gotCtx == nil {
		t.Fatal("metaFn was never called")
	}
	if gotCtx.Err() != context.Canceled {
		t.Fatalf("fetchCmd must pass the model's own cancellable context to metaFn, not context.Background(): Err()=%v", gotCtx.Err())
	}
}

// TestEscCancelsPendingLoad, TestEscDuringConfirmCancelsPendingLoad,
// TestSelectCurrentCancelsPendingLoad and TestConfirmedDirectRequestCancelsPendingLoad are
// finding 8's own "leaving the picker" regression tests: every path that leaves this screen
// for good (Esc outside the confirm dialog, Esc inside it, a plain Enter selection, and a
// confirmed direct-mode request) must cancel the model's own context so a load still in
// flight actually stops — for a mapped repo, ListFunc can walk Forge.Tags through up to 301
// sequential GitHub requests, so an abandoned crawl left running would otherwise keep
// consuming the API rate limit even though its eventual result is already discarded by the
// generation guard. Each test calls m.cancel() indirectly, through the real key-handling code
// path, and reads back m.ctx.Err() on the ORIGINAL model value — cancel's closure operates on
// the shared underlying context regardless of which value-copy invoked it, so this proves the
// call actually happened rather than merely that some copy's field looks right.
func TestEscCancelsPendingLoad(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	if m.ctx.Err() != nil {
		t.Fatal("fixture precondition: context must not be canceled yet")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.ctx.Err() != context.Canceled {
		t.Fatalf("Esc should cancel the model's own load context, got Err()=%v", m.ctx.Err())
	}
}

func TestEscDuringConfirmCancelsPendingLoad(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if !m.confirming {
		t.Fatal("fixture precondition: D on a non-production target should open the confirm dialog")
	}
	if m.ctx.Err() != nil {
		t.Fatal("fixture precondition: context must not be canceled yet")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.ctx.Err() != context.Canceled {
		t.Fatalf("Esc during the confirm dialog should cancel the model's own load context, got Err()=%v", m.ctx.Err())
	}
}

func TestSelectCurrentCancelsPendingLoad(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	if m.ctx.Err() != nil {
		t.Fatal("fixture precondition: context must not be canceled yet")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command")
	}
	if _, ok := cmd().(SelectedMsg); !ok {
		t.Fatalf("got %T, want SelectedMsg", cmd())
	}
	if m.ctx.Err() != context.Canceled {
		t.Fatalf("selecting a tag should cancel the model's own load context, got Err()=%v", m.ctx.Err())
	}
}

func TestConfirmedDirectRequestCancelsPendingLoad(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.ctx.Err() != nil {
		t.Fatal("fixture precondition: context must not be canceled yet")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a command")
	}
	if _, ok := cmd().(DirectRequestedMsg); !ok {
		t.Fatalf("got %T, want DirectRequestedMsg", cmd())
	}
	if m.ctx.Err() != context.Canceled {
		t.Fatalf("confirming a direct commit should cancel the model's own load context, got Err()=%v", m.ctx.Err())
	}
}

// TestViewGolden snapshots the ready screen at 100x30 with a mapped, non-production target
// and a staging mismatch banner — deterministic fixed data, no real registry/forge call.
// Regenerate with: mise exec -- go test ./internal/app/tags -update
func TestViewGolden(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	got := ansi.Strip(m.View())

	for _, want := range []string{
		"hoist tags: ghcr.io/example/app -> app-staging",
		"v1", "v2", "v3",
		"D direct commit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q:\n%s", want, got)
		}
	}

	p := filepath.Join(goldenDir, "tags.txt")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("View() drifted from testdata/golden/tags.txt; if intentional, regenerate with -update.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDirectGestureCompletesThroughRealInput is the regression test for a gesture that had
// never worked: huh.NewConfirm leaves its keymap zero-valued, and a zero key.Binding matches
// nothing, so a Confirm used standalone (rather than inside a huh.Form, which installs the
// keymap itself) ignored y, n and the arrows alike. The D path could therefore not be
// completed by any real operator — and every test covering it set the bool behind the widget's
// back, which is exactly why the whole package was green while the feature was unreachable
// (Copilot, PR #72).
//
// Driven only through keypresses for that reason: nothing here may touch m.confirmValue.
func TestDirectGestureCompletesThroughRealInput(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	if !m.confirming {
		t.Fatal("D did not open the confirmation")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m2, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("y then enter produced no command: the confirmation never saw the keypress")
	}
	if _, ok := cmd().(DirectRequestedMsg); !ok {
		t.Fatalf("y then enter emitted %T, want DirectRequestedMsg", cmd())
	}
	if m2.confirming {
		t.Error("the confirmation should be closed after enter")
	}

	// The asymmetry that makes the above mean something: answering no must emit nothing, so
	// this cannot pass by the confirmation being bypassed entirely.
	n := readyModel(t, "app-staging", true, false)
	n, _ = n.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	n, _ = n.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if _, cmd := n.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Errorf("answering no must not start a direct promotion, got %v", cmd())
	}
}

// TestStagingNoteDoesNotClaimTheSameBuild: the verdict compares tag strings, because tag
// strings are all StagingMismatch carries. A tag is mutable, so a match means staging
// committed that tag, never that staging ran this build — and the note must not let an
// operator read the stronger claim out of it (Copilot, PR #73).
func TestStagingNoteDoesNotClaimTheSameBuild(t *testing.T) {
	m := readyModel(t, "app-production", true, true)
	for i := 0; i < 2; i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	if m.cursorTag() != "v1" {
		t.Fatalf("fixture precondition: cursor should be on v1, got %q", m.cursorTag())
	}
	v := m.View()
	if !strings.Contains(v, "tags move") {
		t.Errorf("the verdict must say what a tag match does and does not prove:\n%s", v)
	}
}
