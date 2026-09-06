// Package tags is the tag-picker screen: given one image repo, it lists the registry's own
// tags and, once each row's metadata loads, its created time and digest — sorted per
// AGENTS.md's M6 brief invariant 3 (prefer the app repo's own git tags for ordering when the
// image repo is mapped; fall back to the registry's own Created metadata otherwise). It
// follows internal/app/plan's package shape (AGENTS.md §4.8): rows.go is the pure derived-
// data file with no terminal dependency, model.go lays it out with a huh.Confirm for the
// direct-mode gesture and a bubbles/v2 spinner for the per-row loading state.
package tags

import (
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/registry"
)

// Row is one tag's display data. GitDate/HasGitDate are known immediately (DeriveRows);
// Meta/MetaLoaded/MetaErr are filled in lazily as Model fetches them (model.go) — a row
// never blocks the picker's own opening on its own Config call (AGENTS.md invariant 4).
type Row struct {
	Tag        string
	HasGitDate bool
	GitDate    time.Time

	Meta        registry.ImageMeta
	MetaLoaded  bool
	MetaLoading bool
	MetaErr     error
}

// Revision is always "—": AGENTS.md's M6 brief scopes pkg/migrate's resolution out of this
// milestone entirely ("leave it blank/'—' for now, don't build pkg/migrate here"). A column
// exists so the screen's shape doesn't change again the milestone pkg/migrate lands.
const Revision = "—"

// DeriveRows orders regTags per AGENTS.md invariant 3. mapped is whether RepoConfig.Apps has
// an entry for this image repo at all — a repo-level, config-known fact, never inferred from
// the tag list itself. When mapped, gitTags supplies the app repo's own tag→commit-date pairs
// (pkg/forge.Forge.Tags): a registry tag whose name matches one of them sorts by that date,
// newest first, ahead of every tag that doesn't (invariant 3: "prefer... over registry's
// Created metadata", read as "instead of", not "merged with" — a mapped repo's unmatched tags
// are never silently sorted by their own registry Created, which View's own divider makes
// visible rather than letting the two orderings blend indistinguishably). The unmatched tags
// keep regTags' own incoming order (pkg/registry.Client.Tags already returns them
// alphabetically) and are listed after — "unordered, clearly marked" is the divider View
// renders between the two groups, not a claim this function encodes as data.
//
// When mapped is false, gitTags is ignored (nil in real wiring) and every row is unordered
// here — sorting by Created is Reorder's job once each row's MetaFunc resolves, since an
// unmapped repo's ordering fact doesn't exist until then (this function never blocks on a
// network call).
func DeriveRows(regTags []string, gitTags []forge.GitTag, mapped bool) []Row {
	dates := make(map[string]time.Time, len(gitTags))
	if mapped {
		for _, gt := range gitTags {
			dates[gt.Name] = gt.Date
		}
	}
	rows := make([]Row, 0, len(regTags))
	for _, t := range regTags {
		if d, ok := dates[t]; ok {
			rows = append(rows, Row{Tag: t, HasGitDate: true, GitDate: d})
		} else {
			rows = append(rows, Row{Tag: t})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].HasGitDate != rows[j].HasGitDate {
			return rows[i].HasGitDate
		}
		if rows[i].HasGitDate {
			return rows[i].GitDate.After(rows[j].GitDate)
		}
		return false // preserve incoming order among the unordered rest
	})
	return rows
}

// Reorder re-sorts rows by each one's own Created metadata, descending, for the "no app repo
// mapping" case only (AGENTS.md invariant 3's fallback) — call it after a row's MetaLoaded
// flips true when mapped is false; it is a no-op when mapped is true, since a mapped repo's
// ordering is fixed once, at DeriveRows, and a matched git-tag row is never re-ranked by its
// own registry Created once loaded (that would silently blend the two orderings invariant 3
// keeps separate). A row not yet loaded keeps its current relative position, appended after
// every loaded row, so a row already visible under the cursor never jumps out from under it
// mid-fetch.
//
// This sort is only ever complete among rows that have actually loaded (finding 4, round 5):
// fetchVisible (model.go) only fetches metadata for whatever window is currently visible, so a
// genuinely newer tag sitting outside every window the cursor has visited yet is never fetched
// at all, and can never be sorted ahead of what's already loaded — Reorder has no way to rank a
// row it has no Created value for. model.go's viewReady renders a count of rows outside the
// current window that are still unevaluated for exactly this reason, rather than let the
// unmapped case's ordering look complete when it may not be.
func Reorder(rows []Row, mapped bool) []Row {
	if mapped {
		return rows
	}
	out := append([]Row(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MetaLoaded != out[j].MetaLoaded {
			return out[i].MetaLoaded
		}
		if out[i].MetaLoaded {
			return out[i].Meta.Created.After(out[j].Meta.Created)
		}
		return false
	})
	return out
}

