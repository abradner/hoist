// Package writepath holds what the plan and deploy confirm screens share about the write they
// are about to make: the Mode values their StartMsgs carry and the diff both render. It exists
// so deploy does not import plan (the two are sibling screens, AGENTS.md §4.8). It is not in
// internal/ui because RenderDiff reads the repo's files and drives pkg/gitops, which the
// shared widgets package keeps clear of; it has no terminal dependency.
package writepath

import (
	"os"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
)

// Mode is the write path a plan would use.
const (
	ModePR     = "pr"
	ModeDirect = "direct"
)

// RenderDiff renders a unified diff of every edit whose repo is ticked, reading each file
// once from disk under root and applying the same read → ApplyBytes → Verify → UnifiedDiff
// path cmd/hoist's plan --dry-run uses (main.go printPlan) — so the screen's diff is
// byte-identical to the CLI's for the same ticked set. A NoOp edit (the target already
// carries the planned ref) is skipped, exactly as printPlan skips it into "Already
// current" rather than a diff hunk.
func RenderDiff(root string, edits []gitops.Edit, ticked map[string]bool) (string, error) {
	byFile := map[string][]gitops.Edit{}
	var files []string
	for _, e := range edits {
		if !ticked[e.Ref.Repo] || e.NoOp() {
			continue
		}
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		p, err := gitops.ResolvePath(root, f)
		if err != nil {
			return "", err
		}
		before, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		after, err := gitops.ApplyBytes(before, byFile[f])
		if err != nil {
			return "", err
		}
		if err := gitops.Verify(map[string][]byte{f: before}, map[string][]byte{f: after}, byFile[f]); err != nil {
			return "", err
		}
		b.WriteString(gitops.UnifiedDiff(f, before, after))
		b.WriteString("\n")
	}
	return b.String(), nil
}
