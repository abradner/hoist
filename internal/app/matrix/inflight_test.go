package matrix

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
)

var fixedNow = time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)

func sat(step engine.StepName) engine.StepStatus {
	return engine.StepStatus{Step: step, Observation: engine.Observation{Satisfied: true}}
}

func parked(id, source, target string, minutesAgo int) flight.Summary {
	st := engine.PromotionState{
		ID: id, SourceEnv: source, TargetEnv: target,
		PR:      &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"},
		History: []engine.HistoryEntry{{Step: engine.StepBranched, At: fixedNow.Add(-time.Duration(minutesAgo) * time.Minute)}},
	}
	return flight.Summarize(st, false, []engine.StepStatus{
		sat(engine.StepBranched), sat(engine.StepCommitted), sat(engine.StepPushed), sat(engine.StepPROpened), sat(engine.StepCIGreen),
		{Step: engine.StepApproved, Observation: engine.Observation{Waiting: true, Detail: "no approval comment yet"}},
	}, nil)
}

func withPane(w, h int, list ...flight.Summary) Model {
	return New(fixture(), []string{"ghcr.io/"}, config.EnvsConfig{}, nil).WithNow(func() time.Time { return fixedNow }).SetInFlight(list, nil).SetSize(w, h)
}

// The pane's three forms, decided by height: expanded when the rows are there, compact when
// fewer are, folded into the notes as one line when not even that fits. The verdict and the
// id survive every form (docs/tui/mockups.html: "degrade by dropping evidence, never the
// verdict").
func TestInFlightPaneSizesToTheTerminal(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)

	expanded := ansi.Strip(withPane(80, 24, p).View())
	for _, want := range []string{"in flight · 1", "5pr6sd333t   app-staging → app-production", "started 12m ago", "✓ PR #103", "⏸ approval", "· rollout", "waiting for an approver to comment `hoist approve 5pr6sd333t` on PR #103"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded pane lacks %q:\n%s", want, expanded)
		}
	}
	uitest.Golden(t, "matrix-inflight", withPane(80, 24, p).View(), 80, 24)

	compact := ansi.Strip(withPane(80, 19, p).View())
	if !strings.Contains(compact, "⟳ 5pr6sd333t → app-production   blocked on approval · 12m") {
		t.Errorf("compact pane lacks the one-liner:\n%s", compact)
	}
	if strings.Contains(compact, "hoist approve") {
		t.Errorf("compact pane must drop the evidence, keeping the verdict:\n%s", compact)
	}
	uitest.Golden(t, "matrix-inflight", withPane(80, 19, p).View(), 80, 19)

	folded := ansi.Strip(withPane(80, 12, p).View())
	if !strings.Contains(folded, "⟳ 1 in flight: 5pr6sd333t blocked on approval") {
		t.Errorf("with no room for a pane the notes carry the line:\n%s", folded)
	}
	if strings.Contains(folded, "in flight · 1") {
		t.Errorf("no pane should be drawn at 12 rows:\n%s", folded)
	}
	// The table keeps its families whenever a pane is drawn (the pane is the guest); at 12
	// rows nothing could show eight families, pane or not.
	for name, v := range map[string]string{"expanded": expanded, "compact": compact} {
		if !strings.Contains(v, "thirdparty") {
			t.Errorf("%s: the last family fell off the table:\n%s", name, v)
		}
	}
	// Two promotions, wide: both expanded, separated by a rule.
	q := parked("9xy8wv777u", "", "app-staging", 3)
	two := withPane(120, 40, p, q).View()
	if v := ansi.Strip(two); !strings.Contains(v, "in flight · 2") || !strings.Contains(v, "deploy → app-staging") {
		t.Errorf("two promotions:\n%s", v)
	}
	uitest.Golden(t, "matrix-inflight-two", two, 120, 40)
}

func TestInFlightPaneAbsentWhenNothingIsInFlight(t *testing.T) {
	v := ansi.Strip(withPane(80, 24).View())
	if strings.Contains(v, "in flight") {
		t.Fatalf("no pane expected:\n%s", v)
	}
	m := withPane(80, 24).SetInFlight(nil, errors.New("open state dir: permission denied"))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "cannot list promotions: open state dir: permission denied") {
		t.Fatalf("a listing failure is said, not hidden:\n%s", v)
	}
}

