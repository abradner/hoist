package service

import (
	"context"
	"fmt"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

// AnyRealEdit reports whether edits contains at least one edit that is not a NoOp (the target
// already carries exactly the planned reference) — the all-NoOp fast-path guard StartPromotion
// and `hoist plan`'s own branch-id line (cmd/hoist/main.go's runPlan) share, so a plan whose
// every edit is already satisfied is recognized identically wherever this codebase asks the
// question. Exported (unlike this file's other helpers) because runPlan's own display-only use
// has no promotion to start at all — duplicating this one-line predicate there would be exactly
// the kind of second copy AGENTS.md §8's "layered checks" section warns drifts from the first.
// Moved from cmd/hoist/promote.go's anyRealEdit, unchanged.
func AnyRealEdit(edits []gitops.Edit) bool {
	for _, e := range edits {
		if !e.NoOp() {
			return true
		}
	}
	return false
}

// Mode is how the operator asked to run this promotion. Direct/Confirmed are the CLI's own
// --direct/--confirm-direct pair or the TUI's keypress-then-confirm gesture — either way,
// Confirmed only ever selects whether a NON-production direct request is honoured;
// engine.DirectCommitGateStep re-derives the production refusal from ProductionEnvs regardless
// of Confirmed (AGENTS.md §4.5), and so does Preflight's own call to that same step above.
type Mode struct {
	Direct    bool
	Confirmed bool
	// OverrideCINone seeds the new (or resumed) PromotionState.CINoneOverride — the CLI's own
	// --override-ci-none. The TUI's `c` gesture instead calls a running Drive's own
	// OverrideCINone() mid-flight, never this: see Driver.OverrideCINone's own doc comment.
	OverrideCINone bool
}

// Hooks are optional, nil-safe callbacks StartPromotion invokes as it works through preflight —
// never anything a caller needs to poll for. Progress reports one short line per preflight
// stage, mirroring the CLI's own progress lines (runHooksForCLI) and the TUI's own preflight log
// (buildStartPromotion's report calls) — both now the identical text, since both go through this
// one function. It carries preflight lines only, never a step's outcome: those are
// PromotionState.History entries, and echoing them here put every event on the flight screen
// twice. OnWaiting is passed straight through to engine.StepsFor, for a later Step/Run
// call's own CommittedStep "waiting for signing approval" wait.
//
// OnAct reports each step's Act as it is about to run (ActEvent) — the one signal that a
// commit or a push is under way, since History, and so OnHistory below, only hears of an Act
// once it has returned. Like OnHistory it runs on the walking goroutine under the Driver's
// lock, so it must not block or call back into the Driver. Only the CLI sets it. AbandonWith and FindInFlightForEnv take a Hooks
// too and use Progress alone.
type Hooks struct {
	Progress  func(string)
	OnWaiting func()
	OnAct     func(ActEvent)

	// OnHistory is called by the Driver for each History entry newly appended by a Step walk, at
	// the moment the state save carrying it has landed — oldest first, once per entry — with a
	// copy of the state as saved (so e.g. s.PR is populated when the pr-opened entry fires). It
	// runs on the walking goroutine under the Driver's lock: it must not block and must not call
	// back into the Driver. Entries already in the state when the Driver was built are not
	// reported. This, not Progress, is how a step's outcome reaches a screen as it happens.
	OnHistory func(e engine.HistoryEntry, s engine.PromotionState)
}

func (h Hooks) report(line string) {
	if h.Progress != nil {
		h.Progress(line)
	}
}

// StartRequest is everything StartPromotion needs to take a confirmed plan the rest of the way:
// claim the target env, build (or resume) its PromotionState, save it and hand back a Drive.
type StartRequest struct {
	Plan gitops.Plan
	Mode Mode
	// Repo is the discovered GitOps repo ArgoAppNames/EditApps read from. nil reads s.Repo().Repo
	// AT CALL TIME, not when this request was built — this fixes FB-M1: the TUI's old
	// buildStartPromotion closed over a boot-time *gitops.Repo that F5 never updated, so an Argo
	// Application renamed after boot was still looked up under its stale name. The CLI passes its
	// own freshly discovered repo here instead, since a one-shot process only ever discovers once
	// anyway.
	Repo *gitops.Repo
	// View picks the freshness check: nil reads s.Repo() at call time (same reasoning as Repo
	// above). A view whose FromOrigin is true is checked with CheckRepoViewCurrent (the TUI's own
	// origin-cache check); everything else — including the CLI's own zero RepoView, since it
	// never calls LoadRepo — is checked with checkCloneCurrentForBase (the local-disk check
	// against a freshly fetched origin/<base>).
	View *RepoView
	// Fresh carries the digests/reasons this plan was actually resolved with, for direct mode's
	// own fresh-base occurrence check (checkFreshBase). nil recovers them from Plan.Edits instead
	// — the TUI's rule, unchanged: a filtered, ticked-down plan has no single resolution
	// behind it any more.
	Fresh *freshInputs
}

// newState builds this promotion's PromotionState, merging forward from any existing state file
// for the same id exactly as cmd/hoist/promote.go's buildPromotionForConfirm did: the state file
// is an index of what to look at, never evidence of what happened (AGENTS.md §4.1) — every
// Observe a later Drive/Step call makes re-derives truth from the worktree and the remote
// regardless of what's loaded here. Carrying History forward is purely so the caller's own
// output can show it; a missing or unreadable prior state never blocks the run. The policy
// fields (Base, CINone, CIGrace, Approval, Approvers, Collaborators) and the rendered artifacts
// are restored from the PRIOR state whenever one exists, rather than left fresh from this call's
// own settings/plan — PromotionState's own doc comment states these are policy "as of when this
// promotion started", carried forward so a promotion never straddles two different policies
// mid-flight (the resume-safety fix, commit f3b1c53, and its sibling fix for this exact call
// site, AGENTS.md's own regression history). An override flag given on a later re-run always
// wins over what a previous run persisted, since the operator just asked for it again.
func (s *Service) newState(id, branch, worktreeDir string, p gitops.Plan, mode Mode, argoApps []string, editApps map[string]string) *engine.PromotionState {
	rc := s.settings.Repo
	st := &engine.PromotionState{
		ID:             id,
		RepoFullName:   rc.GitHub,
		SourceEnv:      p.SourceEnv,
		TargetEnv:      p.TargetEnv,
		Branch:         branch,
		CloneDir:       s.settings.RepoDir,
		WorktreeDir:    worktreeDir,
		Base:           s.settings.Base,
		Direct:         mode.Direct,
		Edits:          p.Edits,
		CommitMessage:  engine.RenderCommitMessage(id, p),
		PRTitle:        engine.PRTitle(p),
		PRBody:         engine.RenderPRBody(id, p),
		CINone:         rc.CI.None,
		CIGrace:        time.Duration(rc.CI.Grace),
		CINoneOverride: mode.OverrideCINone,
		Approval:       rc.Approval(p.TargetEnv),
		Approvers:      rc.Approvers,
		Collaborators:  rc.Collaborators,
		ArgoNamespace:  rc.Kube.ArgoNamespace,
		ArgoApps:       argoApps,
		EditApps:       editApps,
	}
	if prev, err := s.deps.Store.Load(id); err == nil && prev != nil && prev.ID == id {
		st.History = prev.History
		st.Base = prev.Base
		st.CINone = prev.CINone
		st.CIGrace = prev.CIGrace
		st.Approval = prev.Approval
		st.Approvers = prev.Approvers
		st.Collaborators = prev.Collaborators
		if !mode.OverrideCINone {
			st.CINoneOverride = prev.CINoneOverride
		}
		// The rendered artifacts describe a commit and a PR that already exist: re-rendering
		// them from this invocation's plan would leave the state file narrating something the
		// forge does not say. DeriveID deliberately treats a deploy and a promotion landing
		// identical refs as the same promotion, so a `promote` re-run against a `deploy`'s id
		// would otherwise rewrite "deploy" wording to "promote" while the actual commit and PR
		// keep the original.
		if prev.CommitMessage != "" {
			st.CommitMessage = prev.CommitMessage
		}
		if prev.PRTitle != "" {
			st.PRTitle = prev.PRTitle
		}
		if prev.PRBody != "" {
			st.PRBody = prev.PRBody
		}
	}
	return st
}

// StartPromotion takes a confirmed plan the rest of the way to a drivable promotion: preflight,
// freshness, the one-in-flight claim, the first durable state save and the claim's release, all
// in one place — the CLI's own runPromote/runDeploy and the TUI's own buildStartPromotion used to
// each assemble this sequence by hand, in two different orders, which is what let the TUI reach
// engine.DirectCommitGateStep only after its own claim and save, and let the two faces run the
// claim/save order and the preflight order differently. This function is the one, canonical
// order both faces now call through:
//
//	Preflight → freshness (by View) → [direct] fresh-base occurrence check → no-op
//	(*AlreadyCurrentError) → forge identity → ArgoAppNames/EditApps → Argo/Rollout(KubeContext) →
//	claimTarget → newState (Direct set) → Store.Save → release → Driver.
//
// Two behaviour changes fall out of unifying on this order (accepted divergences, not defects):
//
//  1. The claim is now released only after the state's first successful save lands (the TUI's own
//     order) — the CLI used to release inside its save wrapper's own closure, which
//     is observably the same EXCEPT when Drive's very first Observe fails before its own first
//     save: a state file now remains in that case (Phase ""), blocking the env until `resume`/
//     `abandon` — already true for the TUI before this change.
//  2. A direct no-op whose origin has an occurrence the local checkout has never seen is now
//     refused with that specific error, on both faces, rather than the TUI's old "already
//     current" (the fresh-base check now always runs before the no-op check, the
//     CLI's own order).
func (s *Service) StartPromotion(ctx context.Context, req StartRequest, h Hooks) (Drive, error) {
	p := req.Plan

	if err := s.Preflight(p.TargetEnv, req.Mode); err != nil {
		return nil, err
	}

	view := req.View
	// s.Repo() is the service's own CURRENT view, not necessarily the one req.View names — the
	// two are compared here only to answer "is this Service running the TUI's origin-mode read at
	// all", never to substitute one for the other. When it is (cur.FromOrigin — LoadRepo(RepoFromOrigin)
	// has run, at TUI boot or a later F5), the caller MUST have supplied its own PlannedChange.View
	// (Plan's own frozen snapshot, plumbed through a screen's WithView and back through its
	// StartMsg.View) verbatim: a nil req.View, or one whose FromOrigin is false or SHA is empty, is
	// not "no view given, use the service's" here — it is a caller that built a plan through the
	// origin-mode Plan() (which always stamps a real FromOrigin view, or fails closed itself,
	// service:Plan) and then lost that view on the way to StartPromotion, most likely a dropped
	// WithView call or a broken loadedMsg/StartMsg.View plumb. Falling back to s.Repo() in that case
	// would silently downgrade the freshness check from CheckRepoViewCurrent (the tight
	// origin-tip comparison) to checkCloneCurrentForBase (the looser local-disk check the CLI
	// alone is supposed to get) — exactly the fail-open this refusal closes. The CLI never calls
	// LoadRepo(RepoFromOrigin), so cur.FromOrigin is always false there and this branch never
	// fires: req.View nil (or zero) is that path's own normal shape.
	if cur := s.Repo(); cur.FromOrigin {
		if view == nil || !view.FromOrigin || view.SHA == "" {
			return nil, &UsageError{Msg: "the repo view changed while this plan was being built — go back and reopen the plan"}
		}
	}
	if view == nil {
		v := s.Repo()
		view = &v
	}
	g := s.Git()
	h.report("checking your checkout against origin/" + s.settings.Base)
	if view.FromOrigin {
		if err := CheckRepoViewCurrent(ctx, g, s.settings.RepoDir, s.settings.Base, view.SHA); err != nil {
			return nil, err
		}
	} else if err := checkCloneCurrentForBase(ctx, g, s.settings.RepoDir, s.settings.Base, p.Edits); err != nil {
		return nil, err
	}

	// Direct mode's own additional gap ("base-advanced-with-new-occurrence"):
	// this runs BEFORE the no-op check below (the CLI's own historical order) so an
	// unseen origin occurrence is refused outright rather than reported as "already current" on
	// either face.
	if req.Mode.Direct {
		h.report("checking origin/" + s.settings.Base + " for occurrences your checkout hasn't seen")
		if err := s.checkFreshBase(ctx, g, p, req.Fresh); err != nil {
			return nil, err
		}
	}

	if !AnyRealEdit(p.Edits) {
		return nil, &AlreadyCurrentError{SourceEnv: p.SourceEnv, TargetEnv: p.TargetEnv, Deploy: p.IsDeploy(), Ref: deployRefOf(p)}
	}

	// The forge, like the cluster adaptors below, is checked only once a plan has proven to need
	// one: a confirm of an already-current plan on a machine whose `gh` login has lapsed says
	// "already current" from every entry point rather than a GitHub auth failure from this one.
	if s.settings.Repo == nil || s.settings.Repo.GitHub == "" {
		return nil, &UsageError{Msg: "the selected repo has no github: owner/name configured; add repos[].github to the config file"}
	}
	f, err := s.ForgeFor(s.settings.Repo.GitHub)
	if err != nil {
		return nil, err
	}

	r := req.Repo
	if r == nil {
		r = s.Repo().Repo
	}
	argoApps, err := engine.ArgoAppNames(r, p.TargetEnv, p.Edits)
	if err != nil {
		return nil, err
	}
	editApps, err := engine.EditApps(r, p.TargetEnv, p.Edits)
	if err != nil {
		return nil, err
	}

	a, err := s.Argo(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	ro, err := s.Rollout(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}

	id := engine.DeriveID(s.settings.Repo.GitHub, p)
	branch := engine.BranchName(p.TargetEnv, id)
	worktreeDir, err := s.deps.Store.WorktreeDir(id)
	if err != nil {
		return nil, err
	}

	h.report("claiming " + p.TargetEnv + " and checking for a conflicting promotion")
	release, err := s.claimTarget(ctx, s.settings.Repo.GitHub, p.TargetEnv, id)
	if err != nil {
		return nil, err
	}

	state := s.newState(id, branch, worktreeDir, p, req.Mode, argoApps, editApps)

	// Both faces on main released the claim when the first save failed (TUI: wiring.go's
	// release() on a SaveState error; CLI: promote.go's deferred release). This unifies
	// on the TUI's order (explicit initial save, then release on success), and that order still
	// releases on failure: a failed save leaves nothing durable for a future FindInFlight scan
	// to find, so holding the claim would block the target env until an operator deletes the
	// claim file by hand for no reason a re-observation could ever recover from.
	h.report("saving promotion state")
	if err := s.deps.Store.Save(state); err != nil {
		release()
		return nil, fmt.Errorf("writing initial promotion state: %w", err)
	}
	// The claim only needs to outlive the gap up to this first durable state write — once that
	// lands, a future FindInFlight scan of the real state file is what enforces invariant 5 for
	// the rest of the (possibly hours-long) promotion, so it is released here rather than held
	// for the whole run.
	release()

	steps := announce(engine.StepsFor(state, g, f, a, ro, s.settings.ProductionEnvs(), req.Mode.Confirmed, h.OnWaiting), h.OnAct)
	return NewDriver(steps, state, s.deps.Store.Save, s.settings.Poll).withOnHistory(h.OnHistory), nil
}
