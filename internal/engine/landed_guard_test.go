package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
)

// stubStep is a Step whose Observe is a function, for exercising landedGuard's own decisions
// without a repository; acted counts Act calls.
type stubStep struct {
	name    StepName
	observe func(*PromotionState) (Observation, error)
	acted   *int
}

func (s stubStep) Name() StepName { return s.name }
func (s stubStep) Observe(_ context.Context, st *PromotionState) (Observation, error) {
	return s.observe(st)
}
func (s stubStep) Act(context.Context, *PromotionState) error {
	if s.acted != nil {
		*s.acted++
	}
	return nil
}

func TestLandedGuard(t *testing.T) {
	satisfied := func(*PromotionState) (Observation, error) { return Observation{Satisfied: true, Detail: "inner"}, nil }
	unsatisfied := func(*PromotionState) (Observation, error) { return Observation{Detail: "inner says no"}, nil }
	blocked := func(*PromotionState) (Observation, error) { return Observation{Blocked: "inner blocked"}, nil }
	failing := func(*PromotionState) (Observation, error) { return Observation{}, errors.New("inner failed") }
	landingCalls := 0
	landedAs := func(head string) func(*PromotionState) (Observation, error) {
		return func(st *PromotionState) (Observation, error) {
			landingCalls++
			// What MergedStep does on the way to Satisfied: record what it found.
			st.PR = &forge.PR{Number: 7, Merged: true, HeadSHA: head}
			st.MergeSHA = "merge-sha"
			return Observation{Satisfied: true}, nil
		}
	}
	notLanded := func(*PromotionState) (Observation, error) { landingCalls++; return Observation{}, nil }
	landingFails := func(*PromotionState) (Observation, error) {
		landingCalls++
		return Observation{}, errors.New("forge down")
	}

	for _, tc := range []struct {
		name          string
		commit        string
		inner         func(*PromotionState) (Observation, error)
		landing       func(*PromotionState) (Observation, error)
		wantSatisfied bool
		wantBlocked   bool
		wantErr       bool
		wantLanding   int
	}{
		{"inner satisfied: the landing step is never asked", "c1", satisfied, landedAs("c1"), true, false, false, 0},
		{"inner unsatisfied, landed with this commit", "c1", unsatisfied, landedAs("c1"), true, false, false, 1},
		{"inner blocked, landed with this commit", "c1", blocked, landedAs("c1"), true, false, false, 1},
		{"inner errors, landed with this commit", "c1", failing, landedAs("c1"), true, false, false, 1},
		{"forge does not say which commit merged", "c1", unsatisfied, landedAs(""), true, false, false, 1},
		// #41: the merged PR on this branch name merged a DIFFERENT commit — an earlier run of
		// the same id. That is not this run landing.
		{"landed PR merged some other commit", "c2", unsatisfied, landedAs("c1"), false, false, false, 1},
		{"not landed: inner's own answer stands", "c1", unsatisfied, notLanded, false, false, false, 1},
		{"not landed: inner's block stands", "c1", blocked, notLanded, false, true, false, 1},
		{"not landed: inner's error stands", "c1", failing, notLanded, false, false, true, 1},
		{"landing probe fails: inner's answer stands", "c1", unsatisfied, landingFails, false, false, false, 1},
		{"no commit on record: nothing could have landed, nothing is asked", "", unsatisfied, landedAs(""), false, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			landingCalls = 0
			acted := 0
			g := guardLanded(
				stubStep{name: StepBranched, observe: tc.inner, acted: &acted},
				stubStep{name: StepMerged, observe: tc.landing},
			)
			s := &PromotionState{CommitSHA: tc.commit, Base: "main"}
			obs, err := g.Observe(context.Background(), s)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if obs.Satisfied != tc.wantSatisfied || (obs.Blocked != "") != tc.wantBlocked {
				t.Fatalf("obs = %+v, want satisfied=%v blocked=%v", obs, tc.wantSatisfied, tc.wantBlocked)
			}
			if landingCalls != tc.wantLanding {
				t.Fatalf("landing step observed %d times, want %d", landingCalls, tc.wantLanding)
			}
			if s.PR != nil || s.MergeSHA != "" {
				t.Fatalf("the probe must not record anything on the promotion: PR=%v MergeSHA=%q", s.PR, s.MergeSHA)
			}
			if g.Name() != StepBranched {
				t.Fatalf("Name = %s, want the wrapped step's", g.Name())
			}
			if err := g.Act(context.Background(), s); err != nil || acted != 1 {
				t.Fatalf("Act must be the wrapped step's: acted=%d err=%v", acted, err)
			}
		})
	}
}

