package app

import (
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
// program would deliver them — no terminal involved.
func sized(t *testing.T) tea.Model {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil)
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
// it back.
func TestStartMsgPushesFlightScreen(t *testing.T) {
	m := sized(t)
	msg := plan.StartMsg{Source: "app-staging", Target: "app-production"}
	tm, cmd := m.Update(msg)
	m = tm
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after StartMsg, want 2", n)
	}
	if cmd == nil {
		t.Fatal("StartMsg's push produced no Init command")
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
