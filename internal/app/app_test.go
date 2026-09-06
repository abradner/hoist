package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
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
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, promo, nil)
	_ = m.Init()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return tm
}

func press(t *testing.T, m tea.Model, k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t.Helper()
	return m.Update(k)
}

// runBatch invokes cmd the way the real bubbletea runtime would, one level deep: calling a
// tea.Cmd directly (a plain func call, no runtime involved) only ever runs the func itself —
// for a tea.Batch this returns a tea.BatchMsg (a slice of the cmds it wraps) without ever
// invoking any of them, since unwrapping and dispatching a batch is normally the runtime's job.
// flight.Model.Init returns tea.Batch(spinner.Tick, driveCmd()), so calling it directly never
// actually starts the driveCmd goroutine a test needs running. This runs cmd, and if the result
// is a BatchMsg, runs every sub-cmd in its own goroutine too — enough to exercise a real
// in-flight driveCmd without pulling in the whole runtime.
func runBatch(cmd tea.Cmd) {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				go sub()
			}
		}
	}
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
// DriveFunc it returned — no more nil, no more a bare {SourceEnv, TargetEnv}. The fake driveFn
// is a trivial non-nil stub, never nil: a real StartPromotionFunc success always builds one
// (wiring.go's buildStartPromotion never returns a nil driveFn alongside a nil error), and a
// nil driveFn here would now hit the promotionBuiltMsg nil-driveFn guard (Copilot's PR #50
// finding — see TestPromotionBuiltMsgNilDriveFnShowsNotice) instead of exercising this test's
// actual subject, the successful push.
func TestStartMsgBuildsFlightScreenOnSuccess(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	called := false
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		if p.SourceEnv != "app-staging" || p.TargetEnv != "app-production" {
			t.Errorf("startPromotion called with unexpected plan: %+v", p)
		}
		return wantState, stubDriveFn, nil
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

// TestPromotionBuiltMsgStampsCurrentBuildGen is the direct, narrow check that a StartMsg's
// own command stamps promotionBuiltMsg with the Model's buildGen at the moment it was issued —
// mirrors TestDriveCmdStampsCurrentGen at the flight layer (internal/app/flight/model_test.go),
// one layer up the stack, guarding the build step instead of the drive step.
func TestPromotionBuiltMsgStampsCurrentBuildGen(t *testing.T) {
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	pbm, ok := cmd().(promotionBuiltMsg)
	if !ok {
		t.Fatalf("command yields %T, want promotionBuiltMsg", pbm)
	}
	if pbm.gen != m.(Model).buildGen {
		t.Errorf("promotionBuiltMsg.gen = %d, want %d (m.buildGen)", pbm.gen, m.(Model).buildGen)
	}
}

// TestStalePromotionBuiltMsgFromBackedOutPlanIsDropped is PR #50 round-4 review finding #4
// (Codex): the plan screen stays fully interactive while its StartMsg's startPromotion call
// runs in the background, so the operator can press Esc (plan.BackMsg, popping back to the
// matrix) before that call's result ever arrives. Without a generation check, the
// promotionBuiltMsg would still be adopted unconditionally once it landed — pushing a flight
// screen (which immediately starts driving: committing, pushing, opening a PR) for a plan the
// operator already backed out of. This proves the stale result is dropped: the stack stays on
// the matrix, and nothing is pushed.
func TestStalePromotionBuiltMsgFromBackedOutPlanIsDropped(t *testing.T) {
	wantState := engine.PromotionState{ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production"}
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		return wantState, stubDriveFn, nil
	}}
	m := sizedWithPromotion(t, promo)

	// Push the plan screen (mirrors TestPromotePushesPlanScreen) so plan.BackMsg has
	// something real to pop.
	m, _ = m.Update(matrix.OpenPlanMsg{Source: "app-staging"})
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after opening the plan screen, want 2", n)
	}

	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	built := cmd() // the request completes...

	// ...but before its result is delivered, the operator backs out of the plan screen
	// (Esc — plan.Model emits BackMsg for this key regardless of screen state).
	m, _ = m.Update(plan.BackMsg{})
	if n := len(m.(Model).stack); n != 1 {
		t.Fatalf("plan.BackMsg left stack at %d screens, want 1 (popped back to the matrix)", n)
	}

	m, cmd = m.Update(built)
	if cmd != nil {
		t.Errorf("a stale promotionBuiltMsg produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("stack changed to %d screens processing a stale promotionBuiltMsg, want unchanged at 1 (no flight screen pushed for an abandoned plan)", n)
	}
}

