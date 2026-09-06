package matrix

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

func update(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) }

func newFixture() Model { return New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil) }

// TestColumnCursor exercises Left/Right moving CurrentEnv over fixture()'s three envs
// (a, b, c — sorted), clamped at both ends.
func TestColumnCursor(t *testing.T) {
	m := newFixture()
	if got := m.CurrentEnv(); got != "a" {
		t.Fatalf("initial CurrentEnv() = %q, want a", got)
	}
	m = uitest.Keys(m, update, "left")
	if got := m.CurrentEnv(); got != "a" {
		t.Errorf("Left at the first column: CurrentEnv() = %q, want a (clamped)", got)
	}
	m = uitest.Keys(m, update, "right")
	if got := m.CurrentEnv(); got != "b" {
		t.Errorf("after Right: CurrentEnv() = %q, want b", got)
	}
	m = uitest.Keys(m, update, "right", "right")
	if got := m.CurrentEnv(); got != "c" {
		t.Errorf("Right past the last column: CurrentEnv() = %q, want c (clamped)", got)
	}
	m = uitest.Keys(m, update, "left")
	if got := m.CurrentEnv(); got != "b" {
		t.Errorf("after Left: CurrentEnv() = %q, want b", got)
	}
}

func emitted(t *testing.T, m Model, k string) tea.Msg {
	t.Helper()
	_, cmd := m.Update(uitest.Key(k))
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestPromoteOpensPlanForCurrentEnv(t *testing.T) {
	m := uitest.Keys(newFixture(), update, "right")
	if msg, ok := emitted(t, m, "p").(OpenPlanMsg); !ok || msg.Source != "b" || msg.Force {
		t.Fatalf("p emitted %+v", msg)
	}
	if msg, ok := emitted(t, m, "P").(OpenPlanMsg); !ok || msg.Source != "b" || !msg.Force {
		t.Fatalf("P emitted %+v", msg)
	}
}

func TestDeployNewOpensTagsForCurrentCell(t *testing.T) {
	// Default cursor: row 0 (families sort to "absent" first), col 0 (envs sort to "a" first).
	msg, ok := emitted(t, newFixture(), "d").(OpenTagsMsg)
	if !ok || msg.Target != "a" || msg.ImageRepo != "ghcr.io/x/app" {
		t.Fatalf("OpenTagsMsg = %+v, want {ImageRepo: ghcr.io/x/app, Target: a}", msg)
	}
}

// The "thirdparty" family has no first-party image at all — d must not emit
// OpenTagsMsg{ImageRepo: ""}, and the reason lands in the notes section, not a clipped bar.
func TestDeployNewWithNoFirstPartyImageShowsNotice(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	// absent, drift, empty, mixedtags, multi, pinned, sidecar, thirdparty — 7 rows down.
	m = uitest.Keys(m, update, "down", "down", "down", "down", "down", "down", "down")
	m2, cmd := m.Update(uitest.Key("d"))
	if cmd != nil {
		t.Fatalf("d on a third-party-only cell produced a command: %v", cmd())
	}
	if !strings.Contains(m2.View(), "no first-party image") {
		t.Errorf("View() lacks the notice:\n%s", m2.View())
	}
}

// A family with several first-party images asks which one (#85: d used to pick the first
// sorted one silently). The chooser is driven by keys; the answer is read from the widget.
func TestDeployNewWithSeveralImagesOpensAChooser(t *testing.T) {
	m := newFixture().SetSize(100, 30)
	// absent, drift, empty, mixedtags, multi — 4 rows down, env a.
	m = uitest.Keys(m, update, "down", "down", "down", "down")
	m, cmd := m.Update(uitest.Key("d"))
	if m.chooser == nil || !m.CapturesText() {
		t.Fatal("d on a two-image cell must open the chooser")
	}
	m = uitest.Drain(m, cmd, update)
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "deploy which image in a?") || !strings.Contains(view, "ghcr.io/x/worker") {
		t.Fatalf("chooser not drawn:\n%s", view)
	}
	if !strings.Contains(view, "FAMILY") {
		t.Fatalf("the matrix must stay visible under the dialog:\n%s", view)
	}
	// Down to the second option, enter: the choice, not the first sorted repo.
	m = uitest.Keys(m, update, "down")
	m, cmd = m.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter in the chooser emitted nothing")
	}
	msg, ok := cmd().(OpenTagsMsg)
	if !ok || msg.ImageRepo != "ghcr.io/x/worker" || msg.Target != "a" {
		t.Fatalf("chose %+v, want ghcr.io/x/worker in a", msg)
	}
	if m.chooser != nil || m.CapturesText() {
		t.Fatal("the chooser must close on enter")
	}
	// esc backs out with nothing emitted.
	m, _ = m.Update(uitest.Key("d"))
	m, cmd = m.Update(uitest.Key("esc"))
	if cmd != nil || m.chooser != nil {
		t.Fatalf("esc must close the chooser silently (cmd=%v)", cmd)
	}
}

