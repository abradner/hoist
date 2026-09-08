package plan

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/resolve"
)

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
	m := New(r, []string{"ghcr.io/"}, envs, "app-staging", "app-production", false, nil, history.Funcs{})
	m = runInit(t, m)
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady", m.state)
	}
	m = m.SetSize(100, 30)
	m = m.SetStyles(ui.NewStyles(true))
	return m
}

// The hovered repo's migrations list qualifies its count when the forge capped the history,
// exactly as the totals line does — a floor is never presented as the whole list (Codex, #138).
func TestImpactBodySaysAtLeastWhenMigrationsIncomplete(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		r := discoverFixture(t)
		hist := history.Funcs{
			Mapped: func(string) bool { return true },
			Delta: func(context.Context, image.Ref, image.Ref) (migrate.Delta, error) {
				return migrate.Delta{
					Direction: migrate.DirectionForward, Total: 1,
					Commits:              []migrate.Commit{{SHA: "77c0ffe0000", Subject: "db: add index", Migrations: []string{"db/migrate/1_add_index.rb"}}},
					Migrations:           []string{"db/migrate/1_add_index.rb"},
					MigrationCommits:     1,
					MigrationsIncomplete: incomplete,
				}, nil
			},
		}
		m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, nil, hist)
		m = uitest.Drain(m, m.Init(), updateFn).SetSize(120, 40).SetStyles(ui.NewStyles(true))
		v := ansi.Strip(m.View())
		if !strings.Contains(v, "1 migration") || !strings.Contains(v, "run on this promotion:") {
			t.Fatalf("incomplete=%v: the migrations list should render:\n%s", incomplete, v)
		}
		if got := strings.Contains(v, "1 migration (at least) run on this promotion:"); got != incomplete {
			t.Fatalf("incomplete=%v: qualifier present=%v:\n%s", incomplete, got, v)
		}
	}
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
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake, history.Funcs{})
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