// TestStalePromotionBuiltMsgFromSupersededStartMsgIsDropped is PR #50 round-4 review finding
// #4 (Codex): the plan screen has no "confirming" indicator once Enter fires StartMsg, so
// nothing stops the operator pressing Enter again before the first request's result arrives.
// Without a generation check, whichever of the two overlapping requests happened to resolve
// last was adopted unconditionally — risking two independent flight screens (two goroutines)
// driving the very same promotion at once. This proves only the second (current) request's
// result is ever adopted: the first, superseded one is dropped even though it is the one
// actually delivered to Update.
func TestStalePromotionBuiltMsgFromSupersededStartMsgIsDropped(t *testing.T) {
	first := engine.PromotionState{ID: "first-request", SourceEnv: "app-staging", TargetEnv: "app-production"}
	second := engine.PromotionState{ID: "second-request", SourceEnv: "app-staging", TargetEnv: "app-production"}
	stubDriveFn := func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		return s, true, nil, nil
	}
	calls := 0
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		calls++
		if calls == 1 {
			return first, stubDriveFn, nil
		}
		return second, stubDriveFn, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}

	// First confirmation: request issued, not yet resolved.
	m, cmd1 := m.Update(msg)
	if cmd1 == nil {
		t.Fatal("first StartMsg produced no command")
	}
	firstResult := cmd1()

	// Second confirmation, before the first ever resolved (the plan screen is still on top
	// and still fully interactive): supersedes the first.
	m, cmd2 := m.Update(msg)
	if cmd2 == nil {
		t.Fatal("second StartMsg produced no command")
	}
	secondResult := cmd2()

	// The first (now-stale) result arrives first: must be dropped, not pushed.
	before := len(m.(Model).stack)
	m, cmd := m.Update(firstResult)
	if cmd != nil {
		t.Errorf("the stale first result produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Fatalf("stack changed to %d screens processing the stale first result, want unchanged at %d", n, before)
	}

	// The second (current) result arrives: must be adopted.
	m, _ = m.Update(secondResult)
	if n := len(m.(Model).stack); n != before+1 {
		t.Fatalf("stack has %d screens after the current second result, want %d (flight screen pushed)", n, before+1)
	}
	if v := plain(m); !strings.Contains(v, second.ID) {
		t.Errorf("flight screen view missing the second request's own state ID %q:\n%s", second.ID, v)
	}
	if v := plain(m); strings.Contains(v, first.ID) {
		t.Errorf("flight screen view shows the superseded first request's state ID %q:\n%s", first.ID, v)
	}
}

// TestStartMsgFiltersToTickedRepos is PR #50 review finding #2: plan.StartMsg.Ticked is the
// repo subset the operator actually left checked (the same set plan.Model.recomputeDiff
// already filters the confirm screen's own diff by), but msg.Plan carries BuildPlan's full,
// unfiltered edit set. Without filterTicked, startPromotion would be called with every edit
// in the plan — including a repo the operator explicitly unticked and never saw in the
// confirmed diff — and would commit it anyway. This proves only the ticked repo's edit
// reaches startPromotion, never the unticked one.
func TestStartMsgFiltersToTickedRepos(t *testing.T) {
	keep := image.Ref{Repo: "ghcr.io/example/keep", Tag: "v2"}
	drop := image.Ref{Repo: "ghcr.io/example/drop", Tag: "v2"}
	edits := []gitops.Edit{
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: keep.Repo, Tag: "v1"}}, New: keep},
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: drop.Repo, Tag: "v1"}}, New: drop},
	}
	var gotPlan gitops.Plan
	called := false
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		gotPlan = p
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production", Edits: edits},
		Mode:   plan.ModePR,
		Ticked: []string{keep.Repo},
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	cmd()
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	if len(gotPlan.Edits) != 1 {
		t.Fatalf("startPromotion called with %d edits, want exactly 1 (only the ticked repo): %+v", len(gotPlan.Edits), gotPlan.Edits)
	}
	if got := gotPlan.Edits[0].Ref.Repo; got != keep.Repo {
		t.Errorf("startPromotion's one edit is for repo %q, want the ticked repo %q", got, keep.Repo)
	}
}

