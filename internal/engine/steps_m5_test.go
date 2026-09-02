package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/rollout"
)

const testArgoNamespace = "argocd"
const testApp = "app-app-production"

// --- ArgoAppNames -----------------------------------------------------------------------

func TestArgoAppNamesDedupesAndSorts(t *testing.T) {
	r := &gitops.Repo{Envs: map[string]*gitops.Env{
		"app-production": {Name: "app-production", Families: map[string]*gitops.Family{
			"app": {Name: "app", Dir: "cluster/apps/app-production/app", App: "app-app-production"},
			"web": {Name: "web", Dir: "cluster/apps/app-production/web", App: "web-app-production"},
		}},
	}}
	edits := []gitops.Edit{
		{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/web/deployment.yaml"}},
		{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/app/deployment.yaml"}},
		{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/app/job.yaml"}}, // same app again
	}
	names, err := ArgoAppNames(r, "app-production", edits)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app-app-production", "web-app-production"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("ArgoAppNames = %v, want %v", names, want)
	}
}

func TestArgoAppNamesUnknownTargetEnv(t *testing.T) {
	r := &gitops.Repo{Envs: map[string]*gitops.Env{}}
	if _, err := ArgoAppNames(r, "nowhere", nil); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("err = %v, want it to name the missing target env", err)
	}
}

func TestArgoAppNamesEditOutsideAnyFamily(t *testing.T) {
	r := &gitops.Repo{Envs: map[string]*gitops.Env{
		"app-production": {Name: "app-production", Families: map[string]*gitops.Family{
			"app": {Name: "app", Dir: "cluster/apps/app-production/app", App: "app-app-production"},
		}},
	}}
	edits := []gitops.Edit{{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/ghost/deployment.yaml"}}}
	_, err := ArgoAppNames(r, "app-production", edits)
	if err == nil || !strings.Contains(err.Error(), "cluster/apps/app-production/ghost/deployment.yaml") {
		t.Errorf("err = %v, want it to name the orphan edit's file", err)
	}
}

// --- ArgoRefreshedStep --------------------------------------------------------------------

// argoState is a minimal PromotionState for testing ArgoRefreshedStep/ArgoSyncedStep in
// isolation, without any real git/forge machinery — mergedAt only reads s.History, so a
// single synthetic Merged entry is all either step needs.
func argoState() *PromotionState {
	const mergedAgo = time.Minute
	return &PromotionState{
		TargetEnv:     "app-production",
		MergeSHA:      "deadbeef",
		ArgoNamespace: testArgoNamespace,
		ArgoApps:      []string{testApp},
		History:       []HistoryEntry{{Step: StepMerged, At: time.Now().Add(-mergedAgo)}},
	}
}

func TestArgoRefreshedNotSatisfiedBeforeAnyMergeIsRecorded(t *testing.T) {
	s := argoState()
	s.MergeSHA = "" // MergedStep hasn't landed yet in this Observe's view
	a := &argo.Fake{}
	obs, err := (ArgoRefreshedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Errorf("Observe = %+v, want not satisfied before MergeSHA is set", obs)
	}
	if len(a.Calls) != 0 {
		t.Errorf("Argo was called before there was anything merged to refresh toward: %v", a.Calls)
	}
}

func TestArgoRefreshedNoAppsIsTriviallySatisfied(t *testing.T) {
	s := argoState()
	s.ArgoApps = nil
	a := &argo.Fake{}
	obs, err := (ArgoRefreshedStep{Argo: a}).Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Errorf("Observe = %+v, %v; want satisfied when this promotion touched no Application", obs, err)
	}
	if len(a.Calls) != 0 {
		t.Errorf("Argo must never be called when ArgoApps is empty: %v", a.Calls)
	}
}

// TestArgoRefreshedActsThenConvergesWithoutReAnnotating is invariant 2's own "done when":
// Observe finds a stale reconciledAt (before this promotion's own merge), Act annotates, and
// once the Fake's status is updated to reflect Argo having genuinely reconciled afterward,
// Observe reports satisfied without Act ever running again.
func TestArgoRefreshedActsThenConvergesWithoutReAnnotating(t *testing.T) {
	s := argoState() // merged one minute ago
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	a.SetStatus(app, argo.Status{ReconciledAt: time.Now().Add(-time.Hour)}) // stale: before the merge
	step := ArgoRefreshedStep{Argo: a}

	obs, err := step.Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("Observe = %+v, want not satisfied (reconciledAt predates the merge)", obs)
	}
	if err := step.Act(ctx(), s); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if calls := strings.Join(a.Calls, ","); calls != "Get "+app.String()+",Refresh "+app.String() {
		t.Fatalf("Calls = %s, want exactly one Get then one Refresh", calls)
	}

	// Argo actually reconciles after the merge (simulated: the test sets the Fake's status,
	// standing in for the real controller's own reconcile — Refresh itself never mutates
	// status, see argo.Fake's own doc comment).
	a.SetStatus(app, argo.Status{ReconciledAt: time.Now().Add(time.Hour)})
	obs, err = step.Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Fatalf("Observe after reconcile = %+v, %v; want satisfied", obs, err)
	}
	// Drive only ever calls Act when Observe reports not-satisfied (engine.go's own contract);
	// the assertion that matters is that this second, now-satisfied Observe never needed
	// another Act to get there — one Get-then-Refresh pair from the first (unsatisfied)
	// Observe/Act cycle, then Satisfied forever after, with no second Refresh anywhere.
	refreshes := strings.Count(strings.Join(a.Calls, ","), "Refresh")
	if refreshes != 1 {
		t.Fatalf("Refresh called %d times, want exactly 1 across the whole convergence: %v", refreshes, a.Calls)
	}
}

