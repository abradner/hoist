package flight

import (
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
)

// The flight screen at both harness sizes, in the three states an operator stares at:
// parked on approval (the command to type is on screen), blocked, and done — and the
// direct-mode shape, which renders only the steps a direct promotion runs (#85 claimed
// four never-run steps still rendered; this pins that they do not).
func TestFlightGolden(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	base := func() Model {
		s := fixtureState()
		s.PR = &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"}
		return New(s, PollDurations{}, nil).WithNow(now).SetStyles(ui.NewStyles(true))
	}
	parked := base()
	parked.rows = DeriveRows(StepOrder, false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #103"}),
		st(engine.StepCIGreen, engine.Observation{Satisfied: true, Detail: "4/4 checks green"}),
		st(engine.StepApproved, engine.Observation{Waiting: true, Detail: "no approval comment yet"}),
	})
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		uitest.Golden(t, "flight-approval", parked.SetSize(size[0], size[1]).View(), size[0], size[1])
	}
	// The short terminal: the list degrades to the strip and the command survives.
	uitest.Golden(t, "flight-approval", parked.SetSize(80, 12).View(), 80, 12)

	blocked := base()
	blocked.stopped = true
	blocked.rows = DeriveRows(StepOrder, false, []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Blocked: "branch hoist/app-production/abcd1234 already exists with different content"}),
	})
	uitest.Golden(t, "flight-blocked", blocked.SetSize(80, 24).View(), 80, 24)

	done := base()
	done.done = true
	done.rows = DeriveRows(StepOrder, true, []engine.StepStatus{st(engine.StepRolledOut, engine.Observation{Satisfied: true, Detail: "2 deployments rolled out"})})
	uitest.Golden(t, "flight-done", done.SetSize(80, 24).View(), 80, 24)

	s := fixtureState()
	s.Direct, s.SourceEnv = true, ""
	direct := New(s, PollDurations{}, nil).WithNow(now).SetStyles(ui.NewStyles(true))
	direct.rows = DeriveRows(DirectStepOrder, false, []engine.StepStatus{
		st(engine.StepDirectGate, engine.Observation{Satisfied: true}),
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepDirectPushed, engine.Observation{Waiting: true, Detail: "pushing to main"}),
	})
	v := direct.SetSize(80, 24).View()
	for _, never := range []string{"· PR", "· CI", "· approval", "· merge"} {
		if contains(v, never) {
			t.Errorf("a direct promotion must not render %q:\n%s", never, v)
		}
	}
	uitest.Golden(t, "flight-direct", v, 80, 24)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
