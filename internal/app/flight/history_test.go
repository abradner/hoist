package flight

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
)

var historyNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// multiPollSnapshot is a promotion two minutes old that has been polled many times. It is a
// control for exactly one property: History is what engine.appendHistory really writes for
// repeated polls — four `acted`; the NEXT walk appends `already satisfied` once per step and the
// CI wait once more (the wait is no longer the last entry, so it is a return, not a repeat);
// every walk after that appends nothing until the wait's detail changes (2/4 -> 3/4) — and the
// controller's log holds preflight lines ONLY, all older than the first History entry. A
// fixture that mirrored History into the log, or listed History out of order, would read
// differently here (AGENTS.md §9 entry 9: a golden of a fixture that exhibits the bug archives
// the bug).
func multiPollSnapshot() session.Snapshot {
	at := func(secs int) time.Time { return historyNow.Add(-time.Duration(secs) * time.Second) }
	s := fixtureState()
	s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
	s.History = []engine.HistoryEntry{
		{Step: engine.StepBranched, At: at(110), Detail: "acted"},
		{Step: engine.StepCommitted, At: at(105), Detail: "acted"},
		{Step: engine.StepPushed, At: at(100), Detail: "acted"},
		{Step: engine.StepPROpened, At: at(95), Detail: "acted"},
		{Step: engine.StepCIGreen, At: at(90), Detail: "waiting: CI: 2/4 checks complete"},
		{Step: engine.StepBranched, At: at(60), Detail: "already satisfied: worktree already present"},
		{Step: engine.StepCommitted, At: at(60), Detail: "already satisfied: commit already on the branch"},
		{Step: engine.StepPushed, At: at(60), Detail: "already satisfied: branch already on origin"},
		{Step: engine.StepPROpened, At: at(60), Detail: "already satisfied: PR #103 already open"},
		{Step: engine.StepCIGreen, At: at(60), Detail: "waiting: CI: 2/4 checks complete"},
		{Step: engine.StepCIGreen, At: at(30), Detail: "waiting: CI: 3/4 checks complete"},
	}
	snap := stepping(s, false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #103"}),
		st(engine.StepCIGreen, engine.Observation{Waiting: true, Detail: "CI: 3/4 checks complete"}),
	})
	snap.Log = []session.LogLine{
		{At: at(125), Text: "checking your checkout against origin/main"},
		{At: at(120), Text: "claiming app-production"},
		{At: at(115), Text: "saving promotion state"},
	}
	snap.Phase = session.Waiting
	snap.Busy = false
	snap.NextPoll = historyNow.Add(12 * time.Second)
	return snap
}

func multiPollModel() Model {
	return NewAttached(multiPollSnapshot(), PollDurations{}).WithNow(func() time.Time { return historyNow }).SetStyles(ui.NewStyles(true))
}

