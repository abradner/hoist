package flight

import (
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
)

func st(name engine.StepName, obs engine.Observation) engine.StepStatus {
	return engine.StepStatus{Step: name, Observation: obs}
}

// TestDeriveRowsNotYetStarted: nil statuses (a promotion that has not been polled even
// once) — every row not-yet-reached, nothing active.
func TestDeriveRowsNotYetStarted(t *testing.T) {
	rows := DeriveRows(StepOrder, false, nil)
	if len(rows) != len(StepOrder) {
		t.Fatalf("got %d rows, want %d", len(rows), len(StepOrder))
	}
	for _, r := range rows {
		if r.Glyph != GlyphNotReached || r.Active {
			t.Errorf("row %+v: want not-reached, inactive", r)
		}
	}
	if _, ok := ActiveStep(rows); ok {
		t.Error("ActiveStep found one in an all-not-reached list")
	}
}

// TestDeriveRowsMidCIWaiting: branch/commit/push/PR done, CI waiting — the shape the CLI's
// own driveToCompletion loop sees mid-poll while checks are still running.
func TestDeriveRowsMidCIWaiting(t *testing.T) {
	statuses := []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true, Detail: "worktree present"}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true, Detail: "HEAD matches the plan"}),
		st(engine.StepPushed, engine.Observation{Satisfied: true, Detail: "origin/branch up to date"}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true, Detail: "PR #97 already exists"}),
		st(engine.StepCIGreen, engine.Observation{Waiting: true, Detail: "CI: 2/3 checks complete"}),
	}
	rows := DeriveRows(StepOrder, false, statuses)

	byStep := indexRows(rows)
	for _, name := range []engine.StepName{engine.StepBranched, engine.StepCommitted, engine.StepPushed, engine.StepPROpened} {
		r := byStep[name]
		if r.Glyph != GlyphDone || r.Active {
			t.Errorf("%s: got %+v, want done/inactive", name, r)
		}
	}
	ci := byStep[engine.StepCIGreen]
	if ci.Glyph != GlyphWaiting || !ci.Active || ci.Detail != "CI: 2/3 checks complete" {
		t.Errorf("CIGreen row = %+v, want waiting/active with the CI detail", ci)
	}
	for _, name := range []engine.StepName{engine.StepApproved, engine.StepMerged} {
		r := byStep[name]
		if r.Glyph != GlyphNotReached || r.Active {
			t.Errorf("%s: got %+v, want not-reached", name, r)
		}
	}
	active, ok := ActiveStep(rows)
	if !ok || active != engine.StepCIGreen {
		t.Errorf("ActiveStep = %v, %v, want ci-green, true", active, ok)
	}
}

// TestDeriveRowsBlocked: CIGreen reports a real conflict — glyph blocked, active, and the
// Blocked reason (never Detail) is what's shown.
func TestDeriveRowsBlocked(t *testing.T) {
	statuses := []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: true}),
		st(engine.StepCommitted, engine.Observation{Satisfied: true}),
		st(engine.StepPushed, engine.Observation{Satisfied: true}),
		st(engine.StepPROpened, engine.Observation{Satisfied: true}),
		st(engine.StepCIGreen, engine.Observation{Blocked: "2 of 3 checks failed: lint, test"}),
	}
	rows := DeriveRows(StepOrder, false, statuses)
	ci := indexRows(rows)[engine.StepCIGreen]
	if ci.Glyph != GlyphBlocked || !ci.Active || ci.Detail != "2 of 3 checks failed: lint, test" {
		t.Errorf("CIGreen row = %+v, want blocked/active with the Blocked reason", ci)
	}
}

// TestDeriveRowsActiveNotYetActed: a step that is neither Satisfied, Waiting, nor Blocked
// (Drive has not yet acted on it this pass) is the active row, glyph ▶.
func TestDeriveRowsActiveNotYetActed(t *testing.T) {
	statuses := []engine.StepStatus{
		st(engine.StepBranched, engine.Observation{Satisfied: false, Detail: "no worktree yet"}),
	}
	rows := DeriveRows(StepOrder, false, statuses)
	row := indexRows(rows)[engine.StepBranched]
	if row.Glyph != GlyphActive || !row.Active || row.Detail != "no worktree yet" {
		t.Errorf("Branched row = %+v, want active/▶", row)
	}
}

