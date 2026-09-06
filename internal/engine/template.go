package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
)

// RenderPRBody renders the pull request body for plan, identified by id. AGENTS.md invariant
// 5 requires the marker to be the body's exact first line; invariant 6 requires the result to
// carry nothing scripts/public-safety.sh would flag — a table of image/from/to, occurrences
// updated, target-only images left, and the plan's own warnings, all values gitops.Plan and
// pkg/image already produce for `hoist plan`'s dry-run output, nothing new. template_test.go
// asserts the public-safety patterns never appear in the rendered result.
func RenderPRBody(id string, plan gitops.Plan) string {
	var b strings.Builder
	fmt.Fprintln(&b, Marker(id))
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "%s\n\n", lede(plan))

	rows := editRows(plan.Edits)
	if len(rows) > 0 {
		fmt.Fprintln(&b, "| image | from | to | occurrences |")
		fmt.Fprintln(&b, "|---|---|---|---|")
		for _, r := range rows {
			fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", r.repo, r.from, r.to, r.count)
		}
		fmt.Fprintln(&b)
	}

	if len(plan.Untouched) > 0 {
		fmt.Fprintf(&b, "Untouched (not part of this %s):\n", noun(plan))
		for _, ref := range plan.Untouched {
			fmt.Fprintf(&b, "- %s\n", ref)
		}
		fmt.Fprintln(&b)
	}

	if len(plan.Warnings) > 0 {
		fmt.Fprintf(&b, "Warnings (%d):\n", len(plan.Warnings))
		for _, w := range plan.Warnings {
			fmt.Fprintf(&b, "- [%s] %s\n", w.Code, strings.ReplaceAll(w.Message, "\n", "\n  "))
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "%s id: `%s`\n", strings.ToUpper(noun(plan)[:1])+noun(plan)[1:], id)
	return b.String()
}

// noun is what this plan calls itself in rendered prose — "deploy" or "promotion". A deploy
// has no source env, so every sentence that would have read "A -> B" has to be re-worded
// rather than left with an empty side (AGENTS.md principle 1: a rendered artifact that
// misdescribes what happened is a bug, and the PR body is the thing a reader trusts six
// months later).
func noun(plan gitops.Plan) string {
	if plan.IsDeploy() {
		return "deploy"
	}
	return "promotion"
}

// lede is the PR body's opening sentence.
func lede(plan gitops.Plan) string {
	if plan.IsDeploy() {
		return fmt.Sprintf("hoist deploys into `%s`.", plan.TargetEnv)
	}
	return fmt.Sprintf("hoist promotes `%s` -> `%s`.", plan.SourceEnv, plan.TargetEnv)
}

// PRTitle renders the PR title for plan.
func PRTitle(plan gitops.Plan) string {
	rows := editRows(plan.Edits)
	if plan.IsDeploy() {
		return fmt.Sprintf("hoist: deploy %d image(s) to %s", len(rows), plan.TargetEnv)
	}
	return fmt.Sprintf("hoist: promote %d image(s) %s -> %s", len(rows), plan.SourceEnv, plan.TargetEnv)
}

// RenderCommitMessage renders the commit message for plan, identified by id: a summary, then
// the hoist-id trailer on its own line at the end (AGENTS.md invariant 5).
func RenderCommitMessage(id string, plan gitops.Plan) string {
	var b strings.Builder
	if plan.IsDeploy() {
		fmt.Fprintf(&b, "hoist: deploy to %s\n\n", plan.TargetEnv)
	} else {
		fmt.Fprintf(&b, "hoist: promote %s -> %s\n\n", plan.SourceEnv, plan.TargetEnv)
	}
	for _, r := range editRows(plan.Edits) {
		fmt.Fprintf(&b, "- %s: %s -> %s (%d occurrence(s))\n", r.repo, r.from, r.to, r.count)
	}
	fmt.Fprintf(&b, "\n%s\n", CommitTrailer(id))
	return b.String()
}

type editRow struct {
	repo, from, to string
	count          int
}

// editRows groups plan's edits by image repo, in sorted-repo order, for the PR body table and
// the commit message summary: one row per repo, "from" the occurrence's old ref, "to" the
// planned new ref, and how many occurrences change. A no-op edit (Ref == New) still counts
// toward "occurrences" — a reviewer should see the promotion covered every occurrence, not
// just the ones that moved — but rows where every occurrence is a no-op are omitted, since
// there is nothing to show as "from -> to" for a repo nothing actually changes.
func editRows(edits []gitops.Edit) []editRow {
	type agg struct {
		from, to string
		count    int
		changed  bool
	}
	byRepo := map[string]*agg{}
	var repos []string
	for _, e := range edits {
		a, ok := byRepo[e.New.Repo]
		if !ok {
			a = &agg{from: e.Ref.String(), to: e.New.String()}
			byRepo[e.New.Repo] = a
			repos = append(repos, e.New.Repo)
		}
		a.count++
		if !e.NoOp() {
			a.changed = true
		}
	}
	sort.Strings(repos)
	rows := make([]editRow, 0, len(repos))
	for _, repo := range repos {
		a := byRepo[repo]
		if !a.changed {
			continue
		}
		rows = append(rows, editRow{repo: repo, from: a.from, to: a.to, count: a.count})
	}
	return rows
}
