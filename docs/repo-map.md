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

Every one of these is a way *out* of the process as much as a way in: hoist has no listener, but
it reads private state and writes to systems that deploy software.

| Surface | Path / entry point | Auth mechanism | Notes |
|---|---|---|---|
| TUI / CLI | `cmd/hoist` | Local user; whoever runs the binary | The only human input. Keypresses in the plan/confirm and flight screens are the authorisation for everything below. |
| Config file | `internal/config` (`$XDG_CONFIG_HOME/hoist/config.yaml`, `--config`) | Local user's file | Read once, never exec'd, no network. Names repos, envs, approvers, credential *sources* (env, keychain, cluster secret, `op` ref) — never credential values. `config show` redacts the `op` refs. |
| GitOps repo (read + edit) | `pkg/gitops`, `pkg/git` | The user's git checkout and SSH/signing config, via `exec git` | Edits are byte-minimal image-scalar rewrites on a worktree, never the user's checkout. |
| GitHub API | `pkg/forge/github` | `gh` auth token via go-gh | Creates branches/PRs, reads checks/reviews/comments, **merges**, and (M6) lists a repo's git tags with commit dates for the tag picker's ordering (`Tags`) — read-only, bounded to `maxTagPages`. Merge = deploy on an auto-sync repo. |
| Container registry | `pkg/registry` | Credential chain, in the caller's order: env token → docker keychain → cluster pull secret → `op`, then anonymous | Read-only (manifest HEAD, tag list, and — M6 — per-digest config-blob metadata: `Created`, `Labels`). Credentials never leave the adaptor; errors carry status and error codes, never the response body; the winning link is reported by name (`AuthSourceUsed`), and whether the registry was asked at all when nothing won (`Consulted`) — so "not consulted" and "consulted, every source failed" are never confused in the plan output. `op` is executed only when an `op` ref is configured, and links are built one at a time in order, never all up front (§9). `Config`'s per-digest cache under `$XDG_CACHE_HOME/hoist/registry/` holds no credential — digest, `Created`, `Labels` only — and a corrupt or unreadable entry is always a cache miss, never a hard error. |
| Kubernetes API | `pkg/k8s`, `pkg/argo`, `pkg/rollout` | kubeconfig context named in hoist config or `--kube-context` | Reads pods in exactly the source namespace and one named pull secret (`pkg/k8s`, M2); reads and refreshes Argo `Application` CRs in `kube.argo_namespace` (default `argocd` — the CR's own control-plane namespace, distinct from `spec.destination.namespace`/the target env; `pkg/argo`, M5) and reads Deployment/Job/CronJob workload state in the target env's own namespace (`pkg/rollout`, M5). The refresh annotation (`argocd.argoproj.io/refresh: normal`) is the *only* write anywhere in this row — `hoist watch` and every read path touch nothing. Every client error goes through `pkg/redact`, which scrubs the API server's URL, its bare `host:port`, and the host alone (an untyped TLS error can name just the host, with no port to strip); only the context name may reach output. |

## Trust boundaries

- **Approval comment → production merge.** Caller: whoever can comment on the PR (anyone, on a
  public repo). Identity: the GitHub login on the comment (`ApprovedStep`, `internal/engine`),
  checked against the configured `approvers` list and/or a write-or-higher (write, maintain, or
  admin) collaborator permission via `Forge.IsAllowedAuthor` when `RepoConfig.Collaborators` opts
  a repo into that; only `AuthorType == "Bot"` is excluded — `Organization` is a real, non-bot
  account type and is not filtered (corrected from an earlier, stricter statement here that would
  have silently dropped a legitimate org-owned commenter); the comment must post-date the head
  commit's own committer date (`git.Git.CommitTime`, not `PR.CreatedAt` — corrected from an
  earlier, weaker statement here: `PR.CreatedAt` would happen to be a safe anchor too, given this
  repo's ordering/exclusivity invariants, but `ApprovedStep` anchors on the commit's own real time
  directly rather than leaning on those other steps to make an earlier PR unreachable — see
  `steps_m4.go`'s package doc for the full reasoning). If they lie: they cannot — the login comes
  from GitHub, not the comment body. The residual risk is a misconfigured approver list (R-001).
  An approve and a later reject that share the exact same recorded `CreatedAt` (GitHub's comment
  timestamp precision can collide) are ordered by `Comment.ID` instead — GitHub assigns IDs from
  a single, strictly increasing sequence at creation time, so the larger ID on an exact tie
  reliably means "posted later" (`isNewerComment`, M4 hardening).
- **Registry credential chain.** Caller: hoist, on the user's behalf. The cluster-secret source
  reads a pull secret out of a namespace the kubeconfig can reach; that is a deliberate
  convenience and is opt-in per config (`registries[].cluster`, `--cluster-secret`). Credentials
  are used inside `pkg/registry` and never appear in inputs, outputs, state files, or logs
  (R-002): the secret leaves `pkg/k8s` only as an `authn.Keychain`, every credential value the
  chain has seen is scrubbed from every message, and a failed link is reported by source name
  ("keychain: status 403 Forbidden: DENIED"). A third, process-wide guard backs the per-call hide
  lists: `pkg/k8s` and `pkg/registry` call `redact.Register` the instant a credential value is
  read (an env token, a cluster secret's password, `op`'s output), so it is scrubbed from every
  later message anywhere in the process — including the CLI's own printer — even from a path that
  forgot to thread it through as a local hide argument. The env token is sent to `ghcr.io` only,
  never to another host a manifest happens to name.
- **kubeconfig.** hoist trusts the context it is pointed at. It never writes anything to the
  cluster except the Argo refresh annotation; the deploy itself is Argo acting on a merged commit.
  A wrong context is the operator's risk: the context name in use is printed on every plan so a
  digest read from the wrong cluster is visible, and `--digest-sources none` plans without one.
- **The gitops repo's own CI.** hoist treats a green check-run set as a fact it observed, not a
  property it verified. With `ci.none: green` (the default) a PR with *no* reported checks is
  treated as passing after the grace period — that is a stated, enforced policy (`CIGreenStep`),
  not a gap, and it is configurable to `prompt` (Blocked until an explicit
  `hoist resume --override-ci-none`) or `block` (Blocked, no override at all) per repo (R-003).
- **Merge-time stale head.** `MergedStep` refuses to merge if the PR's current head sha
  disagrees with what this promotion last observed pushed (`PromotionState.PushedSHA`), using
  the forge's own atomic "merge iff head is X" (`Forge.MergePR`'s `expectedHeadSHA`) rather than
  a client-side check-then-merge race. A process killed mid-merge-call is handled by re-asking
  the forge (`FindPR`) before ever treating a lost response as a failure worth retrying with a
  second merge call. A PR found by head branch name is also checked against `s.Base` before
  being adopted, at both `PROpenedStep` and (belt-and-suspenders) `MergedStep` — a PR sharing
  this promotion's exact branch name but targeting a different base is refused, never merged
  blind (M4 hardening).