// TestStartMsgFiltersWarningsToTickedRepos is PR #50 round-4 review finding #3 (Copilot):
// filterTicked narrowed Plan.Edits to the operator's ticked selection but left Plan.Warnings
// untouched, so engine.RenderPRBody (which renders p.Warnings verbatim) could describe a repo
// the operator explicitly unticked and never saw change in the confirmed diff. This proves a
// warning about the dropped repo never reaches startPromotion, a warning about the kept repo
// does, and a warning tied to no repo at all (zero Occurrences — plan.WarningRepo's own "no
// convention produces this today, but nothing forbids it" case) is kept regardless, since
// there is no ticked/unticked repo to test it against.
func TestStartMsgFiltersWarningsToTickedRepos(t *testing.T) {
	keep := image.Ref{Repo: "ghcr.io/example/keep", Tag: "v2"}
	drop := image.Ref{Repo: "ghcr.io/example/drop", Tag: "v2"}
	edits := []gitops.Edit{
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: keep.Repo, Tag: "v1"}}, New: keep},
		{Occurrence: gitops.Occurrence{Ref: image.Ref{Repo: drop.Repo, Tag: "v1"}}, New: drop},
	}
	warnings := []gitops.Warning{
		{Code: "keep-warning", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: keep.Repo}}}},
		{Code: "drop-warning", Occurrences: []gitops.Occurrence{{Ref: image.Ref{Repo: drop.Repo}}}},
		{Code: "no-repo-warning"},
	}
	var gotPlan gitops.Plan
	called := false
	promo := Promotion{Start: func(_ context.Context, p gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		called = true
		gotPlan = p
		return engine.PromotionState{ID: "abcd1234"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production", Edits: edits, Warnings: warnings},
		Mode:   plan.ModePR,
		Ticked: []string{keep.Repo},
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	cmd()
	if !called {
		t.Fatal("the command never called the wired startPromotion")
	}
	var codes []string
	for _, w := range gotPlan.Warnings {
		codes = append(codes, w.Code)
	}
	if len(gotPlan.Warnings) != 2 {
		t.Fatalf("startPromotion called with %d warnings, want exactly 2 (kept repo + no-repo): %v", len(gotPlan.Warnings), codes)
	}
	for _, want := range []string{"keep-warning", "no-repo-warning"} {
		found := false
		for _, c := range codes {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("warnings %v missing %q", codes, want)
		}
	}
	for _, c := range codes {
		if c == "drop-warning" {
			t.Errorf("warnings %v still carry drop-warning for the unticked repo", codes)
		}
	}
}

// TestStartMsgRefusesDirectModeNotWired is PR #50 review finding #3: direct mode's real
// step-selection machinery (M6/PR #43's engine.DirectCommitStep) is not present on this
// branch — buildStartPromotion (cmd/hoist/wiring.go) always builds engine.AllSteps regardless
// of what the operator chose, and StartPromotionFunc's signature carries no Mode at all. A
// StartMsg confirmed with Mode: plan.ModeDirect must therefore never reach startPromotion —
// silently driving PR mode instead would mean the confirm screen told the operator "commit
// straight to the branch, no PR" and then opened one anyway.
// A direct-mode confirm reaches the start function AS direct. The TUI used to refuse this
// outright, because nothing downstream could honour it: StartPromotionFunc had no way to carry
// the choice, and buildStartPromotion always built the PR step list. Confirming would have told
// the operator "commit straight to the branch, no PR" and then opened a PR — so refusing was
// the honest option at the time. StartOpts carries it now, and AllDirectSteps honours it.
//
// Confirmed rides along with Direct because reaching ModeDirect already required the plan
// screen's own keypress-then-huh.Confirm gesture, which is exactly what
// engine.DirectCommitGateStep asks Confirmed to attest — and the gate re-derives the production
// refusal independently regardless.
func TestStartMsgCarriesDirectModeThrough(t *testing.T) {
	var got StartOpts
	called := false
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		called, got = true, opts
		return engine.PromotionState{}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{
		Plan:   gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"},
		Mode:   plan.ModeDirect,
		Source: "app-staging",
		Target: "app-production",
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("direct-mode StartMsg produced no command; the confirm should start a promotion")
	}
	cmd() // the start call happens off the Update stack
	if !called {
		t.Fatal("startPromotion was never called for a direct-mode confirm")
	}
	if !got.Direct {
		t.Error("StartOpts.Direct = false for a ModeDirect confirm: the promotion would open a PR the operator declined")
	}
	if !got.Confirmed {
		t.Error("StartOpts.Confirmed = false: the gate would refuse a gesture the operator actually completed")
	}
}

// The PR path must not accidentally inherit direct mode.
func TestStartMsgPRModeIsNotDirect(t *testing.T) {
	var got StartOpts
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		got = opts
		return engine.PromotionState{}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	_, cmd := m.Update(plan.StartMsg{
		Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"},
		Mode: plan.ModePR, Source: "app-staging", Target: "app-production",
	})
	if cmd == nil {
		t.Fatal("PR-mode StartMsg produced no command")
	}
	cmd()
	if got.Direct || got.Confirmed {
		t.Errorf("PR mode leaked direct opts: %+v", got)
	}
}

// TestPromotionBuiltMsgNilDriveFnShowsNotice is Copilot's PR #50 review finding: a
// promotionBuiltMsg with err == nil but driveFn == nil (a contract violation by whatever built
// it) must not silently push a read-only flight screen — that would reintroduce exactly the
// pre-wiring stub behavior this PR exists to remove, with no visible sign anything is wrong.
func TestPromotionBuiltMsgNilDriveFnShowsNotice(t *testing.T) {
	m := sized(t)
	before := len(m.(Model).stack)
	msg := promotionBuiltMsg{state: engine.PromotionState{ID: "abcd1234"}, driveFn: nil, err: nil}
	m, cmd := m.Update(msg)
	if cmd != nil {
		t.Errorf("nil-driveFn promotionBuiltMsg produced a command: %#v", cmd())
	}
	if n := len(m.(Model).stack); n != before {
		t.Errorf("stack changed from %d to %d screens; a nil driveFn must not push the flight screen", before, n)
	}
	if v := plain(m); !strings.Contains(v, "no way to drive it") {
		t.Errorf("view missing the nil-driveFn notice:\n%s", v)
	}
}

// TestStartMsgShowsNoticeOnBuildError: buildPromotionForConfirm's own refusals (a real
// in-flight conflict, missing github config, a claim failure) must surface as a notice on the
// screen that popped up plan.StartMsg (plan, still on top — the flight screen is never
// pushed) rather than crashing.
func TestStartMsgShowsNoticeOnBuildError(t *testing.T) {
	wantErr := errors.New("promotion existing-id targeting app-production is still in flight (at pr-opened: open); run `hoist resume existing-id` instead of starting a second one")
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
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

// TestStartMsgBoundedByPollDeadline: mirrors flight.Model's own TestDriveCmdBoundedByPollDeadline
// — a startPromotion call that hangs (blocks on ctx.Done() rather than ever returning) must not
// stall the plan screen forever. m.poll.Deadline bounds the call the same way it bounds
// flight.Model.driveCmd's own DriveFunc call, so this returns with ctx's deadline error instead
// of the goroutine blocking indefinitely.
func TestStartMsgBoundedByPollDeadline(t *testing.T) {
	hung := func(ctx context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		<-ctx.Done()
		return engine.PromotionState{}, nil, ctx.Err()
	}
	promo := Promotion{Start: hung, Poll: flight.PollDurations{Deadline: 20 * time.Millisecond}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg with a wired startPromotion produced no command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case built := <-done:
		pbm, ok := built.(promotionBuiltMsg)
		if !ok {
			t.Fatalf("command yields %T, want promotionBuiltMsg", built)
		}
		if !errors.Is(pbm.err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", pbm.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartMsg's command did not return within 2s of a 20ms poll.Deadline — a hung startPromotion call can still stall the plan screen forever")
	}
}

// TestBackingOutCancelsOutstandingBuild is Copilot's PR #50 final-round finding: buildGen alone
// (the pre-existing guard) only ever stops an abandoned build's eventual RESULT from being
// acted on — it does nothing to the goroutine itself, which used to run to completion
// regardless, bounded only by poll.Deadline (often hours), potentially still claiming and
// persisting a real, orphaned "in-flight" promotion long after the operator backed out and
// moved on to something else. This proves plan.BackMsg actually interrupts the outstanding
// startPromotion call's own context, not just its result.
func TestBackingOutCancelsOutstandingBuild(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, nil, ctx.Err()
	}
	promo := Promotion{Start: hung}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("StartMsg produced no command")
	}
	go cmd()

	m, _ = m.Update(plan.BackMsg{})
	if m.(Model).buildCancel != nil {
		t.Error("buildCancel should be cleared after BackMsg cancels it")
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("build's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backing out did not cancel the outstanding build's context within 2s")
	}
}

// TestSupersedingStartMsgCancelsPreviousBuild is TestBackingOutCancelsOutstandingBuild's sibling
// for the OTHER way a build gets abandoned: a second confirmation (a new plan.StartMsg) before
// the first one's result ever arrives. The first call's own context must be cancelled too, not
// merely left to run out its full poll.Deadline unwatched.
func TestSupersedingStartMsgCancelsPreviousBuild(t *testing.T) {
	firstErr := make(chan error, 1)
	callCount := 0
	promo := Promotion{Start: func(ctx context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		callCount++
		if callCount == 1 {
			<-ctx.Done()
			firstErr <- ctx.Err()
			return engine.PromotionState{}, nil, ctx.Err()
		}
		return engine.PromotionState{ID: "second"}, nil, nil
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd1 := m.Update(msg)
	if cmd1 == nil {
		t.Fatal("first StartMsg produced no command")
	}
	go cmd1()

	_, cmd2 := m.Update(msg)
	if cmd2 == nil {
		t.Fatal("second StartMsg produced no command")
	}
	// The second call's own result isn't this test's concern, only the first's cancellation.

	select {
	case err := <-firstErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("first build's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a superseding StartMsg did not cancel the first build's context within 2s")
	}
}

// TestFlightScreenSharesBuildDeadlineWithDrive is Copilot's PR #50 round-7 finding: flight.New
// used to be handed the raw m.poll.Deadline and would start a FRESH poll.Deadline-length window
// of its own from the moment it was constructed — after the build step (this StartMsg's own
// startPromotion call) had already spent some of the SAME configured budget. The CLI path wraps
// build+drive under one ctx.WithTimeout, so the TUI's total wait exceeding poll.deadline is a
// real divergence. This proves the opposite: a startPromotion call that consumes most of a tiny
// deadline still leaves the flight screen's own drive call bounded by only whatever's left, not
// a fresh full window — the drive call (which blocks forever on its own ctx.Done() otherwise)
// must report context.DeadlineExceeded almost immediately, not after another full poll.Deadline.
func TestFlightScreenSharesBuildDeadlineWithDrive(t *testing.T) {
	const total = 200 * time.Millisecond
	const buildSleep = 150 * time.Millisecond // leaves ~50ms of the budget for drive
	hungDrive := func(ctx context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		return s, false, nil, ctx.Err()
	}
	promo := Promotion{
		Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
			time.Sleep(buildSleep)
			return engine.PromotionState{ID: "abcd1234"}, hungDrive, nil
		},
		Poll: flight.PollDurations{Deadline: total},
	}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	built := cmd() // runs Start's own buildSleep synchronously in this goroutine
	m, fsInitCmd := m.Update(built)
	if fsInitCmd == nil {
		t.Fatal("pushing the flight screen produced no Init command")
	}
	batch, ok := fsInitCmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("flight screen's Init = %#v, want a 2-command batch (spinner tick, drive)", fsInitCmd())
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- batch[1]() }()
	select {
	case driveMsg := <-done:
		m, _ = m.Update(driveMsg)
		if v := plain(m); !strings.Contains(v, "deadline exceeded") {
			t.Errorf("view missing a deadline-exceeded notice after the shared budget ran out:\n%s", v)
		}
	case <-time.After(120 * time.Millisecond): // comfortably above the ~50ms shared remainder,
		// comfortably below a fresh, unshared 200ms window measured from roughly this same point
		t.Fatal("flight screen's own drive call did not report the shared deadline in time — poll.Deadline was NOT shared with the build step")
	}
}

// TestStartMsgErrorNoticeIsRedacted: buildPromotionForConfirm's error can embed a git/forge
// transport message carrying a credential (a token in a remote URL, say) — the root notice must
// scrub it the same way plan.Model.View and flight.Model.View already redact their own rendered
// output, rather than leaking it to the terminal unredacted.
func TestStartMsgErrorNoticeIsRedacted(t *testing.T) {
	const secret = "ghp_totallysecrettoken1234567890"
	redact.Register(secret)
	promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, _ StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		return engine.PromotionState{}, nil, fmt.Errorf("push failed: authentication using %s rejected", secret)
	}}
	m := sizedWithPromotion(t, promo)
	msg := plan.StartMsg{Plan: gitops.Plan{SourceEnv: "app-staging", TargetEnv: "app-production"}}
	m, cmd := m.Update(msg)
	m, _ = m.Update(cmd())
	v := plain(m)
	if strings.Contains(v, secret) {
		t.Errorf("view leaks the registered secret unredacted:\n%s", v)
	}
	if !strings.Contains(v, redact.Redacted) {
		t.Errorf("view missing %q for the redacted notice:\n%s", redact.Redacted, v)
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

// TestFlightOpenPRMsgDisplayModeNeverCallsOpenURL: preferences.open_pr: display must always
// just show the URL as text — never attempting a launch at all, so a headless/SSH session with
// no OpenURL wired in whatsoever (m.openURL nil) still shows the URL rather than the "not wired
// yet" notice display mode has no use for.
func TestFlightOpenPRMsgDisplayModeNeverCallsOpenURL(t *testing.T) {
	called := false
	promo := Promotion{
		OpenPRMode: "display",
		OpenURL:    func(_ string) error { called = true; return nil },
	}
	m := sizedWithPromotion(t, promo)
	m, cmd := m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if cmd != nil {
		t.Error("OpenPRMsg produced a command")
	}
	if called {
		t.Error("display mode called OpenURL — it must never attempt a launch")
	}
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL in display mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgDisplayModeWorksWithNilOpenURL is display mode's own headless-session
// case: no browser opener wired in at all (m.openURL nil, e.g. a real SSH session with nothing
// to launch into) must still show the URL, not the generic "not wired yet" notice launch/both
// modes fall back to for that case.
func TestFlightOpenPRMsgDisplayModeWorksWithNilOpenURL(t *testing.T) {
	promo := Promotion{OpenPRMode: "display"}
	m := sizedWithPromotion(t, promo)
	m, _ = m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	v := plain(m)
	if !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL:\n%s", v)
	}
	if strings.Contains(v, "not wired yet") {
		t.Errorf("view shows the launch-mode not-wired notice in display mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgBothModeShowsURLOnSuccess: preferences.open_pr: both must show the URL as
// text even when the launch itself succeeds — the whole point of "both" over plain "launch" is
// a copy/paste fallback that exists unconditionally, not only on failure.
func TestFlightOpenPRMsgBothModeShowsURLOnSuccess(t *testing.T) {
	promo := Promotion{
		OpenPRMode: "both",
		OpenURL:    func(_ string) error { return nil },
	}
	m := sizedWithPromotion(t, promo)
	m, _ = m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	if v := plain(m); !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL after a successful launch in both mode:\n%s", v)
	}
}

// TestFlightOpenPRMsgBothModeShowsURLAndErrorOnFailure: both mode's failure path must still
// name the URL (so the operator can act on it manually) alongside the launch error, not just
// the bare error a plain "launch" mode shows.
func TestFlightOpenPRMsgBothModeShowsURLAndErrorOnFailure(t *testing.T) {
	promo := Promotion{
		OpenPRMode: "both",
		OpenURL:    func(_ string) error { return errors.New("no such browser") },
	}
	m := sizedWithPromotion(t, promo)
	m, _ = m.Update(flight.OpenPRMsg{URL: "https://example.invalid/pr/1"})
	v := plain(m)
	if !strings.Contains(v, "https://example.invalid/pr/1") {
		t.Errorf("view missing the URL after a failed launch in both mode:\n%s", v)
	}
	if !strings.Contains(v, "no such browser") {
		t.Errorf("view missing the launch error in both mode:\n%s", v)
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

// TestFlightAbortMsgCancelsInFlightDriveCmd is Copilot's PR #50 round-11 finding: popping the
// flight screen used to leave any driveCmd already in flight running to completion, free to
// keep committing, pushing, opening a PR, or merging after the operator had walked away — and
// since the claim was already released once the initial state saved, a later reconfirmation of
// the same deterministic promotion id could start a second driver racing the first. This proves
// AbortMsg actually cancels the popped screen's own drive context, not just its message.
func TestFlightAbortMsgCancelsInFlightDriveCmd(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, false, nil, ctx.Err()
	}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, hung)}
	initCmd := fs.Init()
	if initCmd == nil {
		t.Fatal("setup: flight screen's Init produced no command")
	}
	root = root.push(fs)
	go runBatch(initCmd)

	rootTM, _ := root.Update(flight.AbortMsg{ID: "abcd1234"})
	root = rootTM.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("setup: AbortMsg should return to the matrix: stack has %d screens, want 1", n)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AbortMsg did not cancel the popped flight screen's in-flight driveCmd within 2s")
	}
}

// TestFlightBackMsgCancelsInFlightDriveCmd is TestFlightAbortMsgCancelsInFlightDriveCmd's
// sibling for the other way a flight screen gets popped: pressing Esc (BackMsg), which had the
// exact same gap.
func TestFlightBackMsgCancelsInFlightDriveCmd(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, false, nil, ctx.Err()
	}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, hung)}
	initCmd := fs.Init()
	if initCmd == nil {
		t.Fatal("setup: flight screen's Init produced no command")
	}
	root = root.push(fs)
	go runBatch(initCmd)

	rootTM, _ := root.Update(flight.BackMsg{})
	root = rootTM.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("setup: BackMsg should pop the flight screen: stack has %d screens, want 1", n)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BackMsg did not cancel the popped flight screen's in-flight driveCmd within 2s")
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

