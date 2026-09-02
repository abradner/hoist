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
| GitHub API | `pkg/forge/github` | `gh` auth token via go-gh | Creates branches/PRs, reads checks/reviews/comments, **merges**. Merge = deploy on an auto-sync repo. |
| Container registry | `pkg/registry` | Credential chain: env token → docker keychain → cluster pull secret → `op` | Read-only (tags, manifests, config blobs). Credentials never leave the adaptor. |
| Kubernetes API | `pkg/k8s`, `pkg/argo`, `pkg/rollout` | kubeconfig context named in hoist config | Reads pods/secrets/Deployments, watches Argo `Application` CRs, writes exactly one thing: the refresh annotation. |

## Trust boundaries

- **Approval comment → production merge.** Caller: whoever can comment on the PR (anyone, on a
  public repo). Identity: the GitHub login on the comment, checked against the configured
  `approvers` list and/or write-collaborator permission via the API; bots (`type != "User"`) are
  ignored; the comment must post-date the PR's head commit. If they lie: they cannot — the login
  comes from GitHub, not the comment body. The residual risk is a misconfigured approver list
  (R-001).
- **Registry credential chain.** Caller: hoist, on the user's behalf. The cluster-secret source
  reads a pull secret out of a namespace the kubeconfig can reach; that is a deliberate
  convenience and is opt-in per config. Credentials are used inside `pkg/registry` and never
  appear in inputs, outputs, state files, or logs (R-002).
- **kubeconfig.** hoist trusts the context it is pointed at. It never writes anything to the
  cluster except the Argo refresh annotation; the deploy itself is Argo acting on a merged commit.
- **The gitops repo's own CI.** hoist treats a green check-run set as a fact it observed, not a
  property it verified. With `ci.none: green` (the default) a PR with *no* reported checks is
  treated as passing after the grace period — that is a stated policy, not a gap, and it is
  configurable to `prompt` or `block` (R-003).
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
  they are written to the target. Invariant: nothing hoist writes to a manifest is ever a bare tag.
- **Production gate.** `envs.production` decides three things at once: PR-only (direct mode refused),
  approval comment required by default, and the "deploying straight to production" warning on the
  registry-pick path. Invariant: a production env can never be set to direct mode by config default.

## Risk register

| ID | Risk | Where it lives | Mitigation / status |
|---|---|---|---|
| R-001 | A misconfigured `approvers` list (or `collaborators: true` on a repo with broad write access) lets an unintended login trigger a production merge | `internal/engine/steps` (Approved), `internal/config` | Author check is login-based via API, never body-based; bots excluded; config validation warns when `collaborators: true` and the repo has more than a handful of write collaborators. Stated; enforcement lands with M4. |
| R-002 | Registry or GitHub credentials leak into state files, logs, or PR bodies | `pkg/registry`, `pkg/forge`, `internal/engine` state file | Adaptor inputs/outputs are secret-free by construction (§4 activity shape); state file schema has no credential fields; template tests grep rendered output. |
| R-003 | `ci.none: green` treats an untested PR as passing when CI silently failed to trigger | `internal/engine/steps` (CIGreen) | Grace period before deciding; flight pane says "no CI reported" explicitly; `prompt`/`block` available per repo. Accepted default for a repo with no branch protection. |
| R-004 | Internal cluster identifiers land in this public repo or in the public gitops repo's PRs | everywhere text is rendered; `testdata/` | `scripts/public-safety.sh` in CI over tracked files; placeholder-only example config; rendered-template tests. |
| R-005 | An image promotion is also a schema migration (the app's entrypoint runs `db:prepare`), so a merge can migrate production as a side effect | `pkg/migrate`, plan screen warnings | Migration delta is surfaced as a warning with the file list; never blocks. The gitops repo's runbook owns the "never bundle a promotion with a manual migration" rule; hoist makes the delta visible, it does not enforce the runbook. |
