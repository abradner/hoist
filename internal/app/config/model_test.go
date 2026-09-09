package config

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
)

func update(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) }

// fixtureText is what `hoist config show` prints for a small placeholder config (AGENTS.md
// §4.4: my-cluster, me/my-gitops), already marshalled and redacted — the screen takes the
// string, never the loader, so the test hands it the same way cmd/hoist does.
const fixtureText = `repos:
    - name: my-gitops
      path: ~/src/my-gitops
      apps_root: cluster/apps
      github: me/my-gitops
      kube:
        context: my-cluster
      envs:
        production:
            - app-production
        pairs:
            app-staging: app-production
registries:
    - prefix: ghcr.io/me/
      auth:
        - env
        - keychain
      op: <redacted>
poll:
    ci: 20s
    approval: 30s
    deadline: 4h0m0s
`

func newFixture() Model {
	return New("/home/me/.config/hoist/config.yaml", true, fixtureText).SetStyles(ui.NewStyles(true))
}

func TestViewGolden(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := newFixture().SetSize(size[0], size[1])
		uitest.Golden(t, "config", m.View(), size[0], size[1])
	}
	// Flags only: the title carries the CLI's own "no file at …; showing defaults" sentence.
	m := New("/home/me/.config/hoist/config.yaml", false, "poll:\n    ci: 20s\n").SetStyles(ui.NewStyles(true)).SetSize(80, 24)
	uitest.Golden(t, "config-nofile", m.View(), 80, 24)
}

func TestEscEmitsBackMsg(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	_, cmd := m.Update(uitest.Key("esc"))
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
}

// A config longer than the terminal scrolls: pgdown moves the viewport past the first page,
// G reaches the last line, g returns to the top. Driven by real keypresses (AGENTS.md §4.8).
func TestPageDownScrollsALongConfig(t *testing.T) {
	var b strings.Builder
	b.WriteString("registries:\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "    - prefix: ghcr.io/me/line-%02d/\n", i)
	}
	m := New("/home/me/.config/hoist/config.yaml", true, b.String()).SetStyles(ui.NewStyles(true)).SetSize(80, 24)
	if v := m.View(); !strings.Contains(v, "line-00") || strings.Contains(v, "line-59") {
		t.Fatalf("first page should show the top, not the end:\n%s", v)
	}
	m = uitest.Keys(m, update, "pgdown")
	if v := m.View(); strings.Contains(v, "line-00") || !strings.Contains(v, "line-20") {
		t.Fatalf("pgdown did not move past the first page:\n%s", v)
	}
	m = uitest.Keys(m, update, "G")
	if v := m.View(); !strings.Contains(v, "line-59") {
		t.Fatalf("G did not reach the end:\n%s", v)
	}
	m = uitest.Keys(m, update, "g")
	if v := m.View(); !strings.Contains(v, "registries:") {
		t.Fatalf("g did not return to the top:\n%s", v)
	}
}