// TestResolveErrorFailsTheScreen: a ResolveFunc error means digest resolution was
// attempted and failed outright (the cluster was unreachable, or its config was
// invalid) — AGENTS.md §4.10 states that as a whole-operation failure, the same
// asymmetry cmd/hoist's plan command already enforces (a runResolution error there
// prints to stderr and exits without ever printing a plan). The screen must not be a
// second, looser gate on that rule: it fails too, rather than building a selectable
// plan from manifest values nobody has confirmed against the running environment.
// (An earlier version of this test asserted the opposite — a Codex review of the
// draft followup caught it as the exact bypass the rule exists to prevent.)
func TestResolveErrorFailsTheScreen(t *testing.T) {
	r := discoverFixture(t)
	fake := ResolveFunc(func(context.Context, *gitops.Repo, string) (ResolveOutcome, error) {
		return ResolveOutcome{}, errCannotReachCluster
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake, history.Funcs{})
	m = runInit(t, m)
	if m.err == nil {
		t.Fatal("m.err = nil, want the resolve error to fail the screen")
	}
	if !strings.Contains(m.err.Error(), errCannotReachCluster.Error()) {
		t.Errorf("m.err = %v, want it to name the resolve failure", m.err)
	}
	if len(m.rows) != 0 {
		t.Errorf("rows = %+v, want none — nothing is planned when resolution fails outright", m.rows)
	}
}

// F10 regression: the CLI printer redacts every rendered field with pkg/redact
// (cmd/hoist's resolutionReport.print and printPlan), but the plan screen's own render
// points — Summary's per-repo Detail, leftBody's disabled-row Reason, rightBody's warning
// Message — did not. A value registered anywhere in the process must be scrubbed
// wherever it later surfaces, the same guarantee TestPlanScrubsARegisteredSecretFromRegistryAndKubeErrors
// proves for the CLI.
func TestViewRedactsRegisteredSecrets(t *testing.T) {
	const secret = "SECRET-TOKEN-XYZ"
	redact.Register(secret)

	r := discoverFixture(t)
	fake := ResolveFunc(func(context.Context, *gitops.Repo, string) (ResolveOutcome, error) {
		return ResolveOutcome{
			KubeContext:  "test-context",
			RegistryAuth: "env",
			Resolutions: map[string]resolve.Resolution{
				// Resolved, so its Detail lands in Summary's per-repo line.
				"ghcr.io/example/web": {
					Repo:   "ghcr.io/example/web",
					Ref:    image.Ref{Repo: "ghcr.io/example/web", Tag: "v202602150930", Digest: "sha256:1f7e5c3a9b2d4e6f8a0c1b3d5e7f9a2b4c6d8e0f1a3b5c7d9e1f2a4b6c8d0e2f"},
					Source: resolve.SourceRegistry,
					Detail: "registry said: token " + secret + " rejected",
				},
				// Unresolved, so its warning becomes the disabled row's Reason (leftBody)
				// as well as a plan warning (rightBody).
				"ghcr.io/example/marketing": {
					Repo:   "ghcr.io/example/marketing",
					Detail: "token " + secret + " rejected",
					Warnings: []gitops.Warning{{
						Code:    resolve.WarnUnresolved,
						Message: "app-staging: no digest for ghcr.io/example/marketing (token " + secret + " rejected)",
					}},
				},
			},
		}, nil
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake, history.Funcs{})
	m = runInit(t, m)
	if m.state != stateReady {
		t.Fatalf("state = %v, want stateReady", m.state)
	}
	m = m.SetSize(120, 40)
	m = m.SetStyles(ui.NewStyles(true))

	// leftBody (disabled-row Reason) and rightBody (the warnings list and the Resolution
	// summary, which embeds Summary's per-repo Detail) are checked directly rather than
	// through View(): the viewport View() renders through can clip content out of the
	// visible window at a fixed terminal size, which would make this test pass for the
	// wrong reason (viewport height, not redaction) on a body long enough to scroll.
	for name, body := range map[string]string{"leftBody": m.leftBody(), "rightBody": m.rightBody()} {
		if strings.Contains(body, secret) {
			t.Errorf("registered secret leaked into %s:\n%s", name, body)
		}
	}
	if flat := strings.Join(strings.Fields(ansi.Strip(m.leftBody())), " "); !strings.Contains(flat, "token <redacted> rejected") {
		t.Errorf("leftBody: expected <redacted> in place of the secret:\n%s", m.leftBody())
	}
	if !strings.Contains(m.rightBody(), "token <redacted> rejected") {
		t.Errorf("rightBody: expected <redacted> in place of the secret:\n%s", m.rightBody())
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
		if _, isStart := cmd().(StartMsg); isStart {
			t.Error("m must not confirm the plan")
		}
	}
	if !m.confirming {
		t.Fatal("m did not open the confirm dialog for a non-production target")
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Switch to direct mode") || !strings.Contains(v, "app-staging  →  app-production") {
		t.Fatalf("the dialog must sit over the screen:\n%s", v)
	}
	// Answered through the widget's own key, never by assigning the bound bool: an earlier
	// version of this test set m.confirmValue directly and so never noticed that
	// huh.NewConfirm ships a zero keymap and ignored every keypress — the m gesture could not
	// be completed by a real operator at all (#85's named trap, the plan screen's instance).
	m, _ = m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.confirming {
		t.Error("confirm dialog still open after enter")
	}
	if m.mode != ModeDirect {
		t.Errorf("mode = %q, want direct: the confirmation never saw the keypress", m.mode)
	}
	// And no: the asymmetry that makes the above mean something.
	n := readyModel(t, config.EnvsConfig{})
	n, _ = n.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	n, _ = n.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	n, _ = n.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if n.mode != ModePR {
		t.Errorf("declining must leave the mode alone, got %q", n.mode)
	}
	// esc leaves the dialog without leaving the screen.
	e := readyModel(t, config.EnvsConfig{})
	e, _ = e.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	e, cmd = e.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil || e.confirming {
		t.Errorf("esc in the dialog: cmd=%v confirming=%v", cmd, e.confirming)
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

// TestEnterEmitsStartMsg: confirming a plan with at least one ticked repo emits StartMsg
// naming the plan, outcome, mode, ticked set and envs — the root's cue to push
// internal/app/flight (AGENTS.md §4.8: a screen requests navigation by emitting its own
// concrete type).
func TestEnterEmitsStartMsg(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	if len(m.ticked) == 0 {
		t.Fatal("setup: fixture produced no tickable rows")
	}
	wantTicked := append([]string(nil), m.ticked...)

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	msg, ok := cmd().(StartMsg)
	if !ok {
		t.Fatalf("enter's command yields %T, want StartMsg", cmd())
	}
	if msg.Source != "app-staging" || msg.Target != "app-production" {
		t.Errorf("StartMsg source/target = %s/%s, want app-staging/app-production", msg.Source, msg.Target)
	}
	if msg.Mode != ModePR {
		t.Errorf("StartMsg.Mode = %q, want %q", msg.Mode, ModePR)
	}
	if !equalSets(msg.Ticked, wantTicked) {
		t.Errorf("StartMsg.Ticked = %v, want %v", msg.Ticked, wantTicked)
	}
	if len(msg.Plan.Edits) == 0 {
		t.Error("StartMsg.Plan carries no edits")
	}
}

// TestEnterWithNothingTickedShowsNotice: unticking every row and pressing enter must not
// emit StartMsg for an empty promotion — it shows a notice instead.
func TestEnterWithNothingTickedShowsNotice(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	m.ticked = nil
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("enter with nothing ticked produced a command: %v", cmd())
	}
	if !strings.Contains(m.notice, "nothing ticked") {
		t.Errorf("notice = %q, want it to mention nothing ticked", m.notice)
	}
}

// TestFilterModeEngagesAndCapturesText proves buildMultiSelect's own huh.Field.WithKeyMap call
// actually did what round 5's finding 3 investigation found missing: pressing "/" against the
// ready screen's multiSelect now reaches huh's own filter-typing mode (GetFiltering flips true),
// and CapturesText reports that live, exactly as tags.Model.CapturesText reports its own
// m.filtering — the root needs this so it doesn't treat a "q" typed into the filter query as its
// own global quit key (see app.go's own regression test for the tag picker's side of the same
// bug). Before the WithKeyMap wiring, "/" didn't even reach huh's filter — this is what would
// regress (CapturesText silently going back to always-false) if that wiring were ever removed.
func TestFilterModeEngagesAndCapturesText(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	if m.CapturesText() {
		t.Fatal("CapturesText should be false before any key is pressed")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !m.multiSelect.GetFiltering() {
		t.Fatal("multiSelect should be filtering after \"/\": WithKeyMap wiring did not reach huh's own filter mode")
	}
	if !m.CapturesText() {
		t.Fatal("CapturesText should be true while the multiSelect is filtering")
	}
	// Esc is deliberately not exercised here: this screen's own Update treats Esc as its Back
	// key unconditionally, before ever consulting CapturesText or forwarding to the active
	// field (unlike tags.Model, which checks m.filtering first) — a pre-existing gap outside
	// this fix's scope (WithKeyMap wiring, not Esc routing), so filtering here can only be
	// closed by typing enter, not by asserting an esc-driven exit this screen doesn't support.
}

// TestDownMovesMultiSelectCursor proves the same WithKeyMap wiring reaches ordinary
// navigation, not just filtering: Down against a freshly built multiSelect used to move
// nothing at all (round 5 finding 3 — every keymap-gated mode on these fields was inert, not
// only "/"). huh doesn't expose the field's own cursor directly, so this drives it indirectly
// through Toggle ("x"): Down then "x" must toggle a DIFFERENT row than "x" alone at the initial
// cursor position, which is only possible if Down actually moved the cursor first.
//
// This also exercises a second, independent gap the WithKeyMap fix alone did not close:
// buildMultiSelect's huh.NewMultiSelect().Value(&m.ticked) captures the address of m.ticked as
// it exists on ONE particular Model copy (the value inside onLoaded's own stack frame, at the
// moment buildMultiSelect runs) — but Model is a value threaded through a chain of value-receiver
// methods that each return a fresh copy (New's own doc comment on the convention), so every
// Update after that point runs against a copy whose own m.ticked field is a different piece of
// memory than the one the accessor still points at. huh's own Toggle handling stayed internally
// consistent (View() and multiSelect.GetValue() agree with each other), but writes made through
// that accessor never reached the traveling m.ticked — the exact case Toggle needs, since
// updateReady reads m.ticked (not the field) for the notice check and StartMsg.Ticked. Confirmed
// directly before adding the fix below: after WithKeyMap alone, pressing "x" visibly unchecked a
// row in multiSelect.View() while m.ticked (and equalSets(before, m.ticked) below) stayed
// unchanged. updateReady now re-reads m.ticked from m.multiSelect.GetValue() after every Update
// call, which is what makes this test — and the real screen — see the toggle at all.
func TestDownMovesMultiSelectCursor(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	if len(m.rows) < 2 {
		t.Fatalf("fixture has %d selectable rows, want at least 2 for this test to be meaningful", len(m.rows))
	}
	before := append([]string(nil), m.ticked...)

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m, _ = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})

	if equalSets(before, m.ticked) {
		t.Fatal("ticked set unchanged after down+x: Toggle never reached the field (keymap still unwired?)")
	}
	// Untoggling the very first row (no Down) must differ from untoggling whatever Down landed
	// on above, or this test can't actually distinguish "Down moved the cursor" from "x toggled
	// row 0 regardless of Down".
	m2 := readyModel(t, config.EnvsConfig{})
	m2, _ = m2.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if equalSets(m.ticked, m2.ticked) {
		t.Fatal("down+x toggled the same row as x alone: Down does not appear to move the cursor")
	}
}

// TestEnvSelectResyncsTargetFromField proves updateSelectEnv's own resync line (the envSelect
// twin of TestDownMovesMultiSelectCursor's multiSelect finding): buildEnvSelect's
// huh.NewSelect().Value(&m.target) has the identical accessor-into-a-value-copy gap, so Down
// moving the highlighted candidate updates the field's own accessor without that write ever
// reaching the traveling m.target — the field the "enter" handler above actually reads to decide
// whether to advance past stateSelectEnv and which target to resolve against. The real
// testdata/repo fixture only has two envs (app-staging/app-production), so TargetsFor never
// offers more than one candidate to promote "app-staging" to — not enough to move Down onto a
// different value through the normal New()/buildEnvSelect() path. This builds a Select with two
// options directly (same construction buildEnvSelect uses: Options + Value + WithKeyMap) and
// drives it through updateSelectEnv itself, which is what's actually under test here.
func TestEnvSelectResyncsTargetFromField(t *testing.T) {
	var target string
	sel := huh.NewSelect[string]().
		Options(huh.NewOption("app-a", "app-a"), huh.NewOption("app-b", "app-b")).
		Value(&target)
	sel.WithKeyMap(huh.NewDefaultKeyMap())

	m := Model{state: stateSelectEnv, envSelect: sel, target: target}
	if m.target != "app-a" {
		t.Fatalf("target = %q, want app-a selected by construction before any key press", m.target)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.target != "app-b" {
		t.Fatalf("target = %q after down, want app-b: updateSelectEnv did not resync m.target from the field's own GetValue()", m.target)
	}
}

// TestBuildEnvSelectWiresFiltering exercises buildEnvSelect itself (unlike
// TestEnvSelectResyncsTargetFromField's own hand-built Select, used there only because the
// fixture repo doesn't have enough envs to move Down onto a different candidate) — proving the
// real construction path's own WithKeyMap call, not just the resync line, actually reaches huh's
// filter mode. Filtering doesn't need a second candidate to be observable, so it works even
// though this fixture's TargetsFor("app-staging") only ever offers one.
func TestBuildEnvSelectWiresFiltering(t *testing.T) {
	r := discoverFixture(t)
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "", false, nil, history.Funcs{})
	if m.state != stateSelectEnv {
		t.Fatalf("state = %v, want stateSelectEnv", m.state)
	}
	if m.CapturesText() {
		t.Fatal("CapturesText should be false before any key is pressed")
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !m.envSelect.GetFiltering() {
		t.Fatal("envSelect should be filtering after \"/\": buildEnvSelect's own WithKeyMap call did not reach huh's filter mode")
	}
	if !m.CapturesText() {
		t.Fatal("CapturesText should be true while envSelect is filtering")
	}
}

// TestSkipStagingWarning: promoting straight to a production env that is not the source's
// configured pair shows the warning, never blocks.
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
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := readyModel(t, envs).SetSize(size[0], size[1])
		got := ansi.Strip(m.View())
		for _, want := range []string{
			"hoist · confirm promotion", "app-staging  →  app-production", "images under ghcr.io/example/", "mode: PR",
			"counta", "v202602201200", "3 repos ticked", "no commit history",
			"d  see the yaml",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("view lacks %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, string(filepath.Separator)+"testdata") {
			t.Error("view shows a path, not a base name")
		}
		uitest.Golden(t, "plan", m.View(), size[0], size[1])
	}
	// The yaml view: the diff, and below the fold the untouched images, warnings and the
	// resolution report — check the body that feeds the viewport for the tail.
	m := readyModel(t, envs).SetSize(120, 40)
	m = uitest.Keys(m, updateFn, "d")
	got := ansi.Strip(m.View())
	if !strings.Contains(got, "· yaml") || !strings.Contains(got, "image:") || !strings.Contains(got, "d  back to impact") {
		t.Errorf("yaml view:\n%s", got)
	}
	body := m.rightBody()
	for _, want := range []string{"Untouched (", "Warnings (", "Resolution:", "cluster not consulted"} {
		if !strings.Contains(body, want) {
			t.Errorf("right pane body lacks %q:\n%s", want, body)
		}
	}
	uitest.Golden(t, "plan-yaml", m.View(), 120, 40)
}

func updateFn(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) }

// #121: a width change rebuilds the huh.MultiSelect to re-truncate its labels, and huh has
// no cursor setter, so the cursor used to jump to the first row and the right pane followed
// it. Driven through real keys: down twice, then a resize the layout turns into a rebuild.
func TestResizeKeepsTheCursor(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{}).SetSize(100, 30)
	if n := len(Selectable(m.rows)); n < 3 {
		t.Fatalf("fixture has %d selectable rows, want at least 3", n)
	}
	m = uitest.Keys(m, updateFn, "down", "down")
	if got := m.hoveredIndex(); got != 2 {
		t.Fatalf("setup: hovered index after down, down = %d, want 2", got)
	}
	want, _ := m.hoveredRow()

	before := m.leftWidth
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m.SetSize(120, 40)
	if m.leftWidth == before {
		t.Fatalf("setup: the resize did not change the left pane's width (%d), so nothing was rebuilt", before)
	}
	if got := m.hoveredIndex(); got != 2 {
		t.Fatalf("hovered index after resize = %d, want 2: the rebuilt field lost the cursor", got)
	}
	if r, _ := m.hoveredRow(); r.Repo != want.Repo {
		t.Errorf("hovered repo after resize = %q, want %q", r.Repo, want.Repo)
	}
	if head := ansi.Strip(strings.SplitN(m.impactBody(), "\n", 2)[0]); head != strings.TrimPrefix(want.Repo, m.prefix) {
		t.Errorf("right pane heads with %q after resize, want %q", head, strings.TrimPrefix(want.Repo, m.prefix))
	}
}

