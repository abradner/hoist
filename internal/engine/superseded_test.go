package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/git"
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
	if !obs.Satisfied || !obs.Superseded || !strings.Contains(obs.Detail, "superseded") || !strings.Contains(obs.Detail, supersededRef) {
		t.Errorf("RolledOutStep should be satisfied, typed Superseded, and name the later reference that superseded it, got %+v", obs)
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
	if obs := w.rolledOutObservation(s); !obs.Satisfied || !obs.Superseded || !strings.Contains(obs.Detail, "superseded") {
		t.Errorf("RolledOutStep should be satisfied, typed Superseded, and say superseded, got %+v", obs)
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
		if !obs.Superseded || !strings.Contains(obs.Detail, "worker") || !strings.Contains(obs.Detail, "superseded") {
			t.Errorf("the observation should be typed Superseded and say the worker was, got %+v", obs)
		}
	})
}

// TestRolledOutIntactRolloutIsNotSuperseded is the control for Observation.Superseded: a
// promotion whose own images rolled out is done, and must not read as replaced.
func TestRolledOutIntactRolloutIsNotSuperseded(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.ro = satisfiedRollout(s.TargetEnv, s.Edits)
	if obs := w.rolledOutObservation(s); !obs.Satisfied || obs.Superseded || strings.Contains(obs.Detail, "superseded") {
		t.Errorf("an intact, rolled-out promotion is done and not superseded, got %+v", obs)
	}
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

// failingFetch is a git.Git whose FetchBranch always fails, every other call real.
type failingFetch struct{ git.Git }

func (failingFetch) FetchBranch(context.Context, string, string, string) (string, bool, error) {
	return "", false, errors.New("fetch failed")
}

// TestRolledOutFetchFailureIsReportedNotJudged: when the base cannot be fetched, nothing can be
// judged superseded at it — not from the clone's stale remote-tracking ref either, which here
// already holds the superseding commit. The failure is returned (RolledOutStep is retryable,
// so the poll asks again), never swallowed into an exemption or a quiet wait.
func TestRolledOutFetchFailureIsReportedNotJudged(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.supersede(supersededRef)
	runHost(t, s.CloneDir, "fetch", "-q", "origin") // the stale ref a fallback could misuse

	w.ro = &rollout.Fake{}
	live(w.ro, s.TargetEnv, "app", supersededRef, true, false)
	obs, err := RolledOutStep{Rollout: w.ro, Git: failingFetch{w.g}}.Observe(context.Background(), s)
	if err == nil {
		t.Fatalf("a failed fetch of the base must be reported, got %+v", obs)
	}
	if obs.Satisfied {
		t.Fatalf("a failed fetch must not exempt anything, got %+v", obs)
	}
}

// TestArgoSyncedHealthGatesAnApplicationWithAnIntactOccurrence is the same granularity rule in
// ArgoSyncedStep: an Application counts as superseded — and skips its Synced/Healthy check —
// only when every occurrence this promotion changed in it is superseded. With the worker
// superseded and the app Deployment still carrying this promotion's reference, the
// Application's health is still this promotion's business. The control supersedes both, and
// the Application is then exempt as before (#182).
func TestArgoSyncedHealthGatesAnApplicationWithAnIntactOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supersede []int
		satisfied bool
	}{
		{"one of two superseded", []int{1}, false},
		{"both superseded (control)", []int{0, 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldWith(t, newFixtureWith(t, appAndWorker, 2))
			s := w.promote()
			w.merge(s)
			s.EditApps = map[string]string{}
			for _, e := range s.Edits {
				s.EditApps[e.File] = testApp
			}
			for _, nth := range tc.supersede {
				supersedeNth(t, w.fx, nth, supersededRef)
			}
			tip, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil || !ok {
				t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
			}
			a := &argo.Fake{}
			a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
				argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing"})
			obs, err := ArgoSyncedStep{Argo: a, Git: w.g}.Observe(context.Background(), s)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if obs.Satisfied != tc.satisfied {
				t.Fatalf("Satisfied = %v, want %v: %+v", obs.Satisfied, tc.satisfied, obs)
			}
			if !tc.satisfied && !obs.Waiting {
				t.Errorf("an Application still carrying this promotion's change and Progressing should be Waiting, got %+v", obs)
			}
		})
	}
}

