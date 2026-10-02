package service

import (
	"context"
	"errors"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
)

// actStep is a Step whose Observe is fixed and whose Act runs a caller-supplied func, so a test
// can look at the world at the moment a later step is about to act.
type actStep struct {
	name engine.StepName
	obs  engine.Observation
	act  func(*engine.PromotionState) error
	// done, when non-nil, makes the step satisfied once Act has run, as a real step is.
	done *bool
}

func (s actStep) Name() engine.StepName { return s.name }
func (s actStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	if s.done != nil && *s.done {
		return engine.Observation{Satisfied: true, Detail: "done"}, nil
	}
	return s.obs, nil
}
func (s actStep) Act(_ context.Context, st *engine.PromotionState) error {
	if s.done != nil {
		*s.done = true
	}
	if s.act != nil {
		return s.act(st)
	}
	return nil
}

// TestDriverReportsEachHistoryEntryAsItIsSaved: OnHistory fires when the save carrying an entry
// lands, not when the whole Step walk returns — the first walk includes a signed commit of up to
// 120s, and the operator must see branch/commit/push/PR as they happen. The proof is made from
// inside the walk: when the second step acts, the first step's entry has ALREADY been reported
// (a walk-end report would have seen zero). The pr-opened entry's state copy carries the PR.
func TestDriverReportsEachHistoryEntryAsItIsSaved(t *testing.T) {
	type call struct {
		e engine.HistoryEntry
		s engine.PromotionState
	}
	var got []call
	var seenWhenSecondActs int
	saved := 0
	var d1, d2 bool
	steps := []engine.Step{
		actStep{name: engine.StepBranched, done: &d1},
		actStep{name: engine.StepPROpened, done: &d2, act: func(st *engine.PromotionState) error {
			seenWhenSecondActs = len(got)
			st.PR = &forge.PR{Number: 42, URL: "https://example.invalid/pull/42"}
			return nil
		}},
		actStep{name: engine.StepCIGreen, obs: engine.Observation{Waiting: true, Detail: "CI: 1/2"}},
	}
	s := &engine.PromotionState{ID: "x"}
	d := NewDriver(steps, s, func(*engine.PromotionState) error { saved++; return nil }, engine.PollIntervals{}).
		withOnHistory(func(e engine.HistoryEntry, st engine.PromotionState) { got = append(got, call{e, st}) })

	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seenWhenSecondActs != 1 {
		t.Fatalf("when step 2 acted, %d entries had been reported, want 1 (reports must not wait for the walk to end)", seenWhenSecondActs)
	}
	if len(got) != 3 {
		t.Fatalf("got %d reports, want 3 (branched, pr-opened, ci waiting): %+v", len(got), got)
	}
	for i, c := range got {
		if c.e != c.s.History[i] || len(c.s.History) <= i {
			t.Fatalf("report %d: the state copy must contain the reported entry at index %d: %+v", i, i, c)
		}
	}
	if got[1].e.Step != engine.StepPROpened || got[1].s.PR == nil || got[1].s.PR.Number != 42 || got[1].s.PR.URL == "" {
		t.Fatalf("the pr-opened report must carry the PR in its state copy: %+v", got[1])
	}

	// The next walk re-observes the two acted steps as satisfied (new entries); the one after is
	// quiet and reports nothing: only NEWLY appended entries are news.
	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(got)
	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != before {
		t.Fatalf("a quiet walk reported %d more entries: %+v", len(got)-before, got[before:])
	}
	if saved == 0 {
		t.Fatal("positive control: the wrapped save never reached the real one")
	}
}

// TestDriverDoesNotReportPriorHistory: entries a previous process recorded are the state's past.
func TestDriverDoesNotReportPriorHistory(t *testing.T) {
	s := &engine.PromotionState{ID: "x", History: []engine.HistoryEntry{{Step: engine.StepBranched, Detail: "acted"}}}
	var got []engine.HistoryEntry
	steps := []engine.Step{actStep{name: engine.StepBranched, obs: engine.Observation{Satisfied: true, Detail: "d"}}}
	d := NewDriver(steps, s, nil, engine.PollIntervals{}).
		withOnHistory(func(e engine.HistoryEntry, _ engine.PromotionState) { got = append(got, e) })
	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Detail != "already satisfied: d" {
		t.Fatalf("only the new entry is reported, got %+v", got)
	}
}

// TestDriverHoldsReportsBackUntilTheSaveLands: a save that fails reports nothing; the entry is
// reported by the next save that lands, exactly once.
func TestDriverHoldsReportsBackUntilTheSaveLands(t *testing.T) {
	s := &engine.PromotionState{ID: "x"}
	fail := true
	var got []engine.HistoryEntry
	steps := []engine.Step{actStep{name: engine.StepBranched, obs: engine.Observation{Satisfied: true, Detail: "d"}}}
	d := NewDriver(steps, s, func(*engine.PromotionState) error {
		if fail {
			return errors.New("disk full")
		}
		return nil
	}, engine.PollIntervals{}).
		withOnHistory(func(e engine.HistoryEntry, _ engine.PromotionState) { got = append(got, e) })
	if _, err := d.Step(context.Background()); err == nil {
		t.Fatal("want the save error")
	}
	if len(got) != 0 {
		t.Fatalf("reported %d entries for a save that failed", len(got))
	}
	fail = false
	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the held-back entry reported once, got %+v", got)
	}
}

// TestResumeReportsHistoryThroughOnHistory is the Resume path's half: driving a real resumed
// promotion to completion reports every entry the drive appended, and by the time each is
// reported the state file already holds it (the save landed first).
func TestResumeReportsHistoryThroughOnHistory(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	svc := withConfig(fx)
	prior := len(s.History)

	var reported []engine.HistoryEntry
	var notOnDisk int
	d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{Hooks: Hooks{OnHistory: func(e engine.HistoryEntry, st engine.PromotionState) {
		reported = append(reported, e)
		onDisk, lerr := fileStore.Load(st.ID)
		if lerr != nil || len(onDisk.History) < len(st.History) {
			notOnDisk++
		}
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background(), RunHooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := d.State().History
	if len(reported) == 0 || len(reported) != len(final)-prior {
		t.Fatalf("reported %d entries, want the %d the drive appended", len(reported), len(final)-prior)
	}
	for i, e := range reported {
		if e != final[prior+i] {
			t.Fatalf("report %d = %+v, want %+v", i, e, final[prior+i])
		}
	}
	if notOnDisk != 0 {
		t.Fatalf("%d reports fired before their save landed", notOnDisk)
	}
}