// TestDeployNewPushesTagsScreen is M6's tag-picker counterpart to TestPromotePushesPlanScreen:
// d on the matrix screen pushes internal/app/tags on top, and esc from there pops back.
func TestDeployNewPushesTagsScreen(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	msg := cmd()
	if _, ok := msg.(matrix.OpenTagsMsg); !ok {
		t.Fatalf("d's command yields %T, want matrix.OpenTagsMsg", msg)
	}
	m, _ = m.Update(msg)
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after d, want 2", n)
	}
	// No tagsFn was supplied (sized(t) passes nil), so the picker's own error state shows
	// rather than hanging — proving the nil case is handled, not just the happy path.
	if v := plain(m); !strings.Contains(v, "hoist tags:") {
		t.Errorf("tags screen view missing its own header:\n%s", v)
	}
	m, backCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if backCmd == nil {
		t.Fatal("esc on the tags screen produced no command")
	}
	m, _ = m.Update(backCmd())
	if n := len(m.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// TestDirectRequestedMsgShowsHonestNotice and TestSelectedMsgShowsHonestNotice are the
// honesty-fix regression tests for a round-N finding: the root used to pop straight back to the
// matrix on tags.SelectedMsg/DirectRequestedMsg with no notice at all — indistinguishable, at a
// glance, from "that worked" — even though neither message is wired to an actual write yet (no
// screen in this codebase drives a real promotion; hoist promote is still CLI-only). Both must
// still pop back to the matrix (unchanged) AND leave a plain, honest notice on it naming what
// was (and wasn't) done. The window is widened past the default 80 columns so the assertions
// aren't fighting the status bar's own truncation (internal/ui.StatusBar) rather than testing
// the notice's content.
// Choosing a tag now opens the deploy confirm screen rather than popping back with a notice
// saying nothing happened. Both picker messages take the same route: the D gesture chooses a
// mode, not a change, so it still has to look at the diff.
func TestSelectedMsgOpensTheDeployConfirmScreen(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	m, _ = m.Update(cmd())

	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/web",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	stack := m.(Model).stack
	if n := len(stack); n != 2 {
		t.Fatalf("stack has %d screens, want 2 (matrix + deploy confirm)", n)
	}
	if _, ok := stack[len(stack)-1].(deployScreen); !ok {
		t.Fatalf("top screen is %T, want the deploy confirm", stack[len(stack)-1])
	}
	v := plain(m)
	if !strings.Contains(v, "v2") || !strings.Contains(v, "app-production") {
		t.Errorf("confirm screen should name the image and the env:\n%s", v)
	}
	// The whole point of the screen: the bytes are on it before anything is written.
	if !strings.Contains(v, "image:") {
		t.Errorf("confirm screen should show the diff:\n%s", v)
	}
}

// A tag with no occurrence in the target env cannot be deployed. The operator gets the reason
// on the matrix rather than an empty confirm screen.
func TestSelectedMsgReportsAnUndeployableChoice(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	m, _ = m.Update(cmd())

	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/not-in-this-env",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	if n := len(m.(Model).stack); n != 1 {
		t.Fatalf("an undeployable choice should return to the matrix: stack has %d screens", n)
	}
	if v := plain(m); !strings.Contains(v, "cannot deploy") {
		t.Errorf("notice should say why it cannot be deployed:\n%s", v)
	}
}

