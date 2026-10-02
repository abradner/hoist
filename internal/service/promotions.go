package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// EnsureArgoApps repairs a state file written before M5 added PromotionState.ArgoApps: JSON
// decoding an older file leaves the field empty, and
// ArgoRefreshedStep/ArgoSyncedStep both take their `len(apps) == 0` "no Argo Application in this
// promotion's plan" success path on an empty ArgoApps — so an upgraded, already-in-flight
// promotion could be reported complete having never actually checked the Application it edited.
//
// An empty ArgoApps alongside a non-empty Edits is unambiguous evidence of exactly that:
// engine.ArgoAppNames' own contract (see its doc comment) means a real call against a
// non-empty edit set can never itself return an empty, non-error slice — every edit's directory
// maps to exactly one family/Application, or the call errors naming the orphan edit. So this
// only ever fires for a state genuinely predating ArgoApps' introduction, never for a fresh,
// post-M5 promotion that legitimately touches no Argo Application (which also has no Edits at
// all, since BuildPlan produces edits only from what an env's own families declare).
//
// s.ArgoApps is otherwise left untouched — state.go's own doc comment ("computed once ... then
// carried unchanged across every resume") still governs every other case, matching Edits/
// CommitMessage/PRTitle/PRBody's own carried-not-recomputed treatment.
//
// s.ArgoNamespace gets the identical treatment in the same pass, for the identical reason: a
// pre-M5 state file decodes it as the empty string too (the field didn't exist yet), and
// argoApplications() builds an argo.Application with whatever s.ArgoNamespace holds verbatim —
// an empty namespace fails Argo.Get's own input validation outright ("application needs a
// namespace"), rather than merely under-reporting like an empty ArgoApps does. Both fields are
// always set together at construction time for every post-M5 promotion (StartPromotion), so
// "ArgoApps is empty and Edits is not" is exactly as unambiguous a legacy signal for
// ArgoNamespace as it is for ArgoApps itself — this function's own name stays EnsureArgoApps
// since Applications remain the primary concern, but it closes both gaps a legacy state can
// have.
//
// s.EditApps gets the same treatment as a third, independent gap (PR #182): a
// state file saved any time between M5 and EditApps' own introduction has a populated ArgoApps
// but a nil EditApps, since the two fields were computed together at construction from that
// point on but ArgoApps alone before it — so "ArgoApps non-empty" cannot stand in for "EditApps
// populated" the way it does for ArgoNamespace above, and this checks EditApps on its own
// terms, discovering the repo only once for whichever of the two repairs this state actually
// needs.
//
// Moved unchanged from cmd/hoist/resume.go's ensureArgoApps, and exported because both List and
// Resume in this file call it, and a
// resume_test.go's own legacy-state regression coverage needs it directly too.
func EnsureArgoApps(s *engine.PromotionState, rc config.RepoConfig) error {
	needsApps := len(s.ArgoApps) == 0 && len(s.Edits) > 0
	needsEditApps := len(s.EditApps) == 0 && len(s.Edits) > 0
	if !needsApps && !needsEditApps {
		return nil
	}
	r, err := gitops.Discover(s.CloneDir, rc.AppsRoot)
	if err != nil {
		return fmt.Errorf("rebuilding Argo Applications for a pre-M5 state file: %w", err)
	}
	if needsApps {
		apps, err := engine.ArgoAppNames(r, s.TargetEnv, s.Edits)
		if err != nil {
			return fmt.Errorf("rebuilding Argo Applications for a pre-M5 state file: %w", err)
		}
		s.ArgoApps = apps
		s.ArgoNamespace = rc.Kube.ArgoNamespace
	}
	if needsEditApps {
		editApps, err := engine.EditApps(r, s.TargetEnv, s.Edits)
		if err != nil {
			return fmt.Errorf("rebuilding per-edit Argo Applications for a state file predating EditApps: %w", err)
		}
		s.EditApps = editApps
	}
	return nil
}

