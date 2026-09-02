// Package gitops reads an Argo CD GitOps repository and plans, applies and verifies
// byte-minimal image promotions between its environments.
//
// The pipeline is Discover → BuildPlan → Apply, with Verify run on every write:
//
//   - Discover reads every kind: Application under <apps-root>/*.yaml. The wrapper's
//     spec.source.path names a family directory and spec.destination.namespace names the
//     env — never the file or directory name. Every YAML file in a family directory is
//     scanned for image: scalars sitting inside items of a containers, initContainers or
//     ephemeralContainers sequence; an image key anywhere else (ConfigMap data, a CRD
//     field) is not an occurrence. Directories with manifests but no wrapper are reported
//     as unmanaged and never scanned.
//   - BuildPlan produces one Edit per occurrence of each promotable source-env image repo in
//     the target env. Every edit writes a pinned <repo>:<tag>@sha256:<digest>; a plan that
//     would write a bare tag fails to build (AGENTS.md §4.2). Disagreement between source
//     occurrences is a Warning with a deterministic choice, never a silent guess.
//   - ApplyBytes replaces only the scalar text at the recorded line and column, preserving
//     quoting, comments and line count. Apply writes files only after Verify accepts the
//     result.
//   - Verify compares every unedited line byte-for-byte, reconstructs each edited line, and
//     walks the yaml.v3 node trees of both versions in lockstep — comments included — so a
//     changed comment, a reordered key or a scalar change at an unplanned path is rejected.
//
// Nothing in this package runs git, talks to a cluster or registry, or reads config: it is
// activity-shaped (AGENTS.md §4.3) and takes everything it needs as arguments.
package gitops