// TestWithMatrixNoticeFindsMatrixWhenNotOnTop is finding 6's own regression test (round N,
// Copilot): withMatrixNotice's doc comment says it sets the notice on the matrix screen
// "wherever it actually sits in the stack", but an earlier revision only ever checked
// m.stack[top] — true only because every real caller today happens to reach it exactly
// there (tags.SelectedMsg/DirectRequestedMsg pop straight back to a matrix that's always
// immediately below, since matrix.OpenTagsMsg is the only thing that ever pushes a tags
// screen). Build a stack where the matrix is deliberately NOT on top — matrix, then a plan
// screen pushed on top of it, mirroring TestPromotePushesPlanScreen — and confirm
// withMatrixNotice still finds and updates it rather than silently doing nothing.
func TestWithMatrixNoticeFindsMatrixWhenNotOnTop(t *testing.T) {
	m := sized(t)
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	m, _ = m.Update(cmd())
	stack := m.(Model).stack
	if n := len(stack); n != 2 {
		t.Fatalf("stack has %d screens after p, want 2", n)
	}
	if _, ok := stack[len(stack)-1].(matrixScreen); ok {
		t.Fatal("fixture precondition: the plan screen, not the matrix, must be on top")
	}

	const notice = "test-notice-not-on-top"
	m2 := m.(Model).withMatrixNotice(notice)
	m3 := m2.pop() // drop the plan screen back off to see the matrix's own view
	if v := plain(m3); !strings.Contains(v, notice) {
		t.Fatalf("withMatrixNotice should have found the matrix even though it wasn't on top:\n%s", v)
	}
}

