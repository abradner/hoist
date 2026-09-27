package service

import (
	"context"
	"fmt"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
)

// IsProduction reports whether env is listed in envs.production (AGENTS.md §4.5): direct mode
// is never offered for it, whatever the source env. Moved from internal/app/plan/rows.go
// unchanged; it is a thin wrapper over config.EnvsConfig.IsProduction, kept as a function here
// (rather than inlined at every call site) so a screen deciding whether to offer direct mode and
// Plan's own deploy-into-production warning read the identical rule.
func IsProduction(env string, envs config.EnvsConfig) bool {
	return envs.IsProduction(env)
}

// WarnDeployIntoProduction attaches gitops.WarnProductionTarget to a deploy plan whose target
// env the operator's own config lists as production, and is the single place that decision is
// made: Plan (below) calls it for every deploy it builds, so the dry run, the confirm screen and
// the PR body all carry it — a warning only one surface shows is a warning the reviewer of a PR
// never sees. Informational only; production's real constraint is the PR — always — plus
// whichever approval mode the repo configures for that env: the comment is the default, but an
// explicit `approval: auto` is permitted (§4.5), so neither this helper nor the warning it
// attaches may claim a human comment is unconditional. Both are enforced in internal/engine.
//
// A no-op for anything but a deploy: a promotion into production is what the paired-env config
// exists to describe, so saying it out loud there is noise, not news. Moved from
// internal/app/plan/rows.go unchanged.
func WarnDeployIntoProduction(pl *gitops.Plan, envs config.EnvsConfig) {
	if pl == nil || !pl.IsDeploy() || !envs.IsProduction(pl.TargetEnv) {
		return
	}
	pl.Warnings = append(pl.Warnings, gitops.Warning{
		Code:    gitops.WarnProductionTarget,
		Message: fmt.Sprintf("%s is a production env: this deploy opens a PR, and waits for an approval comment unless the repo sets approval: auto for it", pl.TargetEnv),
	})
}

// Preflight is StartPromotion's own early gate, called before any freshness check, claim or
// state save — this is the trust-boundary fix this PR exists for (AGENTS.md's own design doc,
// Divergence 5): the CLI has always run its identical checkDirectPreflight before ever building a
// worktree or a claim, but the TUI's own buildStartPromotion only ever reached
// engine.DirectCommitGateStep once Drive itself first ran the step — AFTER the claim and the
// initial state save. Calling the identical gate here, first, closes that gap for both faces
// without weakening engine.DirectCommitGateStep's own status as the SOLE enforcement point
// (AGENTS.md §4.5): deleting this call cannot make a production direct commit possible, because
// StepsFor's own direct step list still carries the same step, checked again the moment Drive
// first runs (AGENTS.md §8, layered checks — the deletion test). ProductionEnvs is always
// s.settings.ProductionEnvs(), unfiltered, exactly as engine.StepsFor's own doc comment demands.
//
// For a non-direct request this only checks the forge identity every promotion eventually needs
// (repos[].github) — the same check the CLI's old runPromote/runDeploy made before ever calling
// their own buildPromotionForConfirm, and the TUI's old buildStartPromotion (cmd/hoist/wiring.go,
// both since removed by this train) made separately for itself; unified here, in
// StartPromotion's one call to Preflight, so neither face can drift from the other's wording
// again.
func (s *Service) Preflight(target string, m Mode) error {
	if !m.Direct {
		if s.settings.Repo == nil || s.settings.Repo.GitHub == "" {
			return &UsageError{Msg: "the selected repo has no github: owner/name configured; add repos[].github to the config file"}
		}
		return nil
	}
	// Direct mode's whole safety rests on knowing envs.production (AGENTS.md invariant 6): a
	// flags-only run has no such list, which would make DirectCommitGateStep's own
	// ProductionEnvs empty and every env look non-production by omission. Fail fast rather than
	// silently treat "unconfigured" as "safe" (AGENTS.md §8: never provide a fallback default
	// for required configuration).
	if s.settings.Repo == nil {
		return &UsageError{Msg: "--direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)"}
	}
	// Direct mode still needs the same github: owner/name the non-direct branch above requires
	// — not for the forge (direct mode never opens one), but because engine.DeriveID hashes it
	// into the promotion's id, which names the state path, the branch and the worktree
	// directory. Two repos both configured without github: that promote the same env+digest set
	// would otherwise derive the IDENTICAL id.
	if s.settings.Repo.GitHub == "" {
		return &UsageError{Msg: "the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)"}
	}
	gate := engine.DirectCommitGateStep{ProductionEnvs: s.settings.ProductionEnvs(), Confirmed: m.Confirmed}
	obs, err := gate.Observe(context.Background(), &engine.PromotionState{TargetEnv: target})
	if err != nil {
		return err
	}
	if obs.Blocked != "" {
		return &engine.BlockedError{Step: engine.StepDirectGate, Reason: obs.Blocked}
	}
	return nil
}