// Find looks up one promotion's state file by id, directly (engine.StatePath(id) is
// deterministic from id, so this never has to scan every state the way FindInFlightForEnv
// does) — the one path `hoist resume <id>` and `hoist abandon <id>` both take to turn an
// operator-given id into a *engine.PromotionState.
//
// engine.StatePath joins id straight into the promotions directory (`id+".json"`), with no
// containment check of its own — main's own ListStates-based lookup never took an
// operator-given string anywhere near a path join, so this join-by-id shortcut (moved here
// from cmd/hoist's own runResume/runAbandon) is a NEW path-traversal surface: an id containing
// a path separator or ".." reaches outside the promotions directory (`../../etc/passwd`), or
// sideways into the archive subdirectory (`archive/<real-id>`), letting `hoist resume`/`hoist
// abandon` act on an archived or aged-out promotion `hoist promotions` never lists as live.
// Find refuses any id containing a path separator or ".." outright, and —
// once a state file does load — refuses one whose own ID does not match id, so a file that
// happens to sit at the expected path but was never saved as this id (or was moved/archived
// since) is treated as not found rather than silently acted on.
func (s *Service) Find(id string) (*engine.PromotionState, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return nil, &NotFoundError{ID: id}
	}
	st, err := s.deps.Store.Load(id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, &NotFoundError{ID: id}
	}
	if st.ID != id {
		return nil, &NotFoundError{ID: id}
	}
	return st, nil
}

// FindInFlightForEnv answers `hoist resume --env <target-env>`'s own question: which single,
// still-in-flight promotion (per AGENTS.md invariant 5, there should never legitimately be more
// than one) targets env. Every live state file targeting env is re-observed (AGENTS.md §4.1,
// never trusted from its own recorded Phase) through the git/forge-only core (Argo/rollout are
// both nil — the same core FindInFlight and abandon's own landed-check use: "is this promotion's
// own write still pending" does not need the cluster).
//
// A candidate whose repo is no longer in the config file, whose forge/Argo-rollout clients
// cannot be built, or whose own re-observation errors, is never silently excluded — that would
// let a transient forge/git failure misleadingly read as "no in-flight promotion" (or resolve an
// otherwise-ambiguous set down to one candidate without ever confirming the choice was actually
// unambiguous). Every such candidate is instead collected into a *UnconfirmedError, returned
// once the scan finishes, naming every candidate it could not confirm.
//
// Moved unchanged in substance from cmd/hoist/resume.go's own --env matching loop.
func (s *Service) FindInFlightForEnv(ctx context.Context, env string) (*engine.PromotionState, error) {
	states, err := s.deps.Store.List()
	if err != nil {
		return nil, err
	}
	var matches []*engine.PromotionState
	var obsErrs []string
	for _, st := range states {
		if st.TargetEnv != env {
			continue
		}
		rc, ok := RepoConfigFor(s.settings.Config, st.RepoFullName)
		if !ok {
			obsErrs = append(obsErrs, fmt.Sprintf("%s: repo %q is not in the current config; restore it or name this promotion's id explicitly", st.ID, st.RepoFullName))
			continue
		}
		f, ferr := s.ForgeFor(rc.GitHub)
		if ferr != nil {
			obsErrs = append(obsErrs, fmt.Sprintf("%s: building a forge client: %v", st.ID, ferr))
			continue
		}
		if ferr := EnsureArgoApps(st, rc); ferr != nil {
			obsErrs = append(obsErrs, fmt.Sprintf("%s: %v", st.ID, ferr))
			continue
		}
		done, _, oerr := engine.ObserveAll(ctx, engine.ObserveSteps(st, s.Git(), f, nil, nil, nil), st)
		if oerr != nil {
			obsErrs = append(obsErrs, fmt.Sprintf("%s: %v", st.ID, oerr))
			continue
		}
		if !done {
			matches = append(matches, st)
		}
	}
	if len(obsErrs) > 0 {
		sort.Strings(obsErrs)
		return nil, &UnconfirmedError{Env: env, Errs: obsErrs}
	}
	switch len(matches) {
	case 0:
		return nil, &NotFoundError{Env: env}
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return nil, &AmbiguousError{Env: env, IDs: ids}
	}
}

