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
	m := New(r, []string{"ghcr.io/"})
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
	if !strings.Contains(v, "promote (later milestone)") {
		t.Errorf("help line missing:\n%s", v)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: '?', Text: "?"})
	if v := plain(m); strings.Contains(v, "promote (later milestone)") {
		t.Error("help line still shown after second ?")
	}
}

func TestPromoteShowsNoticeUntilNextKey(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd != nil {
		t.Error("p produced a command")
	}
	if v := plain(m); !strings.Contains(v, "promotion lands in a later milestone") {
		t.Errorf("notice missing:\n%s", v)
	}
	m, _ = press(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if v := plain(m); strings.Contains(v, "later milestone") || !strings.Contains(v, "envs 2") {
		t.Errorf("notice did not clear:\n%s", v)
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