// drainTags runs a tea.Cmd (and every cmd it in turn produces) to completion, exactly as a
// real tea.Program would deliver messages to the whole app model — needed here because the
// tags screen's Init kicks off an async ListFunc/MetaFunc load (internal/app/tags.Model.Init),
// which TestDeployNewPushesTagsScreen never exercises (its nil tagsFn errors synchronously).
// Mirrors internal/app/tags/model_test.go's own drain helper, one level up the screen stack.
func drainTags(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			var next tea.Cmd
			for _, c := range batch {
				if c == nil {
					continue
				}
				sub := c()
				if _, isTick := sub.(spinner.TickMsg); isTick {
					continue
				}
				var mc tea.Cmd
				m, mc = m.Update(sub)
				next = tea.Batch(next, mc)
			}
			cmd = next
			continue
		}
		if _, isTick := msg.(spinner.TickMsg); isTick {
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
}

// TestDeployNewThreadsRealStagingTag is the regression test for AGENTS.md invariant 4 / M6's
// production/staging tag-mismatch warning, guarding against the exact bug a sibling M6 attempt
// shipped: its matrix.OpenTagsMsg handler called tags.New with the paired staging env's
// currently-running tag hardcoded to "", so the mismatch note could never render from the real
// running app (only from a unit test calling tags.New directly). This test drives the real
// app-wiring path — app.Model.Update's own matrix.OpenTagsMsg case, not a direct tags.New call
// — with a config naming app-staging as app-production's pair, against the fixture repo where
// app-production/web and app-staging/web genuinely carry different tags
// (v202601010101/v202602150930). If a future change reintroduces a hardcoded "" (or otherwise
// drops the real lookup), hasStagingMismatch/stagingTag never reach tags.New with real values,
// the note never renders, and this test fails.
func TestDeployNewThreadsRealStagingTag(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	envs := config.EnvsConfig{
		Production: []string{"app-production"},
		Pairs:      map[string]string{"app-staging": "app-production"},
	}
	tagsFn := func(string) (bool, tags.ListFunc, tags.MetaFunc) {
		listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
			return []string{"v202601010101"}, nil, false, nil
		}
		metaFn := func(_ context.Context, _ string) (registry.ImageMeta, error) {
			return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("a", 64)}, nil
		}
		return false, listFn, metaFn
	}
	m := New(r, []string{"ghcr.io/"}, envs, nil, Promotion{}, tagsFn)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	msg := matrix.OpenTagsMsg{ImageRepo: "ghcr.io/example/web", Target: "app-production"}
	tm, cmd := tm.Update(msg)
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after OpenTagsMsg, want 2", n)
	}
	tm = drainTags(tm, cmd)

	v := plain(tm)
	if !strings.Contains(v, "app-staging") || !strings.Contains(v, "v202602150930") {
		t.Errorf("tags screen view is missing the real staging-mismatch note (want app-staging / v202602150930, the fixture's actual app-staging/web tag):\n%s", v)
	}
}

