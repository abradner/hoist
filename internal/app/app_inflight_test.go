package app

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
)

// The in-flight adaptor is optional: a root built without a Service must boot. session.New's own
// nil-backend convention (ErrNoBackend from Start/Resume, a no-op Init) is what replaces the old
// per-func nil checks this test used to drive directly.
func TestInitWithoutInFlightDoesNotPanic(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, nil, Promotion{}, nil, apprestart.Funcs{})
	cmd := root.Init()
	if cmd == nil {
		t.Fatal("Init produced no command at all (the background-colour request, at least, should always fire)")
	}
	// Init's own command must not panic when it's actually run — the regression this test
	// guards (Copilot, #124): a nil List dereferenced inside the generation-stamped listing
	// helper. tea.Batch collapses to the bare surviving cmd when the others (screenCmd,
	// session.Controller.Init with a nil backend) are nil, so this does not assume a
	// tea.BatchMsg shape — only that running whatever comes back is safe.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			_ = c() // must not panic
		}
	}
	if _, cmd := root.popAndRelist(); cmd != nil {
		t.Fatal("no backend wired: popAndRelist must issue nothing")
	}
}

// Two listings in flight at once: the older answer, landing last, must not paint an older
// snapshot over the newer pane. session.Controller's own listGen is what guards this
// (TestListGenDropsOlderListing, internal/app/session/controller_test.go); this is the
// app-level integration proof that the matrix pane actually reflects the guard.
func TestAnOlderListingCannotOverwriteANewerOne(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	list := func(context.Context) ([]service.Listed, error) {
		calls++
		id := "first00001"
		if calls > 1 {
			id = "second0002"
		}
		st := engine.PromotionState{ID: id, SourceEnv: "app-staging", TargetEnv: "app-production"}
		return []service.Listed{{State: st, Done: false, Statuses: []engine.StepStatus{{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}}}}}, nil
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, svcWithInFlight(fakeInFlight{List: list}), Promotion{}, nil, apprestart.Funcs{})
	tm0, _ := root.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	mm := tm0.(Model)

	// Two explicit relists, through the real popAndRelist path (a no-op pop on this single-
	// screen stack, then Relist) — in place of the old inFlightTickMsg-driven pair.
	// session.Controller.Relist bumps its own listGen every call, and each hop below threads the
	// returned Model forward so the second really is a later generation than the first, the same
	// guard a real tick chain uses.
	mm, olderCmd := mm.popAndRelist()
	mm, newerCmd := mm.popAndRelist()
	// The fixture's own list() answers "first00001" on its first real call, "second0002" on
	// every one after — so olderCmd must actually be CALLED (not just constructed) before
	// newerCmd for the two to carry the values their names promise.
	olderMsg, newerMsg := olderCmd(), newerCmd()

	// Newest answers first, then the stale one.
	var m tea.Model = mm
	m, _ = m.Update(newerMsg)
	m, _ = m.Update(olderMsg)
	got := m.(Model).stack[0].(matrixScreen).InFlight()
	if len(got) != 1 || got[0].ID != "second0002" {
		t.Fatalf("pane shows %+v; want the newer listing only", got)
	}
}
