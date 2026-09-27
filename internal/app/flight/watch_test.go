package flight

import (
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/gitops"
)

// withEdits attaches Edits touching the given families (one occurrence per family, in
// cluster/apps/<env>/<family>/*.yaml shape — AGENTS.md's own glossary) to a screen's state, the
// only input families() reads.
func withEdits(m Model, env string, fams ...string) Model {
	var edits []gitops.Edit
	for _, f := range fams {
		edits = append(edits, gitops.Edit{Occurrence: gitops.Occurrence{File: "cluster/apps/" + env + "/" + f + "/deployment.yaml"}})
	}
	m.state.Edits = edits
	return m
}

// TestWEmitsWatchMsg: a single-family promotion's own w goes straight to WatchMsg naming that
// family and this promotion's target — no dialog.
func TestWEmitsWatchMsg(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = withEdits(m, "app-production", "web")
	m, cmd := m.Update(uitest.Key("w"))
	if cmd == nil {
		t.Fatal("w with one family produced no command")
	}
	msg, ok := cmd().(WatchMsg)
	if !ok || msg.Family != "web" || msg.Target != "app-production" {
		t.Fatalf("w emitted %#v, want WatchMsg{Family: web, Target: app-production}", cmd())
	}
	if m.choosingFamily {
		t.Error("a single family must never raise the chooser")
	}
}

// TestWChoosesWhenSeveralFamilies: several families raises the chooser (huh.Select, the matrix's
// own openChooser shape); enter on the highlighted option emits WatchMsg for it, esc closes with
// no command.
func TestWChoosesWhenSeveralFamilies(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = withEdits(m, "app-production", "web", "worker")
	m, cmd := m.Update(uitest.Key("w"))
	if cmd != nil {
		t.Fatal("w with several families must not emit directly")
	}
	if !m.choosingFamily || m.familyChooser == nil {
		t.Fatal("w with several families must raise the chooser")
	}
	if !m.CapturesText() {
		t.Fatal("the open chooser must capture text")
	}
	m2, cmd2 := m.Update(uitest.Key("down"))
	m3, cmd3 := m2.Update(uitest.Key("enter"))
	if cmd3 == nil {
		t.Fatal("down then enter produced no command")
	}
	_ = cmd2
	msg, ok := cmd3().(WatchMsg)
	if !ok || msg.Family != "worker" || msg.Target != "app-production" {
		t.Fatalf("down then enter emitted %#v, want WatchMsg{Family: worker, Target: app-production}", cmd3())
	}
	if m3.choosingFamily {
		t.Error("the chooser must close once a choice is made")
	}

	// esc closes without emitting.
	e := m
	e, cmd = e.Update(uitest.Key("esc"))
	if cmd != nil || e.choosingFamily {
		t.Errorf("esc should close the chooser silently: cmd=%v choosingFamily=%v", cmd, e.choosingFamily)
	}
}

// TestWChooserEscWhileFilteringClosesOnlyTheFilter is P3 from the T3 review: esc while the
// chooser's own "/" filter was open closed the WHOLE chooser (the same unconditional-esc bug
// P1-2 fixed on the plan screen's own multi-select), instead of huh's own "clear the filter"
// behaviour. Positive control: esc with no filter open still closes the chooser, exactly as
// TestWChoosesWhenSeveralFamilies's own esc case proves.
func TestWChooserEscWhileFilteringClosesOnlyTheFilter(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m = withEdits(m, "app-production", "web", "worker")
	m, _ = m.Update(uitest.Key("w"))
	if !m.choosingFamily {
		t.Fatal("test setup: w with several families did not raise the chooser")
	}
	m, _ = m.Update(uitest.Key("/"))
	if m.familyChooser == nil || !m.familyChooser.GetFiltering() {
		t.Fatal("test setup: \"/\" did not open the chooser's own filter")
	}

	m, cmd := m.Update(uitest.Key("esc"))
	if !m.choosingFamily {
		t.Error("esc while filtering closed the whole chooser, not just the filter")
	}
	if cmd != nil {
		t.Errorf("esc while filtering produced a command: %#v", cmd())
	}
}