// Listed is one promotion's own re-observed standing, as `hoist promotions` and the matrix's
// in-flight pane both now build it from the same List call. Exactly one of Unconfigured/Err/
// (Done or a populated Statuses) describes why: Unconfigured means State.RepoFullName names a
// repo no longer in the config file (nothing was even attempted); Err means a client could not
// be built, EnsureArgoApps failed, or the re-observation itself errored (already redacted —
// AGENTS.md §4.10); otherwise Statuses is engine.Status's own full per-step walk and Last is its
// final entry (the CLI only ever prints Last; the matrix's pane needs every entry to draw
// per-step glyphs. engine.Status is used here,
// not engine.ObserveAll, specifically so one walk serves both, and Last is derived from its own
// final entry rather than a second, separate ObserveAll call).
type Listed struct {
	State        engine.PromotionState
	Done         bool
	Statuses     []engine.StepStatus
	Last         engine.StepStatus
	Err          error
	Unconfigured bool
	// Archived is true when this same List call just archived this promotion (it was Done and
	// older than ArchiveDoneOlderThan) — the CLI's own --archived listing needs this to avoid
	// printing an archived-this-run promotion a second time (#181).
	Archived bool
	// ArchiveErr is set when archiving was attempted (Done, past the retention window) but
	// failed — the promotion is still reported Done, with the archive failure alongside it,
	// exactly as runPromotions always has.
	ArchiveErr error
}

// ListOpts scopes and configures one List call: RepoFullName filters to one repo ("" lists
// every configured repo's promotions, exactly like `hoist promotions` with no --repo), and
// ArchiveDoneOlderThan is the retention window (state.retain) past which a Done promotion is
// moved to the archive as part of this same call — 0 (or negative) disables archiving entirely,
// matching `hoist promotions`' own "retain: 0 means never archive" reading of the config.
type ListOpts struct {
	RepoFullName         string
	ArchiveDoneOlderThan time.Duration
}

// List re-observes every live promotion state file — `hoist promotions`' own listing and the
// matrix's in-flight pane both drive from this one call now (previously two separate re-
// observation loops, cmd/hoist/resume.go's runPromotions and cmd/hoist/wiring.go's
// observeForList, which could in principle disagree about the very same state file). Never
// reads a state file's own recorded Phase (AGENTS.md §4.1) and never returns early on one
// candidate's own error — a promotion that cannot be confirmed is reported as such (Listed.Err),
// never dropped.
func (s *Service) List(ctx context.Context, o ListOpts) ([]Listed, error) {
	states, err := s.deps.Store.List()
	if err != nil {
		return nil, err
	}
	if o.RepoFullName != "" {
		filtered := make([]*engine.PromotionState, 0, len(states))
		for _, st := range states {
			if st.RepoFullName == o.RepoFullName {
				filtered = append(filtered, st)
			}
		}
		states = filtered
	}
	out := make([]Listed, 0, len(states))
	for _, st := range states {
		out = append(out, s.listOne(ctx, st, o))
	}
	return out, nil
}

func (s *Service) listOne(ctx context.Context, st *engine.PromotionState, o ListOpts) Listed {
	rc, ok := RepoConfigFor(s.settings.Config, st.RepoFullName)
	if !ok {
		return Listed{State: *st, Unconfigured: true}
	}
	f, err := s.ForgeFor(rc.GitHub)
	if err != nil {
		return Listed{State: *st, Err: errors.New(redact.Strings(fmt.Sprintf("could not build a forge client: %v", err)))}
	}
	a, ro, err := s.ArgoRolloutFor(rc, s.settings.KubeOverride)
	if err != nil {
		return Listed{State: *st, Err: errors.New(redact.Strings(fmt.Sprintf("could not build an Argo/rollout client: %s", err.Error())))}
	}
	if err := EnsureArgoApps(st, rc); err != nil {
		return Listed{State: *st, Err: errors.New(redact.Strings(err.Error()))}
	}
	done, statuses, err := engine.Status(ctx, engine.ObserveSteps(st, s.Git(), f, a, ro, nil), st)
	if err != nil {
		return Listed{State: *st, Err: errors.New(redact.Strings(err.Error()))}
	}
	l := Listed{State: *st, Done: done, Statuses: statuses}
	if n := len(statuses); n > 0 {
		l.Last = statuses[n-1]
	}
	if done && o.ArchiveDoneOlderThan > 0 && time.Since(st.LastActivity()) > o.ArchiveDoneOlderThan {
		if aerr := s.deps.Store.Archive(st.ID); aerr != nil {
			l.ArchiveErr = aerr
		} else {
			l.Archived = true
		}
	}
	return l
}

