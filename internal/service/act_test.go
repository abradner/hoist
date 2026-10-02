package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/resolve"
)

// recordingStep is an engine.Step whose Act records itself on the state and can fail.
type recordingStep struct {
	name engine.StepName
	err  error
}

func (r recordingStep) Name() engine.StepName { return r.name }
func (r recordingStep) Observe(context.Context, *engine.PromotionState) (engine.Observation, error) {
	return engine.Observation{}, nil
}
func (r recordingStep) Act(_ context.Context, s *engine.PromotionState) error {
	if r.err != nil {
		return r.err
	}
	s.CommitSHA = "sha-from-" + string(r.name)
	return nil
}

// TestAnnounceReportsAnActBeforeItRuns pins the property the CLI's narration rests on: the
// event arrives before the Act does anything, so a slow push has a line while it is slow.
func TestAnnounceReportsAnActBeforeItRuns(t *testing.T) {
	var events []ActEvent
	steps := announce([]engine.Step{recordingStep{name: engine.StepCommitted}}, func(e ActEvent) { events = append(events, e) })
	st := &engine.PromotionState{}

	if steps[0].Name() != engine.StepCommitted {
		t.Fatalf("the wrapper must keep the step's name, got %s", steps[0].Name())
	}
	if _, err := steps[0].Observe(context.Background(), st); err != nil || len(events) != 0 {
		t.Fatalf("Observe is not an Act: err=%v events=%v", err, events)
	}
	if err := steps[0].Act(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Step != engine.StepCommitted || events[0].State.CommitSHA != "" {
		t.Fatalf("want one event, sent before the Act recorded anything: %+v", events)
	}
	if st.CommitSHA != "sha-from-committed" {
		t.Fatalf("the wrapped Act must still run: %+v", st)
	}
}

// A failing Act is announced like any other, and its error comes back unchanged.
func TestAnnounceLeavesAFailingActsErrorAlone(t *testing.T) {
	boom := errors.New("push rejected")
	var events []ActEvent
	steps := announce([]engine.Step{recordingStep{name: engine.StepPushed, err: boom}}, func(e ActEvent) { events = append(events, e) })
	if err := steps[0].Act(context.Background(), &engine.PromotionState{}); !errors.Is(err, boom) {
		t.Fatalf("the Act's own error must come back unchanged, got %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want the one start event, got %+v", events)
	}
}

// Without a listener the steps are the caller's own, untouched — the TUI's path.
func TestAnnounceWithoutAListenerIsTheSameSteps(t *testing.T) {
	in := []engine.Step{recordingStep{name: engine.StepPushed}}
	if out := announce(in, nil); !reflect.DeepEqual(out, in) {
		t.Fatalf("got %#v, want the input back", out)
	}
}

// TestStartPromotionAnnouncesItsActs is the wiring: a promotion started with Hooks.OnAct and
// Hooks.OnHistory hears each Act start and then its outcome, in that order, step by step.
func TestStartPromotionAnnouncesItsActs(t *testing.T) {
	fx := newInflightFixture(t)
	pc, err := fx.svc.Plan(context.Background(), PlanRequest{Repo: mustDiscover(t, fx.clone), Source: "app-staging", Target: "app-production"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	d, err := fx.svc.StartPromotion(context.Background(), pc.Request(Mode{Confirmed: true}), Hooks{
		OnAct: func(e ActEvent) { got = append(got, string(e.Step)+" starting") },
		OnHistory: func(e engine.HistoryEntry, _ engine.PromotionState) {
			got = append(got, string(e.Step)+" "+e.Detail)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"branched starting", "branched acted", "committed starting", "committed acted", "pushed starting", "pushed acted", "pr-opened starting", "pr-opened acted"}
	if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("events = %v, want them to begin %v", got, want)
	}
}

func TestPlanReportsResolutionBeforeItRuns(t *testing.T) {
	root := planFixtureRepo(t)
	svc := planTestService(root)
	svc.settings.Resolve.Order = []resolve.Source{resolve.SourceManifest}

	var lines []string
	if _, err := svc.Plan(context.Background(), PlanRequest{Repo: mustDiscover(t, root), Source: "app-staging", Target: "app-production", Progress: func(l string) { lines = append(lines, l) }}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "resolving what app-staging runs to digests (manifest)" {
		t.Fatalf("got %q", lines)
	}

	// No resolution, nothing to report.
	svc.settings.Resolve.Order = nil
	lines = nil
	if _, err := svc.Plan(context.Background(), PlanRequest{Repo: mustDiscover(t, root), Source: "app-staging", Target: "app-production", Progress: func(l string) { lines = append(lines, l) }}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("digest sources: none must report no resolution, got %q", lines)
	}
}

func TestListAbandonAndResumeByEnvReportProgress(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	var lines []string
	progress := func(l string) { lines = append(lines, l) }

	if _, err := svc.List(context.Background(), ListOpts{Progress: progress}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("an empty store has nothing to re-observe, got %q", lines)
	}

	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.List(context.Background(), ListOpts{Progress: progress}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "re-observing 1 promotion(s)") {
		t.Fatalf("List: got %q", lines)
	}

	lines = nil
	if _, err := svc.FindInFlightForEnv(context.Background(), "app-production", Hooks{Progress: progress}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "app-production") {
		t.Fatalf("FindInFlightForEnv: got %q", lines)
	}

	lines = nil
	if _, err := svc.AbandonWith(context.Background(), s.ID, Hooks{Progress: progress}); err != nil {
		t.Fatal(err)
	}
	want := []string{"re-observing " + s.ID + " to confirm it has not landed", "closing PR #1", "deleting branch " + s.Branch + " on origin"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("AbandonWith: got %q, want %q", lines, want)
	}
}