func TestArgoRefreshedMissingApplicationBlocks(t *testing.T) {
	s := argoState()
	a := &argo.Fake{} // no status configured: Get reports ErrNotFound
	obs, err := (ArgoRefreshedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" || !strings.Contains(obs.Blocked, testApp) {
		t.Errorf("Observe = %+v, want Blocked naming %s", obs, testApp)
	}
}

func TestArgoRefreshedTransientErrorIsReturnedNotSwallowed(t *testing.T) {
	s := argoState()
	a := &argo.Fake{GetErr: errors.New("transient: connection reset")}
	_, err := (ArgoRefreshedStep{Argo: a}).Observe(ctx(), s)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("Observe err = %v, want the transient error surfaced (never swallowed into Blocked/false)", err)
	}
}

// --- ArgoSyncedStep -----------------------------------------------------------------------

func TestArgoSyncedHappyPath(t *testing.T) {
	s := argoState()
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.MergeSHA, HealthStatus: argo.HealthStatusHealthy})
	obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want satisfied", obs, err)
	}
}

// TestArgoSyncedDegradedBlocksImmediately is invariant 3, verbatim: Degraded health Blocks on
// the very first Observe — no Waiting phase precedes it, so nothing in the CLI's poll loop
// ever gets a chance to wait out a deadline for it.
func TestArgoSyncedDegradedBlocksImmediately(t *testing.T) {
	s := argoState()
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	// Revision even matches — Degraded must still Block, regardless of revision (this step's
	// own doc comment: "a promotion has no business declaring itself ... while the app it
	// just changed is unhealthy").
	a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.MergeSHA, HealthStatus: argo.HealthStatusDegraded})
	obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Waiting {
		t.Fatalf("Observe = %+v, want Blocked (Degraded), never Waiting", obs)
	}
	if obs.Blocked == "" || !strings.Contains(obs.Blocked, "Degraded") {
		t.Errorf("Observe = %+v, want Blocked naming Degraded", obs)
	}
}

func TestArgoSyncedOperationFailedBlocksImmediately(t *testing.T) {
	for _, phase := range []string{argo.OperationFailed, argo.OperationError} {
		s := argoState()
		app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
		a := &argo.Fake{}
		a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.MergeSHA, HealthStatus: argo.HealthStatusHealthy, OperationPhase: phase})
		obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Blocked == "" || !strings.Contains(obs.Blocked, phase) {
			t.Errorf("phase %s: Observe = %+v, want Blocked naming the phase", phase, obs)
		}
	}
}