// ListArchived returns every archived promotion state file (state.retain's own destination),
// optionally scoped to one repo — `hoist promotions --archived`'s own listing, called AFTER
// List so that a promotion List just archived (Listed.Archived) can be excluded from a caller's
// own combined rendering instead of appearing twice in the same invocation's output.
func (s *Service) ListArchived(repoFullName string) ([]*engine.PromotionState, error) {
	arch, err := s.deps.Store.ListArchived()
	if err != nil {
		return nil, err
	}
	if repoFullName == "" {
		return arch, nil
	}
	out := make([]*engine.PromotionState, 0, len(arch))
	for _, st := range arch {
		if st.RepoFullName == repoFullName {
			out = append(out, st)
		}
	}
	return out, nil
}

// ResumeOpts is Resume's own per-call configuration: OverrideCINone is the CLI's
// --override-ci-none / the TUI's already-applied `c` gesture (a fresh CINoneOverride to seed
// onto the state before driving, distinct from Driver.OverrideCINone's own mid-flight version of
// the same flag); Hooks carries OnWaiting, passed to engine.StepsFor for the returned Drive.
// Hooks.Progress is preflight-only and Resume has no preflight, so it reports nothing here: a
// resumed promotion's events reach a screen through the state's History (one entry per change),
// never as a second stream of progress lines mirroring them.
type ResumeOpts struct {
	OverrideCINone bool
	Hooks          Hooks
}

// Resume re-derives a drivable promotion from a persisted state file, honouring exactly the
// same carry-forward rules cmd/hoist/resume.go's runResume and cmd/hoist/wiring.go's
// buildInFlightFuncs.Resume both applied by hand (and had drifted on: the TUI passed no
// progress, no onWaiting, and the CLI's own ctx already wraps a deadline the TUI's call site
// never threaded through either — this is the one path both now take):
//
//   - CINone/CIGrace/Approval/Approvers/Collaborators are NEVER re-read from the current config
//     file — PromotionState's own doc comment states the invariant that these are policy "as of
//     when this promotion started", so a promotion never straddles two different policies
//     mid-flight even if the operator edits the config file while it is in flight. Only
//     o.OverrideCINone is a one-shot, explicit override for this call.
//   - ArgoNamespace IS re-read from the current config on every resume: it only names where a
//     live Get for this env's Argo Applications lands, not a decision replayed against
//     historical events, so re-reading it fails loudly (Application not found) rather than
//     misjudging anything quietly if it moved.
//   - EnsureArgoApps repairs a legacy state file missing ArgoApps/ArgoNamespace/EditApps.
//   - engine.StepsFor is asked for the mode this promotion actually IS (a direct promotion never
//     pushed its branch and has no PR, so driving it through the PR-path steps would push a
//     branch and open one — the exact shape --direct exists to avoid); Confirmed is always true,
//     because this promotion's own existence on disk is the confirmation (the state file only
//     exists because the operator already passed --confirm-direct, or the TUI's own gesture,
//     when it first started) — DirectCommitGateStep still re-derives the production refusal
//     independently of Confirmed (AGENTS.md §4.5), so resuming can never reach an env the
//     original run would have been refused.
func (s *Service) Resume(ctx context.Context, id string, o ResumeOpts) (Drive, error) {
	// Honours ctx eagerly, unlike the TUI's own pre-move Resume adapter (it built a state and
	// Driver even against an already-cancelled context) — a caller that
	// cancelled before calling Resume should never see it start building a state at all.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := s.Find(id)
	if err != nil {
		return nil, err
	}
	rc, ok := RepoConfigFor(s.settings.Config, st.RepoFullName)
	if !ok {
		return nil, fmt.Errorf("%s: repo %s is not in the config file", st.ID, st.RepoFullName)
	}
	f, err := s.ForgeFor(rc.GitHub)
	if err != nil {
		return nil, err
	}
	a, ro, err := s.ArgoRolloutFor(rc, s.settings.KubeOverride)
	if err != nil {
		return nil, err
	}

	st.ArgoNamespace = rc.Kube.ArgoNamespace
	if o.OverrideCINone {
		st.CINoneOverride = true
	}
	if err := EnsureArgoApps(st, rc); err != nil {
		return nil, err
	}

	steps := engine.StepsFor(st, s.Git(), f, a, ro, rc.Envs.Production, true, o.Hooks.OnWaiting)
	return NewDriver(steps, st, s.deps.Store.Save, s.settings.Poll), nil
}

