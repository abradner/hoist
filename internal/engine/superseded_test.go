package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/rollout"
)

// These tests are #168's: a promotion that landed and was then superseded by a later deploy of
// the same image repo must reach a terminal phase, and must do so occurrence by occurrence —
// an occurrence still carrying this promotion's own reference keeps gating the rollout.
//
// Each setup produces the situation the way production does. The PR-path state's MergeSHA is
// exactly what MergedStep wrote (an earlier attempt at this fix passed its own test by assigning
// MergeSHA = tip by hand, which hid that it judged the wrong revision), and the live cluster
// reports what a real one would after the later deploy rolled — never s.Edits' own New, which
// would satisfy RolledOutStep whatever the supersede did.

// supersededRef is the later deploy's reference in every test here: the same image repo as the
// fixture's promotion, at a reference that is neither what it planned nor what it edited away
// from.
var supersededRef = "ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64)

// appAndWorker is a family manifest with two Deployments on the promoted image repo — "app" and
// "worker", one container each, in one file — the ordinary shape in which one occurrence can be
// superseded while the other is not.
func appAndWorker(env, ref string) string {
	return oneDeployment(env, ref) + "---\n" + strings.ReplaceAll(oneDeployment(env, ref), "name: app", "name: worker")
}

// supersedeNth writes ref into only the nth (0-based) image line of the family's manifest at
// origin's base, from a separate clone, the way a later deploy that touched one Deployment would.
func supersedeNth(t *testing.T, fx fixture, nth int, ref string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	seen := 0
	replaced := false
	for i, line := range lines {
		if !strings.Contains(line, "image: ghcr.io/") {
			continue
		}
		if seen == nth {
			lines[i] = "          image: " + ref
			replaced = true
			break
		}
		seen++
	}
	if !replaced {
		t.Fatalf("fixture precondition: no image line %d in %s:\n%s", nth, p, b)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	runHost(t, other, "commit", "-q", "-a", "-m", "a later deploy into one Deployment")
	runHost(t, other, "push", "-q", "origin", "main")
}

// live sets one single-container Deployment's live state in ro.
func live(ro *rollout.Fake, ns, name, image string, complete, deadline bool) {
	ro.SetDeployment(ns, name, rollout.DeploymentStatus{
		Namespace: ns, Name: name,
		Images:           []rollout.ContainerImage{{Name: name, Image: image}},
		Complete:         complete,
		DeadlineExceeded: deadline,
		Detail:           "status of " + name,
	})
}

// rolledOutObservation observes RolledOutStep alone over the world's current fakes.
func (w *world) rolledOutObservation(s *PromotionState) Observation {
	w.t.Helper()
	obs, err := RolledOutStep{Rollout: w.ro, Git: w.g}.Observe(context.Background(), s)
	if err != nil {
		w.t.Fatalf("RolledOutStep.Observe: %v", err)
	}
	return obs
}

// TestScenarioPRPathSupersededPromotionGoesTerminal: a PR-mode promotion merges, a later deploy
// supersedes it, Argo syncs the later deploy and the cluster rolls it out. The promotion is
// finished — its change landed and was legitimately replaced — so the full pipeline, rollout
// included, reports done, saying it was superseded.
func TestScenarioPRPathSupersededPromotionGoesTerminal(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	mergeSHA := s.MergeSHA
	if mergeSHA == "" {
		t.Fatal("setup: the merge step should have recorded MergeSHA")
	}
	w.supersede(supersededRef)
	w.argoSyncs(s)

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, true, false)
	if err := Drive(context.Background(), w.prSteps(), s, nil); err != nil {
		t.Fatalf("a superseded promotion must reach a full terminal pass, got: %v", err)
	}
	if s.MergeSHA != mergeSHA {
		t.Fatalf("setup: MergeSHA moved from %s to %s; the test must judge the revision production code leaves", mergeSHA, s.MergeSHA)
	}
	obs := w.rolledOutObservation(s)
	if !obs.Satisfied || !strings.Contains(obs.Detail, "superseded") || !strings.Contains(obs.Detail, supersededRef) {
		t.Errorf("RolledOutStep should be satisfied and name the later reference that superseded it, got %+v", obs)
	}
}

// TestScenarioDirectPathSupersededPromotionGoesTerminal is the direct-path mirror: LandedSHA()
// is the direct push, and the later deploy lands on top of it.
func TestScenarioDirectPathSupersededPromotionGoesTerminal(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.supersede(supersededRef)
	w.argoSyncs(s)

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, true, false)
	if err := Drive(context.Background(), w.directSteps(), s, nil); err != nil {
		t.Fatalf("a superseded direct promotion must reach a full terminal pass, got: %v", err)
	}
	if obs := w.rolledOutObservation(s); !obs.Satisfied || !strings.Contains(obs.Detail, "superseded") {
		t.Errorf("RolledOutStep should be satisfied and say superseded, got %+v", obs)
	}
}

// TestRolledOutSupersededDeploymentDoesNotBlockOnTheLaterRollout: once a Deployment's every
// occurrence is superseded, its rollout belongs to the later deploy. That rollout exceeding its
// progress deadline is the later deploy's failure, and must not Block a promotion that is
// already finished.
func TestRolledOutSupersededDeploymentDoesNotBlockOnTheLaterRollout(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.supersede(supersededRef)

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, false, true)
	if obs := w.rolledOutObservation(s); !obs.Satisfied || obs.Blocked != "" {
		t.Errorf("a superseded Deployment's own failed rollout is not this promotion's; want Satisfied, got %+v", obs)
	}
}