- **Post-merge base drift.** `pr.Merged == true` is a historical forge record, not proof the
  base branch still holds what this promotion produced: `MergedStep.Observe` additionally
  fetches `s.Base`'s live tip from origin (`Git.FetchBranch`, never a locally cached ref) and
  confirms the promotion's own merge commit (the forge's `pr.MergeSHA`) is still an ancestor of
  that live tip (`Git.IsAncestor`, `git merge-base --is-ancestor`) before ever reporting done.
  This is an ancestry check, not a content comparison, precisely because a later, legitimate
  promotion to the same env can move the base forward past this one's merge without reverting
  it — content would misclassify that ordinary case as reverted. Someone resetting the base
  directly outside hoist after a real merge (making the merge commit unreachable) is caught this
  way — re-running the identical promotion (same deterministic id/branch/PR) Blocks, naming the
  base, instead of silently reporting success (M4 hardening, closes a gap AGENTS.md invariant 2
  exists to prevent).
- **PR bodies, commit messages, logs.** These leave the machine. Nothing internal (cluster
  addresses, context names, hostnames) may be written into them; enforced by `scripts/public-safety.sh`
  over the repo's own tracked files and by template tests over rendered output (R-004).

## Cross-cutting flows

- **Promotion lifecycle.** Discover repo → resolve digests from running source pods → build plan →
  user confirms → engine steps (branch, commit, push, PR, CI, approval, merge, Argo refresh, Argo
  synced, rolled out). Invariant: **every step's `Observe` re-derives truth from the remote before
  its `Act` runs, and `Act` is skipped when `Observe` is satisfied** — so a crash and resume at any
  point yields exactly one branch, one commit, one PR. The state file is an index, never a source of
  truth.
- **Deterministic identity.** Promotion id is a hash of (repo, target env, sorted `repo@digest`
  set). It names the branch, the PR body marker, the commit trailer, and the approval comment token.
  Changing the image set changes the id — which is correct: a different digest set is a different
  promotion and must not reuse an in-flight PR.
- **Digest normalisation.** Bare tags and `sha-` tags in the source env are pinned to a digest before
  they are written to the target: `pkg/resolve` asks the source namespace's pods, then the
  manifest's pin, then the registry, and every disagreement between them is a warning with a
  stated choice. Invariant: nothing hoist writes to a manifest is ever a bare tag — an image no
  source can pin stays unresolved and `gitops.BuildPlan` refuses it as before.
- **Production gate.** `envs.production` decides three things at once: PR-only (direct mode
  refused), approval comment required by default, and the "deploying straight to production"
  warning on the registry-pick path. Invariant: a production env can never be set to direct mode
  by config default — there is no separate "direct allowed" config field at all (M6): the single
  `envs.production` list, unfiltered, is the whole switch. As of M6 this is a real engine-level
  mechanism, not a stated intention: `internal/engine.DirectCommitGateStep` refuses an env listed
  in `envs.production` before `DirectSteps`' branch/commit/push steps ever run, independent of the
  TUI tag picker's own UI-side gating (`internal/app/tags`, whose `D` key simply isn't offered for
  a production target) and independent of `cmd/hoist`'s `--direct`/`--confirm-direct` CLI flags.
  `TestDirectModeRefusesProductionEnvByConstruction` (`internal/engine/direct_test.go`) constructs
  exactly the adversarial attempt — a production-listed env, already "confirmed" — and asserts the
  refusal.