// TestQuitKeyTypedIntoTagsFilterDoesNotQuit is round 5's finding 3 regression test: the root's
// global quit binding used to run before the top screen's own key handling ever saw the press,
// so typing "q" into the tag picker's filter box quit the whole program instead of landing in
// the filter query. Drives the real app-wiring path (matrix.OpenTagsMsg through app.Model.Update,
// like TestDeployNewThreadsRealStagingTag above), opens the filter with "/", then types "q".
func TestQuitKeyTypedIntoTagsFilterDoesNotQuit(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	tagsFn := func(string) (bool, tags.ListFunc, tags.MetaFunc) {
		listFn := func(context.Context) ([]string, []forge.GitTag, bool, error) {
			return []string{"v1", "v2"}, nil, false, nil
		}
		metaFn := func(_ context.Context, _ string) (registry.ImageMeta, error) {
			return registry.ImageMeta{Digest: "sha256:" + strings.Repeat("a", 64)}, nil
		}
		return false, listFn, metaFn
	}
	m := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, tagsFn)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: width, Height: height})

	msg := matrix.OpenTagsMsg{ImageRepo: "ghcr.io/example/web", Target: "app-production"}
	tm, cmd := tm.Update(msg)
	tm = drainTags(tm, cmd)

	tm, _ = tm.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	tm, cmd = tm.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("typing q into the tag picker's filter quit the program")
		}
	}
	if v := plain(tm); !strings.Contains(v, "filter: > q") {
		t.Errorf("filter should have gained a q character, view:\n%s", v)
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

// Confirming on the deploy screen starts a real promotion, and carries the mode with it. This
// is the end of the path the tag picker used to dead-end: matrix -> d -> picker -> confirm ->
// a write.
func TestDeployStartMsgStartsAPromotionWithItsMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		confirmed  bool
		wantDirect bool
	}{
		{"PR mode", deploy.ModePR, false, false},
		{"direct mode", deploy.ModeDirect, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got StartOpts
			called := false
			promo := Promotion{Start: func(_ context.Context, _ gitops.Plan, opts StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
				called, got = true, opts
				return engine.PromotionState{}, nil, nil
			}}
			m := sizedWithPromotion(t, promo)
			_, cmd := m.Update(deploy.StartMsg{
				Plan:      gitops.Plan{Variant: gitops.VariantDeploy, TargetEnv: "app-production"},
				Mode:      tc.mode,
				Confirmed: tc.confirmed,
				Target:    "app-production",
				Image:     "ghcr.io/example/web:v2",
			})
			if cmd == nil {
				t.Fatal("confirming a deploy produced no command")
			}
			cmd()
			if !called {
				t.Fatal("startPromotion was never called for a confirmed deploy")
			}
			if got.Direct != tc.wantDirect {
				t.Errorf("StartOpts.Direct = %v, want %v", got.Direct, tc.wantDirect)
			}
			if got.Confirmed != tc.confirmed {
				t.Errorf("StartOpts.Confirmed = %v, want %v", got.Confirmed, tc.confirmed)
			}
		})
	}
}

// TestEscOnTheDeployScreenPopsIt: the deploy screen emits deploy.BackMsg on Esc, but the root
// had no case for it, so the message was forwarded to the top screen — the deploy screen — which
// fed it to its own viewport. Esc did nothing and the screen could not be left (Copilot, PR #72).
func TestEscOnTheDeployScreenPopsIt(t *testing.T) {
	m := sized(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 300, Height: height})
	m, cmd := press(t, m, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	m, _ = m.Update(cmd())
	m, _ = m.Update(tags.SelectedMsg{
		ImageRepo: "ghcr.io/example/web",
		Tag:       "v2",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Target:    "app-production",
	})
	if n := len(m.(Model).stack); n != 2 {
		t.Fatalf("fixture precondition: stack has %d screens, want 2", n)
	}

	// Esc goes to the screen, which asks the root to pop it; the root must act on that ask.
	m2, escCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if escCmd == nil {
		t.Fatal("esc on the deploy screen produced no command")
	}
	m3, _ := m2.Update(escCmd())
	if n := len(m3.(Model).stack); n != 1 {
		t.Fatalf("esc left %d screens on the stack, want 1 (back to the matrix)", n)
	}
}
