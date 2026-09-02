package gitops

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/pkg/image"
)

// Warning codes emitted by BuildPlan.
const (
	// WarnSourceDisagrees: the source env's occurrences of one repo carry different refs.
	WarnSourceDisagrees = "source-disagrees"
	// WarnMissingInTarget: a promotable repo runs in the source env but has no occurrence in
	// the target env, so there is nothing to move.
	WarnMissingInTarget = "missing-in-target"
)

// BuildPlan plans the promotion of every promotable image repo from env src to env dst.
//
// promotable lists repo prefixes (e.g. "ghcr.io/example/"); a repo matching none is
// third-party and only reported. digests overrides the reference chosen for a repo and
// always wins; the override must be well-formed, pinned and tagged. The plan fails — rather
// than the later write — if the chosen ref is a bare tag (AGENTS.md §4.2) or a tagless
// digest (the pod imageID form; the written form is <repo>:<tag>@sha256:<digest>), or if any
// edit would touch a block scalar.
func BuildPlan(r *Repo, src, dst string, promotable []string, digests map[string]image.Ref) (Plan, error) {
	if r == nil {
		return Plan{}, errors.New("nil repo")
	}
	if src == dst {
		return Plan{}, fmt.Errorf("source and target env are both %q", src)
	}
	srcEnv, ok := r.Envs[src]
	if !ok {
		return Plan{}, fmt.Errorf("source env %q not found; envs: %s", src, strings.Join(envNames(r), ", "))
	}
	dstEnv, ok := r.Envs[dst]
	if !ok {
		return Plan{}, fmt.Errorf("target env %q not found; envs: %s", dst, strings.Join(envNames(r), ", "))
	}
	if len(promotable) == 0 {
		return Plan{}, errors.New("no promotable image prefixes given; nothing can be planned")
	}
	srcOcc := envOccurrences(srcEnv)
	dstOcc := envOccurrences(dstEnv)

	bySrc := map[string][]Occurrence{}
	var repos []string
	for _, o := range srcOcc {
		if !isPromotable(o.Ref.Repo, promotable) {
			continue
		}
		if _, seen := bySrc[o.Ref.Repo]; !seen {
			repos = append(repos, o.Ref.Repo)
		}
		bySrc[o.Ref.Repo] = append(bySrc[o.Ref.Repo], o)
	}
	sort.Strings(repos)

	plan := Plan{SourceEnv: src, TargetEnv: dst, GeneratedAt: time.Now()}
	planned := map[string]bool{}
	for _, repo := range repos {
		occ := bySrc[repo]
		chosen, reason, disagree := chooseRef(occ)
		if ov, ok := digests[repo]; ok {
			if ov.Repo == "" {
				ov.Repo = repo
			}
			if ov.Repo != repo {
				return Plan{}, fmt.Errorf("digest override for %s names a different repo %s", repo, ov.Repo)
			}
			// The override is the one ref that did not come through image.Parse, so its shape
			// is checked here (checkEditable checks again before any write).
			if err := ov.Validate(); err != nil {
				return Plan{}, fmt.Errorf("digest override for %s is malformed: %w", repo, err)
			}
			if !ov.Pinned() {
				return Plan{}, fmt.Errorf("digest override for %s is not pinned: %s", repo, ov)
			}
			chosen, reason = ov, "caller-supplied digest"
		}
		if disagree {
			plan.Warnings = append(plan.Warnings, Warning{
				Code:        WarnSourceDisagrees,
				Message:     disagreeMessage(src, repo, occ, chosen, reason),
				Occurrences: occ,
			})
		}
		if !chosen.Pinned() {
			return Plan{}, fmt.Errorf("%s: %s runs as %s in %s, a bare tag with no digest; nothing hoist writes is a bare tag (AGENTS.md §4.2) — supply a digest for this repo", repo, chosen, occ[0].Container, src)
		}
		// A pod imageID (repo@sha256:…) is pinned but tagless, and hoist writes
		// <repo>:<tag>@sha256:<digest> only. The target's existing tag is not borrowed: it
		// would then claim to describe a digest it never pointed at.
		if chosen.Tag == "" {
			return Plan{}, fmt.Errorf("%s: %s runs as %s in %s, a digest with no tag; hoist writes <repo>:<tag>@sha256:<digest> so a tag is required — supply a digest override for this repo carrying both the tag and the digest", repo, chosen, occ[0].Container, src)
		}
		planned[repo] = true
		n := 0
		for _, t := range dstOcc {
			if t.Ref.Repo != repo {
				continue
			}
			e := Edit{Occurrence: t, New: chosen}
			if err := checkEditable(&e); err != nil {
				return Plan{}, err
			}
			plan.Edits = append(plan.Edits, e)
			n++
		}
		if n == 0 {
			plan.Warnings = append(plan.Warnings, Warning{
				Code:        WarnMissingInTarget,
				Message:     fmt.Sprintf("%s runs in %s but has no occurrence in %s; nothing to move", repo, src, dst),
				Occurrences: occ,
			})
		}
	}

	seen := map[string]bool{}
	for _, t := range dstOcc {
		if planned[t.Ref.Repo] || seen[t.Ref.String()] {
			continue
		}
		seen[t.Ref.String()] = true
		plan.Untouched = append(plan.Untouched, t.Ref)
	}
	sort.Slice(plan.Untouched, func(i, j int) bool { return plan.Untouched[i].String() < plan.Untouched[j].String() })
	sortOccurrences(plan.Edits, func(e Edit) Occurrence { return e.Occurrence })
	return plan, nil
}

