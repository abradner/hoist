package plan

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

const goldenDir = "../../../testdata/golden"

func discoverFixture(t *testing.T) *gitops.Repo {
	t.Helper()
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runInit drives Init()'s batched cmd (spinner tick, then the load) through Update in
// order, exactly as internal/app's root would deliver them one at a time — no
// tea.Program involved.
func runInit(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		return m
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		m, _ = m.Update(msg)
		return m
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		m, _ = m.Update(c())
	}
	return m
}

// readyModel builds a plan screen for source/target and drives it straight to stateReady
// with no resolution ("digest sources: none" — resolveFn nil), sized for a terminal.
func readyModel(t *testing.T, envs config.EnvsConfig) Model {
	t.Helper()
	r := discoverFixture(t)
	m := New(r, []string{"ghcr.io/"}, envs, "app-staging", "app-production", false, nil)
	m = runInit(t, m)
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady", m.state)
	}
	m = m.SetSize(100, 30)
	m = m.SetStyles(ui.NewStyles(true))
	return m
}

// TestAsyncLoad drives Init()'s two batched cmds one at a time: the spinner tick lands
// first and must not disturb the loading state or the status line; the load cmd (a fake
// ResolveFunc, deterministic — no real cluster/registry) lands second and produces rows.
func TestAsyncLoad(t *testing.T) {
	r := discoverFixture(t)
	calls := 0
	fake := ResolveFunc(func(_ context.Context, _ *gitops.Repo, source string) (ResolveOutcome, error) {
		calls++
		if source != "app-staging" {
			t.Errorf("resolveFn source = %q, want app-staging", source)
		}
		return ResolveOutcome{KubeContext: "test-context", RegistryAuth: "env"}, nil
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake)
	if m.state != stateLoading {
		t.Fatalf("state = %v, want stateLoading", m.state)
	}

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init produced no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("Init's command = %#v, want a 2-command batch (spinner tick, load)", cmd())
	}

	// First Update: the spinner tick. resolveFn has not run yet — driving cmds one at a
	// time, like the real root does, is exactly what keeps this deterministic.
	m, _ = m.Update(batch[0]())
	if calls != 0 {
		t.Fatalf("resolveFn ran before its own cmd landed (calls=%d)", calls)
	}
	if m.state != stateLoading {
		t.Errorf("state after the spinner tick = %v, want stateLoading", m.state)
	}
	if v := m.viewLoading(); !strings.Contains(v, "resolving digests from app-staging pods") {
		t.Errorf("loading view missing the status line:\n%s", v)
	}

	// Second Update: the load cmd completes.
	m, _ = m.Update(batch[1]())
	if calls != 1 {
		t.Fatalf("resolveFn called %d times, want 1", calls)
	}
	if m.state != stateReady {
		t.Fatalf("state after load = %v, want stateReady", m.state)
	}
	if len(m.rows) == 0 {
		t.Error("want rows after load")
	}
	if m.outcome.KubeContext != "test-context" {
		t.Errorf("outcome.KubeContext = %q, want test-context", m.outcome.KubeContext)
	}
}

