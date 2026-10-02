package engine

import (
	"errors"
	"testing"
)

// TestHistoryStopRecordedWhenItIsNotTheLastEntry: approval waiting -> a check re-runs -> the
// check is satisfied -> approval waiting again. The second approval wait sits after other
// entries, so it is a new fact and must be recorded; History ends on the step actually stopping
// the promotion, which waitingReporter and historyDetail read.
func TestHistoryStopRecordedWhenItIsNotTheLastEntry(t *testing.T) {
	ci := &Observation{Satisfied: true, Detail: "green"}
	approval := &Observation{Waiting: true, Detail: "no approval yet"}
	steps := []Step{detailStep{StepCIGreen, ci}, detailStep{StepApproved, approval}}
	s := &PromotionState{}
	walk := func() {
		t.Helper()
		if _, _, err := DriveStatus(ctx(), steps, s, nil); !errors.Is(err, ErrWaiting) {
			t.Fatalf("err = %v, want ErrWaiting", err)
		}
	}
	walk() // ci satisfied, approval waiting
	*ci = Observation{Waiting: true, Detail: "CI: 3/4 checks complete"}
	walk() // a check re-runs: ci waiting
	*ci = Observation{Satisfied: true, Detail: "green again"}
	walk() // ci satisfied, approval waiting again
	last := s.History[len(s.History)-1]
	if last.Step != StepApproved || last.Detail != "waiting: no approval yet" {
		t.Fatalf("History must end on the approval wait that stops the promotion: %+v", s.History)
	}
	n := 0
	for _, h := range s.History {
		if h.Step == StepApproved {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("approval waited twice with a check between: want 2 entries, got %d: %+v", n, s.History)
	}
}

// TestHistoryQuietPollAppendsNothing is the control for the rule above: four satisfied steps
// and an unchanged wait, walked again and again, add nothing after the first walk.
func TestHistoryQuietPollAppendsNothing(t *testing.T) {
	steps := []Step{
		detailStep{StepBranched, &Observation{Satisfied: true, Detail: "a"}},
		detailStep{StepCommitted, &Observation{Satisfied: true, Detail: "b"}},
		detailStep{StepPushed, &Observation{Satisfied: true, Detail: "c"}},
		detailStep{StepPROpened, &Observation{Satisfied: true, Detail: "d"}},
		detailStep{StepApproved, &Observation{Waiting: true, Detail: "no approval yet"}},
	}
	s := &PromotionState{}
	for i := 0; i < 5; i++ {
		if _, _, err := DriveStatus(ctx(), steps, s, nil); !errors.Is(err, ErrWaiting) {
			t.Fatal(err)
		}
	}
	if len(s.History) != 5 {
		t.Fatalf("a quiet poll must append nothing: %d entries: %+v", len(s.History), s.History)
	}
}

// TestHistoryBlockedAgainRecorded: blocked -> fixed (satisfied) -> blocked again with the same
// reason is two stops, not one; the repeat before the fix is still one.
func TestHistoryBlockedAgainRecorded(t *testing.T) {
	ci := &Observation{Satisfied: true, Detail: "green"}
	merged := &Observation{Blocked: "branch protection"}
	steps := []Step{detailStep{StepCIGreen, ci}, detailStep{StepMerged, merged}}
	s := &PromotionState{}
	walk := func(want error) {
		t.Helper()
		var be *BlockedError
		_, _, err := DriveStatus(ctx(), steps, s, nil)
		if want == nil && !errors.As(err, &be) || want != nil && !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	}
	walk(nil)
	walk(nil) // the repeat is still one entry
	if len(s.History) != 2 {
		t.Fatalf("a repeated block must not append: %+v", s.History)
	}
	*ci = Observation{Waiting: true, Detail: "CI: 3/4 checks complete"}
	walk(ErrWaiting) // a check re-runs
	*ci = Observation{Satisfied: true, Detail: "green"}
	walk(nil) // blocked again, same reason, after other entries
	last := s.History[len(s.History)-1]
	if last.Step != StepMerged || last.Detail != "blocked: branch protection" || len(s.History) != 5 {
		t.Fatalf("the second block must be recorded and be last: %+v", s.History)
	}
}

// TestHistoryRepeatedActedIsKept: a second write to the world leaves a second record.
func TestHistoryRepeatedActedIsKept(t *testing.T) {
	steps := []Step{detailStep{StepArgoRefreshed, &Observation{Satisfied: false}}}
	s := &PromotionState{}
	for i := 0; i < 2; i++ {
		if _, _, err := DriveStatus(ctx(), steps, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.History) != 2 || s.History[0].Detail != "acted" || s.History[1].Detail != "acted" {
		t.Fatalf("two acts must leave two entries: %+v", s.History)
	}
}
