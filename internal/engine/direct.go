package engine

import (
	"context"
	"fmt"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/rollout"
)

// StepDirectGate and StepDirectPushed are direct mode's own two steps (AGENTS.md M6 brief,
// "Direct mode"). They run instead of — never alongside — StepPushed and StepPROpened:
// DirectSteps assembles a disjoint step list from Steps, and nothing in this package ever
// combines the two. StepBranched and StepCommitted are shared unchanged: a direct-mode
// promotion still needs a worktree and a commit, exactly as the PR flow does (BranchedStep,
// CommittedStep — untouched by this file), only the publish step differs.
const (
	StepDirectGate   StepName = "direct-gate"
	StepDirectPushed StepName = "direct-pushed"
)

// DirectCommitGateStep is the sole enforcement point for AGENTS.md invariant 5/6: direct mode
// is a distinct commit path that must be unreachable for a production env "by construction,
// not convention", and no config combination may weaken that.
//
// The mechanism: this step runs first in DirectSteps' list, before BranchedStep/CommittedStep/
// DirectPushedStep ever touch a worktree or the remote. Its Observe reports either Satisfied
// (both conditions below hold, so Drive proceeds to the steps that actually write) or Blocked
// (either fails); Drive (engine.go) stops at the first Blocked step and never calls a later
// step's Observe or Act at all — so a production env's block here is not "this step declines
// to act", it is "nothing after this step in the list ever runs". Act re-checks the identical
// two conditions before doing anything (belt and suspenders, matching appendHistory's own
// pattern in engine.go): Drive only calls Act when Observe reported not-Satisfied-and-not-
// Blocked, which this step's Observe never returns, so Act is not reachable through Drive at
// all — the check is duplicated there anyway so that a caller who builds a *BlockedError-blind
// driver of their own (bypassing Drive) still cannot reach a production commit by calling Act
// directly.
//
//   - (a) s.TargetEnv must not be listed in ProductionEnvs. ProductionEnvs must always be
//     the repo's full, unfiltered RepoConfig.Envs.Production — see the doc comment on
//     DirectSteps for why passing anything narrower here would defeat this entirely, and
//     AGENTS.md invariant 6 for why no additional config field is needed or wanted: "not
//     listed in envs.production" is already the one and only switch, and it is the same list
//     that already governs the PR-required and approval-required behaviors elsewhere
//     (AGENTS.md §4.5) — there is no second "direct allowed" toggle for a config bug or a
//     future caller to set inconsistently with it.
//   - (b) Confirmed must be true: the operator has already completed the keypress + huh.
//     Confirm gesture invariant 5 requires (internal/app/tags). This step does not itself
//     render or drive that UI — it only trusts the bool it was built with — so the caller
//     that constructs DirectCommitGateStep (cmd/hoist) is the one place responsible for never
//     setting Confirmed true except in direct response to that confirmed gesture (or, at the
//     CLI, its own explicit two-flag equivalent — see cmd/hoist's own doc comment on
//     runPromote's --direct/--confirm-direct=<env> flags — the latter must equal --to exactly,
//     refused otherwise; see runPromote's own doc comment).
type DirectCommitGateStep struct {
	// ProductionEnvs is RepoConfig.Envs.Production, verbatim.
	ProductionEnvs []string
	Confirmed      bool
}

// Name implements Step.
func (DirectCommitGateStep) Name() StepName { return StepDirectGate }

// refuse returns the reason direct mode is refused for s, or "" when both conditions hold.
func (d DirectCommitGateStep) refuse(s *PromotionState) string {
	for _, p := range d.ProductionEnvs {
		if p == s.TargetEnv {
			return fmt.Sprintf(
				"direct mode refused: %q is listed in envs.production; production always goes through a PR and never direct mode — this is enforced here regardless of what any UI layer offered or any caller believed",
				s.TargetEnv,
			)
		}
	}
	if !d.Confirmed {
		return "direct mode refused: the operator has not completed the required keypress + confirmation for this env"
	}
	return ""
}

// Observe implements Step. It is intentionally never "not satisfied but not blocked" — see
// the type's own doc comment for why that shape is what makes the later steps unreachable
// through Drive whenever this step refuses.
func (d DirectCommitGateStep) Observe(_ context.Context, s *PromotionState) (Observation, error) {
	if reason := d.refuse(s); reason != "" {
		return Observation{Blocked: reason}, nil
	}
	return Observation{Satisfied: true, Detail: fmt.Sprintf("direct mode confirmed for non-production env %q", s.TargetEnv)}, nil
}

