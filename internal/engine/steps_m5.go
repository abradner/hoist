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
//
// A residual gap, raised and confirmed in round-1 review but deliberately not fixed here: the
// "guaranteed to be no earlier than the real merge event" claim above holds by causality (the
// merge must have already happened, on GitHub's own servers, before this process's Observe can
// see pr.Merged==true over the network) but the anchor's *value* is this process's own
// operator-machine clock reading at that moment (time.Now()), not GitHub's or the cluster's. If
// that machine's clock reads ahead of the Argo controller's, a genuinely-processed refresh can
// still appear stale (reconciledAt.Before(anchor)) after it has actually landed, so Observe
// re-Acts every poll.argo tick until real time catches up to the skewed anchor or poll.deadline
// gives up — annoying (repeated, harmless Refresh calls; still never a false Satisfied, still
// bounded by the deadline, so still in the "wasteful but must not be treated as broken" bucket
// above) but a worse wait than clock skew ought to cost. The persisted anchor is also, in this
// shape, local History evidence standing in for whether the remote action occurred, which is in
// tension with AGENTS.md §4.1's "re-observe, never remember" in spirit even though nothing here
// trusts it as proof of completion (only as a lower bound on wait time). Closing this properly
// means anchoring on a value the *forge* stamps (e.g. the merged PR's own server-side merge
// timestamp) instead of a local wall clock — pkg/forge.PR carries no such field today (only
// CreatedAt), so doing this right is a real, multi-package change (PR.MergedAt on the forge
// interface, the GitHub adaptor, the fake, and a new PromotionState field with the same
// legacy-decode question ArgoApps already has, see state.go) — bigger than a review-round fix,
// and worse to rush than to track. Tracked as
// https://github.com/abradner/hoist/issues/53.
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
	byFile, err := EditApps(r, targetEnv, edits)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, app := range byFile {
		if !seen[app] {
			seen[app] = true
			names = append(names, app)
		}
	}
	sort.Strings(names)
	return names, nil
}

