package resolve

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
)

// Source names where a digest came from.
type Source string

// The sources. Override is never listed in an order; it is what a Resolution reports when
// the caller supplied the reference.
const (
	SourcePods     Source = "pods"
	SourceManifest Source = "manifest"
	SourceRegistry Source = "registry"
	SourceOverride Source = "override"
)

// DefaultOrder is the chain when the caller states none: what runs, what the manifest
// pins, what the registry says the tag is.
var DefaultOrder = []Source{SourcePods, SourceManifest, SourceRegistry}

// Warning codes emitted by Resolve.
const (
	// WarnRunningDisagrees: the running containers of one repo carry different digests.
	WarnRunningDisagrees = "running-disagrees"
	// WarnRunningVsManifest: the pods run one digest and the manifest pins another — a
	// rollout may be incomplete, or a bump has not synced yet.
	WarnRunningVsManifest = "running-vs-manifest"
	// WarnUnresolved: no source could supply a writable reference for the repo.
	WarnUnresolved = "unresolved"
)

// ParseOrder turns source names into an order, refusing unknown names and duplicates.
// It does not accept "none": an empty order is the caller's way to skip resolution.
func ParseOrder(names []string) ([]Source, error) {
	var out []Source
	seen := map[Source]bool{}
	for _, n := range names {
		s := Source(strings.TrimSpace(n))
		switch s {
		case SourcePods, SourceManifest, SourceRegistry:
		default:
			return nil, fmt.Errorf("unknown digest source %q (want pods, manifest or registry)", n)
		}
		if seen[s] {
			return nil, fmt.Errorf("digest source %q listed twice", n)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// Input is everything Resolve needs beyond its adaptors: plain data, so the call keeps the
// activity shape of AGENTS.md §4.3. It replaces the positional arguments the M2 brief
// sketched because the namespace to list pods in is not carried by an Occurrence and had
// to be added.
type Input struct {
	// Namespace is the source env: the namespace whose pods are read.
	Namespace string
	// Occurrences are the source env's occurrences to resolve, promotable ones only — the
	// caller filters; every repo among them gets a Resolution.
	Occurrences []gitops.Occurrence
	// Order is the sources to consult, first wins; empty resolves nothing.
	Order []Source
	// Overrides are caller-supplied references by repo (`--digest`); each wins outright.
	Overrides map[string]image.Ref
}

// Resolution is the outcome for one image repo.
type Resolution struct {
	Repo string
	// Ref is the reference to write: <repo>:<tag>@<digest>. Zero when unresolved.
	Ref image.Ref
	// Source is where Ref's digest came from; "" when unresolved.
	Source Source
	// Detail says how the source decided, for the plan's resolution section.
	Detail string
	// Alternatives are the other digests the sources offered, as references with the same
	// repo and tag, in source order then lexical order; empty when everything agreed.
	Alternatives []image.Ref
	Warnings     []gitops.Warning
}

// Resolved reports whether Ref can be written: it carries both a tag and a digest.
func (r Resolution) Resolved() bool { return r.Ref.Tag != "" && r.Ref.Digest != "" }

// Resolve resolves every repo among in.Occurrences. See the package doc for the rules.
// cluster may be nil when pods is not in the order; reg may be nil when registry is not.
func Resolve(ctx context.Context, in Input, cluster k8s.Cluster, reg registry.Registry) (map[string]Resolution, error) {
	if err := checkOrder(in.Order); err != nil {
		return nil, err
	}
	out := map[string]Resolution{}
	if len(in.Order) == 0 || len(in.Occurrences) == 0 {
		return out, nil
	}
	if in.Namespace == "" {
		return nil, errors.New("resolve: namespace is required")
	}
	wants := map[Source]bool{}
	for _, s := range in.Order {
		wants[s] = true
	}
	if wants[SourcePods] && cluster == nil {
		return nil, errors.New("resolve: the pods source needs a cluster")
	}
	if wants[SourceRegistry] && reg == nil {
		return nil, errors.New("resolve: the registry source needs a registry")
	}

	byRepo := map[string][]gitops.Occurrence{}
	var repos []string
	for _, o := range in.Occurrences {
		if _, seen := byRepo[o.Ref.Repo]; !seen {
			repos = append(repos, o.Ref.Repo)
		}
		byRepo[o.Ref.Repo] = append(byRepo[o.Ref.Repo], o)
	}
	sort.Strings(repos)

	running := map[string][]k8s.RunningImage{} // keyed by canonical repo
	if wants[SourcePods] {
		imgs, err := cluster.RunningImages(ctx, in.Namespace)
		if err != nil {
			return nil, fmt.Errorf("resolve: %w", err)
		}
		for _, ri := range imgs {
			k := image.Canonical(ri.Ref.Repo)
			running[k] = append(running[k], ri)
		}
	}

	for _, repo := range repos {
		out[repo] = resolveRepo(ctx, in, repo, byRepo[repo], running[image.Canonical(repo)], reg)
	}
	return out, nil
}

func checkOrder(order []Source) error {
	seen := map[Source]bool{}
	for _, s := range order {
		switch s {
		case SourcePods, SourceManifest, SourceRegistry:
		default:
			return fmt.Errorf("resolve: unknown digest source %q", s)
		}
		if seen[s] {
			return fmt.Errorf("resolve: digest source %q listed twice", s)
		}
		seen[s] = true
	}
	return nil
}

// resolveRepo applies the rules to one repo.
func resolveRepo(ctx context.Context, in Input, repo string, occ []gitops.Occurrence, running []k8s.RunningImage, reg registry.Registry) Resolution {
	res := Resolution{Repo: repo}
	manifest, _, _ := gitops.ChooseRef(occ)
	tag := manifest.Tag
	if tag == "" {
		for _, o := range occ {
			if o.Ref.Tag != "" {
				tag = o.Ref.Tag
				break
			}
		}
	}
	withTag := func(digest string) image.Ref { return image.Ref{Repo: repo, Tag: tag, Digest: digest} }

	var podsDigest, podsDetail string
	var podsAlternatives []string
	var podsWarnings []gitops.Warning
	if len(running) > 0 {
		podsDigest, podsDetail, podsAlternatives, podsWarnings = choosePods(in.Namespace, repo, occ, running, manifest.Digest)
	}

	if ov, ok := in.Overrides[repo]; ok {
		res.Ref, res.Source, res.Detail = ov, SourceOverride, "caller-supplied digest"
		for _, d := range append([]string{podsDigest, manifest.Digest}, podsAlternatives...) {
			if d != "" && d != ov.Digest {
				res.Alternatives = appendRef(res.Alternatives, withTag(d))
			}
		}
		return res
	}

	var chosen string
	var notes []string
	for _, s := range in.Order {
		switch s {
		case SourcePods:
			switch {
			case podsDigest == "":
				notes = append(notes, "no running pods")
			case chosen == "":
				chosen, res.Source, res.Detail = podsDigest, SourcePods, podsDetail
			case podsDigest != chosen:
				res.Alternatives = appendRef(res.Alternatives, withTag(podsDigest))
			}
		case SourceManifest:
			switch {
			case manifest.Digest == "":
				notes = append(notes, "manifest is not pinned")
			case chosen == "":
				chosen, res.Source, res.Detail = manifest.Digest, SourceManifest, "pinned in the manifest"
			case manifest.Digest != chosen:
				res.Alternatives = appendRef(res.Alternatives, withTag(manifest.Digest))
			}
		case SourceRegistry:
			if chosen != "" {
				continue
			}
			if tag == "" {
				notes = append(notes, "registry not asked: the manifest has no tag")
				continue
			}
			d, err := reg.Head(ctx, image.Ref{Repo: repo, Tag: tag})
			if err != nil {
				notes = append(notes, "registry: "+err.Error())
				continue
			}
			chosen, res.Source, res.Detail = d, SourceRegistry, "registry HEAD of tag "+tag
		}
	}
	for _, d := range podsAlternatives {
		if d != chosen {
			res.Alternatives = appendRef(res.Alternatives, withTag(d))
		}
	}
	res.Warnings = append(res.Warnings, podsWarnings...)
	if podsDigest != "" && manifest.Digest != "" && podsDigest != manifest.Digest {
		res.Warnings = append(res.Warnings, runningVsManifest(in.Namespace, repo, occ, manifest.Digest, podsDigest, res.Source))
	}
	if res.Source == SourcePods && manifest.Digest == "" {
		res.Detail += "; manifest is not pinned"
	}
	if res.Source == SourceManifest && podsDigest == "" && len(running) == 0 {
		res.Detail += "; no running pods"
	}

	switch {
	case chosen == "":
		res.Source, res.Detail = "", ""
		res.Warnings = append(res.Warnings, gitops.Warning{
			Code:        WarnUnresolved,
			Message:     fmt.Sprintf("%s: no digest for %s (%s); the plan falls back to the manifest's own reference", in.Namespace, repo, strings.Join(notes, "; ")),
			Occurrences: occ,
		})
	case tag == "":
		res.Warnings = append(res.Warnings, gitops.Warning{
			Code:        WarnUnresolved,
			Message:     fmt.Sprintf("%s: %s resolved to %s from %s but its manifests carry no tag; hoist writes <repo>:<tag>@<digest> and never fabricates a tag", in.Namespace, repo, chosen, res.Source),
			Occurrences: occ,
		})
		res.Detail += "; no tag to write"
		res.Alternatives = nil
	default:
		res.Ref = withTag(chosen)
	}
	return res
}

// choosePods decides the running digest for one repo. With one digest across every
// container it is that; otherwise the manifest's pin if the pods include it, else the
// most frequent, ties to the lexically smallest — a deterministic choice, stated in the
// warning that names every pod, container and digest. alternatives are the other digests,
// in lexical order.
func choosePods(namespace, repo string, occ []gitops.Occurrence, running []k8s.RunningImage, manifestDigest string) (digest, detail string, alternatives []string, warnings []gitops.Warning) {
	counts := map[string]int{}
	for _, ri := range running {
		counts[ri.Ref.Digest]++
	}
	digests := make([]string, 0, len(counts))
	for d := range counts {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	if len(digests) == 1 {
		return digests[0], fmt.Sprintf("%s agree", containers(len(running))), nil, nil
	}

	var reason string
	if _, ok := counts[manifestDigest]; ok {
		digest, reason = manifestDigest, "it matches the manifest pin"
	} else {
		digest = digests[0]
		for _, d := range digests[1:] {
			if counts[d] > counts[digest] {
				digest = d
			}
		}
		if counts[digest] > 1 || len(running) > len(digests) {
			reason = fmt.Sprintf("the most frequent, %d of %d", counts[digest], len(running))
		} else {
			reason = "the lexically smallest; every digest runs once"
		}
	}
	for _, d := range digests {
		if d != digest {
			alternatives = append(alternatives, d)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s of %s run %d different digests; using %s (%s):", namespace, containers(len(running)), repo, len(digests), digest, reason)
	for _, ri := range running {
		init := ""
		if ri.Init {
			init = " (init)"
		}
		fmt.Fprintf(&b, "\n  pod %s container %s%s %s", ri.Pod, ri.Container, init, ri.Ref.Digest)
	}
	detail = fmt.Sprintf("%s run %d digests; chose %s", containers(len(running)), len(digests), reason)
	return digest, detail, alternatives, []gitops.Warning{{Code: WarnRunningDisagrees, Message: b.String(), Occurrences: occ}}
}

func runningVsManifest(namespace, repo string, occ []gitops.Occurrence, manifestDigest, podsDigest string, chosen Source) gitops.Warning {
	using := "using the running digest; the manifest pin is listed as an alternative"
	if chosen == SourceManifest {
		using = "using the manifest pin; the running digest is listed as an alternative"
	}
	return gitops.Warning{
		Code: WarnRunningVsManifest,
		Message: fmt.Sprintf("%s: %s manifests pin %s but its pods run %s — a rollout may be incomplete, or a bump has not synced; %s",
			namespace, repo, manifestDigest, podsDigest, using),
		Occurrences: occ,
	}
}

func containers(n int) string {
	if n == 1 {
		return "1 running container"
	}
	return fmt.Sprintf("%d running containers", n)
}

func appendRef(refs []image.Ref, r image.Ref) []image.Ref {
	for _, have := range refs {
		if have == r {
			return refs
		}
	}
	return append(refs, r)
}

// Digests is the BuildPlan digests argument: every resolved repo's reference. Unresolved
// repos are absent, so BuildPlan's own rules decide them.
func Digests(res map[string]Resolution) map[string]image.Ref {
	out := map[string]image.Ref{}
	for repo, r := range res {
		if r.Resolved() {
			out[repo] = r.Ref
		}
	}
	return out
}

// Warnings collects every resolution's warnings in repo order.
func Warnings(res map[string]Resolution) []gitops.Warning {
	var out []gitops.Warning
	for _, repo := range Repos(res) {
		out = append(out, res[repo].Warnings...)
	}
	return out
}

// Repos lists the resolved repos in order.
func Repos(res map[string]Resolution) []string {
	repos := make([]string, 0, len(res))
	for repo := range res {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}
