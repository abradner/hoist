package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/engine"
)

// TestFlightAbandonMsgReturnsToMatrixAndCallsAbandonFn: the flight screen's own X gesture
// already confirmed the operator wants this (flight.AbandonMsg's own doc comment) — the root
// pops back to the matrix immediately (mirroring AbortMsg's own shape) and fires the real
// abandonFn as a command; once that resolves, the notice reports the outcome.
func TestFlightAbandonMsgReturnsToMatrixAndCallsAbandonFn(t *testing.T) {
	var gotID string
	root := sized(t).(Model)
	root = root.WithAbandon(func(_ context.Context, id string) error {
		gotID = id
		return nil
	})
	root = root.push(flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, nil)})
	if n := len(root.stack); n != 2 {
		t.Fatalf("setup: stack has %d screens after pushing flight, want 2", n)
	}

	tm, cmd := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = tm.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("AbandonMsg should return to the matrix immediately: stack has %d screens, want 1", n)
	}
	if cmd == nil {
		t.Fatal("AbandonMsg produced no command — the real abandonFn was never called")
	}
	msg := cmd()
	if bm, ok := msg.(tea.BatchMsg); ok {
		// listInFlight's own relist command is nil here (no WithInFlight wired), so tea.Batch
		// filters it out — but stay robust to a future change that wires one in too.
		for _, c := range bm {
			if c == nil {
				continue
			}
			if r, ok := c().(abandonResultMsg); ok {
				msg = r
			}
		}
	}
	res, ok := msg.(abandonResultMsg)
	if !ok {
		t.Fatalf("command produced %#v, want abandonResultMsg", msg)
	}
	if res.err != nil {
		t.Fatalf("abandonResultMsg.err = %v, want nil", res.err)
	}
	if gotID != "abcd1234" {
		t.Errorf("abandonFn called with id %q, want abcd1234", gotID)
	}

	tm2, _ := root.Update(res)
	root = tm2.(Model)
	if !strings.Contains(root.notice, "abandoned abcd1234") {
		t.Errorf("notice = %q, want it to report the successful abandon", root.notice)
	}
}

// TestFlightAbandonMsgFailureShowsNotice: abandonFn's own refusal (e.g. cmd/hoist's real
// implementation refusing an already-landed promotion) surfaces as a notice, never a panic or a
// silently-dropped error.
func TestFlightAbandonMsgFailureShowsNotice(t *testing.T) {
	root := sized(t).(Model)
	root = root.WithAbandon(func(context.Context, string) error {
		return errors.New("abcd1234 has already landed; abandoning is not a rollback")
	})
	root = root.push(flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, nil)})

	tm, cmd := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = tm.(Model)
	if cmd == nil {
		t.Fatal("AbandonMsg produced no command")
	}
	res, ok := cmd().(abandonResultMsg)
	if !ok {
		t.Fatalf("command produced %#v, want abandonResultMsg", cmd())
	}

	tm2, _ := root.Update(res)
	root = tm2.(Model)
	if !strings.Contains(root.notice, "abandon abcd1234 failed") || !strings.Contains(root.notice, "not a rollback") {
		t.Errorf("notice = %q, want it to report the real failure", root.notice)
	}
}

// TestFlightAbandonMsgWithoutHandlerShowsNotice mirrors startPromotion/openURL's own nil
// convention: a launch that never called WithAbandon still pops back to the matrix (the
// operator already confirmed through the X gesture) but reports that nothing was actually
// wired, rather than panicking on a nil abandonFn.
func TestFlightAbandonMsgWithoutHandlerShowsNotice(t *testing.T) {
	root := sized(t).(Model)
	root = root.push(flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, nil)})

	tm, cmd := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = tm.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("AbandonMsg should still return to the matrix: stack has %d screens, want 1", n)
	}
	if cmd != nil {
		t.Error("AbandonMsg produced a command with no abandonFn wired")
	}
	if !strings.Contains(root.notice, "not wired up") || !strings.Contains(root.notice, "abcd1234") {
		t.Errorf("notice = %q, want it to say abandon isn't wired up and name the id", root.notice)
	}
}

// TestFlightAbandonMsgCancelsInFlightDriveCmd is TestFlightAbortMsgCancelsInFlightDriveCmd's
// own twin: popping the flight screen must cancel its shared drive context immediately, not
// only once the (slower, real) abandonFn call eventually resolves.
func TestFlightAbandonMsgCancelsInFlightDriveCmd(t *testing.T) {
	gotErr := make(chan error, 1)
	hung := func(ctx context.Context, _ engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		<-ctx.Done()
		gotErr <- ctx.Err()
		return engine.PromotionState{}, false, nil, ctx.Err()
	}
	root := sized(t).(Model)
	root = root.WithAbandon(func(context.Context, string) error { return nil })
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, hung)}
	initCmd := fs.Init()
	if initCmd == nil {
		t.Fatal("setup: flight screen's Init produced no command")
	}
	root = root.push(fs)
	go runBatch(initCmd)

	rootTM, _ := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = rootTM.(Model)
	if n := len(root.stack); n != 1 {
		t.Fatalf("setup: AbandonMsg should return to the matrix: stack has %d screens, want 1", n)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AbandonMsg did not cancel the popped flight screen's in-flight driveCmd within 2s")
	}
}
