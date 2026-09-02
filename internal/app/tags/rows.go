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

// StagingMismatch reports the paired staging env's own currently-running tag for imageRepo —
// AGENTS.md invariant 4 / principle 5: informs, never blocks (mirroring plan.SkippedStaging's
// shape for the plan screen's own production-skip warning). ok is false when target is not a
// production env, has no paired staging env (envs.pairs has no source env whose value is
// target — pairs maps source env -> target env, so the staging env is target's own key
// there), that env has no occurrence of imageRepo, or its occurrence carries no tag to compare
// (a bare digest pin, nothing to differ from).
func StagingMismatch(repo *gitops.Repo, imageRepo, target string, envs config.EnvsConfig) (stagingEnv, stagingTag string, ok bool) {
	isProd := false
	for _, p := range envs.Production {
		if p == target {
			isProd = true
			break
		}
	}
	if !isProd || repo == nil {
		return "", "", false
	}
	for src, dst := range envs.Pairs {
		if dst == target {
			stagingEnv = src
			break
		}
	}
	if stagingEnv == "" {
		return "", "", false
	}
	env, ok2 := repo.Envs[stagingEnv]
	if !ok2 {
		return "", "", false
	}
	for _, f := range env.Families {
		for _, o := range f.Occurrences {
			if o.Ref.Repo == imageRepo {
				if o.Ref.Tag == "" {
					return "", "", false
				}
				return stagingEnv, o.Ref.Tag, true
			}
		}
	}
	return "", "", false
}