// TestLogViewIsOneChronologicalListEachEventOnce: preflight lines first, every History entry
// exactly once with its plain step label, times never going back up, and no raw engine step
// name or driver "<step>: <detail>" echo anywhere.
func TestLogViewIsOneChronologicalListEachEventOnce(t *testing.T) {
	lines := strings.Split(multiPollModel().logView(), "\n")
	if len(lines) != 14 {
		t.Fatalf("log has %d lines, want 14 (3 preflight + 11 history):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range []string{"checking your checkout", "claiming app-production", "saving promotion state"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want preflight line %q first", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[3], "branch") || !strings.Contains(lines[3], "acted") {
		t.Errorf("line 3 = %q, want the first history entry after the preflight lines", lines[3])
	}
	ago := regexp.MustCompile(`^(\d+)(s|m) ago`)
	prev := 1 << 30
	for _, l := range lines {
		if strings.HasPrefix(l, "just now") {
			prev = 0
			continue
		}
		g := ago.FindStringSubmatch(l)
		if g == nil {
			t.Fatalf("line %q does not start with a relative time", l)
		}
		n := 0
		for _, c := range g[1] {
			n = n*10 + int(c-'0')
		}
		if g[2] == "m" {
			n *= 60
		}
		if n > prev {
			t.Errorf("time runs backwards at %q (%ds after %ds ago):\n%s", l, n, prev, strings.Join(lines, "\n"))
		}
		prev = n
	}
	all := strings.Join(lines, "\n")
	for _, raw := range []string{"ci-green", "branched:", "pr-opened"} {
		if strings.Contains(all, raw) {
			t.Errorf("log shows raw engine name %q:\n%s", raw, all)
		}
	}
	// The engine records the 2/4 wait twice (before and after the satisfied block: a return to a
	// stop, not a repeat); everything else once.
	for _, once := range []string{"CI: 3/4 checks complete", "worktree already present"} {
		if c := strings.Count(all, once); c != 1 {
			t.Errorf("%q appears %d times, want exactly once", once, c)
		}
	}
	if c := strings.Count(all, "CI: 2/4 checks complete"); c != 2 {
		t.Errorf("2/4 wait appears %d times, want 2", c)
	}
}

// TestLogViewKeepsHistoryOrderForEqualTimes: the satisfied block and the wait that follows it
// share one timestamp, as entries saved in one walk can. The merge is stable, so they read in
// the order the engine wrote them — the order the walk visited the steps — never reshuffled.
func TestLogViewKeepsHistoryOrderForEqualTimes(t *testing.T) {
	lines := strings.Split(multiPollModel().logView(), "\n")
	var got []string
	for i, l := range lines {
		if strings.Contains(l, "worktree already present") {
			got = lines[i : i+5]
		}
	}
	want := []string{"worktree already present", "commit already on the branch", "branch already on origin", "PR #103 already open", "CI: 2/4"}
	if len(got) != len(want) {
		t.Fatalf("tied block has %d lines, want %d:\n%s", len(got), len(want), strings.Join(lines, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("tied line %d = %q, want it to contain %q", i, got[i], w)
		}
	}
}

// TestLogFollowsItsNewestLine: at the bottom, a new entry is on screen; scrolled up, the
// operator's place is kept (not yanked) and End takes them back. Real keys throughout.
func TestLogFollowsItsNewestLine(t *testing.T) {
	m := multiPollModel().SetSize(80, 24)
	// The viewport is 4 lines tall at 80x24 and the log 14 long: a freshly opened screen shows
	// the tail, not the first screenful.
	if v := m.View(); !strings.Contains(v, "CI: 3/4 checks complete") || strings.Contains(v, "checking your checkout") {
		t.Fatalf("a freshly opened log must show its newest line, not its first:\n%s", v)
	}

	// New entry while at the bottom: it is visible.
	snap := multiPollSnapshot()
	snap.State.History = append(snap.State.History, engine.HistoryEntry{Step: engine.StepApproved, At: historyNow.Add(-10 * time.Second), Detail: "waiting: no approval comment yet"})
	m = m.Mirror(snap)
	if !strings.Contains(m.View(), "no approval comment yet") {
		t.Fatalf("at the bottom, the new entry must follow into view:\n%s", m.View())
	}

	// Scroll up with real keys: a further entry must not move the view.
	m = uitest.Keys(m, Model.Update, "up", "up", "up")
	before := m.log.YOffset()
	snap.State.History = append(snap.State.History, engine.HistoryEntry{Step: engine.StepApproved, At: historyNow.Add(-5 * time.Second), Detail: "waiting: still no approval"})
	m = m.Mirror(snap)
	if m.log.YOffset() != before {
		t.Fatalf("a new entry yanked the scrolled-up view: offset %d -> %d", before, m.log.YOffset())
	}
	if strings.Contains(m.View(), "still no approval") {
		t.Fatalf("the scrolled-up view shows the newest entry; it should have stayed put:\n%s", m.View())
	}
	m = uitest.Keys(m, Model.Update, "end")
	if !strings.Contains(m.View(), "still no approval") {
		t.Fatalf("End must return to the newest line:\n%s", m.View())
	}
}

// TestLogViewMergesByTimeNotBySource: a progress line newer than some History entries (a
// signing wait while a later step is mid-Act) lands between them, not after all of them.
func TestLogViewMergesByTimeNotBySource(t *testing.T) {
	snap := multiPollSnapshot()
	snap.Log = append(snap.Log, session.LogLine{At: historyNow.Add(-98 * time.Second), Text: "waiting for signing approval"})
	m := NewAttached(snap, PollDurations{}).WithNow(func() time.Time { return historyNow }).SetStyles(ui.NewStyles(true))
	lines := strings.Split(m.logView(), "\n")
	for i, l := range lines {
		if strings.Contains(l, "waiting for signing approval") {
			if i != 6 {
				t.Fatalf("signing line at index %d, want 6 (between the push at 100s and the PR at 95s):\n%s", i, strings.Join(lines, "\n"))
			}
			return
		}
	}
	t.Fatal("signing line missing")
}

// TestHeaderStartedUsesTheEarliestLine: the header's "started" counts from the first progress
// line (preflight runs before any History exists), so it never reads later than the oldest line
// in the log below it.
func TestHeaderStartedUsesTheEarliestLine(t *testing.T) {
	m := multiPollModel().SetSize(100, 30)
	if got := m.startedAt(); !got.Equal(historyNow.Add(-125 * time.Second)) {
		t.Fatalf("startedAt = %v, want the first preflight line's time", got)
	}
	if !strings.Contains(m.View(), "started 2m ago") {
		t.Errorf("header does not say started 2m ago:\n%s", m.View())
	}
}

func TestFlightMultiPollHistoryGolden(t *testing.T) {
	m := multiPollModel()
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		uitest.Golden(t, "flight-multipoll", m.SetSize(size[0], size[1]).View(), size[0], size[1])
	}
}