// TestTabEnterResumes is T3-04's own replacement for the retired r-as-resume gesture (train3-
// design.md): tab moves the cursor keys onto the in-flight pane, and enter there resumes/re-
// attaches to the promotion under the pane's own cursor — up/down move between several,
// exactly like the grid's own row cursor, rather than a modal chooser.
func TestTabEnterResumes(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	one := withPane(80, 24, p)
	if one.focus != FocusGrid {
		t.Fatalf("setup: focus = %v, want FocusGrid", one.focus)
	}
	one = uitest.Keys(one, update, "tab")
	if one.focus != FocusPane {
		t.Fatal("tab must move the cursor onto the pane when something is in flight")
	}
	if msg, ok := emitted(t, one, "enter").(ResumeMsg); !ok || msg.ID != "5pr6sd333t" {
		t.Fatalf("enter on the pane emitted %+v, want ResumeMsg for 5pr6sd333t", msg)
	}
	if msg, ok := emitted(t, one, "o").(flight.OpenPRMsg); !ok || msg.URL != "https://forge.example.invalid/pr/103" {
		t.Fatalf("o emitted %+v", msg)
	}

	none := withPane(80, 24)
	m, _ := none.Update(uitest.Key("tab"))
	if m.focus != FocusGrid {
		t.Fatal("tab with nothing in flight must not move focus off the grid")
	}

	q := parked("9xy8wv777u", "", "app-staging", 3)
	two := withPane(120, 40, p, q)
	two = uitest.Keys(two, update, "tab", "down")
	if two.paneCursor != 1 {
		t.Fatalf("paneCursor = %d, want 1 after tab+down over two entries", two.paneCursor)
	}
	if msg, ok := emitted(t, two, "enter").(ResumeMsg); !ok || msg.ID != "9xy8wv777u" {
		t.Fatalf("enter on the second pane row emitted %+v, want ResumeMsg for 9xy8wv777u", msg)
	}
}

// A finished promotion is not in flight: the pane lists what is still moving, and tab+enter
// resumes the one active promotion without asking about the done one.
func TestFinishedPromotionsLeaveThePane(t *testing.T) {
	active := parked("5pr6sd333t", "app-staging", "app-production", 12)
	done := parked("0d0n3d0n3d", "app-staging", "app-production", 90)
	done.Done = true
	m := withPane(120, 40, done, active)
	if got := m.InFlight(); len(got) != 1 || got[0].ID != "5pr6sd333t" {
		t.Fatalf("pane holds %+v; want the active one only", got)
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "0d0n3d0n3d") || !strings.Contains(v, "in flight · 1") {
		t.Fatalf("view:\n%s", v)
	}
	m = uitest.Keys(m, update, "tab")
	msg := emitted(t, m, "enter")
	if rm, ok := msg.(ResumeMsg); !ok || rm.ID != "5pr6sd333t" {
		t.Fatalf("enter emitted %+v; want the active promotion resumed", msg)
	}
}

// o with several PRs asks which, and a promotion without a PR is not offered.
func TestOpenPRAsksWhichWhenSeveralHaveOne(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	q := parked("9xy8wv777u", "", "app-staging", 3)
	q.PR = &forge.PR{Number: 104, URL: "https://forge.example.invalid/pr/104"}
	noPR := parked("0n0pr0n0pr", "app-staging", "app-production", 1)
	noPR.PR = nil
	m := withPane(120, 40, p, q, noPR)
	m, _ = m.Update(uitest.Key("o"))
	if m.chooser == nil || m.chooserKind != chooserOpenPR {
		t.Fatal("o with two PRs must ask which")
	}
	// The chooser's own options, not the whole screen: the dimmed pane beneath the dialog
	// still lists every in-flight promotion, the no-PR one included.
	v := ansi.Strip(m.chooser.View())
	if !strings.Contains(v, "open which promotion's PR?") || strings.Contains(v, "0n0pr0n0pr") || !strings.Contains(v, "9xy8wv777u") {
		t.Fatalf("chooser must list only promotions with a PR:\n%s", v)
	}
	m = uitest.Keys(m, update, "down")
	m, cmd := m.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter chose nothing")
	}
	if got, ok := cmd().(flight.OpenPRMsg); !ok || got.URL != "https://forge.example.invalid/pr/104" {
		t.Fatalf("chose %+v; want the second promotion's PR", cmd())
	}
	if m.chooser != nil || m.chooserKind != chooserImage {
		t.Fatal("the chooser must close and reset its kind")
	}
	// esc resets the kind too: the next d chooser must be an image chooser.
	m, _ = m.Update(uitest.Key("o"))
	m, _ = m.Update(uitest.Key("esc"))
	if m.chooser != nil || m.chooserKind != chooserImage {
		t.Fatal("esc must reset the chooser kind")
	}
}

// TestPaneCursorMarksTheSelectedRow proves the pane-row cursor (commit 1 of the T3-06 train)
// only ever marks the entry under paneCursor while focus is on the pane — never the grid's own
// cursor, and never any row when focus is still FocusGrid — at both a golden width and a wide
// one, since paneMarker's width must stay fixed (two cells) either way or every other row would
// shift depending on which one is selected.
func TestPaneCursorMarksTheSelectedRow(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	q := parked("9xy8wv777u", "", "app-staging", 3)

	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		m := withPane(size.w, size.h, p, q)
		if containsPrefixed(strings.Split(ansi.Strip(m.View()), "\n"), "▸ 5pr6sd333t") {
			t.Fatalf("%dx%d: no pane row should carry the cursor while focus is on the grid:\n%s", size.w, size.h, ansi.Strip(m.View()))
		}
		m = uitest.Keys(m, update, "tab") // FocusGrid -> FocusPane, cursor starts on row 0
		if !paneRowHasCursor(m.View(), "5pr6sd333t") {
			t.Fatalf("%dx%d: row 0 must carry the cursor once focus is on the pane:\n%s", size.w, size.h, ansi.Strip(m.View()))
		}
		if paneRowHasCursor(m.View(), "9xy8wv777u") {
			t.Fatalf("%dx%d: only the selected row may carry the cursor:\n%s", size.w, size.h, ansi.Strip(m.View()))
		}
		m = uitest.Keys(m, update, "down")
		if !paneRowHasCursor(m.View(), "9xy8wv777u") || paneRowHasCursor(m.View(), "5pr6sd333t") {
			t.Fatalf("%dx%d: cursor must move to row 1 after down:\n%s", size.w, size.h, ansi.Strip(m.View()))
		}
	}
}

