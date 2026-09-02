package engine

// M5 adds three steps after Merged (AGENTS.md §1: "... Argo refresh, Deployment watch"):
// ArgoRefreshed, ArgoSynced, RolledOut. Argo CD is driven entirely through the Kubernetes API
// (AGENTS.md §4.7, pkg/argo's own package doc) — no Argo API server, no Argo token, no
// argo-cd/v3 import.
//
// ArgoRefreshedStep's Observe re-derivation strategy (a judgment call the M5 brief left open,
// naming two candidates): the M5 brief offers two ways to detect "a refresh already happened
// for this promotion" — (a) status.reconciledAt newer than the promotion's own merge, or (b)
// the refresh annotation still present with a value/timestamp this promotion itself would have
// set. This picks (a), for a reason specific to real Argo CD's own behavior: the controller
// clears argocd.argoproj.io/refresh once it has processed a request, so "annotation present"
// means "requested but not yet processed" and "annotation absent" is ambiguous between "never
// requested" and "already processed" — exactly the zero-means-cannot-determine trap this
// repo's own steering has hit before. A timestamp ordering has no such ambiguity. The anchor
// itself is s.History's own Merged entry (appendHistory's own timestamp, written by Drive the
// moment MergedStep is first found or made satisfied) rather than a new field: it requires no
// new dependency (git or forge) on this step, and it is guaranteed to be no earlier than the
// real merge event (Drive can only log "Merged" once the remote actually reports it merged),
// which is the safe direction for this comparison — a later anchor only ever delays
// satisfaction, never falsely advances it. mergedAt takes the *earliest* such entry across
// every resume, which is the closest available proxy to the real merge instant.
//
// The known failure mode the brief calls out — re-annotating on every Observe because
// "already refreshed" was never actually detected — cannot happen with this mechanism: once
// status.reconciledAt genuinely advances past the anchor, Observe reports Satisfied forever
// after (reconciledAt only moves forward). What *can* still happen, and is accepted rather than
// engineered around (the brief's own "wasteful but must not be treated as broken"): a kill
// between Act's Refresh call and Argo's own reconcile landing means a resumed Observe still
// sees a stale reconciledAt and Acts (refreshes) again. Argo's refresh is idempotent by design
// (a second request while one is already in flight is a no-op to the controller), so this costs
// an extra API call, never a second real action.
import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// The three M5 steps, run after Merged in that order: Argo must see the merged commit before
// anyone asks whether it synced, and syncing precedes asking whether the rollout it drove has
// actually landed.
const (
	StepArgoRefreshed StepName = "argo-refreshed"
	StepArgoSynced    StepName = "argo-synced"
	StepRolledOut     StepName = "rolled-out"
)

