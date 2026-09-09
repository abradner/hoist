// Package uitest is the one harness every screen's tests render through (AGENTS.md §4.8,
// M10): a golden comparison at a stated terminal size that also asserts the shape a
// terminal would actually show — exactly height lines, none wider than width — and a way
// to drive a screen with real keypresses, so a test can never pass by setting the field a
// key would have set. Golden files live in testdata/golden/<name>-<w>x<h>.txt at the repo
// root and regenerate with `go test ./internal/... -update`; every screen renders at least
// 80×24 and 120×40 (the narrow terminal and the wide one), since a layout that only works at
// one size is the defect the redesign exists to end.
package uitest

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"
)

// Update is the shared -update flag: one for every package that imports this one, rather
// than a copy per package (three existed before M10).
var Update = flag.Bool("update", false, "rewrite golden files under testdata/golden")

// Golden compares view, ANSI-stripped, with testdata/golden/<name>-<w>x<h>.txt, after
// checking the shape: exactly height lines and every line at most width cells. The shape
// checks run even with -update, so a golden can never record a view that would have
// scrolled or wrapped a real terminal.
func Golden(t *testing.T, name, view string, width, height int) {
	t.Helper()
	plain := ansi.Strip(view)
	lines := strings.Split(plain, "\n")
	if len(lines) != height {
		t.Errorf("%s at %dx%d: %d lines, want exactly %d", name, width, height, len(lines), height)
	}
	for i, line := range lines {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("%s at %dx%d: line %d is %d cells wide, over %d:\n%s", name, width, height, i+1, w, width, line)
		}
	}
	path := filepath.Join(goldenDir(t), fmt.Sprintf("%s-%dx%d.txt", name, width, height))
	if *Update {
		if err := os.WriteFile(path, []byte(plain), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if diff := cmp.Diff(string(want), plain); diff != "" {
		t.Errorf("%s (-want +got):\n%s", path, diff)
	}
}

// goldenDir finds testdata/golden by walking up from the package directory to go.mod, so
// a test needs no ../../../ arithmetic.
func goldenDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "testdata", "golden")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// UpdateFunc is a screen's Update: the concrete-typed shape every screen package exposes.
type UpdateFunc[M any] func(M, tea.Msg) (M, tea.Cmd)

// Drain runs cmd and feeds every message it produces back through update, recursively,
// until no command is left — the way a test settles a screen after Init or a keypress.
// tea.BatchMsg is unpacked in order; spinner ticks and cursor blinks are dropped, since a
// spinner — and a focused text input's cursor (#102's o dialog) — reschedules itself forever
// and a test never wants to wait on one.
func Drain[M any](m M, cmd tea.Cmd, update UpdateFunc[M]) M {
	if cmd == nil {
		return m
	}
	msg := cmd()
	switch v := msg.(type) {
	case nil:
		return m
	case tea.BatchMsg:
		for _, c := range v {
			m = Drain(m, c, update)
		}
		return m
	case spinner.TickMsg, cursor.BlinkMsg:
		return m
	}
	var next tea.Cmd
	m, next = update(m, msg)
	return Drain(m, next, update)
}

// Keys presses each key in turn, draining every command a press produces before the next.
// A key is a single character ("y", "d", "/") or one of the names Key knows.
func Keys[M any](m M, update UpdateFunc[M], keys ...string) M {
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = update(m, Key(k))
		m = Drain(m, cmd, update)
	}
	return m
}

// Key builds the tea.KeyPressMsg a terminal would send for k: a single character is itself,
// with Text set the way a real press carries it; the names below are the special keys.
func Key(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "f5":
		return tea.KeyPressMsg{Code: tea.KeyF5}
	case "ctrl+r":
		return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	if len(r) != 1 {
		panic(fmt.Sprintf("uitest.Key: %q is neither one character nor a known key name", k))
	}
	return tea.KeyPressMsg{Code: r[0], Text: k}
}
