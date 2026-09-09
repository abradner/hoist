package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/resolve"
)

// overrideFixture is a plan screen loaded through a fake ResolveFunc that records the
// overrides it was handed and answers as pkg/resolve would for them — [override],
// "caller-supplied digest" — so a test can prove the o dialog's override reached the
// resolver (the map itself) and came back as the row's provenance (#102). The recorded
// calls double as the count of rebuilds, so a refused or cancelled dialog is shown to
// rebuild nothing. Control property (§9 entry 9): no row reads override before the gesture.
func overrideFixture(t *testing.T) (Model, *[]map[string]image.Ref) {
	t.Helper()
	r := discoverFixture(t)
	var seen []map[string]image.Ref
	fake := ResolveFunc(func(_ context.Context, _ *gitops.Repo, _ string, overrides map[string]image.Ref) (ResolveOutcome, error) {
		seen = append(seen, overrides)
		res := map[string]resolve.Resolution{}
		for repo, ov := range overrides {
			res[repo] = resolve.Resolution{Repo: repo, Ref: ov, Source: resolve.SourceOverride, Detail: "caller-supplied digest"}
		}
		return ResolveOutcome{KubeContext: "test-context", Resolutions: res}, nil
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake, history.Funcs{})
	m = uitest.Drain(m, m.Init(), updateFn)
	if m.state != stateReady || len(m.rows) == 0 {
		t.Fatalf("state = %v rows = %d, want a ready screen with rows", m.state, len(m.rows))
	}
	for _, row := range m.rows {
		if row.Source == "override" {
			t.Fatalf("setup: %s reads override before any gesture", row.Repo)
		}
	}
	return m.SetSize(100, 30).SetStyles(ui.NewStyles(true)), &seen
}

// typeInto presses every character of s into the open dialog, the way an operator types a
// reference — real keypresses through Update, never a write to the bound string (§9 entry
// 6). The per-key commands are dropped rather than drained: a focused text input answers
// every keypress with a cursor-blink command that sleeps half a second before reporting,
// and a hundred-character reference would otherwise take a minute per test to type.
func typeInto(m Model, s string) Model {
	for _, r := range s {
		m, _ = m.Update(uitest.Key(string(r)))
	}
	return m
}

const overrideDigest = "sha256:c0ffee0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// TestOverrideGestureRebuildsThePlan: o, a pinned reference, enter — the fake resolver is
// handed the override for the hovered repo, the plan is rebuilt with it, and the row names
// override as its source, the word the CLI's Resolution section prints (#102).
func TestOverrideGestureRebuildsThePlan(t *testing.T) {
	m, seen := overrideFixture(t)
	hovered, ok := m.hoveredRow()
	if !ok {
		t.Fatal("setup: no hovered row")
	}
	m = uitest.Keys(m, updateFn, "o")
	if !m.overriding {
		t.Fatal("o did not open the override dialog")
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "override digest") || !strings.Contains(v, hovered.Repo+"=") {
		t.Fatalf("the dialog must be up, pre-filled with the hovered repo:\n%s", v)
	}
	ref := hovered.Repo + ":v9@" + overrideDigest
	m = typeInto(m, ref)
	m = uitest.Keys(m, updateFn, "enter")
	if m.overriding {
		t.Fatalf("dialog still open after a valid reference; err = %q", m.overrideErr)
	}
	if len(*seen) != 2 {
		t.Fatalf("resolver called %d times, want 2 (the load, then the rebuild)", len(*seen))
	}
	got := (*seen)[1]
	if want, _ := image.Parse(ref); got[hovered.Repo] != want {
		t.Errorf("resolver received overrides %v, want %s=%s", got, hovered.Repo, want)
	}
	row, ok := m.hoveredRow()
	if !ok {
		t.Fatal("no hovered row after the rebuild")
	}
	if row.Source != "override" || row.New.Digest != overrideDigest || row.New.Tag != "v9" {
		t.Errorf("row after override = %+v, want source override and the typed reference", row)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "digest from override") {
		t.Errorf("the right pane must name the override as the digest's source:\n%s", v)
	}
	if body := m.rightBody(); !strings.Contains(body, "[override] caller-supplied digest") {
		t.Errorf("the resolution section must read as the CLI's does:\n%s", body)
	}
}

// TestOverrideRefusesAnUnpinnedTag: an unpinned reference is refused in the dialog, with
// the same words --digest uses, and the dialog stays open; nothing is rebuilt. The pinned
// form of the same reference, typed into the same dialog, is the positive control.
func TestOverrideRefusesAnUnpinnedTag(t *testing.T) {
	m, seen := overrideFixture(t)
	hovered, _ := m.hoveredRow()
	m = uitest.Keys(m, updateFn, "o")
	m = typeInto(m, hovered.Repo+":v9")
	m = uitest.Keys(m, updateFn, "enter")
	if !m.overriding {
		t.Fatal("dialog closed on an unpinned reference")
	}
	want := "override for " + hovered.Repo + " has no digest"
	if v := ansi.Strip(m.View()); !strings.Contains(v, want) {
		t.Errorf("dialog does not show the refusal %q:\n%s", want, v)
	}
	if len(*seen) != 1 {
		t.Errorf("resolver called %d times, want 1: a refused override rebuilds nothing", len(*seen))
	}
	if len(m.overrides) != 0 {
		t.Errorf("overrides recorded despite the refusal: %v", m.overrides)
	}
	m = typeInto(m, "@"+overrideDigest)
	m = uitest.Keys(m, updateFn, "enter")
	if m.overriding || len(*seen) != 2 {
		t.Errorf("pinned reference: overriding=%v calls=%d err=%q", m.overriding, len(*seen), m.overrideErr)
	}
}

// TestOverrideEscCancels: o then esc leaves the plan, the overrides and the resolver alone,
// and does not leave the screen (esc in the dialog is not the screen's esc).
func TestOverrideEscCancels(t *testing.T) {
	m, seen := overrideFixture(t)
	before := m.rows
	m, cmd := m.Update(uitest.Key("o"))
	m = uitest.Drain(m, cmd, updateFn)
	m, cmd = m.Update(uitest.Key("esc"))
	if cmd != nil {
		if _, back := cmd().(BackMsg); back {
			t.Error("esc in the dialog left the screen")
		}
	}
	if m.overriding {
		t.Error("dialog still open after esc")
	}
	if len(*seen) != 1 || len(m.overrides) != 0 {
		t.Errorf("esc changed something: calls=%d overrides=%v", len(*seen), m.overrides)
	}
	if len(m.rows) != len(before) || m.rows[0].Repo != before[0].Repo || m.rows[0].New != before[0].New {
		t.Error("esc changed the rows")
	}
}

// TestOverrideCapturesText: while the dialog is up every character is the input's — the
// root must not treat q as quit.
func TestOverrideCapturesText(t *testing.T) {
	m, _ := overrideFixture(t)
	if m.CapturesText() {
		t.Fatal("captures text before the dialog is open")
	}
	m = uitest.Keys(m, updateFn, "o")
	if !m.CapturesText() {
		t.Error("the open dialog must capture text")
	}
}

// TestOverrideWithoutAResolver: in "digest sources: none" mode there is no resolver to hand
// the override to, and the row must still name override as its source — the override is a
// fact about the plan, not about the resolver.
func TestOverrideWithoutAResolver(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	hovered, _ := m.hoveredRow()
	m = uitest.Keys(m, updateFn, "o")
	m = typeInto(m, hovered.Repo+":v9@"+overrideDigest)
	m = uitest.Keys(m, updateFn, "enter")
	row, _ := m.hoveredRow()
	if m.overriding || row.Source != "override" || row.New.Digest != overrideDigest {
		t.Errorf("row = %+v overriding=%v err=%q", row, m.overriding, m.overrideErr)
	}
}

func TestViewGoldenOverride(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m, _ := overrideFixture(t)
		m = m.SetSize(size[0], size[1])
		hovered, _ := m.hoveredRow()
		m = uitest.Keys(m, updateFn, "o")
		uitest.Golden(t, "plan-override-dialog", m.View(), size[0], size[1])
		m = typeInto(m, hovered.Repo+":v9@"+overrideDigest)
		m = uitest.Keys(m, updateFn, "enter")
		if row, _ := m.hoveredRow(); row.Source != "override" {
			t.Errorf("row after override = %+v, want source override", row)
		}
		uitest.Golden(t, "plan-override", m.View(), size[0], size[1])
	}
}
