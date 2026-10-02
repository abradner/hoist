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
// control for exactly one property: History holds one entry per CHANGE (as engine.appendHistory
// now writes it — the CI wait appears at 2/4 and again at 3/4, nothing in between), and the
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
	if len(lines) != 10 {
		t.Fatalf("log has %d lines, want 10 (3 preflight + 7 history):\n%s", len(lines), strings.Join(lines, "\n"))
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
	for _, once := range []string{"CI: 2/4 checks complete", "CI: 3/4 checks complete", "worktree already present"} {
		if c := strings.Count(all, once); c != 1 {
			t.Errorf("%q appears %d times, want exactly once", once, c)
		}
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
