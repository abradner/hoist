package engine

import (
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// DeriveID computes a promotion's deterministic identity (AGENTS.md §4.1): the hash of
// (repoFullName, plan.TargetEnv, the plan's edits' new references). It reuses image.
// PromotionID unchanged — this milestone does not reimplement or vary the hash — passing
// every edit's New ref, duplicates included: PromotionID's own dedup step (by repo@digest)
// is what the fixed-vector tests in pkg/image freeze, and calling it with the plan's edits
// as they come is what proves this package used that function rather than a parallel one.
func DeriveID(repoFullName string, plan gitops.Plan) string {
	if plan.IsRestart() {
		// A restart changes no image, so the ref set every other variant is identified by is
		// empty — and every restart of one env would derive the SAME id, which is exactly
		// backwards. Re-running a promotion should resume it, because a promotion's identity
		// is the end state it lands and re-running names the same end state. Re-running a
		// restart should restart again: that is what the operator asked for, and two restarts
		// of the same Deployment are two different events.
		//
		// The timestamp being written is the entire content of the change, so it is what
		// distinguishes them. It is folded into the target-env argument rather than added as a
		// fourth parameter because image.PromotionID's output is frozen by a fixed-vector test
		// in pkg/image; this calls it unchanged. The NUL separator cannot occur in an env name,
		// so a restart's id can never collide with a promotion's into an oddly-named env.
		return image.PromotionID(repoFullName, plan.TargetEnv+"\x00restart="+restartStamp(plan), nil)
	}
	refs := make([]image.Ref, 0, len(plan.Edits))
	for _, e := range plan.Edits {
		refs = append(refs, e.New)
	}
	return image.PromotionID(repoFullName, plan.TargetEnv, refs)
}

// restartStamp is the one timestamp a restart plan writes. BuildRestartPlan stamps every
// RestartEdit in a plan from the same instant, so the first is the plan's.
func restartStamp(plan gitops.Plan) string {
	if len(plan.Restarts) == 0 {
		return ""
	}
	return plan.Restarts[0].New
}

// BranchName is the deterministic branch name a promotion's id names (AGENTS.md §4.1):
// hoist/<targetEnv>/<id>.
func BranchName(targetEnv, id string) string {
	return "hoist/" + targetEnv + "/" + id
}

// Marker is the PR body's identity line, verbatim: a PR is findable by searching for exactly
// this string even if its branch was renamed or recreated (AGENTS.md §4.1, invariant 5). It
// must be the first line of the rendered body — RenderPRBody enforces that.
func Marker(id string) string {
	return "<!-- hoist:id=" + id + " -->"
}

// CommitTrailer is the commit trailer line naming the promotion, verbatim, on its own line at
// the end of the commit message.
func CommitTrailer(id string) string {
	return "hoist-id: " + id
}