func TestCurrentEnvEmptyRepo(t *testing.T) {
	m := New(&gitops.Repo{Root: "r", Envs: map[string]*gitops.Env{}}, []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
	if got := m.CurrentEnv(); got != "" {
		t.Fatalf("CurrentEnv() = %q, want empty", got)
	}
	if got := emitted(t, m, "p"); got != nil {
		t.Fatalf("p with no envs emitted %+v", got)
	}
	m, _ = m.Update(uitest.Key("p"))
	if !strings.Contains(m.View(), "no environments discovered") {
		t.Errorf("View() lacks the notice:\n%s", m.View())
	}
	uitest.Golden(t, "matrix-empty", m.View(), 80, 24)
}

func TestRestartKeyEmitsOpenRestartMsg(t *testing.T) {
	m := uitest.Keys(newFixture(), update, "right", "down")
	msg, ok := emitted(t, m, "R").(OpenRestartMsg)
	if !ok || msg.Family != "drift" || msg.Target != "b" {
		t.Fatalf("R emitted %+v, want {Family: drift, Target: b}", msg)
	}
	if got := emitted(t, newFixture(), "r"); got != nil {
		t.Fatalf("lower-case r must not restart; emitted %+v", got)
	}
}

func TestSelectedEnvIsVisible(t *testing.T) {
	m := newFixture().SetSize(120, 20)
	v := ansi.Strip(m.View())
	if !strings.Contains(v, selectedMarker+"A") || !strings.Contains(v, "env a") {
		t.Errorf("the selected env should be marked in the header and named in the footer:\n%s", v)
	}
	m = uitest.Keys(m, update, "right")
	v = ansi.Strip(m.View())
	if !strings.Contains(v, selectedMarker+"B") || strings.Contains(v, selectedMarker+"A") || !strings.Contains(v, "env b") {
		t.Errorf("the marker should follow the cursor to b:\n%s", v)
	}
}

// #86: the attacker is an operator with the cursor on a production column about to press
// d. The header, the footer and the notes must all say so; a non-production column says
// nothing of the kind.
func TestProductionColumnIsMarkedEverywhere(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"b"}}
	m := New(fixture(), []string{"ghcr.io/"}, envs, nil).SetSize(120, 24)
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "B"+productionMarker) {
		t.Errorf("header lacks the production marker on b:\n%s", v)
	}
	if strings.Contains(v, "A"+productionMarker) || strings.Contains(v, "C"+productionMarker) {
		t.Errorf("a non-production column carries the marker:\n%s", v)
	}
	if strings.Contains(v, "(production)") || strings.Contains(v, "is a production env") {
		t.Errorf("cursor on a: nothing should say production yet:\n%s", v)
	}
	m = uitest.Keys(m, update, "right")
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "env b (production)") {
		t.Errorf("footer lacks the production word with the cursor on b:\n%s", v)
	}
	if !strings.Contains(v, "⚠ b is a production env: writes there always open a PR") {
		t.Errorf("notes lack the production sentence:\n%s", v)
	}
}