// TestArgoSyncedDoesNotBlockASupersededApplicationOnTheLaterDeploysHealth: Argo CD's own health
// check reports a Deployment past its progress deadline as Degraded, so a later deploy whose
// rollout failed makes the Application Degraded. When every occurrence this promotion changed
// in that Application is superseded, that failure is the later deploy's, and a finished
// promotion must not be Blocked at argo-synced on it — the step-level twin of
// TestRolledOutSupersededDeploymentDoesNotBlockOnTheLaterRollout, which RolledOutStep alone
// could not show. The control keeps one occurrence intact: the Application's Degraded health is
// then this promotion's too, and Blocks as it always has.
func TestArgoSyncedDoesNotBlockASupersededApplicationOnTheLaterDeploysHealth(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supersede []int
		blocked   bool
	}{
		{"every occurrence superseded", []int{0, 1}, false},
		{"one occurrence still intact (control)", []int{1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldWith(t, newFixtureWith(t, appAndWorker, 2))
			s := w.promote()
			w.merge(s)
			s.EditApps = map[string]string{}
			for _, e := range s.Edits {
				s.EditApps[e.File] = testApp
			}
			for _, nth := range tc.supersede {
				supersedeNth(t, w.fx, nth, supersededRef)
			}
			tip, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil || !ok {
				t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
			}
			for _, st := range []argo.Status{
				{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: argo.HealthStatusDegraded},
				{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing", OperationPhase: argo.OperationFailed},
			} {
				a := &argo.Fake{}
				a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp}, st)
				obs, err := ArgoSyncedStep{Argo: a, Git: w.g}.Observe(context.Background(), s)
				if err != nil {
					t.Fatalf("Observe: %v", err)
				}
				if (obs.Blocked != "") != tc.blocked {
					t.Errorf("health=%s phase=%s: Blocked = %q, want blocked=%v", st.HealthStatus, st.OperationPhase, obs.Blocked, tc.blocked)
				}
				if !tc.blocked && !obs.Satisfied {
					t.Errorf("health=%s phase=%s: a superseded Application needs nothing from this step, got %+v", st.HealthStatus, st.OperationPhase, obs)
				}
			}
		})
	}
}

// ancestryFailsAt is a git.Git whose IsAncestor fails for one descendant, every other call real.
type ancestryFailsAt struct {
	git.Git
	descendant string
}

func (f ancestryFailsAt) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	if descendant == f.descendant {
		return false, errors.New("git merge-base: fatal: exit status 128")
	}
	return f.Git.IsAncestor(ctx, dir, ancestor, descendant)
}

// TestArgoSyncedTipAncestryFailureIsReportedNotJudged (#265): when Argo's synced revision
// superseded this promotion and the base has moved on since, the step asks whether the tip
// still has this promotion's landing in its history. Both commits are in the clone by then, so
// a failure of that question is a failure, reported for the poll to ask again — never a Wait
// claiming the tip "no longer contains this promotion's landing", and never an exemption.
func TestArgoSyncedTipAncestryFailureIsReportedNotJudged(t *testing.T) {
	w := newWorld(t)
	s := w.promote()
	w.merge(s)
	w.supersede(supersededRef)
	synced, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
	if err != nil || !ok {
		t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
	}
	supersedeNth(t, w.fx, 0, "ghcr.io/example/app:v4@sha256:"+strings.Repeat("4", 64))
	tip, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
	if err != nil || !ok || tip == synced {
		t.Fatalf("setup: the base should have moved past the synced revision: %v (ok=%v)", err, ok)
	}
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: synced, HealthStatus: argo.HealthStatusHealthy})
	obs, err := ArgoSyncedStep{Argo: a, Git: ancestryFailsAt{Git: w.g, descendant: tip}}.Observe(context.Background(), s)
	if err == nil {
		t.Fatalf("a failed ancestry check must be reported, got %+v", obs)
	}
	if obs.Satisfied || strings.Contains(obs.Detail, "no longer contains") {
		t.Fatalf("a failed ancestry check is no answer; got %+v", obs)
	}
}