// TestDeriveRowsDone: done=true renders every row done regardless of how many statuses
// engine.Status's own short-circuit produced (see its doc comment) — here, just the final
// step's own StepStatus, matching what Status actually returns in that case.
func TestDeriveRowsDone(t *testing.T) {
	statuses := []engine.StepStatus{
		st(engine.StepRolledOut, engine.Observation{Satisfied: true, Detail: "app: rollout complete"}),
	}
	rows := DeriveRows(StepOrder, true, statuses)
	if len(rows) != len(StepOrder) {
		t.Fatalf("got %d rows, want %d", len(rows), len(StepOrder))
	}
	for i, r := range rows {
		if r.Glyph != GlyphDone || r.Active {
			t.Errorf("row %d (%s) = %+v, want done/inactive", i, r.Step, r)
		}
	}
	last := rows[len(rows)-1]
	if last.Step != engine.StepRolledOut || last.Detail != "app: rollout complete" {
		t.Errorf("last row = %+v, want RolledOut (engine.AllSteps' own last step, M5 on) with the short-circuited detail", last)
	}
	if _, ok := ActiveStep(rows); ok {
		t.Error("ActiveStep found one in a fully-done list")
	}
}

// TestDeriveRowsDoneWithNoStatuses: defensive — done=true with an empty statuses slice must
// not panic indexing the last element, and still marks every row done with no detail.
func TestDeriveRowsDoneWithNoStatuses(t *testing.T) {
	rows := DeriveRows(StepOrder, true, nil)
	for _, r := range rows {
		if r.Glyph != GlyphDone {
			t.Errorf("row %+v, want done", r)
		}
	}
	if rows[len(rows)-1].Detail != "" {
		t.Errorf("last row Detail = %q, want empty", rows[len(rows)-1].Detail)
	}
}

func indexRows(rows []Row) map[engine.StepName]Row {
	out := make(map[engine.StepName]Row, len(rows))
	for _, r := range rows {
		out[r.Step] = r
	}
	return out
}

func TestLabelKnowsEveryStepOrderEntry(t *testing.T) {
	for _, name := range StepOrder {
		if l := Label(name); l == "" {
			t.Errorf("Label(%s) is empty", name)
		}
	}
}

func TestLabelFallsBackToRawName(t *testing.T) {
	if got := Label(engine.StepName("made-up-step")); got != "made-up-step" {
		t.Errorf("Label(made-up-step) = %q, want the raw name back", got)
	}
}

func TestStartedAtReadsFirstHistoryEntry(t *testing.T) {
	if got := StartedAt(engine.PromotionState{}); !got.IsZero() {
		t.Errorf("StartedAt of an empty state = %v, want zero", got)
	}
	first := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s := engine.PromotionState{History: []engine.HistoryEntry{
		{Step: engine.StepBranched, At: first, Detail: "acted"},
		{Step: engine.StepCommitted, At: first.Add(time.Minute), Detail: "acted"},
	}}
	if got := StartedAt(s); !got.Equal(first) {
		t.Errorf("StartedAt = %v, want %v (History[0].At)", got, first)
	}
}

func TestPRURL(t *testing.T) {
	if _, ok := PRURL(engine.PromotionState{}); ok {
		t.Error("PRURL found one with no PR at all")
	}
	if _, ok := PRURL(engine.PromotionState{PR: &forge.PR{}}); ok {
		t.Error("PRURL found one with an empty URL")
	}
	url, ok := PRURL(engine.PromotionState{PR: &forge.PR{URL: "https://example.invalid/pr/97"}})
	if !ok || url != "https://example.invalid/pr/97" {
		t.Errorf("PRURL = %q, %v, want the PR's URL, true", url, ok)
	}
}
