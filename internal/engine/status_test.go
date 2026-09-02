package engine

import (
	"context"
	"errors"
	"testing"
)

// stepStub is a minimal Step with a fixed Observation and no side effects — used to exercise
// Status's stopping and short-circuit rules directly, without a real worktree or forge
// (mirrors failingStep in engine_test.go, which does the same for Drive's Act path).
type stepStub struct {
	name StepName
	obs  Observation
	err  error
}

func (s stepStub) Name() StepName { return s.name }

func (s stepStub) Observe(context.Context, *PromotionState) (Observation, error) {
	return s.obs, s.err
}

func (s stepStub) Act(context.Context, *PromotionState) error { return nil }

func names(statuses []StepStatus) []StepName {
	out := make([]StepName, len(statuses))
	for i, st := range statuses {
		out[i] = st.Step
	}
	return out
}

func namesEqual(a, b []StepName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStatusStopsAtFirstWaiting: two satisfied steps, then a waiting one, then a step that
// would report Satisfied if reached — Status must stop right after the waiting step and
// never observe the one behind it, so its own name is absent from the returned statuses
// (flight.DeriveRows reads that absence as "not yet reached").
func TestStatusStopsAtFirstWaiting(t *testing.T) {
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true, Detail: "branched"}},
		stepStub{name: StepCommitted, obs: Observation{Satisfied: true, Detail: "committed"}},
		stepStub{name: StepPushed, obs: Observation{Waiting: true, Detail: "waiting for push"}},
		// Status (like ObserveAll) always probes this last element once, up front, as the
		// short-circuit check — that probe is unavoidable and does not by itself mean this
		// step was "reached" by the ordinary walk. Satisfied is deliberately false here so
		// that probe does not short-circuit the whole call; the assertion below is that
		// this step's name is absent from statuses, not that Observe was never called.
		stepStub{name: StepPROpened, obs: Observation{Satisfied: false, Detail: "not reached by the walk"}},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("done = true, want false: a waiting step is still in flight")
	}
	want := []StepName{StepBranched, StepCommitted, StepPushed}
	if got := names(statuses); !namesEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	last := statuses[len(statuses)-1]
	if !last.Waiting || last.Detail != "waiting for push" {
		t.Errorf("last status = %+v, want the waiting observation", last)
	}
}

// TestStatusStopsAtFirstBlocked mirrors the waiting case for a Blocked observation.
func TestStatusStopsAtFirstBlocked(t *testing.T) {
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true}},
		stepStub{name: StepCommitted, obs: Observation{Blocked: "someone else moved the branch"}},
		// Satisfied: false, not true — see TestStatusStopsAtFirstWaiting's comment on why
		// the last element must not short-circuit the call on its own.
		stepStub{name: StepPushed, obs: Observation{Satisfied: false}},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("done = true, want false: a blocked step is terminal, not finished")
	}
	want := []StepName{StepBranched, StepCommitted}
	if got := names(statuses); !namesEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if last := statuses[len(statuses)-1]; last.Blocked != "someone else moved the branch" {
		t.Errorf("last status = %+v, want the blocked observation", last)
	}
}

// TestStatusNotYetActedIsActive: a step that is neither Satisfied, Waiting, nor Blocked
// (Drive has not acted on it yet this pass) still ends the walk — it is the step
// flight.DeriveRows renders with the "active" glyph.
func TestStatusNotYetActedIsActive(t *testing.T) {
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true}},
		stepStub{name: StepCommitted, obs: Observation{Satisfied: false, Detail: "not yet committed"}},
		// Satisfied: false — see TestStatusStopsAtFirstWaiting's comment.
		stepStub{name: StepPushed, obs: Observation{Satisfied: false}},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("done = true, want false")
	}
	want := []StepName{StepBranched, StepCommitted}
	if got := names(statuses); !namesEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
}

