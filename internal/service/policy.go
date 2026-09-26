package service

import (
	"fmt"

	"github.com/abradner/hoist/internal/config"
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
