package engine

import (
	"context"
	"errors"
	"fmt"
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

// TestArgoRefreshedBlocksWhenMergeHasNoHistoryAnchor is Copilot's PR #51 review finding:
// Observe used to discard mergedAt's own ok return, trusting the zero time as an anchor
// whenever s.MergeSHA is set but s.History carries no matching StepMerged entry (a legacy or
// otherwise inconsistent state file — mergedAt's own doc comment assumes this "should never
// happen" given MergeSHA's guard just above, but blindly trusting that is exactly the
// zero-means-cannot-determine trap: almost any real ReconciledAt is "after" the zero time, so
// every Application would report already-reconciled with zero actual evidence a refresh ever
// landed after the merge). This proves the fix Blocks clearly instead, naming the
// inconsistency, rather than silently reporting Satisfied on no evidence at all.
func TestArgoRefreshedBlocksWhenMergeHasNoHistoryAnchor(t *testing.T) {
	s := argoState()
	s.History = nil // MergeSHA is set (argoState's own setup) but no StepMerged entry recorded
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp}, argo.Status{
		ReconciledAt: time.Now(), // any real timestamp is "after" the zero-value anchor
	})
	obs, err := (ArgoRefreshedStep{Argo: a}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Satisfied {
		t.Fatalf("Observe = %+v, want Blocked, not Satisfied on a zero-value anchor with no real evidence", obs)
	}
	if obs.Blocked == "" {
		t.Fatalf("Observe = %+v, want a clear Blocked signal naming the missing history anchor", obs)
	}
	if len(a.Calls) != 0 {
		t.Errorf("Argo should not even be consulted once the anchor itself is known to be untrustworthy: %v", a.Calls)
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

// TestRolledOutMissingDeploymentBlocks is round-1's regression: a Deployment this promotion
// edited but which no longer exists on the cluster (rollout.ErrNotFound) used to be returned as
// a generic StepError indistinguishable from a plumbing failure — this must instead Block,
// naming the Deployment, mirroring ArgoRefreshedStep/ArgoSyncedStep's own errorsIsNotFound
// handling of a missing Application just above in this file.
func TestRolledOutMissingDeploymentBlocks(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{DeploymentErr: fmt.Errorf("reading Deployment app-production/app: %w", rollout.ErrNotFound)}
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Blocked == "" || !strings.Contains(obs.Blocked, "app") {
		t.Errorf("Observe = %+v, want Blocked naming the missing Deployment", obs)
	}
}

// TestRolledOutDeploymentTransientErrorIsReturnedNotSwallowed mirrors
// TestArgoRefreshedTransientErrorIsReturnedNotSwallowed: a non-ErrNotFound error reading a
// Deployment (a connection reset, say) must still surface as a plain error for the CLI's poll
// loop to retry — never swallowed into Blocked or a false Observation.
func TestRolledOutDeploymentTransientErrorIsReturnedNotSwallowed(t *testing.T) {
	s := argoState()
	s.Edits = []gitops.Edit{deploymentEdit("app", "ghcr.io/example/app:v2@sha256:"+strings.Repeat("1", 64))}
	ro := &rollout.Fake{DeploymentErr: errors.New("transient: connection reset")}
	_, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("Observe err = %v, want the transient error surfaced (never swallowed into Blocked/false)", err)
	}
}

// TestRolledOutJobLikeMissingIsReportedNotBlocking is round-1's regression: a Job/CronJob this
// promotion touched but which is already gone (a short ttlSecondsAfterFinished, or an Argo hook's
// deletion policy — rollout.ErrNotFound) used to hard-error the whole Observe, contradicting this
// step's own doc comment that Jobs/CronJobs are "listed, never gated on" (invariant 4). It must
// instead become a report line and let a Deployment that itself finished rolling out still
// satisfy the step.
func TestRolledOutJobLikeMissingIsReportedNotBlocking(t *testing.T) {
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
	ro.JobLikeErr = fmt.Errorf("reading Job app-production/migrate: %w", rollout.ErrNotFound)
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil || !obs.Satisfied {
		t.Fatalf("Observe = %+v, %v; want satisfied — a missing Job must never gate", obs, err)
	}
	if !strings.Contains(obs.Detail, "migrate") {
		t.Errorf("Detail = %q, want the Job still reported (even though it could not be checked)", obs.Detail)
	}
}

// TestRolledOutJobLikeTransientErrorIsReportedNotBlocking is the sibling case: a transient error
// reading a Job/CronJob (not ErrNotFound) must ALSO become a report line, never a hard error that
// gates the whole promotion — the report-only contract (invariant 4) draws no distinction between
// "gone" and "couldn't check right now" for a Job/CronJob, unlike the Deployment case above.
func TestRolledOutJobLikeTransientErrorIsReportedNotBlocking(t *testing.T) {
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
	ro.JobLikeErr = errors.New("transient: connection reset")
	obs, err := (RolledOutStep{Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe err = %v, want nil (a transient JobLike error must become a report line, never a hard error)", err)
	}
	if !obs.Satisfied {
		t.Fatalf("Observe = %+v, want satisfied — a Job hoist could not even check must never gate", obs)
	}
	if !strings.Contains(obs.Detail, "connection reset") {
		t.Errorf("Detail = %q, want the transient error reported so the operator can see it", obs.Detail)
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

// countingGit wraps a real git.Exec, counting Push and DeleteRemoteBranch calls — used by
// TestDriveDoesNotChurnPushDeleteWhileWaitingOnArgoAfterMerge below to prove, against a real git
// remote, that Drive's Merged short-circuit (engine.go) actually stops the real re-push/re-delete
// cycle TestResumeRebuildsArgoAppsForALegacyStateFile (cmd/hoist/resume_test.go) first documented
// and worked around with a widened poll.argo, rather than just the stubbed shape of it that
// TestDriveSkipsPushedAndMergedOncePastMergedOnAPriorPass (engine_test.go) already covers.
type countingGit struct {
	git.Exec
	pushes, deletes *int
}

func (g countingGit) Push(ctx context.Context, worktreeDir, remote, branch string) error {
	*g.pushes++
	return g.Exec.Push(ctx, worktreeDir, remote, branch)
}

func (g countingGit) DeleteRemoteBranch(ctx context.Context, cloneDir, remote, branch string) error {
	*g.deletes++
	return g.Exec.DeleteRemoteBranch(ctx, cloneDir, remote, branch)
}

// TestDriveDoesNotChurnPushDeleteWhileWaitingOnArgoAfterMerge drives one real promotion (real
// git, real bare origin remote — the same "world is the state" wiring AllSteps uses in
// production) through to a genuine "merged, waiting on Argo to sync" stop, with Argo
// deliberately left OutOfSync so it never converges (mirroring
// TestResumeRebuildsArgoAppsForALegacyStateFile's own setup). It then calls Drive on the very
// same PromotionState several more times — standing in for driveToCompletion's own poll loop
// (cmd/hoist/drive.go), which calls Drive repeatedly every poll.argo/poll.rollout tick while
// waiting — and asserts the branch is pushed and deleted exactly once each, not once per poll.
func TestDriveDoesNotChurnPushDeleteWhileWaitingOnArgoAfterMerge(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	f := &forge.Fake{}
	app := argo.Application{Namespace: testArgoNamespace, Name: testApp}
	a := &argo.Fake{}
	// OutOfSync, and already "refreshed" (ReconciledAt ahead of the merge anchor Drive is about
	// to record) so ArgoRefreshedStep is satisfied without issuing its own extra Refresh call,
	// leaving ArgoSyncedStep as the one, deliberately permanent, waiting point this test needs.
	a.SetStatus(app, argo.Status{
		SyncStatus:   "OutOfSync",
		SyncRevision: "some-earlier-commit",
		HealthStatus: argo.HealthStatusHealthy,
		ReconciledAt: time.Now().Add(time.Hour),
	})
	ro := &rollout.Fake{} // unconfigured: RolledOutStep must never be reached

	s := newState(fx, wt)
	s.CINone, s.CIGrace, s.Approval = "green", time.Nanosecond, "auto"
	s.ArgoNamespace = testArgoNamespace
	s.ArgoApps = []string{testApp}

	var pushes, deletes int
	g := countingGit{Exec: git.Exec{}, pushes: &pushes, deletes: &deletes}
	all := AllSteps(g, f, a, ro, nil)

	if err := Drive(ctx(), all, s, nil); !errors.Is(err, ErrWaiting) {
		t.Fatalf("first Drive call: expected ErrWaiting (Argo left OutOfSync on purpose), got %v", err)
	}
	if s.Phase != StepArgoSynced {
		t.Fatalf("expected to stop at %s, stopped at %s", StepArgoSynced, s.Phase)
	}
	if s.MergeSHA == "" {
		t.Fatal("expected the promotion to have actually merged before this test's own Argo wait")
	}
	if pushes != 1 {
		t.Fatalf("expected exactly one push (the original promotion push), got %d", pushes)
	}
	if deletes != 1 {
		t.Fatalf("expected exactly one branch delete (MergedStep's cleanup), got %d", deletes)
	}
	// What a real GitHub squash-merge would have done to the base branch — forge.Fake's own
	// MergePR never touches real git (mergeToBase's own doc comment) — needed so every
	// subsequent poll's fresh Observe on MergedStep still finds its merge commit a genuine,
	// verifiable ancestor of the base's current tip, exactly as production would.
	mergeToBase(t, s)

	for i := 0; i < 4; i++ {
		if err := Drive(ctx(), all, s, nil); !errors.Is(err, ErrWaiting) {
			t.Fatalf("poll %d: expected ErrWaiting (Argo is still OutOfSync), got %v", i, err)
		}
	}

	if pushes != 1 {
		t.Fatalf("branch was re-pushed on a later poll while merged and waiting on Argo: %d total push(es), want 1", pushes)
	}
	if deletes != 1 {
		t.Fatalf("branch was re-deleted on a later poll while merged and waiting on Argo: %d total delete(s), want 1", deletes)
	}

	if remoteSHA, ok, err := (git.Exec{}).LsRemoteBranch(ctx(), fx.cloneDir, "origin", s.Branch); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("branch %s should still be deleted on origin, found at %s", s.Branch, remoteSHA)
	}
}

// gitBackedArgoState is one real, merged promotion in a real git fixture: the plan committed
// and pushed onto origin/main, MergeSHA set to that commit, and the Argo fields argoState()
// fakes. Everything the revision checks in #165 need — a clone, an object graph, and the
// planned content actually present at a known revision — is real here; only Argo is a fake.
func gitBackedArgoState(t *testing.T) (*PromotionState, fixture) {
	t.Helper()
	fx := newFixture(t)
	s := newState(fx, filepath.Join(t.TempDir(), "wt"))
	g := git.Exec{}
	if _, err := (BranchedStep{Git: g}).Observe(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if err := (BranchedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	if err := (CommittedStep{Git: g}).Act(ctx(), s); err != nil {
		t.Fatal(err)
	}
	mergeToBase(t, s)
	s.MergeSHA = s.CommitSHA
	s.ArgoNamespace = testArgoNamespace
	s.ArgoApps = []string{testApp}
	s.History = []HistoryEntry{{Step: StepMerged, At: time.Now().Add(-time.Minute)}}
	return s, fx
}

func syncedAt(t *testing.T, rev string) *argo.Fake {
	t.Helper()
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: rev, HealthStatus: argo.HealthStatusHealthy})
	return a
}

// TestArgoSyncedAcceptsARevisionThatCarriesTheMerge is #165's regression. Argo tracks the
// BRANCH and reports whatever tip it last synced, so exact equality with the merge SHA holds
// only until the next commit lands on the base — after which the reported revision is a
// descendant and the comparison fails permanently: Waiting forever, RolledOutStep never runs,
// and the promotion never goes terminal. Three merged, long-since-rolled-out promotions sat in
// the TUI's in-flight pane for days exactly this way.
func TestArgoSyncedAcceptsARevisionThatCarriesTheMerge(t *testing.T) {
	s, fx := gitBackedArgoState(t)
	g := git.Exec{}

	// Someone else's later, unrelated commit advances main past this promotion's merge.
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "commit", "-q", "--allow-empty", "-m", "a later, unrelated change")
	runHost(t, other, "push", "-q", "origin", "main")
	tip, ok, err := g.LsRemoteBranch(ctx(), other, "origin", "main")
	if err != nil || !ok {
		t.Fatalf("reading origin/main: %v (ok=%v)", err, ok)
	}
	if tip == s.LandedSHA() {
		t.Fatal("fixture precondition: main should have moved past the merge")
	}

	obs, err := (ArgoSyncedStep{Argo: syncedAt(t, tip), Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.Satisfied {
		t.Fatalf("Argo is synced and healthy at %s, which contains this promotion's merge %s and still declares the promoted digest; must satisfy: %+v", tip, s.LandedSHA(), obs)
	}
}

// TestArgoSyncedRejectsARevisionThatRevertedTheMerge is the control that keeps the ancestry
// check honest — the whole reason it is not ancestry ALONE. A revert commit never removes the
// reverted commit from history, so "the merge is an ancestor" stays true forever after the
// promoted digest has been undone. Satisfying on ancestry there would declare a promotion
// deployed while the cluster runs the old image.
func TestArgoSyncedRejectsARevisionThatRevertedTheMerge(t *testing.T) {
	s, fx := gitBackedArgoState(t)
	g := git.Exec{}

	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "revert", "--no-edit", s.MergeSHA)
	runHost(t, other, "push", "-q", "origin", "main")
	tip, ok, err := g.LsRemoteBranch(ctx(), other, "origin", "main")
	if err != nil || !ok {
		t.Fatalf("reading origin/main: %v (ok=%v)", err, ok)
	}
	isAncestor, err := g.IsAncestor(ctx(), other, s.MergeSHA, tip)
	if err != nil || !isAncestor {
		t.Fatalf("fixture precondition: the reverted merge must still be an ancestor of the tip (%v, %v)", isAncestor, err)
	}

	obs, err := (ArgoSyncedStep{Argo: syncedAt(t, tip), Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Satisfied {
		t.Fatalf("the promoted digest was reverted at %s; ancestry alone must not satisfy: %+v", tip, obs)
	}
	if !obs.Waiting {
		t.Errorf("Observe = %+v, want Waiting (retryable), not Blocked", obs)
	}
}

// TestArgoSyncedAcceptsARevisionThatSupersededTheMerge: a later deploy into the same env
// replaced the promoted reference. This promotion landed and has been legitimately replaced —
// the same judgement DirectPushedStep makes for the same situation (#166) — so it must go
// terminal rather than wait for a revision that will never be reported again.
func TestArgoSyncedAcceptsARevisionThatSupersededTheMerge(t *testing.T) {
	s, fx := gitBackedArgoState(t)
	g := git.Exec{}
	tip := supersedeBase(t, fx, "ghcr.io/example/app:v3@sha256:"+strings.Repeat("2", 64))

	obs, err := (ArgoSyncedStep{Argo: syncedAt(t, tip), Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.Satisfied {
		t.Fatalf("a promotion superseded by a later deploy has landed and is finished: %+v", obs)
	}
}

// TestArgoSyncedRejectsAnUnrelatedRevisionWithAGit: with a real clone wired in, a revision that
// does not contain the merge at all is still not synced — the ancestry check must not degrade
// into "any revision will do".
func TestArgoSyncedRejectsAnUnrelatedRevisionWithAGit(t *testing.T) {
	s, _ := gitBackedArgoState(t)
	// A well-formed sha this clone has never seen.
	obs, err := (ArgoSyncedStep{Argo: syncedAt(t, strings.Repeat("a", 40)), Git: git.Exec{}}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Satisfied {
		t.Fatalf("a revision that does not contain the merge must not satisfy: %+v", obs)
	}
}

// TestArgoSyncedCarriedRevisionStillNeedsHealth: invariant 3 in the new shape — a revision that
// carries the merge satisfies only alongside Synced/Healthy, never on its own.
func TestArgoSyncedCarriedRevisionStillNeedsHealth(t *testing.T) {
	s, fx := gitBackedArgoState(t)
	g := git.Exec{}
	other := filepath.Join(t.TempDir(), "other-clone")
	runHost(t, "", "clone", "-q", fx.originDir, other)
	runHost(t, other, "commit", "-q", "--allow-empty", "-m", "a later, unrelated change")
	runHost(t, other, "push", "-q", "origin", "main")
	tip, _, err := g.LsRemoteBranch(ctx(), other, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing"})
	obs, err := (ArgoSyncedStep{Argo: a, Git: g}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Satisfied {
		t.Fatalf("revision carries the merge but health is Progressing; must not satisfy: %+v", obs)
	}
}

// TestArgoSyncedNamesTheStalledDeploymentWhenHealthIsProgressing is #PR4's own regression test:
// Argo can report sync=Synced health=Progressing indefinitely with no cause of its own to name
// (never Degraded, never a Failed/Error operation phase) — exactly what a real stuck promotion
// (y2ef7pknhu, spritz-production) reported. Wired with a Rollout, ArgoSyncedStep borrows
// RolledOutStep's own per-Deployment read one gate earlier and names it, instead of leaving the
// operator with the bare sync/health tuple they can already read straight off Argo.
func TestArgoSyncedNamesTheStalledDeploymentWhenHealthIsProgressing(t *testing.T) {
	s, _ := gitBackedArgoState(t)
	g := git.Exec{}
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.LandedSHA(), HealthStatus: "Progressing"})

	deployments, _ := groupEditsByWorkload(s.Edits)
	ro := &rollout.Fake{}
	for name, wants := range deployments {
		imgs := make([]rollout.ContainerImage, len(wants))
		for i, w := range wants {
			imgs[i] = rollout.ContainerImage{Name: w.Container, Init: w.Init, Image: w.New}
		}
		ro.SetDeployment(s.TargetEnv, name, rollout.DeploymentStatus{
			Namespace: s.TargetEnv, Name: name, Images: imgs,
			Complete: false, Detail: "1 of 2 updated replicas are available",
		})
	}

	obs, err := (ArgoSyncedStep{Argo: a, Git: g, Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Satisfied || !obs.Waiting {
		t.Fatalf("Observe = %+v, want Waiting (not yet satisfied, retryable)", obs)
	}
	if !strings.Contains(obs.Detail, "1 of 2 updated replicas") {
		t.Errorf("Detail = %q, want it to name what the rollout is actually doing, not just the bare sync=Synced health=Progressing tuple", obs.Detail)
	}
}

// TestArgoSyncedBlocksOnADeadlineExceededRollout is #PR4's own DeadlineExceeded regression
// test: without a Rollout wired, ArgoSyncedStep would Wait out its own poll interval forever on
// a rollout that RolledOutStep, one step later, would immediately Block on — this saves the
// operator that extra cycle by surfacing the identical verdict here.
func TestArgoSyncedBlocksOnADeadlineExceededRollout(t *testing.T) {
	s, _ := gitBackedArgoState(t)
	g := git.Exec{}
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: s.LandedSHA(), HealthStatus: "Progressing"})

	deployments, _ := groupEditsByWorkload(s.Edits)
	ro := &rollout.Fake{}
	for name := range deployments {
		ro.SetDeployment(s.TargetEnv, name, rollout.DeploymentStatus{
			Namespace: s.TargetEnv, Name: name,
			DeadlineExceeded: true, Detail: "ProgressDeadlineExceeded: ReplicaSet has timed out progressing",
		})
	}

	obs, err := (ArgoSyncedStep{Argo: a, Git: g, Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Blocked == "" {
		t.Fatalf("Observe = %+v, want Blocked — a deadline-exceeded rollout will not resolve by waiting", obs)
	}
	if !strings.Contains(obs.Blocked, "ProgressDeadlineExceeded") {
		t.Errorf("Blocked = %q, want it to name kubectl's own deadline-exceeded detail", obs.Blocked)
	}
}

// TestArgoSyncedDoesNotGateOnRolloutOnceThisPromotionWasSuperseded is the round-2 review
// regression test: an earlier version called rolloutCause even when this Application's synced
// revision had already been SUPERSEDED (a later, legitimate deploy replaced this promotion's
// own change — landedSuperseded, satisfied per AGENTS.md §4.1). At that point the live
// containers legitimately run someone ELSE's later change, not this promotion's own Edit.New —
// comparing them produced a confusing "image not yet live" message at best, and could wrongly
// Block an already-finished promotion on an unrelated later deploy's own failed rollout at
// worst, which this test drives directly: the rollout fake reports the superseded Deployment as
// DeadlineExceeded, and Observe must still report Satisfied.
func TestArgoSyncedDoesNotGateOnRolloutOnceThisPromotionWasSuperseded(t *testing.T) {
	s, fx := gitBackedArgoState(t)
	g := git.Exec{}
	tip := supersedeBase(t, fx, "ghcr.io/example/app:v3@sha256:"+strings.Repeat("2", 64))
	// Deliberately NOT syncedAt (which reports Healthy): the superseded revision has only
	// just synced and is still converging, exactly the case that reaches the
	// sync/health-mismatch branch where rolloutCause used to be called unconditionally.
	a := &argo.Fake{}
	a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
		argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing"})

	deployments, _ := groupEditsByWorkload(s.Edits)
	ro := &rollout.Fake{}
	for name := range deployments {
		ro.SetDeployment(s.TargetEnv, name, rollout.DeploymentStatus{
			Namespace: s.TargetEnv, Name: name,
			DeadlineExceeded: true, Detail: "an unrelated later deploy's own rollout failed",
		})
	}

	obs, err := (ArgoSyncedStep{Argo: a, Git: g, Rollout: ro}).Observe(ctx(), s)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.Satisfied {
		t.Fatalf("a promotion superseded by a later deploy has landed and is finished; an unrelated deploy's own failed rollout must not gate it: %+v", obs)
	}
}

// twoDeploymentState is a minimal, git-free PromotionState (rolloutCause reads only
// s.Edits/s.TargetEnv, never git or forge) with two distinct Deployments — "app" and "web",
// sorted-iteration order — for the multi-Deployment tests below, which an adversarial review
// found nothing in this package exercised: every existing fixture has exactly one Deployment,
// so a regression in the sorted-iteration or first-match-wins logic would pass unnoticed.
func twoDeploymentState() *PromotionState {
	return &PromotionState{
		TargetEnv: "app-production",
		Edits: []gitops.Edit{
			{Occurrence: gitops.Occurrence{Kind: "Deployment", Name: "app", Container: "app"}, New: image.Ref{Repo: "ghcr.io/example/app", Tag: "v3"}},
			{Occurrence: gitops.Occurrence{Kind: "Deployment", Name: "web", Container: "web"}, New: image.Ref{Repo: "ghcr.io/example/web", Tag: "v3"}},
		},
	}
}

// TestRolloutCauseFindsADeadlineExceededDeploymentThatIsNotFirst proves the sorted-iteration
// path actually reaches every Deployment, not just whichever happened to iterate first out of
// the underlying map: "app" (first alphabetically, and reported as rolled out) is fine; "web"
// (second) is the one that has exceeded its deadline, and must still be found and named.
func TestRolloutCauseFindsADeadlineExceededDeploymentThatIsNotFirst(t *testing.T) {
	s := twoDeploymentState()
	ro := &rollout.Fake{}
	ro.SetDeployment(s.TargetEnv, "app", rollout.DeploymentStatus{
		Images: []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v3"}}, Complete: true,
	})
	ro.SetDeployment(s.TargetEnv, "web", rollout.DeploymentStatus{
		DeadlineExceeded: true, Detail: "ProgressDeadlineExceeded: web's own ReplicaSet timed out",
	})

	detail, blocked := (ArgoSyncedStep{Rollout: ro}).rolloutCause(ctx(), s)
	if !blocked {
		t.Fatalf("rolloutCause = (%q, %v), want blocked=true — web has exceeded its deadline", detail, blocked)
	}
	if !strings.Contains(detail, "web") || !strings.Contains(detail, "timed out") {
		t.Errorf("detail = %q, want it to name web specifically, not app (which is fine)", detail)
	}
}

// errOnDeployment wraps a *rollout.Fake and returns a transient (non-ErrNotFound) error for one
// named Deployment, delegating everything else — the shape needed to prove rolloutCause does
// not build a confident verdict from an incomplete read (see the test below).
type errOnDeployment struct {
	*rollout.Fake
	name string
	err  error
}

func (e errOnDeployment) Deployment(ctx context.Context, namespace, name string) (rollout.DeploymentStatus, error) {
	if name == e.name {
		return rollout.DeploymentStatus{}, e.err
	}
	return e.Fake.Deployment(ctx, namespace, name)
}

// TestRolloutCauseDegradesRatherThanReportAPartialVerdict is the P3 review finding's own
// regression test: "app" (sorted first) errors transiently while "web" (sorted second) has
// genuinely exceeded its deadline. rolloutCause must not swallow app's error and confidently
// report/Block on web alone — a verdict built on an incomplete read is worse than the plain
// sync/health tuple the caller falls back to when rolloutCause returns nothing.
func TestRolloutCauseDegradesRatherThanReportAPartialVerdict(t *testing.T) {
	s := twoDeploymentState()
	inner := &rollout.Fake{}
	inner.SetDeployment(s.TargetEnv, "web", rollout.DeploymentStatus{
		DeadlineExceeded: true, Detail: "ProgressDeadlineExceeded: web's own ReplicaSet timed out",
	})
	ro := errOnDeployment{Fake: inner, name: "app", err: errors.New("dial tcp: connection reset by peer")}

	detail, blocked := (ArgoSyncedStep{Rollout: ro}).rolloutCause(ctx(), s)
	if blocked || detail != "" {
		t.Fatalf("rolloutCause = (%q, %v), want (\"\", false) — app's transient error must not be silently skipped in favor of a confident verdict about web", detail, blocked)
	}
}
