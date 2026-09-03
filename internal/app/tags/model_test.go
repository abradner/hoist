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
	m := New("ghcr.io/example/app", target, mapped, production, "app-staging", "v1", target == "app-production", listFn, fixedMetas(metas))
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
	m := New("ghcr.io/example/app", "app-staging", true /* config says mapped */, false, "", "", false, listFn, fixedMetas(metas))
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
	m := New("ghcr.io/example/app", "app-staging", true, false, "", "", false, listFn, fixedMetas(metas))
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

	m.confirmValue = true
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

// TestDirectConfirmDeclineEmitsNothing: accepting the dialog with confirmValue false (the
// operator declined) must not emit DirectRequestedMsg.
func TestDirectConfirmDeclineEmitsNothing(t *testing.T) {
	m := readyModel(t, "app-staging", true, false)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'D', Text: "D"})
	m.confirmValue = false
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("declining should emit nothing, got %v", cmd())
	}
	if m.confirming {
		t.Fatal("confirming should be cleared after enter, decline or not")
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
	left := New("ghcr.io/example/other", "app-staging", false, false, "", "", false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) { return []string{"stale"}, nil, false, nil },
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
	first := New(repo, "app-staging", true, false, "", "", false,
		func(context.Context) ([]string, []forge.GitTag, bool, error) { return []string{"stale-from-first"}, nil, false, nil },
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
	first := New(repo, "app-staging", true, false, "", "", false,
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
	m := New("ghcr.io/example/app", "app-staging", false, false, "", "", false,
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