// TestArgoSyncedSupersedeMustStillHoldAtTheBaseTip (Codex, #263): Argo reports the revision it
// last synced, which can lag the base. When that revision superseded this promotion but the
// base has since moved on in a way that undoes the supersede — the image put back to what this
// promotion edited away from, or to this promotion's own reference, or a base rewritten without
// this promotion's landing, or gone — Argo's stale revision is no evidence the promotion is
// still replaced, and nor is its health this promotion's: Argo has not synced what the base now
// says, so the step waits for the tip, whatever the stale revision's health or operation phase
// (#265 — those belong to the later deploy, and reverting a deploy that broke is the likeliest
// way a supersede is undone). The Waiting line names the tip it waits for, not LandedSHA(),
// which is already in the stale revision's history.
//
// The controls are the movements that do not undo it, which in a live repo are the common
// case: an unrelated commit after the supersede, and a third deploy of the same image repo.
// Both keep the Application exempt, Degraded or not.
func TestArgoSyncedSupersedeMustStillHoldAtTheBaseTip(t *testing.T) {
	unrelated := func(t *testing.T, fx fixture) {
		other := filepath.Join(t.TempDir(), "unrelated-clone")
		runHost(t, "", "clone", "-q", fx.originDir, other)
		if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runHost(t, other, "add", "README.md")
		runHost(t, other, "commit", "-q", "-m", "an unrelated change")
		runHost(t, other, "push", "-q", "origin", "main")
	}
	for _, tc := range []struct {
		name string
		then func(t *testing.T, w *world, s *PromotionState)
		// exempt: the Application needs nothing from the step, whatever its health.
		exempt bool
		// why is what the Waiting detail must name when the supersede no longer holds.
		why string
	}{
		{"base still superseded (control)", func(*testing.T, *world, *PromotionState) {}, true, ""},
		{"an unrelated commit after the supersede (control)", func(t *testing.T, w *world, _ *PromotionState) { unrelated(t, w.fx) }, true, ""},
		{"a third deploy of the same image repo (control)", func(t *testing.T, w *world, _ *PromotionState) {
			supersedeNth(t, w.fx, 0, "ghcr.io/example/app:v4@sha256:"+strings.Repeat("4", 64))
		}, true, ""},
		{"base reverted to the original", func(t *testing.T, w *world, s *PromotionState) {
			supersedeNth(t, w.fx, 0, s.Edits[0].Ref.String())
		}, false, "no longer supersedes"},
		{"base back on this promotion's reference", func(t *testing.T, w *world, s *PromotionState) {
			supersedeNth(t, w.fx, 0, s.Edits[0].New.String())
		}, false, "no longer supersedes"},
		{"base rewritten without this promotion's landing", func(t *testing.T, w *world, s *PromotionState) {
			other := filepath.Join(t.TempDir(), "rewrite-clone")
			runHost(t, "", "clone", "-q", w.fx.originDir, other)
			runHost(t, other, "reset", "-q", "--hard", s.MergeSHA+"^")
			p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(strings.Replace(string(b), s.Edits[0].Ref.String(), "ghcr.io/example/app:v5@sha256:"+strings.Repeat("5", 64), 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			runHost(t, other, "commit", "-q", "-a", "-m", "a deploy on a rewritten base")
			runHost(t, other, "push", "-q", "--force", "origin", "main")
		}, false, "no longer contains"},
		{"base branch gone", func(t *testing.T, w *world, s *PromotionState) {
			runHost(t, w.fx.originDir, "update-ref", "-d", "refs/heads/"+s.Base)
		}, false, "has no branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			s := w.promote()
			w.merge(s)
			s.EditApps = map[string]string{}
			for _, e := range s.Edits {
				s.EditApps[e.File] = testApp
			}
			w.supersede(supersededRef)
			synced, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil || !ok {
				t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
			}
			// The clone has the revision Argo synced, as it would from an earlier poll; without
			// it, a base that no longer descends from it leaves it unresolvable here, and the
			// step never reaches the tip check this test is about.
			runHost(t, s.CloneDir, "fetch", "-q", "origin")
			tc.then(t, w, s)
			tip, tipOK, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil {
				t.Fatalf("reading origin/%s: %v", s.Base, err)
			}
			for _, st := range []argo.Status{
				{HealthStatus: argo.HealthStatusDegraded},
				{HealthStatus: "Progressing", OperationPhase: argo.OperationFailed},
				{HealthStatus: argo.HealthStatusHealthy},
			} {
				st.SyncStatus, st.SyncRevision = argo.SyncStatusSynced, synced
				label := "health=" + st.HealthStatus + " phase=" + st.OperationPhase
				a := &argo.Fake{}
				a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp}, st)
				obs, err := ArgoSyncedStep{Argo: a, Git: w.g}.Observe(context.Background(), s)
				if err != nil {
					t.Fatalf("%s: Observe: %v", label, err)
				}
				if tc.exempt {
					if !obs.Satisfied {
						t.Errorf("%s: still superseded at the base tip, so exempt; want Satisfied, got %+v", label, obs)
					}
					continue
				}
				// Not exempt: Argo has not synced what the base now says, so the revision it did
				// sync no longer stands for this promotion, and nor does that revision's health
				// or operation — those are the later deploy's (#265). Wait for the tip.
				if !obs.Waiting || obs.Blocked != "" || !strings.Contains(obs.Detail, tc.why) {
					t.Errorf("%s: want Waiting naming %q, not a Block on the stale revision's health, got %+v", label, tc.why, obs)
				}
				if tipOK && !strings.Contains(obs.Detail, "want "+tip) {
					t.Errorf("%s: the step waits for Argo to sync the tip; Detail should say want %s, got %q", label, tip, obs.Detail)
				}
				if strings.Contains(obs.Detail, "want "+s.LandedSHA()) {
					t.Errorf("%s: LandedSHA is already in the synced revision's history, so it is not what the step waits for; got %q", label, obs.Detail)
				}
			}
		})
	}
}
