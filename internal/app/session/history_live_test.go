package session

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
)

func histEntry(step engine.StepName, detail string, at time.Time) engine.HistoryEntry {
	return engine.HistoryEntry{Step: step, At: at, Detail: detail}
}

func stateWith(hist ...engine.HistoryEntry) engine.PromotionState {
	return engine.PromotionState{ID: "promo-1", SourceEnv: "staging", TargetEnv: "prod", History: hist}
}

// liveHistoryFlow drives one entry (started by start, which receives the service.Hooks the
// controller built) through the live-report lifecycle and asserts every property the flight
// screen depends on:
//   - an OnHistory report landing mid-walk puts the entry into the snapshot's state History and
//     marks its step's row, WITHOUT adding a Snapshot.Log line (no second copy of the event);
//   - it emits ChangeProgress, so an attached screen re-mirrors;
//   - when the walk's tick lands, its state replaces the live one — each entry once, no
//     duplicates — and a report that arrives AFTER the tick (separate command, no fixed order)
//     cannot roll the snapshot back.
func liveHistoryFlow(t *testing.T, start func(c Controller, backend *fakeBackend) (Controller, tea.Cmd)) {
	t.Helper()
	now := fixedClock(time.Now())
	at := now()
	e1 := histEntry(engine.StepBranched, "acted", at)
	e2 := histEntry(engine.StepCommitted, "acted", at.Add(time.Second))
	e3 := histEntry(engine.StepPushed, "waiting: pushing", at.Add(2*time.Second))

	var hooks service.Hooks
	drive := &fakeDrive{id: "promo-1", steps: []service.Tick{
		{State: stateWith(e1, e2, e3), Waiting: true, Wait: time.Second,
			Statuses: []engine.StepStatus{{Step: engine.StepPushed, Observation: engine.Observation{Waiting: true, Detail: "pushing"}}}},
	}}
	backend := &fakeBackend{
		startFn: func(_ context.Context, _ service.StartRequest, h service.Hooks) (service.Drive, error) {
			hooks = h
			return drive, nil
		},
		resumeFn: func(_ context.Context, _ string, o service.ResumeOpts) (service.Drive, error) {
			hooks = o.Hooks
			return drive, nil
		},
	}
	c, cmd := start(New(backend, testConfig(now)), backend)
	h := harness{c: c}
	h, stepCmd := started(h, cmd)
	if stepCmd == nil {
		t.Fatal("the build produced no Step command")
	}
	if hooks.OnHistory == nil {
		t.Fatal("the controller built Hooks without OnHistory")
	}
	build := h.c.entries[BuildID(1)].build
	listen := func() {
		t.Helper()
		e := h.c.entries[build]
		m := listenCmd(e.ctx, e.build, e.gen, e.progressCh)()
		h, _ = harnessUpdate(h, m)
	}

	// Mid-walk: the Driver reports e1 then e2 as their saves land; the Step has not returned.
	hooks.OnHistory(e1, stateWith(e1))
	listen()
	hooks.OnHistory(e2, stateWith(e1, e2))
	listen()
	snap, _ := h.c.BuildSnapshot(build)
	if got := snap.State.History; len(got) != 2 || got[0] != e1 || got[1] != e2 {
		t.Fatalf("live History = %+v, want [e1 e2] before the tick lands", got)
	}
	if len(snap.Log) != 0 {
		t.Fatalf("a typed history report must not become a Log line: %+v", snap.Log)
	}
	var committedDone bool
	for _, s := range snap.Statuses {
		if s.Step == engine.StepCommitted && s.Satisfied {
			committedDone = true
		}
	}
	if !committedDone {
		t.Fatalf("the committed row is not marked done mid-walk: %+v", snap.Statuses)
	}
	var progress int
	for _, ch := range h.changes {
		if ch.Kind == ChangeProgress {
			progress++
		}
	}
	if progress != 2 {
		t.Fatalf("want 2 ChangeProgress (one per report), got %d in %+v", progress, h.changes)
	}

	// The tick lands: its state (e1 e2 e3) replaces the live one — no duplicates.
	h, _ = hop(h, stepCmd)
	snap, _ = h.c.BuildSnapshot(build)
	if got := snap.State.History; len(got) != 3 || got[2] != e3 {
		t.Fatalf("History after the tick = %+v, want exactly [e1 e2 e3] (no duplicates)", got)
	}

	// A report that arrives after its tick must not roll the snapshot back.
	hooks.OnHistory(e2, stateWith(e1, e2))
	listen()
	snap, _ = h.c.BuildSnapshot(build)
	if len(snap.State.History) != 3 {
		t.Fatalf("a late report rolled History back to %d entries: %+v", len(snap.State.History), snap.State.History)
	}
}

func TestStartReportsHistoryLiveAndTheTickReplacesIt(t *testing.T) {
	liveHistoryFlow(t, func(c Controller, _ *fakeBackend) (Controller, tea.Cmd) {
		c, _, cmd, err := c.Start(service.StartRequest{}, "staging", "prod")
		if err != nil {
			t.Fatal(err)
		}
		return c, cmd
	})
}

func TestResumeReportsHistoryLiveAndTheTickReplacesIt(t *testing.T) {
	liveHistoryFlow(t, func(c Controller, _ *fakeBackend) (Controller, tea.Cmd) {
		c, _, cmd, err := c.Resume("promo-1")
		if err != nil {
			t.Fatal(err)
		}
		return c, cmd
	})
}
