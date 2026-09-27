// Package service is the one use-case layer both cmd/hoist and the TUI call — application
// logic that used to live only in package main, duplicated (and quietly diverging, AGENTS.md
// §4's Divergences) between the CLI's flags-to-render path and the TUI's own wiring.
//
// It imports internal/engine, internal/config and pkg/* freely, and constructs the same
// pkg/* clients (git, forge, argo, rollout, k8s, registry) either face needs. It never
// imports internal/app or any Bubble Tea / Lip Gloss package (charm.land/...): a screen may
// import service's own value types (a later train's decision — AGENTS.md §4.8), but service
// itself must stay renderer-agnostic, or the CLI would start pulling in a terminal UI it
// never runs. internal/service/imports_test.go enforces this mechanically rather than by
// convention alone (AGENTS.md §10 meta-rule 5).
//
// This train (PR A) starts the package with the pieces every later PR needs first: Settings
// (what a run resolved from flags and config, once), Deps (constructor seams a caller injects,
// so a real invocation and a test share the same shape), Service (the lazy, memoized clients
// built from Deps), and the repo view (a single current read of the GitOps repo, from the
// operator's own clone or from a cached view of origin, replacing cmd/hoist's own
// boot-frozen equivalents).
package service
