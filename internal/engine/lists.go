package engine

// lists.go is the one place every step-list constructor lives — separated from the step
// implementations themselves (steps_git.go, steps_review.go, steps_converge.go, direct.go) in
// the by-concern split, because a list constructor is a different kind of code from a step: it
// answers "which steps, in what order, for this mode", never "what does one step do".

import (
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/rollout"
)

// Steps returns the four steps, in order, wired to g and f.
func Steps(g git.Git, f forge.Forge, onWaiting func()) []Step {
	return []Step{
		BranchedStep{Git: g},
		CommittedStep{Git: g, OnWaiting: onWaiting},
		PushedStep{Git: g},
		PROpenedStep{Forge: f},
	}
}

// CoreSteps returns the seven steps a promotion drives through up to and including the merge:
// Steps' four (branch, commit, push, PR) plus CIGreen, Approved and Merged. This is exactly the
// step list (and signature) `AllSteps` had before M5 — see steps_review.go's own trailing
// comment — kept alive under a new name because M5 needed the name `AllSteps` for the ten-step
// list below.
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

// DirectSteps returns the steps a direct-mode promotion drives: the production/confirmation
// gate, then the same branch-and-commit steps the PR flow uses (BranchedStep, CommittedStep —
// unmodified), then DirectPushedStep in place of PushedStep+PROpenedStep.
//
// productionEnvs MUST be RepoConfig.Envs.Production passed through exactly as loaded, never
// filtered, narrowed, or recomputed by the caller — DirectCommitGateStep's whole guarantee
// rests on this list actually being the one config authority that also governs PR-required
// and approval-required elsewhere (AGENTS.md §4.5); a caller that "helpfully" pre-filters it
// (e.g. "only pass the envs relevant to this repo") reintroduces exactly the config-bug risk
// invariant 6 asks to be structurally impossible. confirmed must be true only in direct
// response to the operator's own keypress + huh.Confirm gesture (internal/app/tags) or, at the
// CLI, its documented equivalent (cmd/hoist) — never a default, never inferred from anything
// else in the promotion.
func DirectSteps(g git.Git, productionEnvs []string, confirmed bool, onWaiting func()) []Step {
	return []Step{
		DirectCommitGateStep{ProductionEnvs: productionEnvs, Confirmed: confirmed},
		BranchedStep{Git: g},
		CommittedStep{Git: g, OnWaiting: onWaiting},
		DirectPushedStep{Git: g},
	}
}

// AllDirectSteps is DirectSteps plus the Argo/rollout convergence both modes share — the direct
// mirror of CoreSteps/AllSteps, and what `hoist promote --direct` and `hoist deploy --direct`
// actually drive. The split exists for the same reason the PR path's does: DirectSteps is the
// git-only core, useful on its own in tests that exercise the gate and the push without a
// cluster, while the exported pairing keeps a caller from silently driving a promotion that
// lands a commit and then never tells Argo about it (issue #66).
func AllDirectSteps(g git.Git, a argo.Argo, ro rollout.Rollout, productionEnvs []string, confirmed bool, onWaiting func()) []Step {
	return append(DirectSteps(g, productionEnvs, confirmed, onWaiting), ConvergeSteps(g, a, ro)...)
}

// StepsFor is the one place the drive-side step list is chosen by mode — the mirror of
// ObserveSteps above, which does the same for the observe-side list. Every production call site
// that used to switch on a state's Direct flag between AllSteps and AllDirectSteps now calls
// this instead, so the switch is written once rather than copied at every driver (it had drifted
// to five call sites: cmd/hoist's promote, deploy, resume and wiring — twice — before this).
//
// productionEnvs and confirmed are passed straight through to AllDirectSteps for the Direct
// case and are simply unused for the non-Direct case — see AllDirectSteps' own doc comment for
// why productionEnvs must be the caller's unfiltered RepoConfig.Envs.Production and confirmed
// must come from an actual confirmation gesture, never a default.
func StepsFor(s *PromotionState, g git.Git, f forge.Forge, a argo.Argo, ro rollout.Rollout, productionEnvs []string, confirmed bool, onWaiting func()) []Step {
	if s.Direct {
		return AllDirectSteps(g, a, ro, productionEnvs, confirmed, onWaiting)
	}
	return AllSteps(g, f, a, ro, onWaiting)
}