// Drift: the cells say "resolving…" until the cluster answers, then the answer's word; a
// stale generation's answer is dropped; F5 asks again; a cluster error is a sentence.
func TestDriftAnswersRefineTheColumn(t *testing.T) {
	asked := map[string]int{}
	drift := func(_ context.Context, env string) (map[string]image.Ref, error) {
		asked[env]++
		switch env {
		case "b":
			return map[string]image.Ref{"ghcr.io/x/app": {Repo: "ghcr.io/x/app", Tag: "v7", Digest: digestC}}, nil
		case "c":
			return nil, errors.New("kube context \"my-cluster\" is not in the kubeconfig")
		}
		return map[string]image.Ref{}, nil
	}
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, drift).SetSize(120, 24)
	before := ansi.Strip(m.View())
	if !strings.Contains(before, "resolving…") || strings.Contains(before, "drifted") {
		t.Fatalf("before any answer the pinned cells say resolving:\n%s", before)
	}
	m = uitest.Drain(m, m.Init(), update)
	if asked["a"] != 1 || asked["b"] != 1 || asked["c"] != 1 {
		t.Fatalf("asked = %v; want every env once", asked)
	}
	// Row "pinned" (5 down), col b.
	m = uitest.Keys(m, update, "right", "down", "down", "down", "down", "down")
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "v1  drifted") && !strings.Contains(v, "drifted") {
		t.Fatalf("pinned/b must be drifted after b's answer:\n%s", v)
	}
	if !strings.Contains(v, "! pinned runs v7 in b; manifest says v1") {
		t.Fatalf("the drift sentence is missing:\n%s", v)
	}
	if strings.Contains(v, "resolving…") {
		t.Fatalf("every env has answered; nothing should still be resolving:\n%s", v)
	}
	// Column c: the cluster could not be asked, and the reason is on screen, not clipped.
	m = uitest.Keys(m, update, "right")
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "! c: cluster not asked: kube context \"my-cluster\" is not in the kubeconfig") {
		t.Fatalf("cluster error not shown for c:\n%s", v)
	}
	// A stale answer (an earlier generation) is dropped.
	stale := driftMsg{gen: m.gen - 1, env: "a", running: map[string]image.Ref{"ghcr.io/x/app": {Repo: "ghcr.io/x/app", Tag: "v99", Digest: digestC}}}
	m2, _ := m.Update(stale)
	if strings.Contains(ansi.Strip(m2.View()), "v99") {
		t.Fatal("a stale generation's answer was applied")
	}
	// F5 asks every env again, marking them resolving until they answer.
	m, cmd := m.Update(uitest.Key("f5"))
	if !m.pending["a"] || cmd == nil {
		t.Fatal("F5 must re-ask the cluster")
	}
	m = uitest.Drain(m, cmd, update)
	if asked["a"] != 2 {
		t.Fatalf("asked = %v after F5", asked)
	}
	uitest.Golden(t, "matrix-drift", m.View(), 120, 24)
}

// A long notice wraps inside the frame instead of being clipped at the first clause (#85).
func TestLongNoticeWrapsInsideTheFrame(t *testing.T) {
	notice := "cannot deploy ghcr.io/x/app:v3 to a: env: GHCR_TOKEN not set; keychain: no credential for ghcr.io; cluster: not configured (registries[].cluster); op: not configured (registries[].op)"
	m := newFixture().SetSize(80, 24).WithNotice(notice)
	v := ansi.Strip(m.View())
	for _, want := range []string{"GHCR_TOKEN not set", "registries[].op"} {
		if !strings.Contains(v, want) {
			t.Fatalf("notice clause %q lost:\n%s", want, v)
		}
	}
	uitest.Golden(t, "matrix-notice", m.View(), 80, 24)
}

func TestViewGolden(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"c"}}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := New(fixture(), []string{"ghcr.io/"}, envs, nil).SetStyles(ui.NewStyles(true)).SetSize(size[0], size[1])
		m = uitest.Keys(m, update, "right", "right", "down")
		uitest.Golden(t, "matrix", m.View(), size[0], size[1])
	}
	m := New(fixture(), []string{"ghcr.io/"}, envs, nil).SetSize(80, 24)
	m = uitest.Keys(m, update, "?")
	uitest.Golden(t, "matrix-help", m.View(), 80, 24)
}
