package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

// TestTUIOverrideCINoneReachesTheEngineStepPerPromotion is the wiring test for #103: the
// TUI's confirm path (buildStartPromotion) starts a promotion with CINoneOverride false, so
// under ci.none: prompt the drive blocks at CI with the reason the flight screen recognises
// (engine.IsCINonePromptBlock); setting the override on that promotion's state — what
// flight.Model.ApplyCINoneOverride does in answer to flight.OverrideCINoneMsg — is carried by
// the same DriveFunc into engine.Drive, where CIGreenStep.Observe reads it and the drive's own
// save persists it. The override lives in the per-promotion state, never in the DriveFunc:
// the same DriveFunc handed a state without the flag blocks again.
func TestTUIOverrideCINoneReachesTheEngineStepPerPromotion(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	// The fixture's config says ci.none: green; this test needs prompt, the one policy with
	// an override.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "none: green") {
		t.Fatalf("fixture precondition: config should say ci.none: green:\n%s", raw)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(raw), "none: green", "none: prompt", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	eff := buildEffForFixture(t, cfgPath)

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", eff.promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ro, cerr := tuiCluster(t)
	start := buildStartPromotion(eff, r, newGit, f, nil, a, ro, cerr)
	state, driveFn, err := start(context.Background(), plan, app.StartOpts{})
	if err != nil {
		t.Fatalf("startPromotion: %v", err)
	}
	if state.CINoneOverride {
		t.Fatal("the confirm path must start a promotion with CINoneOverride false — nothing at launch may weaken the gate (AGENTS.md §4.5)")
	}
	if state.CINone != "prompt" {
		t.Fatalf("state.CINone = %q, want prompt", state.CINone)
	}

	ciStatus := func(statuses []engine.StepStatus) engine.StepStatus {
		for _, st := range statuses {
			if st.Step == engine.StepCIGreen {
				return st
			}
		}
		return engine.StepStatus{}
	}
	// Drive until CI blocks on the ci.none reason (the fake forge reports no checks; the
	// fixture's grace is 5ms).
	cur := state
	var blocked engine.StepStatus
	for i := 0; i < 200; i++ {
		next, _, statuses, err := driveFn(context.Background(), cur)
		if err != nil {
			t.Fatalf("drive %d: %v", i, err)
		}
		cur = next
		if st := ciStatus(statuses); st.Blocked != "" {
			blocked = st
			break
		}
	}
	if blocked.Blocked == "" {
		t.Fatalf("the drive never blocked at CI; last state: %+v", cur)
	}
	if !engine.IsCINonePromptBlock(blocked.Blocked) {
		t.Fatalf("blocked at CI for a reason the flight screen would not offer c for: %q", blocked.Blocked)
	}

	// Per promotion, not per DriveFunc: the same DriveFunc handed the state again, still
	// without the flag, blocks again — nothing the confirm path built carries the override.
	_, _, statuses, err := driveFn(context.Background(), cur)
	if err != nil {
		t.Fatalf("second drive without the override: %v", err)
	}
	if st := ciStatus(statuses); !engine.IsCINonePromptBlock(st.Blocked) {
		t.Fatalf("a state without the override should block again at CI, got %+v", st)
	}
	if len(f.PRs()) != 1 || f.PRs()[0].Merged {
		t.Fatalf("nothing may merge while CI is blocked, got %+v", f.PRs())
	}

	// What the root does for flight.OverrideCINoneMsg, on this promotion's state only.
	overridden := cur
	overridden.CINoneOverride = true
	next, _, statuses, err := driveFn(context.Background(), overridden)
	if err != nil {
		t.Fatalf("re-drive with the override: %v", err)
	}
	// CIGreenStep.Observe read the flag: the drive went on past CI. Approval is auto in this
	// fixture, so the same call can reach the merge, after which engine.Status no longer
	// reports the CI step at all — either shape proves the step saw the override.
	if st := ciStatus(statuses); !st.Satisfied && next.MergeSHA == "" {
		t.Fatalf("CI should be satisfied once the override is on the state, got %+v (state %+v)", st, next)
	}
	if !next.CINoneOverride {
		t.Error("the re-drive's returned state lost the override")
	}
	// Persisted by the drive's own save, exactly as `hoist resume --override-ci-none` leaves
	// it — so a later CLI resume of this id keeps the operator's answer.
	saved, err := engine.LoadState(mustStatePath(t, next.ID))
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || !saved.CINoneOverride {
		t.Errorf("state file should persist CINoneOverride=true after the re-drive, got %+v", saved)
	}
}