// TestStatusShortCircuitsWhenFinalSatisfied reproduces ObserveAll's own short-circuit
// (engine.go's doc comment on ObserveAll explains why: MergedStep's own Observe is
// self-contained proof the whole promotion finished, since an earlier step's Observe can
// depend on state Merged's own Act just removed). The first step here would report
// Satisfied: false if it were ever observed — proving Status never reaches it once the
// final step alone answers the question.
func TestStatusShortCircuitsWhenFinalSatisfied(t *testing.T) {
	observedEarlier := false
	steps := []Step{
		observeTrackingStub{stepStub{name: StepBranched, obs: Observation{Satisfied: false}}, &observedEarlier},
		stepStub{name: StepMerged, obs: Observation{Satisfied: true, Detail: "merged as abc123; branch deleted"}},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("done = false, want true: the final step reported satisfied")
	}
	if observedEarlier {
		t.Fatal("Status observed the earlier step despite the final step's short-circuit")
	}
	if len(statuses) != 1 || statuses[0].Step != StepMerged {
		t.Fatalf("statuses = %+v, want exactly one entry for StepMerged", statuses)
	}
	if statuses[0].Detail != "merged as abc123; branch deleted" {
		t.Errorf("statuses[0].Detail = %q, want the final step's own detail", statuses[0].Detail)
	}
}

// observeTrackingStub wraps a stepStub and records whether Observe was ever called, so
// TestStatusShortCircuitsWhenFinalSatisfied can assert the short-circuit actually skipped it
// rather than merely happening to produce the right answer.
type observeTrackingStub struct {
	stepStub
	observed *bool
}

func (o observeTrackingStub) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	*o.observed = true
	return o.stepStub.Observe(ctx, s)
}

// TestStatusPropagatesObserveError: a plumbing error (a 404/permissions hiccup, in real use)
// from any step's Observe is returned immediately, with the statuses accumulated so far —
// never folded into a false Satisfied/Waiting/Blocked reading (AGENTS.md: "Known bug
// classes — a 404 or permissions hiccup must be retried, never read as authoritative").
func TestStatusPropagatesObserveError(t *testing.T) {
	wantErr := errors.New("GET /repos/.../check-runs: 404")
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true}},
		stepStub{name: StepCommitted, err: wantErr},
		// A third, distinct-from-the-erroring-step element: the short-circuit probes
		// steps[len-1] first (see TestStatusStopsAtFirstWaiting's comment), so the erroring
		// step must not itself be last or the probe would consume the error before the
		// ordinary walk ever runs, which is not what this test means to exercise.
		stepStub{name: StepPushed, obs: Observation{Satisfied: false}},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", err, wantErr)
	}
	if done {
		t.Error("done = true, want false on error")
	}
	want := []StepName{StepBranched}
	if got := names(statuses); !namesEqual(got, want) {
		t.Fatalf("statuses = %v, want %v (the erroring step itself must not appear)", got, want)
	}
}

// observeCountingStub wraps a stepStub and counts how many times Observe is called on it —
// used by TestStatusObservesFinalStepExactlyOnce to prove the final step's Observe is not
// called a second time when the walk reaches it (PR #39 review finding #3), rather than only
// checking the returned statuses happen to look right.
type observeCountingStub struct {
	stepStub
	calls *int
}

func (o observeCountingStub) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	*o.calls++
	return o.stepStub.Observe(ctx, s)
}

// TestStatusObservesFinalStepExactlyOnce: when the initial short-circuit probe on the final
// step comes back not-satisfied, Status's own walk reaches that same step again in its
// ordinary turn — the fix reuses the probe's own Observation there instead of calling Observe
// a second time. Before the fix, every not-yet-done poll cost one extra, wasted remote call
// on the final step (amplified every tick of the flight screen's own poll loop).
func TestStatusObservesFinalStepExactlyOnce(t *testing.T) {
	calls := 0
	steps := []Step{
		stepStub{name: StepBranched, obs: Observation{Satisfied: true}},
		stepStub{name: StepCommitted, obs: Observation{Satisfied: true}},
		observeCountingStub{stepStub{name: StepMerged, obs: Observation{Satisfied: false, Detail: "not yet merged"}}, &calls},
	}
	done, statuses, err := Status(ctx(), steps, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("done = true, want false: the final step reported not satisfied")
	}
	if calls != 1 {
		t.Fatalf("final step's Observe called %d times, want exactly 1", calls)
	}
	want := []StepName{StepBranched, StepCommitted, StepMerged}
	if got := names(statuses); !namesEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	if last := statuses[len(statuses)-1]; last.Detail != "not yet merged" {
		t.Errorf("last status = %+v, want the final step's own (reused) observation", last)
	}
}

// TestStatusEmptySteps: zero steps is vacuously done, with no statuses — Status must not
// panic indexing steps[len(steps)-1].
func TestStatusEmptySteps(t *testing.T) {
	done, statuses, err := Status(ctx(), nil, &PromotionState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("done = false, want true for zero steps")
	}
	if len(statuses) != 0 {
		t.Errorf("statuses = %v, want empty", statuses)
	}
}
