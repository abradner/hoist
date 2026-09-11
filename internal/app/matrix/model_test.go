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
	drift := func(_ context.Context, env string) (map[string][]image.Ref, error) {
		asked[env]++
		switch env {
		case "b":
			return map[string][]image.Ref{"ghcr.io/x/app": {{Repo: "ghcr.io/x/app", Tag: "v7", Digest: digestC}}}, nil
		case "c":
			return nil, errors.New("kube context \"my-cluster\" is not in the kubeconfig")
		}
		return map[string][]image.Ref{}, nil
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
	if !strings.Contains(v, "! pinned runs v7 in b; manifest says v1 (compared by digest)") {
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
	stale := DriftMsg{gen: m.gen - 1, env: "a", running: map[string][]image.Ref{"ghcr.io/x/app": {{Repo: "ghcr.io/x/app", Tag: "v99", Digest: digestC}}}}
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

// WithDrift installed a second time (the root re-handing its resolver) must not re-mark an env
// whose answer already landed: it starts no request, so that env would show "asking the
// cluster" forever. An unanswered env stays pending; a nil function clears pending.
func TestWithDriftLeavesAnsweredEnvsAlone(t *testing.T) {
	drift := func(_ context.Context, _ string) (map[string][]image.Ref, error) {
		return map[string][]image.Ref{}, nil
	}
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).WithDrift(drift)
	if !m.pending["a"] || !m.pending["b"] {
		t.Fatalf("pending = %v; want every env pending after the first WithDrift", m.pending)
	}
	m, _ = m.Update(DriftMsg{gen: m.gen, env: "a", running: map[string][]image.Ref{}})
	m, _ = m.Update(DriftMsg{gen: m.gen, env: "c", err: errors.New("cluster unreachable")})
	m = m.WithDrift(drift)
	if m.pending["a"] {
		t.Error("env a answered before the second WithDrift, yet it is pending again")
	}
	if m.pending["c"] {
		t.Error("env c failed before the second WithDrift, yet it is pending again")
	}
	if !m.pending["b"] {
		t.Error("env b never answered, yet it is not pending")
	}
	if p := m.WithDrift(nil).pending; len(p) != 0 {
		t.Errorf("a nil function must clear pending; got %v", p)
	}
}

// TestRefreshKeyAlsoRefreshesTheRepo (#PR7): F5 asks the cluster (its own long-standing job)
// and, now, re-reads the repo too — driven through a real keypress and a real drained
// command (uitest.Keys/Drain), never by constructing RepoRefreshedMsg by hand and calling
// Update directly (AGENTS.md §9 entry 6's own lesson, generalized: a gesture's test presses
// the key).
func TestRefreshKeyAlsoRefreshesTheRepo(t *testing.T) {
	// A realistic refresh: still a real, populated repo — not an all-empty one, which is its
	// own separate, pre-existing edge case (internal/app/matrix's own layout panics on zero
	// columns, unrelated to WithRepo; filed rather than chased here).
	refreshed := fixture()
	refreshed.Root = "refreshed-repo"
	called := false
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).
		WithRefreshRepo(func(_ context.Context) (*gitops.Repo, error) {
			called = true
			return refreshed, nil
		})
	m = m.SetSize(80, 24)
	if strings.Contains(ansi.Strip(m.View()), "refreshed-repo") {
		t.Fatal("setup: the refreshed repo's root is already visible before F5")
	}

	m = uitest.Keys(m, update, "f5")

	if !called {
		t.Fatal("F5 never called the installed RefreshRepoFunc")
	}
	if !strings.Contains(ansi.Strip(m.View()), "refreshed-repo") {
		t.Errorf("view does not reflect the refreshed repo's root after F5:\n%s", ansi.Strip(m.View()))
	}
}

// TestWithRepoToEmptyRepoDoesNotPanic is a regression test for a real bug WithRepo (#PR7)
// introduced: bubbles' own table.SetColumns re-renders synchronously against whatever rows
// the table already holds (layout's own comment explains why), so shrinking the column count
// while old, differently-shaped rows are still set panicked in table.renderRow. Never
// reachable before WithRepo existed — a drift answer or a resize only ever change cell
// content or width, never how many envs (columns) the matrix has; a live repo refresh can,
// down to zero in the limit.
func TestWithRepoToEmptyRepoDoesNotPanic(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
	empty := &gitops.Repo{Root: "empty", Envs: map[string]*gitops.Env{}}
	m = m.WithRepo(empty) // must not panic
	if m.repo.Root != "empty" {
		t.Errorf("repo not adopted: root = %q, want %q", m.repo.Root, "empty")
	}
}

// TestRefreshRepoFailureKeepsTheOldRepo: a failed refresh (network down, origin unreachable)
// must not blank the table or crash — the same graceful-degradation boot's own fallback
// applies (repoview.go). The matrix keeps showing what it already had and surfaces the
// error as a notice, never a silent drop.
func TestRefreshRepoFailureKeepsTheOldRepo(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).
		WithRefreshRepo(func(_ context.Context) (*gitops.Repo, error) {
			return nil, errors.New("dial tcp: no route to host")
		})
	m = m.SetSize(80, 24)

	m = uitest.Keys(m, update, "f5")

	if got := ansi.Strip(m.View()); !strings.Contains(got, "no route to host") {
		t.Errorf("view missing the refresh failure notice:\n%s", got)
	}
	if m.repo.Root != "repo" {
		t.Errorf("repo root changed to %q after a failed refresh, want unchanged %q", m.repo.Root, "repo")
	}
}

