package app

import (
	"context"
	"flag"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/registry"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

const (
	fixtureRoot = "../../testdata/repo"
	goldenDir   = "../../testdata/golden"
	width       = 80
	height      = 24
)

// sized returns the root model after Init and the first WindowSizeMsg, as a running
// program would deliver them — no terminal involved.
func sized(t *testing.T) tea.Model {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil)
	_ = m.Init()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return tm
}

func press(t *testing.T, m tea.Model, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(k)
}

func plain(m tea.Model) string { return ansi.Strip(m.View().Content) }

func TestViewSnapshot(t *testing.T) {
	got := plain(sized(t))
	lines := strings.Split(got, "\n")
	if len(lines) != height {
		t.Errorf("view has %d lines, want %d", len(lines), height)
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > width {
			t.Errorf("line %d is %d cells wide, want <= %d: %q", i+1, w, width, l)
		}
	}
	for _, want := range []string{"family", "app-production", "app-staging", "@≠ v202602201200", "!  2 images", "repo  envs 2 · families 4 · unmanaged 2", "? help"} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q", want)
		}
	}
	if strings.Contains(got, string(filepath.Separator)+"testdata") || strings.Contains(got, "..") {
		t.Error("view shows a path, not a base name")
	}
	p := filepath.Join(goldenDir, "matrix.txt")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/app -update)", err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("matrix.txt differs from golden (-want +got):\n%s", diff)
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		_, cmd := press(t, sized(t), k)
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: command yields %T, want tea.QuitMsg", k, cmd())
		}
	}
}

