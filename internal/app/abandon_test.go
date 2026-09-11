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

// TestFlightAbandonMsgWaitsForBusyDriveCmdBeforeAbandoning is the round-2 review finding
// against PR #182: Cancel() signals the shared ctx but does not wait for the goroutine a
// driveCmd already in flight is running in to actually notice and return, so popping and
// firing abandonFn immediately (the original shape this test used to assert) could delete the
// state file — or close the PR, or delete the branch — the instant before that canceled drive
// finished its own last write. flight.New sets busy=true at construction whenever driveFn is
// non-nil (the ordinary "adopted a real, already-driving promotion" shape), so this fixture is
// genuinely Busy() the moment it is pushed, without needing Init() run at all. Cancellation
// must still reach the hung driveFn right away; the pop itself must wait, bounded by
// abandonWaitMaxAttempts since this fixture's driveFn blocks forever and never flips Busy()
// back to false through the normal driveResultMsg path.
func TestFlightAbandonMsgWaitsForBusyDriveCmdBeforeAbandoning(t *testing.T) {
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

	rootTM, cmd := root.Update(flight.AbandonMsg{ID: "abcd1234"})
	root = rootTM.(Model)
	if n := len(root.stack); n != 2 {
		t.Fatalf("AbandonMsg must not pop while the drive is still Busy(): stack has %d screens, want 2", n)
	}
	if cmd == nil {
		t.Fatal("AbandonMsg produced no wait command while the drive is busy")
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung driveFn's own ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AbandonMsg did not cancel the still-busy flight screen's in-flight driveCmd within 2s")
	}

	for i := 0; i <= abandonWaitMaxAttempts; i++ {
		if cmd == nil {
			t.Fatalf("wait loop stopped producing commands at attempt %d, before giving up", i)
		}
		msg := cmd()
		var tm tea.Model
		tm, cmd = root.Update(msg)
		root = tm.(Model)
	}
	if n := len(root.stack); n != 1 {
		t.Fatalf("AbandonMsg should abandon once the wait gives up (the fixture's driveFn never clears Busy()): stack has %d screens, want 1", n)
	}
}