// TestWWithNoFamilyShowsNotice: nothing computed from Edits yet (Building, or a state whose
// plan carries none) — w says so rather than opening a chooser over nothing or guessing a
// family.
func TestWWithNoFamilyShowsNotice(t *testing.T) {
	m := NewAttached(building("app-staging", "app-production", false), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	m, cmd := m.Update(uitest.Key("w"))
	if cmd != nil {
		t.Fatal("w with no family produced a command")
	}
	if !contains(m.View(), "nothing to watch yet") {
		t.Errorf("view missing the no-family notice:\n%s", m.View())
	}
}

// TestEnterUnbound: enter does nothing on the flight screen (FB-L4) — there is nothing here to
// confirm the way plan/deploy's own enter starts a drive.
func TestEnterUnbound(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	before := m.View()
	got, cmd := m.Update(uitest.Key("enter"))
	if cmd != nil {
		t.Errorf("enter produced a command: %#v", cmd())
	}
	if got.View() != before {
		t.Errorf("enter changed the view:\nbefore:\n%s\nafter:\n%s", before, got.View())
	}
}

// TestReasonOnceNamesShiftC proves the blocked reason (a real ci.none=prompt block) appears
// EXACTLY once in the whole view — as the blocked row's own indented detail line in the step
// list — and never a second time in the action section below it, which before this PR repeated
// it verbatim ("blocked — <reason>"). It also proves the action section names shift+c, never
// --override-ci-none.
func TestReasonOnceNamesShiftC(t *testing.T) {
	m := blockedOnCINone(t).SetSize(100, 30)
	v := m.View()
	const reason = "no checks reported after the grace period"
	if n := countSubstr(v, reason); n != 1 {
		t.Fatalf("blocked reason appears %d times, want exactly 1:\n%s", n, v)
	}
	if !contains(v, "shift+c") {
		t.Errorf("view must name shift+c:\n%s", v)
	}
	// The engine's OWN reason text legitimately mentions the CLI's --override-ci-none (it is
	// the CLI's real re-run command) — what must never name the flag is THIS screen's own
	// instruction, the action section, which speaks the TUI's own gesture instead.
	if action := m.actionSection(); contains(action, "--override-ci-none") {
		t.Errorf("the action section must never name the CLI flag: %q", action)
	}
}

func countSubstr(s, sub string) int {
	n := 0
	for {
		i := indexOf(s, sub)
		if i < 0 {
			return n
		}
		n++
		s = s[i+len(sub):]
	}
}

// TestWrappedDetailKeepsIndent proves a long blocked-row detail's continuation lines carry the
// same indent as the first (UX-M3) — before this PR, ansi.Wrap alone left every line after the
// first flush against the frame's own left edge.
func TestWrappedDetailKeepsIndent(t *testing.T) {
	snap := stepping(fixtureState(), false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Blocked: "branch hoist/app-production/abcd1234 already exists with different content that runs well past one line at this width"}),
	})
	m := NewAttached(snap, PollDurations{}).SetSize(80, 24).SetStyles(ui.NewStyles(true))
	list := m.stepList()
	lines := splitLines(list)
	first := -1
	for i, l := range lines {
		if contains(l, "already exists") {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("fixture precondition: the detail line was not found:\n%s", list)
	}
	var continuation []string
	for _, l := range lines[first+1:] {
		if len(l) < 4 || l[:4] != "    " {
			break
		}
		continuation = append(continuation, l)
	}
	if len(continuation) == 0 {
		t.Fatal("fixture precondition: the detail must wrap onto a second line")
	}
	for _, l := range continuation {
		if l[:4] != "    " {
			t.Errorf("wrapped continuation line lost its indent: %q", l)
		}
	}
}

// TestHistoryHasNoRFC3339 proves the history section words its timestamps through ui.Ago
// (relative, "4 days ago") rather than an RFC3339 stamp — UX-M4.
func TestHistoryHasNoRFC3339(t *testing.T) {
	m := NewAttached(stepping(fixtureState(), false, nil), PollDurations{}).SetSize(100, 30).SetStyles(ui.NewStyles(true))
	v := m.View()
	if matchRFC3339(v) {
		t.Errorf("view carries an RFC3339 timestamp:\n%s", v)
	}
	if !contains(v, "ago") {
		t.Errorf("view missing a relative time:\n%s", v)
	}
}

// matchRFC3339 is a small, dependency-free stand-in for the design's own regex
// (`\d{4}-\d\d-\d\dT`) — this package's own test files already avoid pulling in regexp for a
// single fixed pattern.
func matchRFC3339(s string) bool {
	for i := 0; i+11 <= len(s); i++ {
		if isDigits(s[i:i+4]) && s[i+4] == '-' && isDigits(s[i+5:i+7]) && s[i+7] == '-' && isDigits(s[i+8:i+10]) && s[i+10] == 'T' {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// TestFooterKeepsEscAt80 proves esc survives at 80 columns even on the ci-none fixture, whose
// footer carries the most hints of any state this screen renders (o, w, l, shift+c, shift+x, r,
// ?) — keys.Footer's own pinned-esc guarantee (UX-H12).
func TestFooterKeepsEscAt80(t *testing.T) {
	m := blockedOnCINone(t).SetSize(80, 24)
	f := m.footer()
	if !contains(f, "esc") {
		t.Fatalf("footer at 80 columns dropped esc: %q", f)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