// TestResolveErrorDegradesToWarning: a ResolveFunc error must not fail the screen — it
// plans from the manifests alone and reports the failure as a warning (AGENTS.md
// principle 5), matching "digest sources: none" mode's own graceful path.
func TestResolveErrorDegradesToWarning(t *testing.T) {
	r := discoverFixture(t)
	fake := ResolveFunc(func(context.Context, *gitops.Repo, string) (ResolveOutcome, error) {
		return ResolveOutcome{}, errCannotReachCluster
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake)
	m = runInit(t, m)
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady even when resolveFn errors", m.state)
	}
	if m.err != nil {
		t.Fatalf("m.err = %v, want nil (a resolve error degrades, it does not fail the screen)", m.err)
	}
	if len(m.rows) == 0 {
		t.Error("want rows planned from the manifests alone")
	}
	found := false
	for _, w := range m.plan.Warnings {
		if strings.Contains(w.Message, errCannotReachCluster.Error()) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings missing the resolve error: %+v", m.plan.Warnings)
	}
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const errCannotReachCluster = sentinelErr("cluster unreachable: dial tcp: no route to host")

// TestModeToggleAfterConfirm: m on a non-production target opens the confirm dialog; enter
// commits the operator's choice.
func TestModeToggleAfterConfirm(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	if m.mode != ModePR {
		t.Fatalf("initial mode = %q, want pr", m.mode)
	}
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	if cmd != nil {
		t.Error("m produced a command")
	}
	if !m.confirming {
		t.Fatal("m did not open the confirm dialog for a non-production target")
	}
	// Simulate the operator landing on "yes": confirmValue is the variable
	// huh.Confirm.Value bound at buildConfirm, so setting it here is the same thing a
	// left/right keypress inside the dialog would do to it.
	m.confirmValue = true
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.confirming {
		t.Error("confirm dialog still open after enter")
	}
	if m.mode != ModeDirect {
		t.Errorf("mode = %q, want direct", m.mode)
	}
}

// TestModeBlockedForProduction: m on a production target never opens the confirm dialog;
// the bar explains why (AGENTS.md §4.5 — direct mode is not offered at all).
func TestModeBlockedForProduction(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"app-production"}}
	m := readyModel(t, envs)
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	if cmd != nil {
		t.Error("m produced a command")
	}
	if m.confirming {
		t.Error("m opened the confirm dialog for a production target")
	}
	if !strings.Contains(m.notice, "production") {
		t.Errorf("notice = %q, want an explanation naming production", m.notice)
	}
	if m.mode != ModePR {
		t.Errorf("mode changed to %q despite being blocked", m.mode)
	}
	if !strings.Contains(m.modeLabel(), "production") {
		t.Errorf("bar = %q, want it to explain why", m.modeLabel())
	}
}

// TestEscReturnsToMatrix: esc emits BackMsg from the ready state, for the root to pop.
func TestEscReturnsToMatrix(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Errorf("esc's command yields %T, want BackMsg", cmd())
	}
}

// TestSkipStagingWarning: promoting straight to a production env that is not the source's
// configured pair shows the warning, never blocks (AGENTS.md §4.5).
func TestSkipStagingWarning(t *testing.T) {
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	// app-production *is* app-staging's configured pair: no warning.
	m := readyModel(t, envs)
	if got := m.skipNotice(); got != "" {
		t.Errorf("configured pair produced a skip warning: %q", got)
	}
}

// TestViewGolden snapshots the ready screen at 100×30 over testdata/repo, resolveFn nil
// ("digest sources: none") so the run is fully deterministic. Regenerate with:
// mise exec -- go test ./internal/app/plan -update
func TestViewGolden(t *testing.T) {
	envs := config.EnvsConfig{Pairs: map[string]string{"app-staging": "app-production"}}
	m := readyModel(t, envs)
	got := ansi.Strip(m.View())

	for _, want := range []string{
		"hoist plan: app-staging -> app-production",
		"mode: PR",
		"ghcr.io/example/counta",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view lacks %q:\n%s", want, got)
		}
	}
	// The right pane is a scrolling viewport at this terminal size, so its tail (Untouched,
	// Warnings, Resolution) is below the fold in View() itself; check the full body text
	// that feeds the viewport instead of what happens to be visible.
	body := m.rightBody()
	for _, want := range []string{"Untouched (", "Warnings (", "Resolution:", "cluster not consulted"} {
		if !strings.Contains(body, want) {
			t.Errorf("right pane body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(got, string(filepath.Separator)+"testdata") {
		t.Error("view shows a path, not a base name")
	}

	p := filepath.Join(goldenDir, "plan.txt")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/app/plan -update)", err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("plan.txt differs from golden (-want +got):\n%s", diff)
	}
}
