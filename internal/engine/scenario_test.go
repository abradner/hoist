package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/rollout"
)

// This file drives internal/engine's own world (world_test.go) through the matrix AGENTS.md
// §4.1 names: intact × superseded × reverted, for both the PR path and the direct path. Every
// existing test in this package asks "given this fake's answer, what does one step return";
// these ask "after this SEQUENCE of real events, is the system still able to make progress" —
// exactly the class of bug #165/#166 were, where no single step's Observe was ever wrong for
// its own inputs, but the system as a whole stopped admitting new promotions into an env.

// TestScenarioPRPathIntactConverges: nothing else touches the target env. A PR-mode promotion
// merges, Argo syncs to it, the rollout completes — Drive reports the whole pipeline done, and
// findInFlight's own equivalent never sees it as blocking anything later.
func TestScenarioPRPathIntactConverges(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.argoSyncs(s)
	w.rollsOut(s)

	if err := Drive(context.Background(), w.prSteps(), s, nil); err != nil {
		t.Fatalf("a fully-converged intact promotion should report done, got: %v", err)
	}
	if w.stillInFlight(s) {
		t.Fatal("a landed, converged promotion must not still read as in-flight")
	}
}

// TestScenarioDirectPathIntactConverges is TestScenarioPRPathIntactConverges's direct-mode
// twin: no PR, no merge, but the same convergence shape once the push itself has landed.
func TestScenarioDirectPathIntactConverges(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.argoSyncs(s)
	w.rollsOut(s)

	if err := Drive(context.Background(), w.directSteps(), s, nil); err != nil {
		t.Fatalf("a fully-converged intact direct deploy should report done, got: %v", err)
	}
	if w.stillInFlight(s) {
		t.Fatal("a landed, converged direct deploy must not still read as in-flight")
	}
}

// TestScenarioPRPathMergedRetiresInFlightRegardlessOfWhatHappensNext is what an earlier version
// of this test (named …SupersededRetiresInFlight) actually needed to say. MergedStep's own
// check is ANCESTRY-only (a revert commit never removes the merge from history — AGENTS.md §9
// entry 11), so findInFlight's shallow git/forge-only observation (stillInFlight) retires a
// PR-mode promotion the moment its merge lands and never reconsiders content afterward —
// deliberate (findInFlight's own real scope: "did this promotion's own write land," not "is it
// still what's deployed"), unlike direct mode's own content-based DirectPushedStep
// (TestScenarioDirectPathSupersededRetiresInFlight / TestScenarioDirectPathRevertedStaysInFlight
// below, which DO distinguish the two). An adversarial review found the earlier version asserted
// only the post-supersede half and would have passed identically with the supersede call deleted
// outright — this proves both halves explicitly: retirement happens at the merge itself, and a
// later supersede changes nothing about that verdict, rather than implying supersession was
// somehow load-bearing for the PR path the way it genuinely is for direct mode.
func TestScenarioPRPathMergedRetiresInFlightRegardlessOfWhatHappensNext(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)

	if w.stillInFlight(s) {
		t.Fatal("a merged PR-mode promotion must retire from findInFlight the moment the merge lands")
	}

	w.supersede("ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64))

	if w.stillInFlight(s) {
		t.Fatal("a later supersede must not change a PR-mode promotion's already-retired findInFlight verdict — MergedStep is ancestry-only and never re-examines content")
	}
}

