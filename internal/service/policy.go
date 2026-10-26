package service

import (
	"context"
	"fmt"
	"strings"

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

// SkippedStaging reports the configured pipeline predecessor of target's production env that a
// promotion from source steps over: staging is the env envs.pairs sends source to, skip is true
// when target is production but is not that configured pair. False when target is not
// production, when it is exactly the configured pair, or when source has no pair to skip
// (AGENTS.md §4.5). Moved from internal/app/plan unchanged; the plan screen's own sentence and
// WarnSkippedStagingProduction read this one rule.
func SkippedStaging(source, target string, envs config.EnvsConfig) (staging string, skip bool) {
	if !envs.IsProduction(target) {
		return "", false
	}
	staging, ok := envs.Pairs[source]
	if !ok || staging == "" || staging == target {
		return "", false
	}
	return staging, true
}

// WarnSkippedStagingProduction attaches gitops.WarnSkippedStaging to a promotion into a
// production env from a source that is not its configured predecessor: "promoting straight to
// production, skipping <staging>". Plan calls it for every promotion it builds, so the dry run,
// the confirm screen and the PR body all carry it, as WarnDeployIntoProduction does for a deploy
// (§4.5). It informs and never blocks (principle 5). A deploy has no source env, so it is a no-op
// there.
func WarnSkippedStagingProduction(pl *gitops.Plan, envs config.EnvsConfig) {
	if pl == nil || pl.IsDeploy() {
		return
	}
	staging, skip := SkippedStaging(pl.SourceEnv, pl.TargetEnv, envs)
	if !skip {
		return
	}
	pl.Warnings = append(pl.Warnings, gitops.Warning{
		Code:    gitops.WarnSkippedStaging,
		Message: fmt.Sprintf("promoting %s straight to production (%s), skipping %s: the configured pipeline sends %s to %s first", pl.SourceEnv, pl.TargetEnv, staging, pl.SourceEnv, staging),
	})
}

// Preflight is StartPromotion's own early gate, called before any freshness check, claim or
// state save, and the one place a start's prerequisites are stated: the CLI calls its two halves
// (PreflightConfig, PreflightGate) around its own --confirm-direct match, so `hoist promote`,
// `hoist deploy` and the TUI refuse the same shapes in the same words (AR-M8, #201.5).
//
// Reaching engine.DirectCommitGateStep here, first, is what closes the TUI's old hole: its
// buildStartPromotion only reached the gate once Drive first ran the step — AFTER the claim and
// the initial state save. This does not make the gate any less the SOLE enforcement point
// (AGENTS.md §4.5): deleting this call cannot make a production direct commit possible, because
// StepsFor's direct step list carries the same step, checked again the moment Drive first runs
// (§8, layered checks — the deletion test). ProductionEnvs is always the settings' own,
// unfiltered, exactly as engine.StepsFor's doc comment demands.
func (s *Service) Preflight(target string, m Mode) error {
	if err := s.settings.PreflightConfig(m); err != nil {
		return err
	}
	if !m.Direct {
		return nil
	}
	return s.settings.PreflightGate(target, m)
}

// PreflightConfig checks the configuration a start needs, before anything else is asked. Every
// promotion needs repos[].github (the forge identity); a direct one needs a configured repo too.
// Refusals are *UsageError.
func (s Settings) PreflightConfig(m Mode) error {
	if !m.Direct {
		if s.Repo == nil || s.Repo.GitHub == "" {
			return &UsageError{Msg: "the selected repo has no github: owner/name configured; add repos[].github to the config file"}
		}
		return nil
	}
	// Direct mode's whole safety rests on knowing envs.production (AGENTS.md invariant 6): a
	// flags-only run has no such list, which would make DirectCommitGateStep's own
	// ProductionEnvs empty and every env look non-production by omission. Fail fast rather than
	// silently treat "unconfigured" as "safe" (AGENTS.md §8: never provide a fallback default
	// for required configuration).
	if s.Repo == nil {
		return &UsageError{Msg: "--direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)"}
	}
	// Direct mode still needs the same github: owner/name the non-direct branch above requires
	// — not for the forge (direct mode never opens one), but because engine.DeriveID hashes it
	// into the promotion's id, which names the state path, the branch and the worktree
	// directory. Two repos both configured without github: that promote the same env+digest set
	// would otherwise derive the IDENTICAL id.
	if s.Repo.GitHub == "" {
		return &UsageError{Msg: "the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)"}
	}
	return nil
}

// PreflightGate runs engine.DirectCommitGateStep for a direct start into target: a production env
// is refused outright, and an unconfirmed direct commit is blocked (m.Confirmed). It is the
// step's own Observe, not a copy of its rule.
func (s Settings) PreflightGate(target string, m Mode) error {
	gate := engine.DirectCommitGateStep{ProductionEnvs: s.ProductionEnvs(), Confirmed: m.Confirmed}
	obs, err := gate.Observe(context.Background(), &engine.PromotionState{TargetEnv: target})
	if err != nil {
		return err
	}
	if obs.Blocked != "" {
		return &engine.BlockedError{Step: engine.StepDirectGate, Reason: obs.Blocked}
	}
	return nil
}

// PairEnvs fills in a promotion's source from envs.pairs (source env -> target env), so
// `hoist plan` and `hoist promote` need only name where to promote TO (#173): --to is the required
// end, and --from is optional, defaulting to the one env envs.pairs promotes into it (the target's
// immediate ancestor). The target is never derived: a lone --from does not say where to promote,
// and filling it in would let a command line that never names an env write into it (production
// included), so it is refused, as it was before pairs were read by the CLI at all. With both given,
// or neither, from and to are returned as they are; neither names no promotion (a config with
// several pairs does not say which is meant) and the caller refuses it as it always did.
//
// Given only to, the source is the one env envs.pairs promotes into it. Anything short of exactly
// one is a *UsageError that says what the config could not decide and which flag to give, so a
// defaulting that is ambiguous is refused, never guessed (principle 5 warns, but a wrong source env
// reads the wrong digests).
func (s Settings) PairEnvs(from, to string) (string, string, error) {
	if (from == "") == (to == "") {
		return from, to, nil
	}
	if to == "" {
		return "", "", &UsageError{Msg: fmt.Sprintf("--from %s alone does not say where to promote; name the target with --to (--to alone takes its source from envs.pairs)", from)}
	}
	if s.Repo == nil {
		return "", "", &UsageError{Msg: "no repos[] entry is selected, so envs.pairs is unknown; name the source with --from"}
	}
	switch sources := s.Repo.Envs.SourcesOf(to); len(sources) {
	case 0:
		return "", "", &UsageError{Msg: fmt.Sprintf("no envs.pairs entry promotes into %s; name the source with --from", to)}
	case 1:
		return sources[0], to, nil
	default:
		return "", "", &UsageError{Msg: fmt.Sprintf("several envs.pairs entries promote into %s (%s); name the source with --from", to, strings.Join(sources, ", "))}
	}
}
