package engine

import "testing"

// TestStepsForChoosesByMode asserts StepsFor picks AllSteps for a non-direct state and
// AllDirectSteps for a direct one — the one place the mode switch used to be copied at every
// driver (cmd/hoist's promote, deploy, resume and wiring, twice) is now this single function,
// so this test pins its behaviour against the two step lists it delegates to rather than
// duplicating their own step-order tests.
func TestStepsForChoosesByMode(t *testing.T) {
	names := func(steps []Step) []StepName {
		out := make([]StepName, len(steps))
		for i, s := range steps {
			out[i] = s.Name()
		}
		return out
	}
	sameNames := func(t *testing.T, got, want []StepName) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d steps, want %d: got=%v want=%v", len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("step %d = %s, want %s (got=%v want=%v)", i, got[i], want[i], got, want)
			}
		}
	}

	prod := []string{"prod-env"}

	t.Run("Direct=false", func(t *testing.T) {
		s := &PromotionState{Direct: false}
		got := names(StepsFor(s, nil, nil, nil, nil, prod, true, nil))
		want := names(AllSteps(nil, nil, nil, nil, nil))
		sameNames(t, got, want)
	})

	t.Run("Direct=true", func(t *testing.T) {
		s := &PromotionState{Direct: true}
		got := names(StepsFor(s, nil, nil, nil, nil, prod, true, nil))
		want := names(AllDirectSteps(nil, nil, nil, prod, true, nil))
		sameNames(t, got, want)
	})
}