// TestLandedGuardAsksTheLandingStepOncePerWalk: a walk guards up to three steps with one landing
// step, and each guard needs the same answer. Asked per guard, a landed promotion whose worktree
// is gone cost three landing observations per walk — three trips to origin, every poll.
func TestLandedGuardAsksTheLandingStepOncePerWalk(t *testing.T) {
	for _, landedAnswer := range []bool{true, false} {
		calls := 0
		landing := stubStep{name: StepMerged, observe: func(*PromotionState) (Observation, error) {
			calls++
			return Observation{Satisfied: landedAnswer}, nil
		}}
		unsatisfied := func(name StepName) Step {
			return guardLanded(stubStep{name: name, observe: func(*PromotionState) (Observation, error) { return Observation{}, nil }}, landing)
		}
		steps := []Step{unsatisfied(StepBranched), unsatisfied(StepCommitted), unsatisfied(StepPushed)}
		s := &PromotionState{CommitSHA: "c1", Base: "main"}

		walk := withLandingWalk(context.Background())
		for _, st := range steps {
			obs, err := st.Observe(walk, s)
			if err != nil || obs.Satisfied != landedAnswer {
				t.Fatalf("landed=%v: %s observed %+v, %v", landedAnswer, st.Name(), obs, err)
			}
		}
		if calls != 1 {
			t.Errorf("landed=%v: one walk over three guarded steps asked the landing step %d times, want 1", landedAnswer, calls)
		}
		// The memo is the walk's, not the step list's: the next walk asks again.
		if _, err := steps[0].Observe(withLandingWalk(context.Background()), s); err != nil || calls != 2 {
			t.Errorf("landed=%v: a new walk must ask again: calls=%d err=%v", landedAnswer, calls, err)
		}
		// Outside any walk nothing is remembered.
		before := calls
		for i := 0; i < 2; i++ {
			if _, err := steps[0].Observe(context.Background(), s); err != nil {
				t.Fatal(err)
			}
		}
		if calls != before+2 {
			t.Errorf("landed=%v: outside a walk each observation asks: %d calls, want %d", landedAnswer, calls, before+2)
		}
	}
}

// TestWalkersDoNotAskAgainAfterTheirOwnProbe: ObserveAll, Status and DriveStatus ask MergedStep
// up front. When that probe fails or says no, the guards on the steps before it must not ask a
// second time — against an unreachable origin that is a second failed connection per state, in a
// scan over every state file (AGENTS.md §9 entry 15).
func TestWalkersDoNotAskAgainAfterTheirOwnProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fails   bool
		landing func() (Observation, error)
	}{
		{"probe fails", true, func() (Observation, error) { return Observation{}, errors.New("could not read from remote") }},
		{"probe says not merged", false, func() (Observation, error) { return Observation{}, nil }},
	} {
		for _, walker := range []string{"ObserveAll", "Status", "DriveStatus"} {
			t.Run(tc.name+"/"+walker, func(t *testing.T) {
				calls := 0
				merged := stubStep{name: StepMerged, observe: func(*PromotionState) (Observation, error) { calls++; return tc.landing() }}
				// A step that is not satisfied and whose Act does nothing: the walk stops or acts
				// here, and either way its guard is consulted.
				acted := 0
				unsatisfied := guardLanded(stubStep{name: StepBranched, observe: func(*PromotionState) (Observation, error) { return Observation{Waiting: walker != "DriveStatus"}, nil }, acted: &acted}, merged)
				after := stubStep{name: StepArgoRefreshed, observe: func(*PromotionState) (Observation, error) { return Observation{Waiting: true}, nil }}
				steps := []Step{unsatisfied, merged, after}
				s := &PromotionState{CommitSHA: "c1", Base: "main", Phase: StepArgoRefreshed}
				switch walker {
				case "ObserveAll":
					_, _, _ = ObserveAll(context.Background(), steps, s)
				case "Status":
					_, _, _ = Status(context.Background(), steps, s)
				case "DriveStatus":
					_, _, _ = DriveStatus(context.Background(), steps, s, nil)
				}
				// The up-front probe, plus the walk reaching MergedStep in its own turn only if it
				// got that far; never an ask on the guard's behalf.
				want := 1
				if walker == "DriveStatus" && tc.fails {
					// Branched acts and the walk reaches MergedStep in its own turn; a probe that
					// failed is not reused there, so that turn is a second, legitimate ask.
					want = 2
				}
				if calls != want {
					t.Fatalf("%s asked the landing step %d times, want %d: the guard asked again after the probe", walker, calls, want)
				}
			})
		}
	}
}

func TestLandingObservable(t *testing.T) {
	blobs := map[string]string{"a.yaml": "blob"}
	edits := []gitops.Edit{{}}
	for _, tc := range []struct {
		name string
		s    PromotionState
		want bool
	}{
		{"PR path with a commit", PromotionState{CommitSHA: "c"}, true},
		{"PR path with no commit", PromotionState{}, false},
		{"direct with commit, blobs and edits", PromotionState{Direct: true, CommitSHA: "c", ExpectedBlobs: blobs, Edits: edits}, true},
		{"direct with no commit", PromotionState{Direct: true, ExpectedBlobs: blobs, Edits: edits}, false},
		// With nothing to compare, DirectPushedStep's content check is vacuously "intact".
		{"direct with no blobs", PromotionState{Direct: true, CommitSHA: "c", Edits: edits}, false},
		{"direct with no edits", PromotionState{Direct: true, CommitSHA: "c", ExpectedBlobs: blobs}, false},
	} {
		if got := LandingObservable(&tc.s); got != tc.want {
			t.Errorf("%s: LandingObservable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
