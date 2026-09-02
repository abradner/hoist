# Repository Map

> Read this before touching anything boundary-sensitive: auth, tenancy, deletion/visibility,
> serialization, background delivery, routing, any external surface. Update it in the same PR as
> any change that moves an edge here. Never leave it describing a stale security boundary — a
> wrong map is worse than no map, because it is trusted.

## What belongs here — and what doesn't

This is a **boundary map, not a directory tree.** Anything an agent can answer with grep/glob in
seconds — where a symbol is defined, which files match a pattern — does *not* belong here: that
content goes stale fastest and helps least. What belongs is the knowledge that lives *between*
files and can't be reconstructed from any one of them:

- **Surfaces** — every distinct way into the system, each with its auth mechanism.
- **Trust boundaries** — for each edge: who is the caller, how is identity established, and what
  happens if they lie.
- **Cross-cutting flows** — the handful of sequences where one action touches many parts, and
  where a change to one part silently breaks an invariant held elsewhere (the classic: soft-
  deleting a record does nothing to a session that looks it up by ID unless every lookup is
  scoped).
- **Risk register** — numbered, stable IDs for known concentrations of risk, so rules, PRs, and
  code comments can cite them.

Start small and grow by incident: three accurate entries beat thirty aspirational ones. When the
map and the code disagree, the code is the truth and the map is the bug — fix it in the same
change that found the disagreement.

## Surfaces

| Surface | Path / entry point | Auth mechanism | Notes |
|---|---|---|---|
| [KEEL:FILL] | | | |

## Trust boundaries

[KEEL:FILL — one short entry per boundary edge: caller, how identity is established, failure mode
if the caller lies. If the honest answer is "we trust what they tell us," write that down — it's
the map's job to make that visible until it's fixed.]

## Cross-cutting flows

[KEEL:FILL — the 3–6 flows worth diagramming in words: request lifecycle, auth/session
establishment, the write path, deletion/visibility propagation. For each: the parts it touches in
order, and the invariant that must survive the whole traversal.]

## Risk register

| ID | Risk | Where it lives | Mitigation / status |
|---|---|---|---|
| R-001 | [KEEL:FILL] | | |