// TestArgoSyncedWrongRevisionDoesNotSatisfy is invariant 3's revision-mismatch clause: right
// sync status and health, wrong revision (self-heal already synced something else) must Wait,
// never Satisfy.
func TestArgoSyncedWrongRevisionDoesNotSatisfy(t *testing.T) {
	s := argoState()
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: "some-other-commit", HealthStatus: argo.HealthStatusHealthy})
	obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("Observe = %+v, want NOT satisfied: right sync/health, wrong revision", obs)
	}
	if !obs.Waiting {
		t.Errorf("Observe = %+v, want Waiting (retryable), not Blocked", obs)
	}
}

func TestArgoSyncedRightRevisionWrongSyncStatusDoesNotSatisfy(t *testing.T) {
	s := argoState()
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	a.SetStatus(app, argo.Status{SyncStatus: "OutOfSync", SyncRevision: s.MergeSHA, HealthStatus: argo.HealthStatusHealthy})
	obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
	if err != nil || obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want NOT satisfied: right revision but not Synced", obs, err)
	}
}

func TestArgoSyncedMissingApplicationBlocks(t *testing.T) {
	s := argoState()
	a := &argo.Fake{}
	obs, err := (ArgoSyncedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" || !strings.Contains(obs.Blocked, testApp) {
		t.Errorf("Observe = %+v, want Blocked naming %s", obs, testApp)
	}
}

// --- RolledOutStep ------------------------------------------------------------------------

func deploymentEdit(container, newRef string) gitops.Edit {
	const file = "cluster/apps/app-production/app/deployment.yaml"
	const name = "app"
	ref, err := image.Parse(newRef)
	if err != nil {
		panic(err)
	}
	return gitops.Edit{
		Occurrence: gitops.Occurrence{File: file, Kind: "Deployment", Name: name, Container: container, Path: "spec.template.spec.containers[0].image"},
		New:        ref,
	}
}

func TestRolledOutHappyPath(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{}
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		Complete: true,
	})
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want satisfied", obs, err)
	}
}

func TestRolledOutImageMismatchWaits(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{}
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v1@sha256:" + strings.Repeat("0", 64)}}, // still the old image
		Complete: true,
	})
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied || !obs.Waiting {
		t.Fatalf("Observe = %+v, want Waiting: the live image hasn't updated yet", obs)
	}
}

func TestRolledOutDeadlineExceededBlocks(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{}
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:           []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		DeadlineExceeded: true,
		Detail:           `deployment "app" exceeded its progress deadline`,
	})
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" || !strings.Contains(obs.Blocked, "progress deadline") {
		t.Fatalf("Observe = %+v, want Blocked naming the exceeded deadline", obs)
	}
}

// TestRolledOutNeverGatesOnJobsOrCronJobs is invariant 4's report-only clause: a Job/CronJob
// this promotion touched is listed, but a broken one must never block a Deployment that has
// itself finished rolling out.
func TestRolledOutNeverGatesOnJobsOrCronJobs(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{
		deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64)),
		{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/app/migrate-job.yaml", Kind: "Job", Name: "migrate"}},
	}
	ro := &rollout.Fake{}
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
		Complete: true,
	})
	ro.SetJobLike("app-production", "migrate", "Job", rollout.JobLikeStatus{Detail: "active=0 succeeded=0 failed=3"}) // a failing Job
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want satisfied — a failing Job must never gate", obs, err)
	}
	if !strings.Contains(obs.Detail, "migrate") {
		t.Errorf("Detail = %q, want the Job still reported", obs.Detail)
	}
}

func TestRolledOutMissingContainerIsAMismatchNotAPanic(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("sidecar", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{}
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}}, // no "sidecar" container at all
		Complete: true,
	})
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil || obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want NOT satisfied when a wanted container is missing", obs, err)
	}
}

// --- Full pipeline: kill mid-ArgoSynced, Argo reconciles, resume converges -----------------

// driveNewStateThroughMerged builds a fresh PromotionState from fx (mirroring newState) with
// auto-approval and ci.none=green so it converges through Merged without any human input, and
// wires it with a real git + the given fake forge/argo/rollout. Reused for both the initial
// attempt and every "resume" in TestArgoAndRolloutFullPipelineConverges — a fresh state each
// time is what a restarted `hoist promote`/`hoist resume` process actually builds (AGENTS.md
// invariant 4), exactly the pattern steps_m4_test.go's own convergence tests use.
func driveNewStateThroughMerged(fx fixture, wt string, f forge.Forge, a argo.Argo, ro rollout.Rollout) (*PromotionState, error) {
	s := newState(fx, wt)
	s.CINone, s.CIGrace, s.Approval = "green", time.Nanosecond, "auto"
	s.ArgoNamespace = testArgoNamespace
	s.ArgoApps = []string{testApp}
	all := AllSteps(git.Exec{}, f, a, ro, nil)
	err := Drive(ctx(), all, s, nil)
	return s, err
}

