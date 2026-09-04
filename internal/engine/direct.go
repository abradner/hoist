package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/abradner/hoist/pkg/git"
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
				"direct mode refused: %q is listed in envs.production; production always goes through a PR and never direct mode (AGENTS.md §4.5) — this is enforced here regardless of what any UI layer offered or any caller believed",
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

// Observe implements Step: satisfied when origin's Base ref already carries what this
// promotion actually planned to write — either because the tip IS this promotion's own commit
// (the common case, checked by exact equality first) or because every one of this promotion's
// planned paths already matches its planned content at whatever the tip currently is (AGENTS.md
// gotcha, same class as MergedStep's own revert check elsewhere in this milestone's history).
//
// This deliberately compares planned blob CONTENT at the tip, never mere object-graph ANCESTRY
// (an earlier revision of this method used git.Git.IsAncestor instead — reverted here; see this
// package's own doc.go for the history). Ancestry alone cannot tell two cases apart that need
// opposite answers: "Base advanced further with a distinct, later, legitimate change" (this
// promotion's own commit is still an ancestor, AND the planned content is still genuinely there
// — Satisfied is correct) from "someone git-reverted this exact promotion's commit" (this
// promotion's own commit remains an ancestor forever — a revert commit never removes it from
// history — but the file content it changed is no longer at the tip; treating that as Satisfied
// would let a re-run of the identical promotion exit "successfully" without restoring anything).
// Comparing content directly gets both right without needing the ancestry relationship at all: a
// legitimate later change that never touches these paths still matches, and a revert (or any
// other rewrite) that changes them back no longer does.
//
// A Base ref that exists but doesn't yet carry the planned content is reported unsatisfied, not
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
		s.PushedSHA = remoteSHA
		return Observation{Satisfied: true, Detail: "origin/" + s.Base + " is already at " + remoteSHA}, nil
	}
	// remoteSHA differs from this promotion's own commit. Fetch first: the per-path content
	// check below needs the object for remoteSHA to actually exist in s.CloneDir's own
	// repository (a bare sha from LsRemoteBranch alone is not enough — ls-tree operates on
	// local history), and FetchBranch only ever refreshes the remote-tracking ref, never
	// s.CloneDir's own local branch of the same name (AGENTS.md §4.6; see FetchBranch's own doc
	// comment in pkg/git).
	if _, _, err := d.Git.FetchBranch(ctx, s.CloneDir, "origin", s.Base); err != nil {
		return Observation{}, err
	}
	// s.ExpectedBlobs is guaranteed populated by the time this runs: Drive runs steps strictly
	// in order (engine.go) and CommittedStep — which always computes it before reporting
	// Satisfied — precedes this step in DirectSteps' own list.
	paths := make([]string, 0, len(s.ExpectedBlobs))
	for p := range s.ExpectedBlobs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		blob, ok, err := d.Git.LsTreeBlob(ctx, s.CloneDir, remoteSHA, p)
		if err != nil {
			return Observation{}, err
		}
		if !ok || blob != s.ExpectedBlobs[p] {
			return Observation{Satisfied: false}, nil
		}
	}
	// PushedSHA names THIS promotion's own commit, not whatever else the tip now is — mirroring
	// the exact-match branch above and PushedStep's own field semantics: "PushedSHA" confirms
	// this promotion's own commit is effectively present, not "here is the tip's current SHA"
	// (which s.Base's own remote ref already tells a caller, if that's what they want).
	s.PushedSHA = s.CommitSHA
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