// TestRolledOutKeepsWaitingOnAnIntactOccurrenceBesideASupersededOne is the granularity case
// #167's review found: a block editing two Deployments, one superseded by a later deploy and
// one still carrying this promotion's reference. The superseded one is exempt; the other still
// gates the step until it has rolled out.
func TestRolledOutKeepsWaitingOnAnIntactOccurrenceBesideASupersededOne(t *testing.T) {
	w := newWorldWith(t, newFixtureWith(t, appAndWorker, 2))
	s := w.promote()
	w.merge(s)
	if s.MergeSHA == "" {
		t.Fatal("setup: the merge step should have recorded MergeSHA")
	}
	supersedeNth(t, w.fx, 1, supersededRef) // the worker only
	ours := s.Edits[0].New.String()
	old := s.Edits[0].Ref.String()

	for _, tc := range []struct {
		name     string
		appImage string
		complete bool
	}{
		{"app mid-rollout of this promotion", ours, false},
		{"app not yet on this promotion's image", old, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.ro = &rollout.Fake{}
			live(w.ro, s.TargetEnv, "app", tc.appImage, tc.complete, false)
			live(w.ro, s.TargetEnv, "worker", supersededRef, true, false)
			obs := w.rolledOutObservation(s)
			if obs.Satisfied || !obs.Waiting {
				t.Fatalf("the app Deployment still carries this promotion's change and has not rolled out; want Waiting, got %+v", obs)
			}
			if !strings.Contains(obs.Detail, "app:") {
				t.Errorf("Detail should name the app Deployment it waits on, got %q", obs.Detail)
			}
		})
	}

	t.Run("app container missing from the live spec", func(t *testing.T) {
		w.ro = &rollout.Fake{}
		w.ro.SetDeployment(s.TargetEnv, "app", rollout.DeploymentStatus{Namespace: s.TargetEnv, Name: "app", Complete: true})
		live(w.ro, s.TargetEnv, "worker", supersededRef, true, false)
		if obs := w.rolledOutObservation(s); obs.Satisfied {
			t.Fatalf("a container this promotion wrote is missing from the live spec; must not be satisfied, got %+v", obs)
		}
	})

	t.Run("app rolled out", func(t *testing.T) {
		w.ro = &rollout.Fake{}
		live(w.ro, s.TargetEnv, "app", ours, true, false)
		live(w.ro, s.TargetEnv, "worker", supersededRef, true, false)
		obs := w.rolledOutObservation(s)
		if !obs.Satisfied {
			t.Fatalf("app rolled out and worker was superseded; want Satisfied, got %+v", obs)
		}
		if !strings.Contains(obs.Detail, "worker") || !strings.Contains(obs.Detail, "superseded") {
			t.Errorf("Detail should say the worker was superseded, got %q", obs.Detail)
		}
	})
}

// TestRolledOutDoesNotExemptARevertedPromotion is the control for the exemption: a revert puts
// back the reference this promotion edited away from, and the live cluster running it is this
// promotion NOT being in effect — it must keep waiting, not report finished.
func TestRolledOutDoesNotExemptARevertedPromotion(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.revert(s)

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", s.Edits[0].Ref.String(), true, false)
	if obs := w.rolledOutObservation(s); obs.Satisfied {
		t.Fatalf("a reverted promotion is not finished; got %+v", obs)
	}
}

// TestRolledOutDoesNotExemptWhenTheLandingIsNotInTheBase: the base declares a newer reference of
// the same image repo, but this promotion's own landed commit is not in its history — the base
// was rewritten. A differing reference is not by itself evidence that this promotion ever
// landed (AGENTS.md §4.1), so nothing is exempt.
func TestRolledOutDoesNotExemptWhenTheLandingIsNotInTheBase(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)

	other := filepath.Join(t.TempDir(), "rewrite-clone")
	runHost(t, "", "clone", "-q", w.fx.originDir, other)
	runHost(t, other, "reset", "-q", "--hard", s.MergeSHA+"^")
	p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), s.Edits[0].Ref.String(), supersededRef, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	runHost(t, other, "commit", "-q", "-a", "-m", "a deploy on a rewritten base")
	runHost(t, other, "push", "-q", "--force", "origin", "main")

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, true, false)
	if obs := w.rolledOutObservation(s); obs.Satisfied {
		t.Fatalf("this promotion's landing is not in the base's history; nothing may be exempt, got %+v", obs)
	}
}

// TestRolledOutFetchFailureIsNotAnExemption: when the base cannot be read, nothing can be judged
// superseded, and the step must not report finished on the strength of a read that failed.
func TestRolledOutFetchFailureIsNotAnExemption(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.supersede(supersededRef)

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, true, false)
	broken := *s
	broken.CloneDir = filepath.Join(t.TempDir(), "no-such-clone")
	obs, err := RolledOutStep{Rollout: w.ro, Git: w.g}.Observe(context.Background(), &broken)
	if err == nil && obs.Satisfied {
		t.Fatalf("a failed read of the base must not exempt anything, got %+v", obs)
	}
}