// ArgoAppNames returns the distinct, sorted set of Argo Application names in targetEnv whose
// family directory contains at least one edit's file. The CLI calls this once, from the same
// gitops.Repo Discover already produced, when building a PromotionState — mirroring
// RenderCommitMessage/PRTitle/RenderPRBody: a pure function of the repo's discovered structure
// and the plan, called once and then carried on PromotionState.ArgoApps (see its own doc
// comment for why carrying it does not violate "the world is the state"). An edit whose file
// matches no family in targetEnv is an internal inconsistency — BuildPlan only ever produces
// edits from occurrences it read from an env's own families — and is reported as an error
// naming the file and directory, rather than silently dropped.
func ArgoAppNames(r *gitops.Repo, targetEnv string, edits []gitops.Edit) ([]string, error) {
	env, ok := r.Envs[targetEnv]
	if !ok {
		return nil, fmt.Errorf("argo apps: target env %q not found in the discovered repo", targetEnv)
	}
	byDir := make(map[string]string, len(env.Families))
	for _, f := range env.Families {
		byDir[f.Dir] = f.App
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range edits {
		dir := path.Dir(e.File)
		app, ok := byDir[dir]
		if !ok {
			return nil, fmt.Errorf("argo apps: edit %s: no family in env %q owns directory %s", e.File, targetEnv, dir)
		}
		if !seen[app] {
			seen[app] = true
			names = append(names, app)
		}
	}
	sort.Strings(names)
	return names, nil
}

// argoApplications resolves s.ArgoApps into the argo.Application values pkg/argo's methods
// take, pairing each name with s.ArgoNamespace (the single control-plane namespace every
// Application in this repo's config lives in — see pkg/argo's package doc).
func (s *PromotionState) argoApplications() []argo.Application {
	if len(s.ArgoApps) == 0 {
		return nil
	}
	apps := make([]argo.Application, 0, len(s.ArgoApps))
	for _, name := range s.ArgoApps {
		apps = append(apps, argo.Application{Namespace: s.ArgoNamespace, Name: name})
	}
	return apps
}

// mergedAt is the earliest s.History entry recorded for StepMerged — see this file's own
// package doc comment for why it is a safe (if slightly conservative) proxy for the real merge
// wall-clock time, with no new dependency and no change to MergedStep itself. ok is false only
// if Drive has genuinely never reached Merged yet, which ArgoRefreshedStep's own MergeSHA guard
// already rules out before this is ever called.
func mergedAt(s *PromotionState) (t time.Time, ok bool) {
	for _, h := range s.History {
		if h.Step != StepMerged {
			continue
		}
		if !ok || h.At.Before(t) {
			t, ok = h.At, true
		}
	}
	return t, ok
}

// ArgoRefreshedStep asks Argo CD to look at the merged commit sooner than its own poll
// interval would. See this file's package doc comment for the Observe strategy and the
// idempotent-refresh reasoning.
type ArgoRefreshedStep struct{ Argo argo.Argo }

// Name implements Step.
func (ArgoRefreshedStep) Name() StepName { return StepArgoRefreshed }

// Observe implements Step.
func (a ArgoRefreshedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.MergeSHA == "" {
		// Nothing has merged yet — an earlier step's own Blocked/Waiting already stopped
		// Drive before this is reached in practice; this guard is what makes that an
		// invariant rather than an assumption.
		return Observation{Satisfied: false}, nil
	}
	apps := s.argoApplications()
	if len(apps) == 0 {
		return Observation{Satisfied: true, Detail: "no Argo Application in this promotion's plan"}, nil
	}
	anchor, _ := mergedAt(s) // zero time if somehow absent; see this func's own doc comment
	var pending []string
	for _, app := range apps {
		st, err := a.Argo.Get(ctx, app)
		if err != nil {
			if errorsIsNotFound(err) {
				return Observation{Blocked: fmt.Sprintf("Argo Application %s not found; check kube.argo_namespace and the repo's Application wrappers", app)}, nil
			}
			return Observation{}, fmt.Errorf("reading Argo Application %s: %w", app, err)
		}
		if !st.ReconciledAt.After(anchor) {
			pending = append(pending, app.Name)
		}
	}
	if len(pending) == 0 {
		return Observation{Satisfied: true, Detail: "every Application already reconciled after this promotion's merge"}, nil
	}
	sort.Strings(pending)
	return Observation{Satisfied: false, Detail: "refresh needed: " + strings.Join(pending, ", ")}, nil
}

// Act implements Step: annotates every Application this promotion touches. Annotating one
// already-reconciled Application (Observe found some pending, not necessarily all) is
// harmless — see this file's package doc comment on why a redundant refresh costs an API call,
// never a second real action.
func (a ArgoRefreshedStep) Act(ctx context.Context, s *PromotionState) error {
	for _, app := range s.argoApplications() {
		if err := a.Argo.Refresh(ctx, app); err != nil {
			return fmt.Errorf("refreshing Argo Application %s: %w", app, err)
		}
	}
	return nil
}

// ArgoSyncedStep is satisfied once every Application this promotion touches has synced to
// exactly this promotion's own merge commit and is healthy (invariant 3: revision match alone,
// or sync/health alone, never satisfies without the other).
type ArgoSyncedStep struct{ Argo argo.Argo }

// Name implements Step.
func (ArgoSyncedStep) Name() StepName { return StepArgoSynced }

// Observe implements Step. Health Degraded, or an operation phase of Failed/Error, Blocks
// immediately for that Application — never waits out the deadline first (invariant 3) —
// regardless of what its own revision currently reads, since a promotion has no business
// declaring itself synced-and-rolled-out while the app it just changed is unhealthy.
func (a ArgoSyncedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.MergeSHA == "" {
		return Observation{Satisfied: false}, nil
	}
	apps := s.argoApplications()
	if len(apps) == 0 {
		return Observation{Satisfied: true, Detail: "no Argo Application in this promotion's plan"}, nil
	}
	var notSynced []string
	for _, app := range apps {
		st, err := a.Argo.Get(ctx, app)
		if err != nil {
			if errorsIsNotFound(err) {
				return Observation{Blocked: fmt.Sprintf("Argo Application %s not found; check kube.argo_namespace and the repo's Application wrappers", app)}, nil
			}
			return Observation{}, fmt.Errorf("reading Argo Application %s: %w", app, err)
		}
		if st.HealthStatus == argo.HealthStatusDegraded {
			return Observation{Blocked: fmt.Sprintf("%s health is Degraded (sync=%s revision=%s)", app, st.SyncStatus, st.SyncRevision)}, nil
		}
		if st.OperationPhase == argo.OperationFailed || st.OperationPhase == argo.OperationError {
			return Observation{Blocked: fmt.Sprintf("%s operation phase is %s", app, st.OperationPhase)}, nil
		}
		switch {
		case st.SyncRevision != s.MergeSHA:
			notSynced = append(notSynced, fmt.Sprintf("%s: revision %s, want %s", app.Name, orNone(st.SyncRevision), s.MergeSHA))
		case st.SyncStatus != argo.SyncStatusSynced || st.HealthStatus != argo.HealthStatusHealthy:
			notSynced = append(notSynced, fmt.Sprintf("%s: sync=%s health=%s", app.Name, orNone(st.SyncStatus), orNone(st.HealthStatus)))
		}
	}
	if len(notSynced) > 0 {
		sort.Strings(notSynced)
		return Observation{Waiting: true, Detail: strings.Join(notSynced, "; ")}, nil
	}
	return Observation{Satisfied: true, Detail: "synced and healthy at " + s.MergeSHA}, nil
}

// Act implements Step: nothing to do. Syncing is Argo's own auto-sync/self-heal acting on the
// merged commit hoist already asked it to refresh toward; hoist has no separate "sync" call to
// make (AGENTS.md §4.7 — Argo is driven by the refresh annotation alone).
func (ArgoSyncedStep) Act(context.Context, *PromotionState) error { return nil }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// errorsIsNotFound reports whether err wraps argo.ErrNotFound.
func errorsIsNotFound(err error) bool { return errors.Is(err, argo.ErrNotFound) }

// deploymentWant is one container occurrence a promotion wrote, grouped by the Deployment that
// owns it.
type deploymentWant struct {
	Container string
	Init      bool
	New       string
}

// jobLikeRef is one Job or CronJob a promotion's edits touched — report-only (invariant 4).
type jobLikeRef struct{ Name, Kind string }

// groupEditsByWorkload partitions edits by Kind: Deployment edits, grouped by the Deployment's
// own name (namespace is uniformly s.TargetEnv — see gitops.Env's doc comment: the destination
// namespace a family's Application deploys into is exactly what TargetEnv already names); Job
// and CronJob edits, deduplicated by (kind, name), report-only.
func groupEditsByWorkload(edits []gitops.Edit) (deployments map[string][]deploymentWant, jobLikes []jobLikeRef) {
	deployments = map[string][]deploymentWant{}
	seenJobLike := map[jobLikeRef]bool{}
	for _, e := range edits {
		switch e.Kind {
		case "Deployment":
			deployments[e.Name] = append(deployments[e.Name], deploymentWant{
				Container: e.Container,
				Init:      strings.Contains(e.Path, "initContainers"),
				New:       e.New.String(),
			})
		case "Job", "CronJob":
			ref := jobLikeRef{Name: e.Name, Kind: e.Kind}
			if !seenJobLike[ref] {
				seenJobLike[ref] = true
				jobLikes = append(jobLikes, ref)
			}
		}
	}
	return deployments, jobLikes
}

// imageMismatches reports, for one Deployment's current rollout.DeploymentStatus, every wanted
// occurrence whose current image does not yet match — a container this promotion wrote that is
// missing from the live spec entirely counts as a mismatch too (it means the Deployment's own
// shape has drifted from what the plan assumed, not that the promotion is done).
func imageMismatches(ds rollout.DeploymentStatus, wants []deploymentWant) []string {
	current := make(map[[2]any]string, len(ds.Images))
	for _, img := range ds.Images {
		current[[2]any{img.Name, img.Init}] = img.Image
	}
	var out []string
	for _, w := range wants {
		got, ok := current[[2]any{w.Container, w.Init}]
		if !ok {
			out = append(out, fmt.Sprintf("%s: container not found in the live spec", w.Container))
			continue
		}
		if got != w.New {
			out = append(out, fmt.Sprintf("%s: still %s, want %s", w.Container, got, w.New))
		}
	}
	return out
}

// RolledOutStep is satisfied once every Deployment this promotion edited carries the new image
// in every occurrence it wrote and its rollout is complete by kubectl's own definition
// (invariant 4). A rollout that has exceeded its own progress deadline Blocks, the same
// immediacy ArgoSyncedStep applies to Degraded health — retrying will not fix a deployment
// that is never coming up. Jobs and CronJobs this promotion touched are listed, never gated on.
type RolledOutStep struct{ Rollout rollout.Rollout }

// Name implements Step.
func (RolledOutStep) Name() StepName { return StepRolledOut }

// Observe implements Step.
func (r RolledOutStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.MergeSHA == "" {
		return Observation{Satisfied: false}, nil
	}
	deployments, jobLikes := groupEditsByWorkload(s.Edits)

	var names []string
	for name := range deployments {
		names = append(names, name)
	}
	sort.Strings(names)

	var blocked, waiting, done []string
	for _, name := range names {
		ds, err := r.Rollout.Deployment(ctx, s.TargetEnv, name)
		if err != nil {
			return Observation{}, fmt.Errorf("checking rollout of Deployment %s/%s: %w", s.TargetEnv, name, err)
		}
		mismatches := imageMismatches(ds, deployments[name])
		switch {
		case ds.DeadlineExceeded:
			blocked = append(blocked, fmt.Sprintf("%s/%s: %s", s.TargetEnv, name, ds.Detail))
		case len(mismatches) > 0:
			waiting = append(waiting, fmt.Sprintf("%s: image not yet live (%s)", name, strings.Join(mismatches, ", ")))
		case !ds.Complete:
			waiting = append(waiting, fmt.Sprintf("%s: %s", name, ds.Detail))
		default:
			done = append(done, name+": "+ds.Detail)
		}
	}
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return Observation{Blocked: strings.Join(blocked, "; ")}, nil
	}

	var jobReports []string
	for _, jl := range jobLikes {
		js, err := r.Rollout.JobLike(ctx, s.TargetEnv, jl.Name, jl.Kind)
		if err != nil {
			return Observation{}, fmt.Errorf("checking %s %s/%s: %w", jl.Kind, s.TargetEnv, jl.Name, err)
		}
		jobReports = append(jobReports, fmt.Sprintf("%s %s: %s", jl.Kind, jl.Name, js.Detail))
	}
	sort.Strings(jobReports)

	if len(waiting) > 0 {
		sort.Strings(waiting)
		return Observation{Waiting: true, Detail: strings.Join(append(waiting, jobReports...), "; ")}, nil
	}
	sort.Strings(done)
	return Observation{Satisfied: true, Detail: strings.Join(append(done, jobReports...), "; ")}, nil
}

// Act implements Step: nothing to do. The rollout is the kubelet/Deployment controller acting
// on the manifest Argo already synced; hoist only ever observes it (AGENTS.md invariant 4 of
// M1-M4's own CIGreen/Approved precedent — "there is nothing for hoist itself to do about CI
// running", the same shape here for a rollout already in motion).
func (RolledOutStep) Act(context.Context, *PromotionState) error { return nil }

// AllSteps returns every step a promotion drives through, in order: Steps' four (branch,
// commit, push, PR), CIGreen, Approved and Merged (M4), then ArgoRefreshed, ArgoSynced and
// RolledOut (M5). `hoist promote` and `hoist resume` always drive AllSteps.
func AllSteps(g git.Git, f forge.Forge, a argo.Argo, ro rollout.Rollout, onWaiting func()) []Step {
	steps := append(Steps(g, f, onWaiting), CIGreenStep{Forge: f}, ApprovedStep{Forge: f, Git: g}, MergedStep{Forge: f, Git: g})
	return append(steps, ArgoRefreshedStep{Argo: a}, ArgoSyncedStep{Argo: a}, RolledOutStep{Rollout: ro})
}
