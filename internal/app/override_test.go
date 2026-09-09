package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
)

// recordingDrive is a flight.DriveFunc that remembers the state each call received — the only
// channel through which an override can reach engine.Drive and CIGreenStep. It answers the
// way the real engine would under ci.none: prompt: Blocked on the ci.none reason until the
// state carries CINoneOverride, satisfied after.
type recordingDrive struct {
	mu      sync.Mutex
	seen    []engine.PromotionState
	blocked string
}

func (r *recordingDrive) fn() flight.DriveFunc {
	return func(_ context.Context, s engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, s)
		obs := engine.Observation{Blocked: r.blocked}
		if s.CINoneOverride {
			obs = engine.Observation{Satisfied: true, Detail: "overridden"}
		}
		return s, false, []engine.StepStatus{{Step: engine.StepCIGreen, Observation: obs}}, nil
	}
}

// ciNoneBlocked is CIGreenStep's own ci.none=prompt reason, produced by the step rather than
// copied, so the test fails if the wording drifts from what the flight screen recognises.
func ciNoneBlocked(t *testing.T, id string) string {
	t.Helper()
	st := &engine.PromotionState{ID: id, CINone: "prompt", PR: &forge.PR{Number: 7, CreatedAt: time.Now().Add(-time.Hour)}, PushedSHA: "abc"}
	obs, err := engine.CIGreenStep{Forge: &forge.Fake{}}.Observe(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !engine.IsCINonePromptBlock(obs.Blocked) {
		t.Fatalf("fixture precondition: expected the ci.none=prompt block, got %+v", obs)
	}
	return obs.Blocked
}

// landFirstDrive runs a flight screen's Init synchronously and feeds every message it
// produces back through the root, so the screen reaches the state the first poll leaves it
// in (here: stopped on the ci.none block) the way a running program would.
func landFirstDrive(t *testing.T, root tea.Model, init tea.Cmd) tea.Model {
	t.Helper()
	var feed func(tea.Cmd)
	feed = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				feed(sub)
			}
			return
		}
		root, _ = root.Update(msg)
	}
	feed(init)
	return root
}

func (r *recordingDrive) last() (engine.PromotionState, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return engine.PromotionState{}, 0
	}
	return r.seen[len(r.seen)-1], len(r.seen)
}

// TestFlightOverrideCINoneMsgRedrivesThatPromotion: the root answers OverrideCINoneMsg by
// setting the override on the flight screen on top whose promotion it names and re-driving it
// — the next DriveFunc call carries CINoneOverride — and ignores a message naming any other
// promotion, with a notice and no drive.
func TestFlightOverrideCINoneMsgRedrivesThatPromotion(t *testing.T) {
	drv := &recordingDrive{blocked: ciNoneBlocked(t, "abcd1234")}
	root := sized(t).(Model)
	fs := flightScreen{flight.New(engine.PromotionState{ID: "abcd1234"}, flight.PollDurations{}, drv.fn())}
	root = root.push(fs)
	root = landFirstDrive(t, root, fs.Init()).(Model)
	// The first poll blocked on the ci.none reason; the override's re-drive is the next call.
	if seen, n := drv.last(); n != 1 || seen.CINoneOverride {
		t.Fatalf("setup: %d drive calls before the override (override=%v), want one without it", n, seen.CINoneOverride)
	}
	if !strings.Contains(plain(root), "c treat as green") {
		t.Fatalf("setup: the flight screen should offer c:\n%s", plain(root))
	}

	m, cmd := tea.Model(root).Update(flight.OverrideCINoneMsg{ID: "other-promotion"})
	if cmd != nil {
		t.Error("a message naming a promotion that is not on top must not drive anything")
	}
	if got := m.(Model).notice; !strings.Contains(got, "other-promotion") {
		t.Errorf("notice should name the ignored promotion, got %q", got)
	}
	if _, n := drv.last(); n != 1 {
		t.Fatalf("%d drive calls after an ignored override, want the setup's one", n)
	}

	m, cmd = tea.Model(root).Update(flight.OverrideCINoneMsg{ID: "abcd1234"})
	if cmd == nil {
		t.Fatal("the override produced no re-drive command")
	}
	runBatch(cmd)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, n := drv.last(); n > 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	seen, n := drv.last()
	if n != 2 {
		t.Fatalf("drive calls = %d, want the setup's one plus exactly one re-drive", n)
	}
	if !seen.CINoneOverride {
		t.Error("the re-drive's state did not carry CINoneOverride — the engine step would never see it")
	}
	if seen.ID != "abcd1234" {
		t.Errorf("re-drove %q, want the named promotion", seen.ID)
	}
	if n := len(m.(Model).stack); n != 2 {
		t.Errorf("the flight screen should stay on top: stack has %d screens, want 2", n)
	}
}