// TestSecondRepoRefreshWhileOneIsOutstandingIsSkipped is a regression test for a P2 an
// adversarial review of #PR7 found: askRepoRefresh had no in-flight guard, so a second F5
// pressed before the first refresh resolved could run two concurrent refreshes against the
// same fixed cache path (#PR7's repoview.go), corrupting git state (index.lock contention,
// broken worktree registrations, per the review). This drives askRepoRefresh directly rather
// than through a keypress because the point is to inspect the *outstanding* command before
// it resolves — uitest.Keys drains a command to completion, which would hide the overlap.
func TestSecondRepoRefreshWhileOneIsOutstandingIsSkipped(t *testing.T) {
	var calls int
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).
		WithRefreshRepo(func(_ context.Context) (*gitops.Repo, error) {
			calls++
			return fixture(), nil
		})

	m, cmd1 := m.askRepoRefresh()
	if cmd1 == nil {
		t.Fatal("setup: first askRepoRefresh issued no command")
	}
	_, cmd2 := m.askRepoRefresh() // a second F5 while the first is still outstanding
	if cmd2 != nil {
		t.Error("a second askRepoRefresh while one is outstanding returned a command instead of being skipped")
	}

	cmd1()
	if calls != 1 {
		t.Errorf("refreshRepo called %d times across both askRepoRefresh calls, want 1", calls)
	}
}

// TestStaleRepoRefreshFromAnEarlierModelGenerationIsIgnored is a regression test for the
// paired P3 finding: RepoRefreshedMsg carried no generation, unlike DriftMsg, so an
// outstanding refresh from an earlier instance of this screen (popped, then re-pushed — the
// same "two matrices" case nextGen's own doc comment names) could land after a newer
// instance's own refresh and silently overwrite its repo. m1 stands in for the popped screen;
// m2 for the one currently on the root's stack — m1's stale answer must never reach m2's repo.
func TestStaleRepoRefreshFromAnEarlierModelGenerationIsIgnored(t *testing.T) {
	stale := fixture()
	stale.Root = "stale-repo"

	m1 := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).
		WithRefreshRepo(func(_ context.Context) (*gitops.Repo, error) { return stale, nil })
	_, cmd1 := m1.askRepoRefresh()
	if cmd1 == nil {
		t.Fatal("setup: m1's askRepoRefresh issued no command")
	}

	// A later Model instance draws a later generation from the same shared counter.
	m2 := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).
		WithRefreshRepo(func(_ context.Context) (*gitops.Repo, error) { return fixture(), nil }).
		SetSize(80, 24)
	m2, cmd2 := m2.askRepoRefresh()
	if cmd2 == nil {
		t.Fatal("setup: m2's askRepoRefresh issued no command")
	}

	// m1's outstanding command finally resolves, but is delivered to m2 — the only screen
	// bubbletea's root still has on the stack.
	m2, _ = m2.Update(cmd1())

	if m2.repo.Root == "stale-repo" {
		t.Error("a superseded repo-refresh answer from an earlier Model generation overwrote the current one's repo")
	}
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

// The title names the base only when it is not main, and the kube context whenever one is
// in use (#105): a plain run keeps the plain title, a session against another branch or a
// named cluster says so. The context is its kubeconfig name, never an address. The golden
// proves the longer title still fits 80 columns.
func TestTitleNamesBaseWhenNotMainAndTheContextInUse(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(80, 24)
	if got := m.title(); strings.Contains(got, "base") || got != m.WithRun("main", "").title() {
		t.Fatalf("plain title should not name a base or context: %q", got)
	}
	got := m.WithRun("develop", "my-cluster").title()
	if !strings.Contains(got, "· base develop") || !strings.Contains(got, "· my-cluster") {
		t.Fatalf("title should name both overrides: %q", got)
	}
	if got := m.WithRun("main", "my-cluster").title(); strings.Contains(got, "base") || !strings.Contains(got, "my-cluster") {
		t.Fatalf("main is the default and should not be named, the context should: %q", got)
	}
	uitest.Golden(t, "matrix-run", m.WithRun("develop", "my-cluster").SetStyles(ui.NewStyles(true)).View(), 80, 24)
}

// Below a minimum size the matrix says so in one line instead of drawing a partial table (#16).
func TestTooSmallAWindowSaysSo(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).SetSize(10, 3)
	v := m.View()
	if strings.Count(v, "\n") != 0 || !strings.Contains(v, "window too small") || !strings.Contains(v, "40×8") {
		t.Fatalf("view at 10×3:\n%s", v)
	}
	// Positive control: at the minimum it draws the table.
	if v := m.SetSize(40, 8).View(); strings.Contains(v, "too small") || !strings.Contains(v, "FAMILY") {
		t.Fatalf("view at 40×8 should be the table:\n%s", v)
	}
}

// C emits OpenConfigMsg (#104), whatever the cursor is on — the config is the session's,
// not a cell's — and shows up in ? help.
func TestConfigKeyEmitsOpenConfigMsg(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	if _, ok := emitted(t, m, "C").(OpenConfigMsg); !ok {
		t.Fatalf("C emitted %+v, want OpenConfigMsg", emitted(t, m, "C"))
	}
	if got := emitted(t, m, "c"); got != nil {
		t.Fatalf("lower-case c emitted %+v, want nothing", got)
	}
	// The full help line is longer than 120 columns and truncates from the right, so the
	// key is checked on a terminal wide enough to show all of it.
	m = uitest.Keys(m.SetSize(200, 24), update, "?")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "C config") {
		t.Errorf("? help lacks the C key:\n%s", v)
	}
}
