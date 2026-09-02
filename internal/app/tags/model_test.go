package tags

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
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
	listFn := func(context.Context) ([]string, []forge.GitTag, error) {
		if !mapped {
			return regTags, nil, nil
		}
		return regTags, gitTags, nil
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

func TestListErrorRendersInsteadOfHanging(t *testing.T) {
	m := New("ghcr.io/example/app", "app-staging", false, false, "", "", false,
		func(context.Context) ([]string, []forge.GitTag, error) {
			return nil, nil, errors.New("registry unreachable")
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