## Risk register

| ID | Risk | Where it lives | Mitigation / status |
|---|---|---|---|
| R-001 | A misconfigured `approvers` list (or `collaborators: true` on a repo with broad write access) lets an unintended login trigger a production merge | `internal/engine` (`ApprovedStep`, `steps_m4.go`), `pkg/forge` (`IsAllowedAuthor`), `internal/config` | **Enforced (M4).** Author check is login-based via `Forge.IsAllowedAuthor`/`Comment.Author`, never the comment body; only `AuthorType == "Bot"` is excluded (an `Organization` account is real, per the precedent in `pkg/forge/github`'s `Comments` doc comment); a comment predating the PR's head commit never satisfies (the "re-anchor" trap); a correctly-typed reject after a correctly-typed approve wins, by comment time, falling back to `Comment.ID` on an exact timestamp tie. `RepoConfig.Collaborators` opts a repo into the write-permission check at all — still no warning when a repo has many write collaborators (that mitigation from the original plan is not built; flagged for a follow-up). |
| R-002 | Registry or GitHub credentials leak into state files, logs, or PR bodies | `pkg/registry`, `pkg/forge`, `internal/engine` state file | Adaptor inputs/outputs are secret-free by construction (§4 activity shape); state file schema has no credential fields; template tests grep rendered output. |
| R-003 | `ci.none: green` treats an untested PR as passing when CI silently failed to trigger | `internal/engine` (`CIGreenStep`, `steps_m4.go`) | **Enforced (M4).** `total==0` is `Waiting` for `ci.grace` (default 3m) after the PR opens, then the configured policy: `green` (default) satisfies, `prompt` Blocks until `hoist resume --override-ci-none`, `block` Blocks with no override at all. `failure>0` always Blocks, naming the failed check by name when the forge can give one. The flight pane (`internal/app/flight`, a rendered step list) is now built and, since this PR, wired to `internal/engine`'s own `Drive`/`Status` calls: confirming a plan in the no-command TUI drives the identical real promotion `hoist promote` does (commit, push, PR, CI, approval, merge), not a read-only preview. |
| R-004 | Internal cluster identifiers land in this public repo or in the public gitops repo's PRs | everywhere text is rendered; `testdata/` | `scripts/public-safety.sh` in CI over tracked files; placeholder-only example config; rendered-template tests. |
| R-005 | An image promotion is also a schema migration (the app's entrypoint runs `db:prepare`), so a merge can migrate production as a side effect | `pkg/migrate`, plan screen warnings | Migration delta is surfaced as a warning with the file list; never blocks. The gitops repo's runbook owns the "never bundle a promotion with a manual migration" rule; hoist makes the delta visible, it does not enforce the runbook. |
| R-006 | A misconfigured `kube.argo_namespace` (or an Application renamed/moved since a promotion's plan was built) points `ArgoRefreshedStep`/`ArgoSyncedStep` at the wrong or a nonexistent Application | `pkg/argo`, `internal/engine` (`steps_m5.go`), `internal/config` | **Enforced (M5).** `pkg/argo.Get`/`Refresh` wrap a missing Application in `ErrNotFound`, which both steps turn into an immediate `Blocked` naming the Application and pointing at `kube.argo_namespace` and the repo's wrappers — never a silent no-op and never an infinite retry against nothing. `RolledOutStep` ties a Deployment's namespace to `PromotionState.TargetEnv` directly (the destination namespace a family's Application already deploys into, per `gitops.Env`'s own doc comment), so there is no second, independently-configurable namespace for that half to drift from. |
| R-007 | Direct mode (M6) is a new kind of write surface: a commit straight to a target env's base branch, with no PR, no CI gate, and no approval comment — every review step the normal promotion flow drives never runs at all | `internal/engine/direct.go` (`DirectCommitGateStep`, `DirectSteps`), `cmd/hoist` (`--direct`/`--confirm-direct`), `internal/app/tags` (the `D` keypress + `huh.Confirm` gesture) | Gated solely on "target env not listed in `envs.production`" — the same list that already governs PR-required and approval-required elsewhere (§4.5), never a second, independent toggle. `DirectCommitGateStep` runs first in `DirectSteps`, so a block there leaves every later write step unreached through `Drive`, and both `Observe` and `Act` independently re-derive the refusal rather than trusting the caller's belief (`TestDirectModeRefusesProductionEnvByConstruction`, `internal/engine/direct_test.go`). The residual risk is entirely in this invariant staying single-sourced: `DirectSteps`' own doc comment states `ProductionEnvs` must always be the repo's full, unfiltered `envs.production` list, never pre-filtered by a caller, and that no second "allow direct anyway" config field exists or should be added. A future change that weakens either of those — a per-env override that bypasses the check, or a caller passing a narrowed env list — would reopen exactly the config-bug risk this step exists to make structurally impossible, and would do so silently unless it is read against the doc comment's stated invariant rather than treated as a plausible enhancement. |