func TestMovementKeys(t *testing.T) {
	m := sized(t)
	cursor := func() int { return m.(Model).stack[0].(matrixScreen).Cursor() }
	if cursor() != 0 {
		t.Fatalf("initial cursor %d", cursor())
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if cursor() != 2 {
		t.Errorf("after j, down: cursor %d, want 2", cursor())
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m, _ = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if cursor() != 0 {
		t.Errorf("after k, up, up: cursor %d, want 0 (clamped)", cursor())
	}
}

func TestHelpToggleKeepsHeight(t *testing.T) {
	m := sized(t)
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	v := plain(m)
	if n := len(strings.Split(v, "\n")); n != height {
		t.Errorf("with help: %d lines, want %d", n, height)
	}
	if !strings.Contains(v, "plan promotion") {
		t.Errorf("help line missing:\n%s", v)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if v := plain(m); strings.Contains(v, "plan promotion") {
		t.Error("help line still shown after second ?")
	}
}

// TestPromotePushesPlanScreen is the second half of issue #2: p on the matrix screen opens
// the plan screen (internal/app/plan) rather than M1's placeholder notice. The fixture repo
// has no configured envs.pairs, so the plan screen starts in its env-select state, prompting
// for a target among the repo's other envs.
func TestPromotePushesPlanScreen(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenPlanMsg); !ok {
		t.Fatalf("p's command yields %T, want matrix.OpenPlanMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after p, want 2", n)
	}
	if v := plain(m); !strings.Contains(v, "app-production") || !strings.Contains(v, "app-staging") {
		t.Errorf("plan screen view missing the fixture's env names:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the plan screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// TestStartMsgPushesFlightScreen: plan.StartMsg (emitted when the operator confirms a plan)
// pushes internal/app/flight on top of whatever screen sent it. internal/app has no
// repoFullName, CI/approval policy, or git.Git/forge.Forge adaptor to build a real
// engine.PromotionState or flight.DriveFunc from yet (see app.go's own comment on this
// handler) — this only proves the navigation wiring: the flight screen renders read-only
// (every step not-yet-reached, R shows the read-only notice) and esc (flight.BackMsg) pops
// it back. The pushed screen's Init() is nil here: a nil driveFn (this stub's own shape)
// means there is nothing to animate or observe, so Init correctly returns nil rather than
// starting a spinner tick chain with nothing ever busy to render it (PR #39 review finding
// #5) — this is not "the push produced no command", it's the read-only screen correctly
// having nothing to do at start.
func TestStartMsgPushesFlightScreen(t *testing.T) {
	m := sized(t)
	msg := plan.StartMsg{Source: "app-staging", Target: "app-production"}
	tm, cmd := m.Update(msg)
	m = tm
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2", n)
	}
	if cmd != nil {
		t.Errorf("StartMsg's push produced a command for a read-only (nil driveFn) flight screen, want nil: %#v", cmd())
	}
	if v := plain(m); !strings.Contains(v, "app-staging -> app-production") {
		t.Errorf("flight screen view missing the envs:\n%s", v)
	}

	m, rCmd := m.Update(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if rCmd != nil {
		t.Error("R on a read-only (nil driveFn) flight screen produced a command")
	}
	if v := plain(m); !strings.Contains(v, "nothing to re-observe") {
		t.Errorf("flight screen missing the read-only notice after R:\n%s", v)
	}

	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the flight screen produced no command")
	}
	if _, ok := backCmd().(flight.BackMsg); !ok {
		t.Fatalf("esc's command yields %T, want flight.BackMsg", backCmd())
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop the flight screen: stack has %d screens", n)
	}
}

// TestFlightOpenPRMsgShowsNotice: PR #39 review finding #1 — the root previously dropped
// flight.OpenPRMsg silently (no case in Update at all), so pressing o did nothing visible
// once cmd/hoist eventually wires a real DriveFunc in. Until a real URL-opener is wired in,
// the root must show a visible notice naming the URL instead of a silent no-op.
func TestFlightOpenPRMsgShowsNotice(t *testing.T) {
	m := sized(t)
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		t.Error("OpenPRMsg produced a command")
	}
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the open-PR notice:\n%s", v)
	}
}

// TestFlightAbortMsgShowsNotice mirrors TestFlightOpenPRMsgShowsNotice for x/AbortMsg.
func TestFlightAbortMsgShowsNotice(t *testing.T) {
	m := sized(t)
	m, cmd := m.Update(flight.AbortMsg{ID: "abcd1234"})
	if cmd != nil {
		t.Error("AbortMsg produced a command")
	}
	if v := plain(m); !strings.Contains(v, "abcd1234") {
		t.Errorf("view missing the abort notice:\n%s", v)
	}
}

// TestRootNoticeClearsOnNextKeypress: the root's own notice is transient, same convention as
// every screen's own notice field — it should not linger forever once the operator moves on.
func TestRootNoticeClearsOnNextKeypress(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if !strings.Contains(plain(m), "not wired yet") {
		t.Fatal("setup: notice not shown after OpenPRMsg")
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if strings.Contains(plain(m), "not wired yet") {
		t.Error("root notice still shown after a later keypress")
	}
}

// TestDeployNewPushesTagsScreen is M6's tag-picker counterpart to TestPromotePushesPlanScreen:
// d on the matrix screen pushes internal/app/tags on top, and esc from there pops back.
func TestDeployNewPushesTagsScreen(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenTagsMsg); !ok {
		t.Fatalf("d's command yields %T, want matrix.OpenTagsMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after d, want 2", n)
	}
	// No tagsFn was supplied (sized(t) passes nil), so the picker's own error state shows
	// rather than hanging — proving the nil case is handled, not just the happy path.
	if v := plain(m); !strings.Contains(v, "hoist tags:") {
		t.Errorf("tags screen view missing its own header:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the tags screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// drainTags runs a tea.Cmd (and every cmd it in turn produces) to completion, exactly as a
// real tea.Program would deliver messages to the whole app model — needed here because the
// tags screen's Init kicks off an async ListFunc/MetaFunc load (internal/app/tags.Model.Init),
// which TestDeployNewPushesTagsScreen never exercises (its nil tagsFn errors synchronously).
// Mirrors internal/app/tags/model_test.go's own drain helper, one level up the screen stack.
func drainTags(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
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
			continue
		}
		if _, isTick := msg.(spinner.TickMsg); isTick {
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
}

// TestDeployNewThreadsRealStagingTag is the regression test for AGENTS.md invariant 4 / M6's
// production/staging tag-mismatch warning, guarding against the exact bug a sibling M6 attempt
// shipped: its matrix.OpenTagsMsg handler called tags.New with the paired staging env's
// currently-running tag hardcoded to "", so the mismatch note could never render from the real
// running app (only from a unit test calling tags.New directly). This test drives the real
// app-wiring path — app.Model.Update's own matrix.OpenTagsMsg case, not a direct tags.New call
// — with a config naming app-staging as app-production's pair, against the fixture repo where
// app-production/web and app-staging/web genuinely carry different tags
// (v202601010101/v202602150930). If a future change reintroduces a hardcoded "" (or otherwise
// drops the real lookup), hasStagingMismatch/stagingTag never reach tags.New with real values,
// the note never renders, and this test fails.
func TestDeployNewThreadsRealStagingTag(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	tagsFn := func(string) (bool, tags.ListFunc, tags.MetaFunc) {
		listFn := func(context.Context) ([]string, []forge.GitTag, error) {
			return []string{"v202601010101"}, nil, nil
		}
		metaFn := func(_ context.Context, _ string) (registry.ImageMeta, error) {
			return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("a", 64)}, nil
		}
		return false, listFn, metaFn
	}
	m := New(r, []string{"ghcr.io/"}, envs, nil, tagsFn)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	msg := matrix.OpenTagsMsg{ImageRepo: "ghcr.io/example/web", Target: "app-production"}
	tm, cmd := tm.Update(msg)
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after OpenTagsMsg, want 2", n)
	}
	tm = drainTags(tm, cmd)

	v := plain(tm)
	if !strings.Contains(v, "app-staging") || !strings.Contains(v, "v202602150930") {
		t.Errorf("tags screen view is missing the real staging-mismatch note (want app-staging / v202602150930, the fixture's actual app-staging/web tag):\n%s", v)
	}
}

func TestBackgroundColorRethemes(t *testing.T) {
	m := sized(t)
	if !m.(Model).styles.Dark {
		t.Fatal("default theme is not dark")
	}
	m, _ = m.Update(tea.BackgroundColorMsg{Color: color.White})
	if m.(Model).styles.Dark {
		t.Error("theme did not follow a light background")
	}
	if got := plain(m); !strings.Contains(got, "envs 2") {
		t.Error("view broke after retheme")
	}
	m, _ = m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if !m.(Model).styles.Dark {
		t.Error("theme did not follow a dark background")
	}
}

func TestViewUsesAltScreen(t *testing.T) {
	if !sized(t).View().AltScreen {
		t.Error("view is not in the alternate screen")
	}
}