// chooseRef picks the reference to promote from one repo's source occurrences: the only
// reference when they agree; otherwise the unique pinned reference if exactly one distinct
// pinned reference exists; otherwise the most frequent, ties broken by first occurrence in
// path order. reason names the rule that decided; disagree is true when a choice was needed.
func chooseRef(occ []Occurrence) (chosen image.Ref, reason string, disagree bool) {
	var order []string
	counts := map[string]int{}
	refs := map[string]image.Ref{}
	for _, o := range occ {
		k := o.Ref.String()
		if _, ok := counts[k]; !ok {
			order = append(order, k)
			refs[k] = o.Ref
		}
		counts[k]++
	}
	if len(order) == 1 {
		return refs[order[0]], "all occurrences agree", false
	}
	var pinned []string
	for _, k := range order {
		if refs[k].Pinned() {
			pinned = append(pinned, k)
		}
	}
	if len(pinned) == 1 {
		return refs[pinned[0]], "the only pinned ref", true
	}
	best := order[0]
	for _, k := range order[1:] {
		if counts[k] > counts[best] {
			best = k
		}
	}
	if counts[best] > 1 {
		return refs[best], fmt.Sprintf("the most frequent ref (%d of %d)", counts[best], len(occ)), true
	}
	return refs[best], "first in path order", true
}

func disagreeMessage(env, repo string, occ []Occurrence, chosen image.Ref, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d occurrences of %s carry different refs; planning %s (%s):", env, len(occ), repo, chosen, reason)
	for _, o := range occ {
		fmt.Fprintf(&b, "\n  %s:%d %s/%s container=%s %s", o.File, o.Line, o.Kind, o.Name, o.Container, o.Ref)
	}
	return b.String()
}

func isPromotable(repo string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

func envNames(r *Repo) []string {
	names := make([]string, 0, len(r.Envs))
	for n := range r.Envs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// envOccurrences flattens an env's occurrences in path order: file, then line, then column.
func envOccurrences(env *Env) []Occurrence {
	var out []Occurrence
	for _, f := range env.Families {
		out = append(out, f.Occurrences...)
	}
	sortOccurrences(out, func(o Occurrence) Occurrence { return o })
	return out
}

func sortOccurrences[T any](items []T, key func(T) Occurrence) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := key(items[i]), key(items[j])
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
}
