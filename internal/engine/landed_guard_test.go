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
