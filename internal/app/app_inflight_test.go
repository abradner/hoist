package app

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

// The in-flight adaptor is optional: a root built without WithInFlight must boot. The
// generation-stamped listing helper once dereferenced a nil List (Copilot, #124). Asserted
// on the helper and the pop path directly rather than by draining Init, whose batch holds a
// real 30-second tick.
func TestInitWithoutInFlightDoesNotPanic(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, nil, apprestart.Funcs{})
	if cmd := root.listInFlightAt(root.listGen); cmd != nil {
		t.Fatal("no List wired: the listing command must be nil, not a call on nil")
	}
	if _, cmd := root.popAndRelist(); cmd != nil {
		t.Fatal("no List wired: popAndRelist must issue nothing")
	}
	if _, cmd := root.listInFlight(); cmd != nil {
		t.Fatal("no List wired: listInFlight must issue nothing")
	}
}

// Two listings in flight at once: the older answer, landing last, must not paint an older
// snapshot over the newer pane. Each listing carries its generation; the root keeps the
// current one and drops the rest.
func TestAnOlderListingCannotOverwriteANewerOne(t *testing.T) {
	r, err := gitops.Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	list := func(context.Context) ([]flight.Summary, error) {
		calls++
		id := "first00001"
		if calls > 1 {
			id = "second0002"
		}
		st := engine.PromotionState{ID: id, SourceEnv: "app-staging", TargetEnv: "app-production"}
		return []flight.Summary{flight.Summarize(st, false, []engine.StepStatus{{Step: engine.StepBranched, Observation: engine.Observation{Satisfied: true}}}, nil)}, nil
	}
	root := New(r, []string{"ghcr.io/"}, config.EnvsConfig{}, nil, Promotion{}, nil, apprestart.Funcs{}).WithInFlight(InFlight{List: list})
	var m tea.Model = root
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	listingCmd := func(cmd tea.Cmd) tea.Cmd {
		for _, c := range cmd().(tea.BatchMsg) {
			if c == nil {
				continue
			}
			if _, ok := c().(inFlightMsg); ok {
				return c
			}
		}
		t.Fatal("no listing in the tick's batch")
		return nil
	}
	var cmd tea.Cmd
	m, cmd = m.Update(inFlightTickMsg{})
	older := listingCmd(cmd)
	m, cmd = m.Update(inFlightTickMsg{})
	newer := listingCmd(cmd)

	// Newest answers first, then the stale one.
	m, _ = m.Update(newer())
	m, _ = m.Update(older())
	got := m.(Model).stack[0].(matrixScreen).InFlight()
	if len(got) != 1 || got[0].ID != "second0002" {
		t.Fatalf("pane shows %+v; want the newer listing only", got)
	}
}
