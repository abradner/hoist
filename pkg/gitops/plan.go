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
	// WarnSourceOnlyUnwritable: a promotable repo runs in the source env as a ref hoist could
	// never write (a bare tag, or a digest with no tag) and has no occurrence in the target
	// env. Nothing would be written, so the refusal is reported rather than failing the plan
	// (AGENTS.md principle 5); a digest override for the repo turns it into missing-in-target.
	WarnSourceOnlyUnwritable = "source-only-unwritable"
	// WarnProductionTarget: this plan writes into an env the operator's own config lists as
	// production. Informational and never blocking (AGENTS.md §4.5: the registry-pick path's
	// production warning "informs; it does not block") — what production actually forces is a
	// PR and an approval comment, which the engine enforces on its own.
	//
	// Constructed by the caller rather than by BuildPlan/BuildDeployPlan: which envs are
	// production is config, and pkg never imports internal. That is the same shape
	// pkg/resolve's warnings already arrive in.
	WarnProductionTarget = "production-target"
)

// BuildPlan plans the promotion of every promotable image repo from env src to env dst.
//
// promotable lists repo prefixes (e.g. "ghcr.io/example/"); a repo matching none is
// third-party and only reported. digests overrides the reference chosen for a repo and
// always wins; the override must be well-formed, pinned and tagged. The plan fails — rather
// than the later write — if the chosen ref is a bare tag (AGENTS.md §4.2) or a tagless
// digest (the pod imageID form; the written form is <repo>:<tag>@sha256:<digest>) and the
// target env has an occurrence to write; with no target occurrence the same ref is a
// WarnSourceOnlyUnwritable warning, since invariant 1 forbids writing a bare tag, not reading
// one. The plan also fails if any edit would touch a block scalar.
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
		if !IsPromotable(o.Ref.Repo, promotable) {
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
		chosen, reason, disagree := ChooseRef(occ)
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
			// An override is caller input and fails fast on its own shape, whatever the
			// target holds: the source-only relaxation below is for refs read from manifests,
			// never for a ref the caller asked hoist to write.
			if why := unwritable(ov); why != "" {
				return Plan{}, fmt.Errorf("digest override for %s is %s", repo, why)
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
		var targets []Occurrence
		for _, t := range dstOcc {
			if t.Ref.Repo == repo {
				targets = append(targets, t)
			}
		}
		if why := unwritable(chosen); why != "" {
			// The refusal guards what hoist writes (invariant 1). With nothing in the target
			// to write, a repo that exists only in the source env must not abort the plan for
			// every other repo — it is reported and skipped (issue #13).
			if len(targets) == 0 {
				plan.Warnings = append(plan.Warnings, Warning{
					Code:        WarnSourceOnlyUnwritable,
					Message:     sourceOnlyMessage(src, dst, repo, occ, chosen, why),
					Occurrences: occ,
				})
				continue
			}
			return Plan{}, fmt.Errorf("%s: %s runs as %s in %s, %s", repo, chosen, occ[0].Container, src, why)
		}
		planned[repo] = true
		for _, t := range targets {
			e := Edit{Occurrence: t, New: chosen}
			if err := checkEditable(&e); err != nil {
				return Plan{}, err
			}
			plan.Edits = append(plan.Edits, e)
		}
		if len(targets) == 0 {
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

// ChooseRef picks the reference to promote from one repo's source occurrences: the only
// reference when they agree; otherwise the unique pinned reference if exactly one distinct
// pinned reference exists; otherwise the most frequent, ties broken by first occurrence in
// path order. reason names the rule that decided; disagree is true when a choice was needed.
// It is exported so that pkg/resolve reads the manifest's own pin by the same rule
// BuildPlan plans by, rather than a second copy of it.
func ChooseRef(occ []Occurrence) (chosen image.Ref, reason string, disagree bool) {
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

// unwritable says why ref can never be written as an image scalar, or "" when it can: a bare
// tag has no digest (AGENTS.md §4.2), and a pod imageID (repo@sha256:…) is pinned but
// tagless where hoist writes <repo>:<tag>@sha256:<digest> only. The target's existing tag is
// not borrowed for the latter: it would then claim to describe a digest it never pointed at.
func unwritable(ref image.Ref) string {
	if !ref.Pinned() {
		return "a bare tag with no digest; nothing hoist writes is a bare tag (AGENTS.md §4.2) — supply a digest for this repo"
	}
	if ref.Tag == "" {
		return "a digest with no tag; hoist writes <repo>:<tag>@sha256:<digest> so a tag is required — supply a digest override for this repo carrying both the tag and the digest"
	}
	return ""
}

func disagreeMessage(env, repo string, occ []Occurrence, chosen image.Ref, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d occurrences of %s carry different refs; planning %s (%s):", env, len(occ), repo, chosen, reason)
	listOccurrences(&b, occ)
	return b.String()
}

func sourceOnlyMessage(src, dst, repo string, occ []Occurrence, chosen image.Ref, why string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s runs in %s as %s, %s; it has no occurrence in %s, so nothing would be written and the plan goes on without it:", repo, src, chosen, why, dst)
	listOccurrences(&b, occ)
	return b.String()
}

func listOccurrences(b *strings.Builder, occ []Occurrence) {
	for _, o := range occ {
		fmt.Fprintf(b, "\n  %s:%d %s/%s container=%s %s", o.File, o.Line, o.Kind, o.Name, o.Container, o.Ref)
	}
}

// MatchesPrefix reports whether repo is covered by one configured registry prefix. A
// prefix ending in "/" must literally prefix repo; one that doesn't must still be
// followed by "/" or nothing. A bare strings.HasPrefix would let "ghcr.io" match
// "ghcr.io.attacker.example/org/app" — a different host that merely shares the leading
// bytes — and hand that host the credentials scoped to ghcr.io (AGENTS.md §4.4/§4.10,
// R-002). This is the one place that decision is made; IsPromotable and every registry
// entry selection (cmd/hoist's registryEntryFor) call it rather than keeping their own
// copy of the rule.
func MatchesPrefix(repo, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(repo, prefix) {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return true
	}
	rest := repo[len(prefix):]
	return rest == "" || rest[0] == '/'
}

// IsPromotable reports whether repo matches any of prefixes.
func IsPromotable(repo string, prefixes []string) bool {
	for _, p := range prefixes {
		if MatchesPrefix(repo, p) {
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

// BuildDeployPlan plans writing one image reference into one env: every occurrence of
// ref.Repo in env is rewritten to ref. This is the "image bump" half of hoist's problem
// statement, where BuildPlan is the "promote an env pair" half.
//
// It is a sibling of BuildPlan rather than a mode of it because the two differ in where the
// reference comes from, not in what they do with it. A promotion derives the ref by reading a
// source env (ChooseRef, the disagreement warning, the source-only-unwritable case); a deploy
// is handed one outright by the operator — from the tag picker, or --image. Everything after
// "which ref" is identical, so the two share envOccurrences, checkEditable, unwritable and
// sortOccurrences, and produce the same Plan for the same engine to drive.
//
// Three differences from BuildPlan worth stating, all of them consequences of having no
// source env:
//
//   - An unwritable ref (bare tag, or digest with no tag — invariant 1) is always an error
//     here. BuildPlan can afford to downgrade it to WarnSourceOnlyUnwritable when the target
//     has nothing to write, because the repo was merely observed in the source; a deploy's
//     ref is the whole request, so there is no lesser thing to do with a bad one.
//   - A repo with no occurrence in env is an error, not WarnMissingInTarget. For a promotion
//     that repo is one of many being moved and the others still proceed; for a deploy it means
//     there is nothing to deploy into, and reporting success would be a lie.
//   - Untouched lists every other distinct ref in the env, since exactly one repo is planned.
//     A promotion's Untouched means "not part of this promotion"; a deploy's means the same
//     thing, and is the larger list.
func BuildDeployPlan(r *Repo, env string, ref image.Ref, promotable []string) (Plan, error) {
	if r == nil {
		return Plan{}, errors.New("nil repo")
	}
	e, ok := r.Envs[env]
	if !ok {
		return Plan{}, fmt.Errorf("env %q not found in the discovered repo", env)
	}
	if err := ref.Validate(); err != nil {
		return Plan{}, fmt.Errorf("image %s: %w", ref, err)
	}
	if why := unwritable(ref); why != "" {
		return Plan{}, fmt.Errorf("image %s is %s", ref, why)
	}
	if len(promotable) > 0 && !IsPromotable(ref.Repo, promotable) {
		return Plan{}, fmt.Errorf("%s is not a promotable repo; prefixes: %s", ref.Repo, strings.Join(promotable, ", "))
	}

	plan := Plan{
		Variant:     VariantDeploy,
		TargetEnv:   env,
		GeneratedAt: time.Now().UTC(),
	}
	occ := envOccurrences(e)
	seen := map[string]bool{}
	for _, o := range occ {
		if o.Ref.Repo != ref.Repo {
			if !seen[o.Ref.String()] {
				seen[o.Ref.String()] = true
				plan.Untouched = append(plan.Untouched, o.Ref)
			}
			continue
		}
		ed := Edit{Occurrence: o, New: ref}
		if err := checkEditable(&ed); err != nil {
			return Plan{}, err
		}
		plan.Edits = append(plan.Edits, ed)
	}
	if len(plan.Edits) == 0 {
		return Plan{}, fmt.Errorf("%s has no occurrence in %s; nothing to deploy into", ref.Repo, env)
	}

	sort.Slice(plan.Untouched, func(i, j int) bool { return plan.Untouched[i].String() < plan.Untouched[j].String() })
	sortOccurrences(plan.Edits, func(e Edit) Occurrence { return e.Occurrence })
	return plan, nil
}
