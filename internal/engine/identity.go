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
	refs := make([]image.Ref, 0, len(plan.Edits))
	for _, e := range plan.Edits {
		refs = append(refs, e.New)
	}
	return image.PromotionID(repoFullName, plan.TargetEnv, refs)
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