// Filter narrows rows to those whose Tag contains query, case-insensitively. query == ""
// returns rows unchanged.
func Filter(rows []Row, query string) []Row {
	if query == "" {
		return rows
	}
	q := strings.ToLower(query)
	var out []Row
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Tag), q) {
			out = append(out, r)
		}
	}
	return out
}

// IndexOf finds tag's position in rows, or -1. Used to keep the cursor on the same tag across
// a Reorder or a filter change, rather than trusting a stored index that a reorder could have
// invalidated.
func IndexOf(rows []Row, tag string) int {
	for i, r := range rows {
		if r.Tag == tag {
			return i
		}
	}
	return -1
}

// ShortDigest renders the first 12 hex characters after "sha256:" — enough to be unambiguous
// in this table, short enough to sit next to created/tag/revision.
func ShortDigest(digest string) string {
	const n = 12
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > n {
		d = d[:n]
	}
	return d
}

// StagingMismatch reports the paired staging env's own committed-manifest tag(s) for
// imageRepo — read from repo, the gitops repository's own parsed occurrences, never any live
// cluster or Argo state (this package has no k8s/Argo read wired in at all — AGENTS.md §4.8
// keeps that connection, like every other, at cmd/hoist's layer, and none of it reaches
// here). Finding 5, round 2: an earlier revision of this doc comment, and the text the caller
// (model.go's viewReady) rendered from this result, both called this "currently running" — a
// false claim about live state whenever Argo hasn't synced yet, a rollout is incomplete, or
// the live workload otherwise differs from what's committed; both are corrected to say
// "committed manifest tag" instead, matching what's actually being compared here. AGENTS.md
// invariant 4 / principle 5: informs, never blocks (mirroring plan.SkippedStaging's shape for
// the plan screen's own production-skip warning). ok is false when target is not a production
// env, has no paired staging env (envs.pairs has no source env whose value is target — pairs
// maps source env -> target env, so the staging env is target's own key there), that env has
// no occurrence of imageRepo, or none of its occurrences carry a tag to compare (every one a
// bare digest pin, nothing to differ from).
//
// Round-N findings (Copilot, two; Codex, one — all three against the same nondeterminism):
// envs.Pairs and Env.Families are both maps, and this function used to range over each one
// directly and return on the first match, so (1) a config naming more than one source env
// paired to the same production target (never rejected by config.Validate) picked a
// different "the" staging env from run to run, and (2) a staging env genuinely carrying more
// than one distinct tag for imageRepo across its families/occurrences (or within one family)
// returned an arbitrary one of them, silently, run to run — hiding a real disagreement
// exactly like the one gitops.BuildPlan's own ChooseRef/WarnSourceDisagrees already treats as
// worth surfacing for the analogous case within one env's SOURCE occurrences, never blocking,
// only warning. Both are fixed the same way BuildPlan fixes its own version of this: collect
// every candidate first, then decide deterministically — sort.Strings before picking the
// first source env for (1); collect the full distinct, sorted set of tags for (2), so the
// caller (model.go's viewReady) can render either the single agreed tag or an explicit
// disagreement across every distinct value, never an arbitrary pick that hides the others.
func StagingMismatch(repo *gitops.Repo, imageRepo, target string, envs config.EnvsConfig) (stagingEnv string, stagingTags []string, ok bool) {
	if !envs.IsProduction(target) || repo == nil {
		return "", nil, false
	}
	var candidates []string
	for src, dst := range envs.Pairs {
		if dst == target {
			candidates = append(candidates, src)
		}
	}
	if len(candidates) == 0 {
		return "", nil, false
	}
	sort.Strings(candidates)
	stagingEnv = candidates[0]

	env, ok2 := repo.Envs[stagingEnv]
	if !ok2 {
		return "", nil, false
	}
	seen := map[string]bool{}
	for _, f := range env.Families {
		for _, o := range f.Occurrences {
			if o.Ref.Repo != imageRepo || o.Ref.Tag == "" {
				continue
			}
			if !seen[o.Ref.Tag] {
				seen[o.Ref.Tag] = true
				stagingTags = append(stagingTags, o.Ref.Tag)
			}
		}
	}
	if len(stagingTags) == 0 {
		return "", nil, false
	}
	// The final sort is what makes the result independent of Env.Families' own map iteration
	// order too — collecting the full distinct set first, then sorting once, needs no
	// separate deterministic-iteration-order fix on the way in.
	sort.Strings(stagingTags)
	return stagingEnv, stagingTags, true
}
