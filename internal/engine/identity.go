package engine

import (
	"fmt"
	"sort"
	"strings"

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
		return image.PromotionID(repoFullName, plan.TargetEnv+"\x00restart="+restartDiscriminator(plan), nil)
	}
	refs := make([]image.Ref, 0, len(plan.Edits))
	for _, e := range plan.Edits {
		refs = append(refs, e.New)
	}
	return image.PromotionID(repoFullName, plan.TargetEnv, refs)
}

// restartDiscriminator is what makes one restart a different promotion from another: the
// timestamp it writes, plus the set of workloads it writes to.
//
// The timestamp alone is not enough. It has second resolution, so two invocations in the same
// second with different --family selections would derive the same id — and the second would then
// find the first's state file, skip it as its own, and collide with its branch instead of being
// refused as a different operation (Copilot, PR #79). Including the targets makes two restarts
// the same promotion only when they would write the same thing to the same places at the same
// instant, which is exactly when re-running should resume rather than start again.
//
// Targets are identified by file and document index rather than by name: that pair is what the
// plan actually writes to, and it stays stable across a rename that leaves the manifest in place.
func restartDiscriminator(plan gitops.Plan) string {
	if len(plan.Restarts) == 0 {
		return ""
	}
	targets := make([]string, 0, len(plan.Restarts))
	for _, r := range plan.Restarts {
		targets = append(targets, fmt.Sprintf("%s#%d", r.File, r.Doc))
	}
	sort.Strings(targets)
	return plan.Restarts[0].New + "\x00" + strings.Join(targets, "\x00")
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
