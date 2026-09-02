package plan

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/resolve"
)

// Row is one image repo in the left pane, derived from a built Plan and (when resolution
// ran) its Resolutions. It carries no terminal dependency so it is unit-testable as plain
// values (AGENTS.md §4.8); model.go lays it out and model.go alone talks to huh.
type Row struct {
	Repo string
	// Old and New are the references shown as "old-tag → new-tag"; Old is the first target
	// occurrence's existing ref (occurrences of one repo in one env always share a tag in
	// practice, and a disagreement is its own warning), New is what the plan would write.
	Old, New image.Ref
	// Count is the number of target-env occurrences this repo's edits touch.
	Count int
	// Source names where the digest came from: a resolve.Source ("pods", "manifest",
	// "registry", "override"), "manifest" when no resolution ran at all (digest sources:
	// none, exactly as M1 planned), or "" only for a disabled row.
	Source string
	// Warnings are every warning (resolution or plan) attached to this repo; a non-empty
	// list draws the "!" marker on the row.
	Warnings []gitops.Warning
	// Disabled is true when pkg/resolve could not supply a digest for this repo at all
	// (WarnUnresolved): the row is shown but never offered as a tickable option, since huh
	// has no per-option disable and the row's own reference may still be a stale manifest
	// pin no one has confirmed against the running env.
	Disabled bool
	// Reason explains Disabled; "" otherwise.
	Reason string
}

// Label is the row text: "repo  old → new  (n occurrences)  [source]", with a leading "!"
// when the row carries warnings.
func (r Row) Label() string {
	marker := ""
	if len(r.Warnings) > 0 {
		marker = "! "
	}
	plural := "s"
	if r.Count == 1 {
		plural = ""
	}
	return fmt.Sprintf("%s%s  %s → %s  (%d occurrence%s)  [%s]",
		marker, r.Repo, tagOrDigest(r.Old), tagOrDigest(r.New), r.Count, plural, r.Source)
}

// tagOrDigest is the tag, or a shortened digest for a tag-less reference — the same
// convention internal/app/matrix/cells.go uses for a matrix cell.
func tagOrDigest(r image.Ref) string {
	if r.Tag != "" {
		return r.Tag
	}
	const short = len("sha256:") + 12
	if len(r.Digest) > short {
		return r.Digest[:short]
	}
	return r.Digest
}

// DeriveRows groups pl.Edits by image repo into one Row each, sorted by repo. res is the
// resolve.Resolve output for the source env; a nil or empty map means no resolution ran
// (digest sources: none) and every row's Source reads "manifest", matching what BuildPlan
// alone would have written in M1.
func DeriveRows(pl gitops.Plan, res map[string]resolve.Resolution) []Row {
	byRepo := map[string][]gitops.Edit{}
	var repos []string
	for _, e := range pl.Edits {
		repo := e.Ref.Repo
		if _, ok := byRepo[repo]; !ok {
			repos = append(repos, repo)
		}
		byRepo[repo] = append(byRepo[repo], e)
	}
	sort.Strings(repos)

	warningsByRepo := map[string][]gitops.Warning{}
	for _, w := range pl.Warnings {
		if repo := warningRepo(w); repo != "" {
			warningsByRepo[repo] = append(warningsByRepo[repo], w)
		}
	}

	rows := make([]Row, 0, len(repos))
	for _, repo := range repos {
		edits := byRepo[repo]
		row := Row{
			Repo:     repo,
			Old:      edits[0].Ref,
			New:      edits[0].New,
			Count:    len(edits),
			Source:   "manifest",
			Warnings: warningsByRepo[repo],
		}
		if r, ok := res[repo]; ok {
			if r.Resolved() {
				row.Source = string(r.Source)
			} else {
				row.Disabled = true
				row.Source = "unresolved"
				row.Reason = unresolvedReason(r, warningsByRepo[repo])
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// unresolvedReason picks the WarnUnresolved message for a disabled row, else a generic
// fallback — Resolve always attaches one when Resolution.Resolved() is false, so the
// fallback is defensive rather than expected.
func unresolvedReason(r resolve.Resolution, warnings []gitops.Warning) string {
	for _, w := range warnings {
		if w.Code == resolve.WarnUnresolved {
			return w.Message
		}
	}
	if r.Detail != "" {
		return r.Detail
	}
	return "no digest source resolved this repo"
}

// warningRepo is the repo a warning is about: every Warning built by pkg/gitops and
// pkg/resolve carries Occurrences for exactly one repo (never a mix), so the first
// occurrence's ref names it; "" when a warning carries no occurrence at all.
func warningRepo(w gitops.Warning) string {
	if len(w.Occurrences) == 0 {
		return ""
	}
	return w.Occurrences[0].Ref.Repo
}

// Selectable is the rows huh.MultiSelect offers a checkbox for.
func Selectable(rows []Row) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if !r.Disabled {
			out = append(out, r)
		}
	}
	return out
}

// Disabled is the rows shown without a checkbox, each with its reason.
func Disabled(rows []Row) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		if r.Disabled {
			out = append(out, r)
		}
	}
	return out
}

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

// Summary is the resolution section: which cluster context and registry credential source
// were consulted (by name only, AGENTS.md §4.4), then each resolved repo's source and
// detail, in repo order. It renders the same facts cmd/hoist's resolutionReport.print does.
func Summary(o ResolveOutcome) []string {
	lines := make([]string, 0, len(o.Resolutions)+2)
	if o.KubeContext != "" {
		lines = append(lines, "kube context "+o.KubeContext)
	} else {
		lines = append(lines, "cluster not consulted")
	}
	if o.RegistryAuth != "" {
		lines = append(lines, "registry auth: "+o.RegistryAuth)
	} else {
		lines = append(lines, "registry not consulted")
	}
	for _, repo := range resolve.Repos(o.Resolutions) {
		r := o.Resolutions[repo]
		if !r.Resolved() {
			lines = append(lines, fmt.Sprintf("  %s  unresolved", repo))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s  [%s] %s", r.Ref, r.Source, redact.Strings(r.Detail)))
	}
	return lines
}

// TargetsFor lists the candidate target envs for source: every other discovered env,
// sorted. It backs the huh.Select shown when the current env has no configured pair.
func TargetsFor(r *gitops.Repo, source string) []string {
	out := make([]string, 0, len(r.Envs))
	for name := range r.Envs {
		if name != source {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// IsProduction reports whether env is listed in the repo's envs.production (AGENTS.md
// §4.5): direct mode is never offered for it, whatever the source env.
func IsProduction(env string, envs config.EnvsConfig) bool {
	for _, p := range envs.Production {
		if p == env {
			return true
		}
	}
	return false
}

// SkippedStaging reports the configured staging env for source when target is production
// but is not that configured pair — the "deploying straight to production, skipping
// <staging>" warning (AGENTS.md §4.5). It never blocks (principle 5); skip is false when
// target is not production, or is exactly the configured pair, or source has no configured
// pair to skip.
func SkippedStaging(source, target string, envs config.EnvsConfig) (staging string, skip bool) {
	if !IsProduction(target, envs) {
		return "", false
	}
	staging, ok := envs.Pairs[source]
	if !ok || staging == "" || staging == target {
		return "", false
	}
	return staging, true
}