// TestScenarioDirectPathSupersededRetiresInFlight is the direct-mode twin.
func TestScenarioDirectPathSupersededRetiresInFlight(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.supersede("ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64))

	if w.stillInFlight(s) {
		t.Fatal("a direct deploy superseded by a later deploy of the same image repo has landed and is finished; it must not block later promotions into this env")
	}
}

// TestScenarioPRPathRevertNeverGoesTerminalOnceArgoObservesIt is supersede's mirror case at the
// layer where it actually bites for the PR path: MergedStep's own check is ANCESTRY-only (a
// revert commit never removes the merge from history — AGENTS.md §9 entry 11), so
// stillInFlight's shallow git/forge-only read (findInFlight's own real scope: "did this
// promotion's own write land", not "is it still what's deployed") correctly still retires a
// PR-mode promotion once its merge lands, revert or not — that is deliberate, not a bug: the
// branch is gone and a human already approved the merge, so a later revert is a separate event,
// not evidence this promotion's own action never happened. Where a revert DOES have to matter is
// convergence: once Argo reports synced at a revision that reverted the change,
// ArgoSyncedStep's own content-based check (observeLanded) must refuse to call the promotion
// complete, mirroring steps_m5_test.go's own TestArgoSyncedRejectsARevisionThatRevertedTheMerge
// but driven as a full sequence through Drive rather than one step's Observe in isolation.
func TestScenarioPRPathRevertNeverGoesTerminalOnceArgoObservesIt(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.revert(s)
	w.argoSyncs(s) // reports synced at the post-revert tip — content no longer carries the promotion

	if err := Drive(context.Background(), w.prSteps(), s, nil); err == nil {
		t.Fatal("a promotion whose merge was reverted before Argo ever synced to it must not report done")
	}
}

// TestScenarioDirectPathRevertedStaysInFlight is the direct-mode twin.
func TestScenarioDirectPathRevertedStaysInFlight(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.revert(s)

	if !w.stillInFlight(s) {
		t.Fatal("a reverted direct deploy did not land; it must not read as retired the way a superseded one does")
	}
}

// TestScenarioResumeAfterKillIsIdempotentPRPath simulates the process dying right after a PR
// merges (and Argo/rollout converging) and being restarted cold: a brand-new PromotionState for
// the identical identity, re-driven from the top with no memory of what the original ever
// recorded. It must reach the same landed state without acting twice — no second branch, no
// second commit, no second PR (AGENTS.md §4.1: a random id, or trusting a recorded phase, would
// open a second PR on restart; a deterministic id plus re-observation must not).
func TestScenarioResumeAfterKillIsIdempotentPRPath(t *testing.T) {
	w := newWorld(t)
	original := w.promote()
	w.merge(original)
	w.argoSyncs(original)
	w.rollsOut(original)

	resumed := w.resume(false)

	if resumed.ID != original.ID || resumed.Branch != original.Branch {
		t.Fatalf("fixture precondition: a resumed state must share the original's deterministic identity, got id=%s branch=%s vs %s/%s", resumed.ID, resumed.Branch, original.ID, original.Branch)
	}
	if resumed.MergeSHA != original.MergeSHA {
		t.Errorf("resumed.MergeSHA = %s, want the same merge %s the original already landed", resumed.MergeSHA, original.MergeSHA)
	}
	if err := Drive(context.Background(), w.prSteps(), resumed, nil); err != nil {
		t.Errorf("re-driving the resumed state should find everything already satisfied, got: %v", err)
	}
	if prs := w.forge.PRs(); len(prs) != 1 {
		t.Fatalf("resuming opened %d PRs, want exactly 1 — a killed-and-restarted process must not open a second PR for the same promotion", len(prs))
	}
}

// TestScenarioResumeAfterKillIsIdempotentDirectPath is the direct-mode twin: no PR to
// duplicate, but the same "must not push twice" shape.
func TestScenarioResumeAfterKillIsIdempotentDirectPath(t *testing.T) {
	w := newWorld(t)
	original := w.directDeploy()
	w.argoSyncs(original)
	w.rollsOut(original)

	resumed := w.resume(true)

	if resumed.ID != original.ID {
		t.Fatalf("fixture precondition: a resumed state must share the original's deterministic id, got %s vs %s", resumed.ID, original.ID)
	}
	if resumed.PushedSHA != original.PushedSHA {
		t.Errorf("resumed.PushedSHA = %s, want the same push %s the original already landed", resumed.PushedSHA, original.PushedSHA)
	}
	if err := Drive(context.Background(), w.directSteps(), resumed, nil); err != nil {
		t.Errorf("re-driving the resumed direct state should find everything already satisfied, got: %v", err)
	}
}

// TestScenarioSupersededPromotionNeverReachesRolledOutTerminal is the one case the plan expects
// to fail and says to let it: RolledOutStep still compares live containers against this
// promotion's own Edit.New (internal/engine's own package doc comment, "interim state, stated
// not enforced (#168)") — a promotion superseded by a later deploy will never see that image
// again, so while stillInFlight (findInFlight's own read) correctly retires it on the direct
// path (TestScenarioDirectPathSupersededRetiresInFlight above), the FULL pipeline including
// convergence never goes terminal for it.
//
// Deliberately does NOT use w.rollsOut here: rollsOut builds its rollout.Fake from s.Edits —
// this promotion's OWN planned image — which by construction always satisfies RolledOutStep
// regardless of any supersede, so it could never exercise #168 at all (a mutation-review finding
// against an earlier version of this test, which used rollsOut and therefore silently passed
// even unskipped). The live cluster here instead genuinely runs what superseded it — exactly
// what a real cluster reports once the later deploy's own rollout completed — so RolledOutStep's
// own image comparison against this promotion's stale Edit.New can never match.
//
// Skipped naming #168; #168's own PR deletes this skip once RolledOutStep's gate moves to the
// per-occurrence landed verdict the package doc comment describes as the fix.
func TestScenarioSupersededPromotionNeverReachesRolledOutTerminal(t *testing.T) {
	t.Skip("known gap, #168: RolledOutStep still gates on this promotion's own Edit.New, which a superseded promotion never sees again")

	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	supersededRef := "ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64)
	w.supersede(supersededRef)
	w.argoSyncs(s)

	deployments, _ := groupEditsByWorkload(s.Edits)
	ro := &rollout.Fake{}
	for name, wants := range deployments {
		imgs := make([]rollout.ContainerImage, len(wants))
		for i, want := range wants {
			imgs[i] = rollout.ContainerImage{Name: want.Container, Init: want.Init, Image: supersededRef}
		}
		ro.SetDeployment(s.TargetEnv, name, rollout.DeploymentStatus{
			Namespace: s.TargetEnv, Name: name, Images: imgs,
			Complete: true, Detail: "rolled out the later, superseding deploy",
		})
	}
	w.ro = ro
	w.drive(s, w.prSteps())

	if err := Drive(context.Background(), w.prSteps(), s, nil); err != nil {
		t.Fatalf("#168 fixed: a superseded promotion now reaches a full terminal pass, got: %v", err)
	}
}