// paneRowHasCursor reports whether the frame row naming id starts (right after the frame's own
// left border) with the pane cursor marker — true for either the expanded or the compact form,
// since the marker sits before "⟳" on a compact row and before the id itself on an expanded one.
func containsPrefixed(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.Contains(l, prefix) {
			return true
		}
	}
	return false
}

func paneRowHasCursor(view, id string) bool {
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		if !strings.Contains(l, id) {
			continue
		}
		content := strings.TrimPrefix(l, "│")
		return strings.HasPrefix(content, "▸")
	}
	return false
}

// TestNextCheckCountdownForAWaitingEntry proves a Waiting entry's own NextPoll is worded as
// "next check in Ns" against a fixed now — the pane's counterpart to flight.Model's identical
// actionSection wording (commit 1 of the T3-06 train) — and that an entry with no NextPoll set
// (Summarize never sets one; only a Live snapshot does, app.go's summaryForSnapshot) shows no
// countdown at all rather than a nonsensical one counting down from the zero time.
// waiting builds a bare Summary with nothing yet Active (Verdict/Action both fall back to their
// short default, "starting") so the countdown text this test cares about is never crowded out
// of a narrow line by an unrelated long approval sentence (parked's own fixture).
func waiting(id, target string, next time.Time) flight.Summary {
	s := flight.Summarize(engine.PromotionState{ID: id, TargetEnv: target}, false, nil, nil)
	s.NextPoll = next
	return s
}

func TestNextCheckCountdownForAWaitingEntry(t *testing.T) {
	p := waiting("5pr6sd333t", "app-production", fixedNow.Add(45*time.Second))
	q := waiting("9xy8wv777u", "app-staging", time.Time{}) // no NextPoll: not a waiting entry

	v := ansi.Strip(withPane(80, 24, p, q).View())
	if !strings.Contains(v, "next check in 45s") {
		t.Fatalf("waiting entry must show its own countdown:\n%s", v)
	}
	if strings.Count(v, "next check in") != 1 {
		t.Fatalf("only the entry with a real NextPoll may show a countdown:\n%s", v)
	}

	// Rounds to the nearest second and never goes negative once the deadline has passed.
	p.NextPoll = fixedNow.Add(400 * time.Millisecond)
	if v := ansi.Strip(withPane(80, 24, p).View()); !strings.Contains(v, "next check in 0s") {
		t.Fatalf("sub-second remainder must round, never show a fraction:\n%s", v)
	}
	p.NextPoll = fixedNow.Add(-5 * time.Second)
	if v := ansi.Strip(withPane(80, 24, p).View()); !strings.Contains(v, "next check in 0s") {
		t.Fatalf("a NextPoll already in the past must clamp to 0s, never negative:\n%s", v)
	}
}

// TestInFlightInsideFrame proves the in-flight pane is drawn INSIDE the matrix's own frame
// (T3-05: Frame.Panes is retired, the pane is a Section like any other) rather than as a
// separately-bordered block stacked below it: the box's own closing border (╰) sits on the
// second-to-last row, with the footer alone on the very last one — never a second, nested
// box's own bottom border appearing anywhere in between.
func TestInFlightInsideFrame(t *testing.T) {
	const h = 30 // tall enough that Frame's own overflow trimming never touches this section
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	view := withPane(80, h, p).View()
	lines := strings.Split(view, "\n")
	if len(lines) != h {
		t.Fatalf("view has %d lines, want %d", len(lines), h)
	}
	if got := ansi.Strip(lines[h-2]); !strings.HasPrefix(got, "╰") {
		t.Fatalf("row h-1 (index %d) = %q, want the frame's own closing border", h-2, got)
	}
	if strings.Contains(ansi.Strip(lines[h-1]), "╰") || strings.Contains(ansi.Strip(lines[h-1]), "╭") {
		t.Fatalf("row h (the footer) must not carry a second box border: %q", ansi.Strip(lines[h-1]))
	}
	// No OTHER row (between the frame's own opening and closing border) carries a border
	// character — a nested ui.Box (the pre-T3-05 shape) would show its own ╭/╰ somewhere in
	// between.
	for i, l := range lines[1 : h-2] {
		plain := ansi.Strip(l)
		if strings.Contains(plain, "╰") || strings.Contains(plain, "╭") {
			t.Fatalf("row %d carries a second (nested) box border: %q", i+2, plain)
		}
	}
}