// #121: the left pane's labels stay distinct when two long repo names truncate to the same
// text — a synthetic plan over the fixture repo's root, with NoOp edits so no file is read.
func TestViewGoldenCollidingLabels(t *testing.T) {
	r := discoverFixture(t)
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, nil, history.Funcs{})
	same := func(repo string) gitops.Edit {
		ref := ref(repo + ":v202602201200@sha256:" + strings.Repeat("a", 64))
		return edit("cluster/apps/app-production/web/deployment.yaml", ref, ref)
	}
	pl := gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production", Edits: []gitops.Edit{
		same("ghcr.io/example/web-frontend-service-alpha"),
		same("ghcr.io/example/web-frontend-service-beta"),
		same("ghcr.io/example/db"),
	}}
	m, _ = m.Update(loadedMsg{plan: pl})
	m = m.SetSize(80, 24).SetStyles(ui.NewStyles(true))
	v := ansi.Strip(m.View())
	for _, want := range []string{"…alpha", "…beta"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	uitest.Golden(t, "plan-collide", m.View(), 80, 24)
}

// Codex P2 (draft #29 pass): viewReady renders m.err.Error() directly with no per-call
// redact.Strings — the one render point TestViewRedactsRegisteredSecrets's per-field
// cases don't reach, since they all exercise the success path. A registered secret
// embedded in a fatal resolveFn error must still be scrubbed by View()'s own final-
// boundary call, not only by whichever nested renderer remembers to redact itself.
func TestViewRedactsRegisteredSecretsInFatalError(t *testing.T) {
	const secret = "SECRET-TOKEN-XYZ"
	redact.Register(secret)

	r := discoverFixture(t)
	fake := ResolveFunc(func(context.Context, *gitops.Repo, string) (ResolveOutcome, error) {
		return ResolveOutcome{}, sentinelErr("cluster unreachable: token " + secret + " rejected")
	})
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, "app-staging", "app-production", false, fake, history.Funcs{})
	m = runInit(t, m)
	if m.err == nil {
		t.Fatal("want the resolve error to have failed the screen")
	}
	if got := m.View(); strings.Contains(got, secret) {
		t.Errorf("View() leaked the registered secret in the fatal-error render:\n%s", got)
	}
}

