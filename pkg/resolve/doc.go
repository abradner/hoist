// Package resolve turns the source env's image occurrences into the pinned references a
// promotion writes, from what the env is running first, then its manifests, then the
// registry — never a guess (AGENTS.md §4.2, principle 3).
//
// Resolve is pure orchestration over two interfaces, k8s.Cluster and registry.Registry,
// and is tested entirely against their in-memory fakes. Its input is plain data (the
// namespace, the occurrences, the source order, the caller's overrides) and its output is
// one Resolution per image repo: the reference to write, which source supplied the
// digest, the alternatives the other sources offered, and the warnings that explain every
// disagreement. It holds no credentials and opens no connection of its own (§4.3).
//
// The rules, in the order they apply to one image repo:
//
//   - A caller override (`--digest`) wins over every source: it decides the reference. It
//     does not silence the sources — a running disagreement or a running-vs-manifest split
//     is still a warning, and the other sources' digests are still listed as alternatives.
//   - Sources are consulted in the caller's order (default pods, manifest, registry).
//     The first that yields a digest is the Source of the resolution; a later source that
//     yields a different digest is an alternative and, for pods against manifest, a
//     running-vs-manifest warning. The registry is only asked when nothing before it in
//     the order answered — it is the fallback of §4.2, not a cross-check.
//   - pods: the containers running that repo in the source namespace (k8s.Cluster decides
//     which count). One digest across them all is the answer. Several digests are a
//     running-disagrees warning naming every pod, container and digest, and a stated
//     choice: the digest the manifest pins if it is among them, else the most frequent,
//     ties to the lexically smallest. Which repo a container runs is the repo its imageID
//     names, compared through image.Canonical, so a docker.io alias matches and a mirror
//     does not.
//   - manifest: the pin the manifests carry, chosen by gitops.ChooseRef — the same rule
//     BuildPlan uses, so a repo whose occurrences disagree gets the same reference here as
//     there.
//   - registry: HEAD of the manifest's tag. For a multi-arch image that is the index
//     digest, which is what a pull by tag pins and what imageID reports (see pkg/registry).
//   - The written reference is <repo>:<tag>@<digest> with the manifest's tag. A repo whose
//     manifests carry no tag at all is left unresolved: pods cannot supply one (imageID has
//     none) and hoist never fabricates one, so BuildPlan's existing tagless refusal stands.
//   - A repo no source can answer is unresolved: its Resolution has no Ref and an
//     unresolved warning saying what each source found. It is left out of the digests map
//     so BuildPlan's own rules — refuse to write a bare tag, warn when there is nothing to
//     write — decide, unchanged.
//
// Why the order is the caller's and pods lead by default: the brief for this milestone
// states both "the running digest wins when the pods agree" and "a pinned manifest that
// disagrees with the pods defaults to the manifest". Those conflict in exactly one case
// (pods agree on X, manifest pins Y). §4.2 says the digest comes from what the source env
// is running, so pods lead by default and the manifest pin is the alternative; the order
// flag (`--digest-sources manifest,pods,registry`) gives the other reading verbatim. Either
// way the disagreement is a warning, never silent.
//
// Failure shape: an error from the cluster fails Resolve — the operator asked for the
// running digests and silently planning from the registry instead would promote a tag's
// current digest rather than what staging runs. An error from the registry is scoped to
// the one repo it was asked about (unresolved, with the registry's message), since it is
// the last source and every other repo may still resolve.
package resolve