// Act implements Step. Not reachable through Drive (see the type doc comment) — kept as a
// second, independent check in case anything outside this package ever calls Act without
// going through Drive/Observe first.
func (d DirectCommitGateStep) Act(_ context.Context, s *PromotionState) error {
	if reason := d.refuse(s); reason != "" {
		return fmt.Errorf("%s: %s", StepDirectGate, reason)
	}
	return nil
}

// DirectPushedStep is direct mode's publish step: it pushes the promotion's own worktree
// branch (built by BranchedStep/CommittedStep exactly as the PR flow builds it) straight onto
// s.Base on origin, via git.Git.PushHeadTo, instead of opening a PR from it. Its Observe/Act
// only ever reference s.Base as the remote ref that must move — never s.Branch, which in
// direct mode is purely a local staging name inside the worktree and is never pushed under
// its own name (PushHeadTo's own doc comment explains why: s.Base is very likely already
// checked out in the user's own clone, and git refuses to check that branch name out a second
// time in this promotion's own worktree).
type DirectPushedStep struct{ Git git.Git }

// Name implements Step.
func (DirectPushedStep) Name() StepName { return StepDirectPushed }

// Observe implements Step: satisfied when origin's Base ref already carries this promotion's
// change — either because the tip IS this promotion's own commit (the common case, checked by
// exact equality first) or because observeLanded says the change is intact or has since been
// superseded at whatever the tip currently is.
//
// This deliberately judges CONTENT, never mere object-graph ANCESTRY (an earlier revision of
// this method used git.Git.IsAncestor — reverted; see this package's own doc.go for that
// history). Ancestry cannot tell "Base advanced with a distinct, later change" from "someone
// git-reverted this exact promotion's commit": a revert never removes the original commit from
// history, so the ancestry relation holds forever, and treating that as Satisfied would let a
// re-run exit "successfully" without restoring anything.
//
// But content compared by BLOB HASH alone was only half the answer, and the missing half wedged
// a real env (#166). A later deploy into the same env rewrites the very same image scalars, so
// the planned blob is no longer at the tip — by hash, identical to a revert. Reported
// unsatisfied, that state could never be satisfied by anything: this step's own Act would push
// this promotion's now-stale ref back over the newer deploy, and until then findInFlight (which
// observes a direct state through exactly this step) refused every further promotion into that
// env, permanently. observeLanded separates the two: same image repo at some other reference is
// landedSuperseded — landed, and legitimately replaced — while the promotion's own original
// reference back at the tip is landedReverted, and the repo gone from the occurrence entirely is
// landedGone. Only the first is satisfied.
//
// A Base ref that exists but does not yet carry the planned content is reported unsatisfied, not
// Blocked — Act's own push is what actually discovers whether that is "not pushed yet" or "a
// genuine conflict" (mirroring PushedStep's shape one step later, since direct mode has no
// separate branch push to observe first).
func (d DirectPushedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	remoteSHA, ok, err := d.Git.LsRemoteBranch(ctx, s.CloneDir, "origin", s.Base)
	if err != nil {
		return Observation{}, err
	}
	if !ok || s.CommitSHA == "" {
		return Observation{Satisfied: false}, nil
	}
	if remoteSHA == s.CommitSHA {
		// Direct is recorded by the step that does the landing, not left to whichever caller built
		// this state: LandedSHA reads it to decide whether PushedSHA or MergeSHA is the commit
		// Argo should be looking for, and a state that reached this push is direct by definition
		// regardless of what any CLI flag said. The CLI sets it too, earlier, so resume knows the
		// mode before any step runs; both write the same value.
		s.PushedSHA = remoteSHA
		s.Direct = true
		return Observation{Satisfied: true, Detail: "origin/" + s.Base + " is already at " + remoteSHA}, nil
	}
	// remoteSHA differs from this promotion's own commit. Fetch first: every check below needs
	// the objects for remoteSHA to actually exist in s.CloneDir's own repository (a bare sha
	// from LsRemoteBranch is not enough — ls-tree and show operate on local history), and
	// FetchBranch only ever refreshes the remote-tracking ref, never s.CloneDir's own local
	// branch of the same name (AGENTS.md §4.6; see FetchBranch's own doc comment in pkg/git).
	if _, _, err := d.Git.FetchBranch(ctx, s.CloneDir, "origin", s.Base); err != nil {
		return Observation{}, err
	}
	// s.ExpectedBlobs and s.Edits are guaranteed populated by the time this runs: Drive runs
	// steps strictly in order (engine.go) and CommittedStep — which always computes
	// ExpectedBlobs before reporting Satisfied — precedes this step in DirectSteps' own list.
	verdict, detail, err := observeLanded(ctx, d.Git, s.CloneDir, remoteSHA, s)
	if err != nil {
		return Observation{}, err
	}
	if verdict == landedReverted || verdict == landedGone {
		return Observation{Satisfied: false, Detail: detail}, nil
	}
	if verdict == landedSuperseded {
		// A supersede — unlike an intact match — is not by itself evidence that THIS promotion
		// ever landed. "The occurrence names our image repo at some other reference" is equally
		// true of a promotion that was pushed and then replaced, and of one that was never
		// pushed at all while somebody else deployed that repo in the meantime. Reporting the
		// second as satisfied would skip the push and claim, in the Detail below, that the
		// promotion "landed and has since been replaced" — a claim the mechanism does not
		// deliver (AGENTS.md principle 1), leaving `hoist deploy` exiting successfully without
		// having written anything.
		//
		// Ancestry is exactly the missing evidence, and is sound HERE precisely because it is
		// paired with content: on its own it cannot see a revert (which is why the intact/
		// reverted judgement above is content-only), but "our commit is in the base's history"
		// is a fact about whether we published, which is the question a supersede leaves open.
		// ArgoSyncedStep pairs them the same way round.
		landed, err := d.Git.IsAncestor(ctx, s.CloneDir, s.CommitSHA, remoteSHA)
		if err != nil || !landed {
			return Observation{Satisfied: false, Detail: fmt.Sprintf(
				"origin/%s declares another reference for this promotion's image repo, but this promotion's own commit %s is not in its history — it was never pushed",
				s.Base, s.CommitSHA,
			)}, nil
		}
	}
	// PushedSHA is the base-branch revision that CARRIES this promotion's content, which here
	// is remoteSHA, not s.CommitSHA. Recording the original commit instead reads better as a
	// field name and is unobservable in the world: Argo tracks the branch and reports the tip,
	// so ArgoSyncedStep — which compares status.sync.revision against LandedSHA() — would wait
	// out its whole deadline for a SHA the branch has already moved past and will never report
	// again. Re-derived on every observation rather than pinned once, so a base that keeps
	// moving keeps converging (§4.1: re-observe, never remember). The exact-match branch above
	// records the same thing; the two only look different because there the tip and this
	// promotion's commit happen to be equal.
	s.PushedSHA = remoteSHA
	s.Direct = true
	if verdict == landedSuperseded {
		return Observation{Satisfied: true, Detail: fmt.Sprintf(
			"origin/%s has moved to %s and %s — this promotion landed and has since been replaced; nothing left to do",
			s.Base, remoteSHA, detail,
		)}, nil
	}
	return Observation{Satisfied: true, Detail: fmt.Sprintf(
		"origin/%s has moved to %s (not this promotion's own commit %s), but every planned path still matches the planned content there — already effectively promoted, not reverted",
		s.Base, remoteSHA, s.CommitSHA,
	)}, nil
}