// Leaving the screen — esc, or enter handing off to the flight screen — cancels the context
// every history request runs under, so a delta still loading stops calling the forge.
func TestLeavingThePlanScreenCancelsItsHistory(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	if m.ctx.Err() != nil {
		t.Fatal("fresh screen: context already cancelled")
	}
	m, _ = m.Update(uitest.Key("esc"))
	if !errors.Is(m.ctx.Err(), context.Canceled) {
		t.Fatalf("after esc: ctx.Err() = %v; want cancelled", m.ctx.Err())
	}

	m = readyModel(t, config.EnvsConfig{})
	m, cmd := m.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter emitted nothing")
	}
	if _, ok := cmd().(StartMsg); !ok {
		t.Fatalf("enter emitted %T", cmd())
	}
	if !errors.Is(m.ctx.Err(), context.Canceled) {
		t.Fatalf("after enter: ctx.Err() = %v; want cancelled", m.ctx.Err())
	}
}

// The impact and the yaml are unrelated documents: a scroll offset from one must not hide
// the other's head when d switches between them.
func TestSwitchingTheRightPaneStartsAtTheTop(t *testing.T) {
	m := readyModel(t, config.EnvsConfig{})
	m.viewport.SetContent(strings.Repeat("line\n", 200))
	m.viewport.SetYOffset(40)
	if m.viewport.YOffset() != 40 {
		t.Fatalf("setup: offset %d", m.viewport.YOffset())
	}
	m, _ = m.Update(uitest.Key("d"))
	if !m.showYAML || m.viewport.YOffset() != 0 {
		t.Fatalf("after d: yaml=%v offset=%d; want the yaml from its first line", m.showYAML, m.viewport.YOffset())
	}
	m.viewport.SetYOffset(10)
	m, _ = m.Update(uitest.Key("d"))
	if m.showYAML || m.viewport.YOffset() != 0 {
		t.Fatalf("back: yaml=%v offset=%d", m.showYAML, m.viewport.YOffset())
	}
}
