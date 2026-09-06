// Package migrate answers the question the confirm screens lead with: between the build an
// env is running and the build about to be written into it, which commits ship, and does any
// of them migrate the database (R-005 in docs/repo-map.md — on the target repo every
// Application auto-syncs and the app entrypoint runs db:prepare, so a merge can migrate
// production as a side effect).
//
// Three activity-shaped pieces (AGENTS.md §4.3: func(ctx, In) (Out, error), no secrets, no
// internal/ imports), each over a forge.Forge bound to the *app* repo, never the gitops one:
//
//   - Resolver turns an image reference into a git revision: the OCI revision label the
//     build stamped, else a git tag named like the image tag, else a "sha-<prefix>" tag, else
//     unknown. It reports which source answered, because a tag is mutable and a label is not,
//     and the screen should say which it trusted.
//   - Comparer lists the commits between two revisions and attributes migration files to the
//     commits that added them, under a per-app path prefix (MigrationsPath decides the prefix:
//     the app repo's own .hoist.yaml, else config, else db/migrate/).
//   - Blamer dates one manifest line: how long the reference being replaced has been live.
//
// Everything here informs and nothing blocks (AGENTS.md principle 5): the gitops repo's runbook
// owns the rule that a promotion is not bundled with a manual migration; this package makes
// the delta visible. A delta that cannot be computed is a typed ErrUnresolved with a reason,
// never an empty list that reads as "no commits".
package migrate