// EditApps maps each edit's file to the Argo Application name that owns it — the same
// Family->Application walk ArgoAppNames dedupes and sorts, kept per-file here so a caller that
// needs to scope a question to one Application's own share of a promotion (ArgoSyncedStep's
// revisionCarries, PR #182 round-2 review) doesn't have to re-derive the mapping. The CLI calls
// this once, from the same gitops.Repo Discover already produced, when building a
// PromotionState, and carries the result on PromotionState.EditApps (see its own doc comment)
// rather than recomputing it on every resume — the same "structural fact about the plan,
// computed once" treatment ArgoApps already gets. An edit whose file matches no family in
// targetEnv is an internal inconsistency — BuildPlan only ever produces edits from occurrences
// it read from an env's own families — and is reported as an error naming the file and
// directory, rather than silently dropped.
func EditApps(r *gitops.Repo, targetEnv string, edits []gitops.Edit) (map[string]string, error) {
	env, ok := r.Envs[targetEnv]
	if !ok {
		return nil, fmt.Errorf("argo apps: target env %q not found in the discovered repo", targetEnv)
	}
	byDir := make(map[string]string, len(env.Families))
	for _, f := range env.Families {
		byDir[f.Dir] = f.App
	}
	out := make(map[string]string, len(edits))
	for _, e := range edits {
		dir := path.Dir(e.File)
		app, ok := byDir[dir]
		if !ok {
			return nil, fmt.Errorf("argo apps: edit %s: no family in env %q owns directory %s", e.File, targetEnv, dir)
		}
		out[e.File] = app
	}
	return out, nil
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

// editsForApp returns just the edits (and matching ExpectedBlobs entries) s.EditApps attributes
// to appName — the scoping a per-Application landed-verdict question needs (revisionCarries,
// round-2 review, PR #182: see observeLanded's own doc comment for why asking the whole
// promotion's verdict on behalf of one Application was wrong). Every driver of ArgoSyncedStep
// (cmd/hoist's driveToCompletion and the TUI's DriveFunc) runs against a state that has already
// been through ensureArgoApps — a fresh promotion has EditApps set at construction, a resumed
// one is repaired before Drive ever sees it (cmd/hoist/resume.go) — so an appName EditApps
// genuinely never names should not arise; if it somehow did, this returns no edits for that
// app, which drives blobsIntact to its vacuous-true/landedIntact path and reports carries=true,
// superseded=false — an unverified but harmless default, since the switch above still checks
// that app's own sync/health status rather than skipping it (only superseded=true skips).
func (s *PromotionState) editsForApp(appName string) (edits []gitops.Edit, expectedBlobs map[string]string) {
	expectedBlobs = make(map[string]string)
	for _, e := range s.Edits {
		if s.EditApps[e.File] != appName {
			continue
		}
		edits = append(edits, e)
		if b, ok := s.ExpectedBlobs[e.File]; ok {
			expectedBlobs[e.File] = b
		}
	}
	return edits, expectedBlobs
}

// mergedAt is the earliest s.History entry recorded for StepMerged — see this file's own
// package doc comment for why it is a safe (if slightly conservative) proxy for the real merge
// wall-clock time, with no new dependency and no change to MergedStep itself. ok is false only
// if Drive has genuinely never reached Merged yet, which ArgoRefreshedStep's own MergeSHA guard
// already rules out before this is ever called.
func landedAt(s *PromotionState) (t time.Time, ok bool) {
	want := s.landedStep()
	for _, h := range s.History {
		if h.Step != want {
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
	if s.LandedSHA() == "" {
		// Nothing has landed on Base yet — a merge for a PR promotion, a direct push for a
		// direct one (LandedSHA's own doc comment). An earlier step's Blocked/Waiting already
		// stopped Drive before this is reached in practice; the guard makes that an invariant
		// rather than an assumption.
		return Observation{Satisfied: false}, nil
	}
	apps := s.argoApplications()
	if len(apps) == 0 {
		return Observation{Satisfied: true, Detail: "no Argo Application in this promotion's plan"}, nil
	}
	anchor, ok := landedAt(s)
	if !ok {
		// s.MergeSHA is set (checked above), so mergedAt's own doc comment's invariant says
		// this "should" never happen — but trusting that blindly is exactly the zero-means-
		// cannot-determine trap: a zero-value anchor makes st.ReconciledAt.After(anchor) true
		// for essentially any real timestamp, so a state that reaches here anyway (a legacy or
		// otherwise inconsistent state file: MergeSHA persisted with no matching StepMerged
		// History entry) would report every Application "already reconciled" with zero actual
		// evidence a refresh ever landed after the merge (Copilot review). Block clearly,
		// naming the inconsistency, rather than silently trusting a wait that can't happen
		// (Satisfied: false would hang forever; Satisfied: true would be worse) or waiting
		// indefinitely for History to grow an entry nothing here will ever add retroactively.
		return Observation{Blocked: fmt.Sprintf(
			"this promotion landed at %s but has no recorded %s step in its own history — cannot anchor the Argo refresh check; investigate the state file manually",
			s.LandedSHA(), s.landedStep(),
		)}, nil
	}
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

// ArgoSyncedStep is satisfied once every Application this promotion touches has synced to a
// revision that CARRIES this promotion's change and is healthy (invariant 3: revision agreement
// alone, or sync/health alone, never satisfies without the other).
//
// "Carries", not "is". Argo tracks the BRANCH and reports whatever tip it last synced, so exact
// equality with LandedSHA() holds only in the window between this promotion's merge and the next
// commit to the base — after which the reported revision is a descendant, the comparison fails,
// and it fails forever: the step returned Waiting indefinitely, RolledOutStep after it never
// ran, and the promotion never reached a terminal phase. Three merged, long-since-rolled-out
// promotions sat in the TUI's in-flight pane for days that way, each reporting a revision that
// was the current tip of main and contained its own merge commit (#165).
type ArgoSyncedStep struct {
	Argo argo.Argo
	// Git is the clone the ancestry and content checks run in (s.CloneDir). Nil is allowed and
	// means exact-equality only — the pre-#165 behaviour — for a caller that has no clone at
	// all. Every real driver has one: ConvergeSteps passes the same git.Git the earlier steps
	// take.
	Git git.Git
	// Rollout is optional, nil-safe like Git above (#PR4): when set, a synced-but-not-yet-
	// healthy Application borrows RolledOutStep's own per-Deployment read — one gate earlier —
	// to name which Deployment is still converging and what kubectl would say, rather than
	// leaving the operator with the bare "sync=Synced health=Progressing" tuple a real stuck
	// promotion (y2ef7pknhu, spritz-production) reported with no cause at all: Argo can sit
	// Progressing indefinitely without ever tripping its own Degraded/Failed cases above, so
	// the pipeline stalled behind the least-informed step while RolledOutStep, one step later,
	// already had the read that would have named it. ConvergeSteps wires the same
	// rollout.Rollout the later step takes.
	Rollout rollout.Rollout
}

// Name implements Step.
func (ArgoSyncedStep) Name() StepName { return StepArgoSynced }

// Observe implements Step. Health Degraded, or an operation phase of Failed/Error, Blocks
// immediately for that Application — never waits out the deadline first (invariant 3) —
// regardless of what its own revision currently reads, since a promotion has no business
// declaring itself synced-and-rolled-out while the app it just changed is unhealthy.
func (a ArgoSyncedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.LandedSHA() == "" {
		return Observation{Satisfied: false}, nil
	}
	apps := s.argoApplications()
	if len(apps) == 0 {
		return Observation{Satisfied: true, Detail: "no Argo Application in this promotion's plan"}, nil
	}
	// fetched keeps the base fetch to at most one per Observe rather than one per Application:
	// every app in a promotion is on the same base, and this method is called on every poll
	// tick (poll.argo, seconds apart) for the whole convergence window.
	var fetched bool
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
		carries, superseded, why, err := a.revisionCarries(ctx, s, app.Name, st.SyncRevision, &fetched)
		if err != nil {
			return Observation{}, err
		}
		switch {
		case !carries:
			notSynced = append(notSynced, fmt.Sprintf("%s: revision %s, want %s%s", app.Name, orNone(st.SyncRevision), s.LandedSHA(), why))
		case superseded:
			// This app's synced revision landed and was replaced by a later, legitimate
			// change (AGENTS.md §4.1: superseded is satisfied, not reverted) — the live
			// containers now legitimately run someone else's later change, not this
			// promotion's own Edit.New. Round-2 review finding: calling rolloutCause here
			// compared the wrong thing against imageMismatches (reporting "image not yet
			// live" for an image this promotion never expects to see live again) and, worse,
			// could Block an already-finished promotion on an unrelated later deploy's own
			// failed rollout. This app needs nothing further from this step.
		case st.SyncStatus != argo.SyncStatusSynced || st.HealthStatus != argo.HealthStatusHealthy:
			msg := fmt.Sprintf("%s: sync=%s health=%s", app.Name, orNone(st.SyncStatus), orNone(st.HealthStatus))
			// Naming a cause only makes sense once Argo itself agrees the sync landed — a
			// SyncStatus other than Synced is Argo's own problem to report, not a rollout
			// question yet.
			if st.SyncStatus == argo.SyncStatusSynced {
				if cause, blocked := a.rolloutCause(ctx, s); cause != "" {
					if blocked {
						return Observation{Blocked: fmt.Sprintf("%s: %s", app.Name, cause)}, nil
					}
					msg += " (" + cause + ")"
				}
			}
			notSynced = append(notSynced, msg)
		}
	}
	if len(notSynced) > 0 {
		sort.Strings(notSynced)
		return Observation{Waiting: true, Detail: strings.Join(notSynced, "; ")}, nil
	}
	return Observation{Satisfied: true, Detail: "synced and healthy at " + s.LandedSHA()}, nil
}

// revisionCarries reports whether the revision Argo says it synced to actually carries this
// promotion's change, and a parenthetical for the Waiting detail when it does not. superseded
// is true only for the landedSuperseded verdict — carries is true for both landedIntact and
// landedSuperseded (the original "carried" meaning, unchanged), but the caller needs to tell
// them apart: a superseded app's live containers legitimately run a LATER, unrelated change,
// so comparing them against this promotion's own stale Edit.New (rolloutCause, below) would be
// meaningless at best and a wrong Block at worst — round-2 review finding against an earlier
// version of this split.
//
// Three cases, in order:
//
//  1. rev IS LandedSHA() — the exact match, unchanged, and still the common one during the
//     window before anything else lands on the base.
//  2. LandedSHA() is not an ancestor of rev — Argo has not caught up (or has synced to a
//     revision this promotion is not part of at all). Not carried.
//  3. LandedSHA() is an ancestor of rev — the base advanced past us. Ancestry alone is NOT
//     enough here, for exactly the reason DirectPushedStep.Observe documents: a revert commit
//     never removes the reverted commit from history, so this stays true forever even after
//     every byte has been undone. observeLanded reads what rev actually declares, and only
//     landedIntact or landedSuperseded count as carried.
//
// With a nil Git only case 1 can be decided, so everything else is "not carried" — the exact
// pre-#165 behaviour, and no caller in this repo takes that path.
func (a ArgoSyncedStep) revisionCarries(ctx context.Context, s *PromotionState, appName, rev string, fetched *bool) (carries, superseded bool, why string, err error) {
	if rev == s.LandedSHA() {
		return true, false, "", nil
	}
	if a.Git == nil || rev == "" || s.CloneDir == "" {
		return false, false, "", nil
	}
	// The revision Argo reports is one this clone may never have seen: fetch the base before
	// asking about its object graph, exactly as DirectPushedStep.Observe does. Once per
	// Observe, not once per Application — see fetched's own declaration.
	if !*fetched {
		if _, _, ferr := a.Git.FetchBranch(ctx, s.CloneDir, "origin", s.Base); ferr != nil {
			return false, false, "", ferr
		}
		*fetched = true
	}
	isAncestor, aerr := a.Git.IsAncestor(ctx, s.CloneDir, s.LandedSHA(), rev)
	if aerr != nil || !isAncestor {
		// An unresolvable revision is not an error worth failing the whole promotion over —
		// Argo can report a revision this clone genuinely does not have (a force-pushed base,
		// a revision from another remote). Treated as "not carried", which is Waiting, which
		// re-observes.
		return false, false, "", nil
	}
	edits, blobs := s.editsForApp(appName)
	verdict, detail, lerr := observeLanded(ctx, a.Git, s.CloneDir, rev, edits, blobs)
	if lerr != nil {
		return false, false, "", lerr
	}
	switch verdict {
	case landedIntact:
		return true, false, "", nil
	case landedSuperseded:
		return true, true, "", nil
	default:
		return false, false, " (" + detail + ")", nil
	}
}

// rolloutCause borrows RolledOutStep's own per-Deployment read (this file, below) to name what
// is actually converging behind a synced-but-not-yet-healthy Application (#PR4). Returns ""
// when Rollout is nil, every read fails (degrades to the plain sync/health tuple — this read is
// a courtesy for the operator, never a gate this step depends on), or nothing here has anything
// to say yet. blocked is true only when a Deployment's own rollout has exceeded its deadline —
// RolledOutStep would Block on that identical read one step later; surfacing it here saves the
// operator a full extra poll cycle waiting to learn what RolledOutStep already knows.
func (a ArgoSyncedStep) rolloutCause(ctx context.Context, s *PromotionState) (detail string, blocked bool) {
	if a.Rollout == nil {
		return "", false
	}
	deployments, _ := groupEditsByWorkload(s.Edits)
	names := make([]string, 0, len(deployments))
	for name := range deployments {
		names = append(names, name)
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		ds, err := a.Rollout.Deployment(ctx, s.TargetEnv, name)
		if err != nil {
			if errors.Is(err, rollout.ErrNotFound) {
				// The same signal RolledOutStep itself would Block on one step later (its
				// own "Deployment not found" case) — safe to skip here rather than build a
				// verdict on it, since this read is a courtesy, never the actual gate.
				continue
			}
			// A transient error (not "genuinely absent") means this read cannot be trusted
			// for ANY Deployment this round — reporting a confident cause built from an
			// incomplete picture (round-2 review finding: an earlier version silently
			// swallowed this on one Deployment while confidently naming or Blocking on
			// another) would be worse than the plain sync/health tuple the caller falls
			// back to. RolledOutStep's own poll loop is what actually retries a transient
			// failure; this courtesy read simply says nothing this round.
			return "", false
		}
		if ds.DeadlineExceeded {
			return fmt.Sprintf("%s/%s: %s", s.TargetEnv, name, ds.Detail), true
		}
		if mismatches := imageMismatches(ds, deployments[name]); len(mismatches) > 0 {
			parts = append(parts, fmt.Sprintf("%s: image not yet live (%s)", name, strings.Join(mismatches, ", ")))
			continue
		}
		if !ds.Complete {
			parts = append(parts, fmt.Sprintf("%s: %s", name, ds.Detail))
		}
	}
	return strings.Join(parts, "; "), false
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

// containerKey identifies one container slot within a Deployment's live spec — a typed
// alternative to a [2]any map key (round-1 review finding: unsafe and easy to misuse for no
// benefit over a two-field struct).
type containerKey struct {
	name string
	init bool
}

// imageMismatches reports, for one Deployment's current rollout.DeploymentStatus, every wanted
// occurrence whose current image does not yet match — a container this promotion wrote that is
// missing from the live spec entirely counts as a mismatch too (it means the Deployment's own
// shape has drifted from what the plan assumed, not that the promotion is done).
func imageMismatches(ds rollout.DeploymentStatus, wants []deploymentWant) []string {
	current := make(map[containerKey]string, len(ds.Images))
	for _, img := range ds.Images {
		current[containerKey{img.Name, img.Init}] = img.Image
	}
	var out []string
	for _, w := range wants {
		got, ok := current[containerKey{w.Container, w.Init}]
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
// that is never coming up. A missing Deployment (rollout.ErrNotFound) Blocks the same way,
// naming the Deployment — mirroring ArgoRefreshedStep/ArgoSyncedStep's own errorsIsNotFound
// handling of a missing Application, for the same reason: retrying cannot make a deleted object
// reappear, and a generic plumbing error would read as "something is broken" rather than "this
// object is gone" (round-1 review finding). Any other error reading a Deployment (a transient
// API hiccup) still propagates as a plain error, for the CLI's poll loop to retry.
//
// Jobs and CronJobs this promotion touched are listed, never gated on: any error reading one
// (not found — a short ttlSecondsAfterFinished or an Argo hook's deletion policy can GC a Job
// before this Observe gets to it — or transient) becomes a report line and the loop continues,
// rather than a hard error that would gate the whole promotion on a status this step's own
// contract says it never gates on (round-1 review finding).
type RolledOutStep struct{ Rollout rollout.Rollout }

// Name implements Step.
func (RolledOutStep) Name() StepName { return StepRolledOut }

// Observe implements Step.
func (r RolledOutStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.LandedSHA() == "" {
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
			if errors.Is(err, rollout.ErrNotFound) {
				blocked = append(blocked, fmt.Sprintf("Deployment %s/%s not found", s.TargetEnv, name))
				continue
			}
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
			// Report-only (this step's own doc comment, invariant 4): a Job/CronJob hoist
			// cannot even read (GC'd, or a transient API error) is surfaced as a report line,
			// never as a Blocked/hard-error that would gate the whole promotion on a status
			// this step never gates on (round-1 review finding).
			jobReports = append(jobReports, fmt.Sprintf("%s %s: could not check (%s)", jl.Kind, jl.Name, err))
			continue
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

// CoreSteps returns the seven steps a promotion drives through up to and including the merge:
// Steps' four (branch, commit, push, PR) plus CIGreen, Approved and Merged. This is exactly the
// step list (and signature) `AllSteps` had before M5 — see steps_m4.go's own trailing comment —
// kept alive under a new name because M5 needed the name `AllSteps` for the ten-step list below.
// It exists for one caller: `findInFlight` in cmd/hoist/drive.go, which deliberately observes
// only through Merged when deciding whether a promotion still counts as "in flight" for AGENTS.md
// invariant 5 — see that function's own doc comment for the reasoning. `hoist promote` and
// `hoist resume` never call this directly; they always drive `AllSteps` to real completion.
func CoreSteps(g git.Git, f forge.Forge, onWaiting func()) []Step {
	return append(Steps(g, f, onWaiting), CIGreenStep{Forge: f}, ApprovedStep{Forge: f, Git: g}, MergedStep{Forge: f, Git: g})
}

// ObserveSteps is the step list to observe a PRIOR promotion state by, chosen from the state
// itself rather than from what the caller happens to be doing now. Three callers ask the same
// question of a state file they did not create — findInFlight ("is another promotion still
// running for this env?"), `hoist promotions` and `resume --env`'s candidate scan — and all
// three used a fixed list, which for a direct state is a list it can never satisfy: a direct
// promotion pushes to the base branch, so PushedStep's branch on origin, PROpenedStep's PR and
// MergedStep's merge are all permanently unsatisfied. The consequence was not cosmetic: one
// completed direct run made findInFlight refuse every later promotion into that env forever,
// and left the finished run listed as in flight.
//
// DirectCommitGateStep is deliberately NOT among the direct list here. The gate decides whether
// a direct promotion may START; re-running it while observing one that already landed would let
// a later config edit (an env newly listed under envs.production) turn a finished run into a
// permanently blocked one — a state file re-interpreted by today's config rather than observed.
// Refusing a new direct promotion is the gate's job, and it still runs first in DirectSteps
// where that decision is actually made (AGENTS.md §4.5, R-007).
//
// through is where to stop: pass nil for the git-only core (findInFlight's own "the branch/PR
// collision risk is retired" boundary — see its doc comment), or a non-nil argo/rollout pair
// for the full list. The PR path's boundary is Merged; the direct path's is the push.
func ObserveSteps(s *PromotionState, g git.Git, f forge.Forge, a argo.Argo, ro rollout.Rollout, onWaiting func()) []Step {
	core := CoreSteps(g, f, onWaiting)
	if s != nil && s.Direct {
		core = []Step{BranchedStep{Git: g}, CommittedStep{Git: g, OnWaiting: onWaiting}, DirectPushedStep{Git: g}}
	}
	if a == nil && ro == nil {
		return core
	}
	return append(core, ConvergeSteps(g, a, ro)...)
}

// AllSteps returns every step a promotion drives through, in order: CoreSteps' seven (branch,
// commit, push, PR, CIGreen, Approved, Merged) then ArgoRefreshed, ArgoSynced and RolledOut
// (M5). `hoist promote` and `hoist resume` always drive AllSteps to completion.
func AllSteps(g git.Git, f forge.Forge, a argo.Argo, ro rollout.Rollout, onWaiting func()) []Step {
	return append(CoreSteps(g, f, onWaiting), ConvergeSteps(g, a, ro)...)
}

// ConvergeSteps is the post-landing tail both modes share: ask Argo to refresh, wait for it to
// agree with what landed, then watch the Deployments roll. Extracted so DirectSteps drives the
// identical three rather than a copy — the design has always said direct mode converges through
// Argo too ("Pushed -> ArgoRefreshed -> ..."), and it only ever stopped at the push because
// every step here used to gate on MergeSHA, which a direct push never produces (issue #66).
//
// g reaches ArgoSyncedStep, which needs the clone to tell "Argo has synced past us with later
// work" from "Argo has synced to a revision that reverted us" (#165). It may be nil only for a
// caller with no clone at all, which costs that caller the ancestry check — see
// ArgoSyncedStep.Git.
func ConvergeSteps(g git.Git, a argo.Argo, ro rollout.Rollout) []Step {
	return []Step{ArgoRefreshedStep{Argo: a}, ArgoSyncedStep{Argo: a, Git: g, Rollout: ro}, RolledOutStep{Rollout: ro}}
}
