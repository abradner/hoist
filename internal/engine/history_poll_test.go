package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/forge"
)

func countEntries(h []HistoryEntry, step StepName, detail string) int {
	n := 0
	for _, e := range h {
		if e.Step == step && e.Detail == detail {
			n++
		}
	}
	return n
}

// TestHistoryReactingStepDoesNotRerecordTheUnchangedWait (#243 H1): while ArgoRefreshedStep keeps
// re-acting, every poll appends "acted" (a real write, kept) and then the argo-synced wait. The
// wait is unchanged, but the "acted" sitting after it made it "not the last entry", so it was
// re-recorded each poll: two entries per poll. Only `acted` entries intervene, so it is not a
// change (AGENTS.md 9 entry 14).
func TestHistoryReactingStepDoesNotRerecordTheUnchangedWait(t *testing.T) {
	steps := []Step{
		detailStep{StepArgoRefreshed, &Observation{Satisfied: false}},
		detailStep{StepArgoSynced, &Observation{Waiting: true, Detail: "Argo at abc, want def"}},
	}
	s := &PromotionState{}
	for i := 0; i < 4; i++ {
		if _, _, err := DriveStatus(ctx(), steps, s, nil); !errors.Is(err, ErrWaiting) {
			t.Fatalf("walk %d: err = %v, want ErrWaiting", i, err)
		}
	}
	if n := countEntries(s.History, StepArgoSynced, "waiting: Argo at abc, want def"); n != 1 {
		t.Fatalf("the unchanged argo-synced wait must be recorded once, got %d: %+v", n, s.History)
	}
	if n := countEntries(s.History, StepArgoRefreshed, "acted"); n != 4 {
		t.Fatalf("every re-acted refresh is a write and stays recorded: got %d: %+v", n, s.History)
	}
}

// TestHistoryWaitAfterActedStillRecordsAChangedWait is the control: the same shape with a wait
// whose detail changed between polls is a change and is recorded; so is the same wait once a
// NON-acted entry (another step's wait) has sat between the two.
func TestHistoryWaitAfterActedStillRecordsAChangedWait(t *testing.T) {
	synced := &Observation{Waiting: true, Detail: "Argo at abc, want def"}
	steps := []Step{
		detailStep{StepArgoRefreshed, &Observation{Satisfied: false}},
		detailStep{StepArgoSynced, synced},
	}
	s := &PromotionState{}
	walk := func() {
		t.Helper()
		if _, _, err := DriveStatus(ctx(), steps, s, nil); !errors.Is(err, ErrWaiting) {
			t.Fatal(err)
		}
	}
	walk()
	walk()
	*synced = Observation{Waiting: true, Detail: "Argo at def, Progressing"}
	walk()
	if n := countEntries(s.History, StepArgoSynced, "waiting: Argo at def, Progressing"); n != 1 {
		t.Fatalf("a changed wait must be recorded: %+v", s.History)
	}
	if n := countEntries(s.History, StepArgoSynced, "waiting: Argo at abc, want def"); n != 1 {
		t.Fatalf("the first wait is recorded once: %+v", s.History)
	}
}

// TestCIGraceDetailDoesNotCountPerSecond (#243 H2): the grace-period wait carried "(Ns of 5m
// grace elapsed)", a per-second counter, so every poll for the length of ci.grace appended a new
// History entry. The detail names the moment the grace ends (like the settling detail does), so
// it is the same text on every poll within the window.
func TestCIGraceDetailDoesNotCountPerSecond(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "green"
	s.CIGrace = time.Minute
	created := s.PR.CreatedAt
	step := CIGreenStep{Forge: f, Now: fixedNow(created.Add(3 * time.Second))}
	first, err := step.Observe(ctx(), s)
	if err != nil || !first.Waiting {
		t.Fatalf("want a grace wait, got %+v err=%v", first, err)
	}
	step.Now = fixedNow(created.Add(41 * time.Second))
	second, err := step.Observe(ctx(), s)
	if err != nil || !second.Waiting {
		t.Fatalf("want a grace wait, got %+v err=%v", second, err)
	}
	if first.Detail != second.Detail {
		t.Fatalf("the grace detail changed between polls inside the window:\n%q\n%q", first.Detail, second.Detail)
	}
	if !strings.Contains(first.Detail, created.Add(time.Minute).Local().Format("15:04:05")) {
		t.Errorf("the detail should name when grace ends: %q", first.Detail)
	}
}