// TestArgoAndRolloutFullPipelineConverges is the M5 brief's own "what done means" checklist,
// end to end: happy path to Done, and kill-and-resume mid-ArgoSynced re-converging without
// ArgoRefreshedStep re-issuing a refresh that already succeeded (invariant 2's idempotency
// clause, exercised here through the real Drive/AllSteps wiring rather than the isolated unit
// test above).
func TestArgoAndRolloutFullPipelineConverges(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	f := &forge.Fake{}
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	// A baseline status predating this promotion entirely — Argo existed and was healthy
	// before, it just hasn't reconciled this promotion's own merge yet.
	a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: "some-earlier-commit", HealthStatus: argo.HealthStatusHealthy, ReconciledAt: time.Now().Add(-time.Hour)})
	ro := &rollout.Fake{} // deliberately unconfigured: RolledOutStep must never be reached yet

	s, err := driveNewStateThroughMerged(fx, wt, f, a, ro)
	if !errors.Is(err, ErrWaiting) {
		t.Fatalf("first attempt: expected ErrWaiting (Argo hasn't synced this promotion's revision yet), got %v", err)
	}
	if s.Phase != StepArgoSynced {
		t.Fatalf("expected to stop at %s, stopped at %s", StepArgoSynced, s.Phase)
	}
	if s.MergeSHA == "" {
		t.Fatal("expected MergedStep to have completed before Argo steps run")
	}
	// forge.Fake's merge never touches real git, but the *second* Drive below builds a fresh
	// PromotionState (driveNewStateThroughMerged's own doc comment) that reaches MergedStep's
	// Observe for the first time in this process — and since s and s2 share the same
	// deterministic id/branch/marker, it finds the same already-merged PR pr.Merged==true and
	// revalidates it against s.Base's live tip (M4 hardening, finding #1) before trusting it.
	// Simulate what a real GitHub squash-merge would have done to the base branch so that
	// revalidation finds what it expects, exactly like steps_m4_test.go's own convergence tests.
	mergeToBase(t, s)
	if calls := strings.Join(a.Calls, ","); !strings.Contains(calls, "Refresh "+app.String()) {
		t.Fatalf("expected ArgoRefreshedStep's Act to have issued a Refresh: %v", a.Calls)
	}
	refreshesSoFar := strings.Count(strings.Join(a.Calls, ","), "Refresh")

	// "Kill" — Argo genuinely reconciles the merge in the interim (simulated via SetStatus,
	// standing in for the real controller), then a fresh process re-drives from scratch.
	a.SetStatus(app, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.MergeSHA, HealthStatus: argo.HealthStatusHealthy, ReconciledAt: time.Now().Add(time.Hour)})
	ro.SetDeployment("app-production", "app", rollout.DeploymentStatus{
		Images:   []rollout.ContainerImage{{Name: "app", Image: fx.plan.Edits[0].New.String()}},
		Complete: true,
	})

	s2, err := driveNewStateThroughMerged(fx, wt, f, a, ro)
	if err != nil {
		t.Fatalf("resumed attempt should complete fully: %v", err)
	}
	if s2.MergeSHA != s.MergeSHA {
		t.Fatalf("resumed attempt produced a different merge: first %s, second %s", s.MergeSHA, s2.MergeSHA)
	}
	if got := strings.Count(strings.Join(a.Calls, ","), "Refresh"); got != refreshesSoFar {
		t.Fatalf("Refresh called %d more time(s) on the resumed attempt, want 0 more (already reconciled): total calls %v", got-refreshesSoFar, a.Calls)
	}
	if len(f.PRs()) != 1 || !f.PRs()[0].Merged {
		t.Fatalf("expected exactly one, merged PR across both attempts: %+v", f.PRs())
	}
}
