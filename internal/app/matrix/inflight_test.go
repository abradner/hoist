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
	for _, want := range []string{"in flight (1)", "5pr6sd333t   app-staging → app-production", "started 12m ago", "● PR #103", "◍ approval", "○ rollout", "blocked on you — comment on PR #103 to release it:", "hoist approve 5pr6sd333t"} {
		if !strings.Contains(expanded, want) {
			t.Errorf("expanded pane lacks %q:\n%s", want, expanded)
		}
	}
	uitest.Golden(t, "matrix-inflight", withPane(80, 24, p).View(), 80, 24)

	compact := ansi.Strip(withPane(80, 16, p).View())
	if !strings.Contains(compact, "⟳ 5pr6sd333t → app-production   blocked on approval · 12m") {
		t.Errorf("compact pane lacks the one-liner:\n%s", compact)
	}
	if strings.Contains(compact, "hoist approve") {
		t.Errorf("compact pane must drop the evidence, keeping the verdict:\n%s", compact)
	}
	uitest.Golden(t, "matrix-inflight", withPane(80, 16, p).View(), 80, 16)

	folded := ansi.Strip(withPane(80, 12, p).View())
	if !strings.Contains(folded, "⟳ 1 in flight: 5pr6sd333t blocked on approval") {
		t.Errorf("with no room for a pane the notes carry the line:\n%s", folded)
	}
	if strings.Contains(folded, "in flight (1)") {
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
	if v := ansi.Strip(two); !strings.Contains(v, "in flight (2)") || !strings.Contains(v, "deploy → app-staging") {
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

// r resumes the one in-flight promotion; with several it asks which; with none it says so.
func TestResumeKeys(t *testing.T) {
	p := parked("5pr6sd333t", "app-staging", "app-production", 12)
	one := withPane(80, 24, p)
	for _, k := range []string{"r", "enter"} {
		msg, ok := emitted(t, one, k).(ResumeMsg)
		if !ok || msg.ID != "5pr6sd333t" {
			t.Fatalf("%s emitted %+v, want ResumeMsg for 5pr6sd333t", k, msg)
		}
	}
	if msg, ok := emitted(t, one, "o").(flight.OpenPRMsg); !ok || msg.URL != "https://forge.example.invalid/pr/103" {
		t.Fatalf("o emitted %+v", msg)
	}

	none := withPane(80, 24)
	if got := emitted(t, none, "r"); got != nil {
		t.Fatalf("r with nothing in flight emitted %+v", got)
	}
	m, _ := none.Update(uitest.Key("r"))
	if !strings.Contains(ansi.Strip(m.View()), "nothing in flight to resume") {
		t.Fatal("r with nothing in flight must say so")
	}
	if got := emitted(t, none, "enter"); got != nil {
		t.Fatalf("enter with nothing in flight emitted %+v (it must stay silent: it is the table's key too)", got)
	}

	q := parked("9xy8wv777u", "", "app-staging", 3)
	two := withPane(120, 40, p, q)
	two, _ = two.Update(uitest.Key("r"))
	if two.chooser == nil || two.chooserKind != chooserResume {
		t.Fatal("r with two in flight must ask which")
	}
	if v := ansi.Strip(two.View()); !strings.Contains(v, "resume which promotion?") || !strings.Contains(v, "9xy8wv777u") {
		t.Fatalf("chooser not drawn:\n%s", v)
	}
	two = uitest.Keys(two, update, "down")
	two, cmd := two.Update(uitest.Key("enter"))
	if cmd == nil {
		t.Fatal("enter in the resume chooser emitted nothing")
	}
	if msg, ok := cmd().(ResumeMsg); !ok || msg.ID != "9xy8wv777u" {
		t.Fatalf("chose %+v", cmd())
	}
	if two.chooser != nil || two.chooserKind != chooserImage {
		t.Fatal("the chooser must close and reset")
	}
}
