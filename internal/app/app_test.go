package app

import (
	"context"
	"errors"
	"flag"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

const (
	fixtureRoot = "../../testdata/repo"
	goldenDir   = "../../testdata/golden"
	width       = 80
	height      = 24
)

// sized returns the root model after Init and the first WindowSizeMsg, as a running
// program would deliver them — no terminal involved. Promotion is the zero value: Start and
// OpenURL both nil, matching a caller that hasn't wired cmd/hoist's real adaptors in yet (see
// TestStartMsgWithNoStartPromotionShowsNotice and TestFlightOpenPRMsgShowsNotice below).
func sized(t *testing.T) tea.Model {
	t.Helper()
	return sizedWithPromotion(t, Promotion{})
}

// sizedWithPromotion is sized's general form, for tests that need a fake Start/OpenURL wired
// in without cmd/hoist's own pkg/git/pkg/forge adaptors (this package must never import
// those — AGENTS.md §4.8).
func sizedWithPromotion(t *testing.T, promo Promotion) tea.Model {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, promo)
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

// TestStartMsgWithNoStartPromotionShowsNotice: a caller that hasn't wired a real
// StartPromotionFunc in (Promotion{} zero value, sized's own default) must show a clear notice
// on confirm rather than pushing a broken flight screen or panicking on a nil call — the same
// nil-adaptor convention plan.ResolveFunc and flight.OpenPRMsg's OpenURL already use.
func TestStartMsgWithNoStartPromotionShowsNotice(t *testing.T) {
	m := sized(t)
	before := len(m.(Model).stack)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("StartMsg with no startPromotion wired produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed from %d to %d screens; an unwired StartMsg must not push", before, n)
	}
	if v := plain(m); !strings.Contains(v, "not wired up") {
		t.Errorf("view missing the not-wired notice:\n%s", v)
	}
}

// TestStartMsgBuildsFlightScreenOnSuccess: plan.StartMsg dispatches the wired
// StartPromotionFunc off the Update call stack (it can talk to a real git remote/forge, so it
// must not run directly inside Update — mirrors plan.ResolveFunc's own loadCmd), and a
// successful promotionBuiltMsg then pushes the flight screen with the real state and
// DriveFunc it returned — no more nil, no more a bare {SourceEnv, TargetEnv}.
func TestStartMsgBuildsFlightScreenOnSuccess(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	called := false
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		if p.SourceEnv != "app-staging" || p.TargetEnv != "app-production" {
			t.Errorf("startPromotion called with unexpected plan: %+v", p)
		}
		return wantState, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	built := cmd()
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	pbm, ok := built.(promotionBuiltMsg)
	if !ok {
		t.Fatalf("command yields %T, want promotionBuiltMsg", built)
	}
	if pbm.err != nil {
		t.Fatalf("unexpected error from a successful startPromotion: %v", pbm.err)
	}
	m, _ = m.Update(pbm)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after a successful promotionBuiltMsg, want 2", n)
	}
	if v := plain(m); !strings.Contains(v, "app-staging -> app-production") || !strings.Contains(v, wantState.ID) {
		t.Errorf("flight screen view missing the real state's envs/id:\n%s", v)
	}
}

// TestStartMsgShowsNoticeOnBuildError: buildPromotionForConfirm's own refusals (a real
// in-flight conflict, missing github config, a claim failure) must surface as a notice on the
// screen that popped up plan.StartMsg (plan, still on top — the flight screen is never
// pushed) rather than crashing.
func TestStartMsgShowsNoticeOnBuildError(t *testing.T) {
	wantErr := errors.New("promotion existing-id targeting app-production is still in flight (at pr-opened: open); run `hoist resume existing-id` instead of starting a second one")
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{}, nil, wantErr
	}}
	m := sizedWithPromotion(t, promo)
	before := len(m.(Model).stack)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	m, _ = m.Update(cmd())
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed from %d to %d screens after a construction error; must stay put", before, n)
	}
	if v := plain(m); !strings.Contains(v, "still in flight") {
		t.Errorf("view missing the construction-error notice:\n%s", v)
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

// TestFlightOpenPRMsgCallsOpenURL: once a real OpenURL is wired in (cmd/hoist's own browser
// opener, in real use), OpenPRMsg must actually call it with the PR's URL instead of showing
// the "not wired yet" notice.
func TestFlightOpenPRMsgCallsOpenURL(t *testing.T) {
	var got string
	promo := Promotion{OpenURL: func(url string) error {
		got = url
		return nil
	}}
	m := sizedWithPromotion(t, promo)
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		t.Error("OpenPRMsg produced a command")
	}
	if got != "https://example.invalid/pr/1" {
		t.Errorf("OpenURL called with %q, want the PR URL", got)
	}
	if v := plain(m); strings.Contains(v, "not wired yet") {
		t.Errorf("view still shows the not-wired notice once OpenURL is wired:\n%s", v)
	}
}

// TestFlightOpenPRMsgShowsErrorFromOpenURL: a real OpenURL that fails (no browser found, the
// operator's platform has none) must surface the error as a notice rather than swallow it.
func TestFlightOpenPRMsgShowsErrorFromOpenURL(t *testing.T) {
	promo := Promotion{OpenURL: func(_ string) error {
		return errors.New("no such browser")
	}}
	m := sizedWithPromotion(t, promo)
	m, _ = m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if v := plain(m); !strings.Contains(v, "no such browser") {
		t.Errorf("view missing the OpenURL error notice:\n%s", v)
	}
}

// TestFlightAbortMsgReturnsToMatrix: AbortMsg's real engine-level semantics (close the PR?
// delete the branch?) are deliberately out of scope for this brief (see app.go's own comment
// on this case) — the one narrow, safe interpretation implemented is a pure navigation
// reset: drop every screen above the matrix, leaving the real branch/PR/state file untouched.
// This pushes matrix -> plan -> flight (three deep) first, specifically to prove AbortMsg
// resets all the way to the matrix rather than popping only the flight screen back to plan.
func TestFlightAbortMsgReturnsToMatrix(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	m, _ = m.Update(cmd())
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("setup: stack has %d screens after opening plan, want 2", n)
	}
	root := m.(Model)
	root = root.push(flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, nil)})
	m = tea.Model(root)
	if n := len(m.(Model).stack); n != 3 {
		t.Fatalf("setup: stack has %d screens after pushing flight, want 3", n)
	}

	m, abortCmd := m.Update(flight.AbortMsg{ID: "abcd1234"})
	if abortCmd != nil {
		t.Error("AbortMsg produced a command")
	}
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("AbortMsg should return all the way to the matrix: stack has %d screens, want 1", n)
	}
	if v := plain(m); strings.Contains(v, "abcd1234") {
		t.Errorf("matrix view should not mention the aborted promotion's id:\n%s", v)
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