// Act implements Step.
func (d DirectPushedStep) Act(ctx context.Context, s *PromotionState) error {
	if err := d.Git.PushHeadTo(ctx, s.WorktreeDir, "origin", s.Base); err != nil {
		remoteSHA, ok, lerr := d.Git.LsRemoteBranch(ctx, s.CloneDir, "origin", s.Base)
		if lerr == nil && ok && s.CommitSHA != "" && remoteSHA != s.CommitSHA {
			return fmt.Errorf(
				"push rejected: origin/%s is at %s, this promotion's commit is %s — the base branch moved after this promotion branched from it; treating this as a real conflict, not retrying with --force: %w",
				s.Base, remoteSHA, s.CommitSHA, err,
			)
		}
		return fmt.Errorf("git push (retryable — check network and try again): %w", err)
	}
	s.PushedSHA = s.CommitSHA
	s.Direct = true
	// Belt and suspenders, not the sole mechanism: a plain `git push` to a ref covered by
	// origin's default fetch refspec already updates cloneDir's own refs/remotes/origin/<Base>
	// as a side effect (verified against real git; this is standard behavior, not something
	// this package arranges), which is what lets pkg/git.Exec.Worktree's own resolveBase see
	// this push's content for a later, independent promotion's BranchedStep without any extra
	// step here at all. This call exists for the corner case where that side effect doesn't
	// apply (a customized remote.origin.fetch, in particular) rather than leaving the fix
	// resting entirely on an implicit git behavior nothing here asserts explicitly. Best-effort:
	// a failure is never fatal — the promotion itself is already done, the push above is what
	// actually matters — and worst case a later promotion falls back to exactly the staleness
	// this call exists to additionally guard against, never worse than before either existed.
	_, _, _ = d.Git.FetchBranch(ctx, s.CloneDir, "origin", s.Base)
	return nil
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