// Abandon retires promotion id for good: releases the state file and, if it opened a PR, closes
// it and deletes its remote branch. The returned actions are one human-readable description per
// real action actually taken, in order (empty when nothing beyond the state file itself needed
// touching — a direct-mode promotion never opens a PR or pushes a branch to origin at all).
//
// Re-observes first and refuses outright if the promotion has already landed (merged, for the PR
// path; pushed, for direct mode) — abandoning is not a rollback: a landed promotion needs `hoist
// deploy` or a fresh promotion to undo, never a state-file delete. The observation is the
// identical git/forge-only core FindInFlight/FindInFlightForEnv both use (engine.ObserveSteps
// with Argo/rollout both nil) — "has this promotion's own write happened" is exactly the
// question that answers, and it is also exactly the three-way intact/superseded/reverted
// judgment (AGENTS.md §4.1) DirectPushedStep/MergedStep already apply, so Abandon inherits it
// rather than re-deriving a second opinion.
//
// `done` alone is not "has this landed": MergedStep.Observe (PR path) and DirectPushedStep
// (direct path) both mutate st in place with the real landed sha the moment their own merge/push
// is CONFIRMED — even when they go on to report Satisfied:false for something that only ever
// happens AFTER landing (the PR path's own "merged as X; branch not yet deleted" case: AGENTS.md
// §6.1 gotcha 7). st.LandedSHA() asks the direct, mode-agnostic question this actually needs
// answered; `done` stays as a second, independent gate (AGENTS.md §8 "layered checks").
//
// No claim file is touched: a promotion with a state file at all already released its own claim
// the moment that file was first saved.
//
// Moved unchanged in substance from cmd/hoist/abandon.go's abandonPromotion.
func (s *Service) Abandon(ctx context.Context, id string) ([]string, error) {
	st, err := s.Find(id)
	if err != nil {
		return nil, err
	}

	rc, ok := RepoConfigFor(s.settings.Config, st.RepoFullName)
	if !ok {
		return nil, fmt.Errorf("%s: repo %s is not in the config file", st.ID, st.RepoFullName)
	}
	f, err := s.ForgeFor(rc.GitHub)
	if err != nil {
		return nil, err
	}
	g := s.Git()

	done, status, err := engine.ObserveAll(ctx, engine.ObserveSteps(st, g, f, nil, nil, nil), st)
	if err != nil {
		return nil, fmt.Errorf("checking whether %s has already landed: %w", id, err)
	}
	if done || st.LandedSHA() != "" {
		return nil, fmt.Errorf("%s has already landed (%s); abandoning is not a rollback — use `hoist deploy` or a fresh promotion to undo it", id, Detail(status.Observation))
	}

	var lines []string
	// st.PR is never stale here, even on a retry after ClosePR succeeded but a LATER step in
	// this same call failed (DeleteRemoteBranch, say): ObserveAll above always probes MergedStep
	// first when it's in the step list, and MergedStep.Observe's own findOwnPR unconditionally
	// re-fetches the live PR and assigns it back to st.PR before this ever runs.
	if st.PR != nil && !st.PR.Merged && !st.PR.Closed {
		if _, err := f.ClosePR(ctx, st.PR.Number); err != nil {
			return lines, fmt.Errorf("closing PR #%d: %w", st.PR.Number, err)
		}
		lines = append(lines, fmt.Sprintf("closed PR #%d", st.PR.Number))
	}
	// Direct mode never pushes st.Branch to origin at all — nothing to delete, and no PR to
	// close either (st.PR is always nil for a direct promotion) — so both blocks above/below are
	// no-ops for it by construction; this guard just skips the pointless remote call rather than
	// relying on DeleteRemoteBranch's own idempotency to make it harmless.
	if !st.Direct && st.Branch != "" {
		if err := g.DeleteRemoteBranch(ctx, st.CloneDir, "origin", st.Branch); err != nil {
			return lines, fmt.Errorf("deleting branch %s: %w", st.Branch, err)
		}
		lines = append(lines, "deleted branch "+st.Branch)
	}

	if err := s.deps.Store.Delete(id); err != nil {
		return lines, fmt.Errorf("deleting state file: %w", err)
	}
	return lines, nil
}
