# hoist — the user guide

The README is the pitch and the reference. This is the thing you read when hoist has just refused
to do something, or stopped, and you need to know whether that is a bug or the point. Every screen
below is rendered from the test fixture, which is why the images are `ghcr.io/example/…` and the
environments have placeholder names; the shapes are exact.

Contents: [the matrix](#the-matrix) · [promote a pair](#promote-a-pair) · [deploy a
build](#deploy-a-build) · [restart a family](#restart-a-family) · [when it
stops](#when-it-stops) · [resuming](#resuming) · [registry credentials](#registry-credentials) ·
[direct mode](#direct-mode)

## The matrix

`hoist --repo <path>` (or just `hoist`, when the config file lists one repo) opens the matrix: one
row per family, one column per environment, and in each cell the reference that environment's
manifests declare plus its state as a word. `--base <branch>` and `--kube-context <name>` given
before the command apply to everything the matrix does — the branch a confirmed plan is created
from and its PR targets, and the cluster drift, restarts and resumed promotions talk to — exactly
as the same flags do on `promote` or `restart`. The title names the base when it is not `main`,
and the kube context in use — the flag's, else the repo's `kube.context` — by name. The same
goes for `--digest-sources`, `--registry-auth`, `--cluster-secret` and `--op-ref`: given before
the command they are what the plan screen resolves with and what the tag picker and the commit
history authenticate to the registry with, exactly as on `plan` or `promote`; the drift column
always asks the pods alone.

```
╭─ hoist · matrix · repo ──────────────────────────────────────────────────────╮
│ FAMILY      ▸ A                   B                    C                     │
│ absent      v1            pinned                       v9    pinned          │
│ drift       v1          unpinned  v2           pinned  v2  unpinned          │
│ empty                             no images                                  │
│ mixedtags   2 versions     split                                             │
│ multi       2 images      pinned  2 images   unpinned                        │
│ pinned      v1            pinned  v1           pinned  v1    pinned          │
│ sidecar     v1            pinned  v1           pinned                        │
│ thirdparty  7           external  8          external  8   external          │
╰──────────────────────────────────────────────────────────────────────────────╯
env a                          p promote • d deploy • r resume • ? help • q quit
```

The words:

| Word | Meaning |
|---|---|
| `pinned` | every reference in the cell carries a digest (`tag@sha256:…`) |
| `unpinned` | at least one is a bare tag — a moved tag is invisible to `imagePullPolicy: IfNotPresent` |
| `split` | one image repo declared at two different versions in the same env; a promotion out of here picks one (the pinned one if exactly one is pinned, else the most common) and warns that it had to choose |
| `external` | third-party images (outside the `promotable` prefixes); shown, never written |
| `drifted` | the cluster is running a build the manifest does not declare — with the running reference named under the table |
| `resolving…` | the cluster has not answered for that column yet |

`←`/`→` move the environment cursor (`▸`), `↑`/`↓` the family; the column under the cursor is
what `p`, `d` and `R` act on. A production column is marked `⚠` in its header and named under the
table. `F5` asks the cluster again. When something is promoting, it is listed under the table with
its step strip and — when it is waiting on you — the exact command:

```
╭─ in flight (1) ──────────────────────────────────────────────────────────────╮
│5pr6sd333t   app-staging → app-production   started 12m ago                   │
│● branch  ● commit  ● push  ● PR #103  ● CI  ◍ approval  ○ merge              │
│○ argo refresh  ○ argo sync  ○ rollout                                        │
├──────────────────────────────────────────────────────────────────────────────┤
│blocked on you — comment on PR #103 to release it:    hoist approve 5pr6sd333t│
╰──────────────────────────────────────────────────────────────────────────────╯
```

That list is re-observed against GitHub and the cluster at boot and on every poll, never read
from a log; `r` reopens one on its flight screen, `o` opens its PR, and each asks which when
several are in flight.

## Promote a pair

`p` promotes the cursor column's paired source into it (`envs.pairs` in the config); `P` asks
which target. The CLI is the same operation:

```bash
hoist plan    --from app-staging --to app-production --dry-run
hoist promote --from app-staging --to app-production
```

**What it plans.** For every first-party image repo the target already carries, the build the
source environment is running: its pods first, then the manifest's own pin, then a registry lookup
of the tag, in `digest_sources` order. A repo the target does not carry is listed as untouched, not
added. A bare tag that nothing can pin is refused — hoist never writes a tag without a digest.

**The confirm screen** leads with what ships, not with the bytes:

```
╭─ hoist · confirm promotion ──────────────────────────────────────────────────╮
│app-staging  →  app-production   images under ghcr.io/example/        mode: PR│
│3 repos ticked · no history for 3                              d  see the yaml│
├──────────────────────────────────────────────────────────────────────────────┤
│┃ > ✓ counta  → v202602201200       │counta                                   │
│┃   ✓ marketing  → sha-1a2b3c4d5e6f…│v202601151010 → v202602201200            │
│┃   ✓ ! web  → v202602150930        │3 occurrences · 2 files · digest from    │
│┃                                   │manifest                                 │
│┃                                   │                                         │
│┃                                   │no commit history —                      │
│┃                                   │ghcr.io/example/counta has no app repo in│
│┃                                   │repos[].apps                             │
╰──────────────────────────────────────────────────────────────────────────────╯
                tab pane · x toggle · d yaml · m mode · enter confirm · esc back
```

Left: the repos, ticked (`x` untick one to leave it out; `!` marks a repo with a warning). Right:
the repo under the cursor — the versions in full, where its digest came from, its warnings, and,
when `repos[].apps` maps the image to its source repo, the commits between what the target
declares and what is about to be written, with any migration among them. `d` swaps the right pane
for the YAML diff; `enter` means the same from either view. `m` switches to direct mode on a
non-production target (see [direct mode](#direct-mode)); on a production target it says why it
cannot.

**What happens after enter**, in order: a worktree under `$XDG_CACHE_HOME/hoist/worktrees/<id>`
from your own clone, a branch `hoist/<env>/<id>`, one commit with only the image lines changed
(verified structurally before `git add`), a push, a PR whose body carries the same warnings, then
waiting — for CI, for approval on a production target, for the merge, for Argo to sync, for every
Deployment the commit touched to roll out. The flight screen shows each step and what the current
one is waiting for:

```
╭─ hoist · promotion · in flight ──────────────────────────────────────────────╮
│abcd1234   app-staging → app-production                     started 4 days ago│
├──────────────────────────────────────────────────────────────────────────────┤
│✓ branch                                                                      │
│✓ commit                                                                      │
│✓ push                                                                        │
│✓ PR                                                                          │
│✓ CI                                                                          │
│… approval                                                                    │
│    no approval comment yet                                                   │
│· merge                                                                       │
│· argo refresh                                                                │
│· argo sync                                                                   │
│· rollout                                                                     │
├──────────────────────────────────────────────────────────────────────────────┤
│blocked on you — comment on PR #103 to release it:                            │
│                                                                              │
│    hoist approve abcd1234                                                    │
╰──────────────────────────────────────────────────────────────────────────────╯
              o open PR · X abandon · R re-observe · x abort · l log · esc back
```

`R` re-observes now instead of at the next poll. `x` stops *watching* — the branch, the PR and
the state file stay exactly as they are; nothing is closed or deleted. `X` (shift, like `R` on the
matrix — a write, kept out of reach of a mistyped `x`) *abandons* it instead: behind a confirm, it
retires the state file and, if it opened a PR, closes it and deletes the branch. It refuses
outright if the promotion has already landed — abandoning is not a rollback, so a landed one needs
`hoist deploy` or a fresh promotion to undo, not a state-file delete — and it is never offered at
all once the promotion is done. `l` shows the log. The CLI prints the same steps as lines, with
`waiting: …` naming what it is waiting for; `hoist abandon <id> --confirm-abandon=<id>` is `X`'s
own CLI form (the repeated id is the confirmation, same shape as `--confirm-direct`).

The promotion's id (`abcd1234` above) is a hash of the repo, the target env and the digest set.
It names the branch, the PR body marker, the commit trailer and the approval token — so running
the identical promotion twice finds the first one's branch and PR rather than opening a second.

## Deploy a build

`d` on a cell (a chooser first, when the family runs several first-party images) opens the tag
picker for that image in the cursor column:

```
╭─ hoist · deploy ─────────────────────────────────────────────────────────────────────────────────╮
│ghcr.io/example/app  →  app-production   production                                               │
│app-production declares  v1 · 111111111111 · since 4 weeks ago                                    │
│note: app-staging (paired staging)'s committed manifest tag is v3; v3 is the tag committed there —│
│tags move, so this is not proof of the same build                                                 │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│   TAG  BUILT          DIGEST                                                                     │
│▸ v3   3 days ago     333333333333  in app-staging                                                │
│  v2   4 weeks ago    222222222222                                                                │
│  v1   2 months ago   111111111111  ◂ declared here                                               │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│v3 is 14 commits ahead of v1 · 1 migration                                                        │
│  4a1c2ef  Add rate limiting to the public API                                                    │
│  e9b0d31  Fix N+1 query when resolving digests                                                   │
│  77c0ffe  db: add index on events.created_at  migration                                          │
│…8 more                                                                                           │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯
                     ↑/↓ move · tab commits · enter read commit · space review the change · esc back
```

The header says what the environment *declares* (the manifest, dated by when that line last
changed) — never "runs", because this screen does not read the cluster. Each tag has its build
age, whether the paired staging environment has committed it, and, under the cursor, the commits
between the declared build and that tag with any migration marked. `tab` moves into the commits
and `enter` reads one in full; `space` reviews the change. A tag whose digest could not be read is
not selectable, for the same reason a bare tag is never written.

The confirm screen then leads with the delta and the migrations:

```
╭─ hoist · confirm deploy ─────────────────────────────────────────────────────╮
│ghcr.io/example/web:v9   →   app-production              mode: PR · production│
├──────────────────────────────────────────────────────────────────────────────┤
│rolling out 14 commits · 2 migrations · replacing v202601010101, live 4 weeks │
├──────────────────────────────────────────────────────────────────────────────┤
│  4a1c2ef  Add rate limiting to the public API                                │
│  e9b0d31  Fix N+1 query when resolving digests                               │
│  77c0ffe  db: add index on events.created_at  migration                      │
├──────────────────────────────────────────────────────────────────────────────┤
│2 migrations run on this deploy:                                              │
│  db/migrate/20260225T101500_add_events_created_at_index.rb                   │
│  db/migrate/20260301T090200_backfill_events_tenant_id.rb                     │
├──────────────────────────────────────────────────────────────────────────────┤
│writes 3 occurrences in 1 file · digest dddddddddddd           d  see the yaml│
╰──────────────────────────────────────────────────────────────────────────────╯
                              enter deploy · ↑/↓ scroll · d yaml diff · esc back
```

Migrations are called out twice on purpose: they are the one class of change that is not
trivially reversible, and on a repo whose entrypoint runs `db:prepare`, the merge *is* the
migration. hoist never blocks on one — the runbook owns that rule — it makes sure you saw it.
"Migrations unknown" means the history was too long or too wide for the forge to list every file;
it is a floor, not a zero.

The CLI form takes the reference already pinned:

```bash
hoist deploy --env app-staging --image ghcr.io/me/web:v3@sha256:…
```

After enter, everything is as for a promotion: the same pipeline, the same flight screen, the same
gates.

## Restart a family

`R` on a cell, or:

```bash
hoist restart --env app-staging --family web            # --dry-run to only list
```

This is the one operation that writes to the cluster instead of to git. It stamps the same
`kubectl.kubernetes.io/restartedAt` annotation `kubectl rollout restart` does on the pod template
of every Deployment the family declares, and Argo leaves that alone even with self-heal on (its
diff is a three-way merge; a field it never set and the manifest does not carry is not drift).
So there is no plan, no branch, no PR, no state file, and nothing to resume: running it again
restarts again, which is the operation.

```
╭─ hoist · restart ────────────────────────────────────────────────────────────╮
│app-staging / web                               production   not yet restarted│
├──────────────────────────────────────────────────────────────────────────────┤
│1 Deployment(s) in app-staging                                                │
│                                                                              │
│   web  1 replica(s) · RollingUpdate · last restart: never restarted this way │
│     ! only 1 replica: it keeps serving until the replacement is ready, but   │
│       there is no redundancy if the replacement fails                        │
│     ! no readiness probe: a new pod counts as available the moment it        │
│       starts, before it can serve                                            │
│     ! unpinned image(s) ghcr.io/example/web:v1: a replacement pod can pull   │
│       a different build than the one running now, so this restart may not be │
│       a no-op                                                                │
╰──────────────────────────────────────────────────────────────────────────────╯
                                                        enter restart · esc back
```

Every target is named first, with its replica count, strategy and last restart, and every reason
the restart will not be graceful is a sentence. None of them blocks. A production env takes a
second acknowledgement — a confirmation dialog on the screen, `--confirm-production=<env>` at the
CLI — because nothing is committed and nothing reviews it.

## When it stops

A stopped promotion says which step and why; this is what to do about each. `R` on the flight
screen (or `hoist resume <id>`) re-observes once you have.

**CI.** `CI: 2/3 checks complete` is waiting, not stopped. `1 of 3 checks failed: <name>` — or
`… were skipped (never ran)`, which blocks the same way — names the check. Do not push a fix to
`hoist/<env>/<id>`: the promotion only ever merges the head it pushed itself, and a branch
someone else moved is refused at the merge. If the failure is transient, re-run the failed
workflow on the same commit in GitHub and `R`. If it needs a real change in the GitOps repo,
land that change on the base branch through its own PR, then abandon this promotion — close its
PR and delete its branch on origin — and run the promotion again, so it starts from the updated
base. *No checks reported after the grace period* depends on the repo's `ci.none` setting:
`green` (the default) treats it as passing, `prompt` waits for you to say so —

```bash
hoist resume <id> --override-ci-none
```

— and `block` has no override at all: fix why CI did not run.

**Approval.** `` waiting for `hoist approve <id>` from an approver `` — someone in the repo's
`approvers` list (or, with `collaborators: true`, anyone with write access) comments exactly

```
hoist approve <id>
```

on the PR, on a line of its own (the rest of the comment can say whatever it likes; the case
and a leading `/` do not matter), after the head commit hoist pushed — an earlier comment does
not count. hoist cannot approve its own PRs, since it acts as you.
`rejected by <login> at <time>` means an approver commented `hoist reject <id>` after the last
approval; a new approval after the rejection wins.

**Degraded.** `<app> health is Degraded (sync=… revision=…)` — Argo synced the merge and the
rollout is failing. Look at the Application in Argo (or `hoist watch --app <name>`); when it is
Healthy again, `R`. `Argo Application <name> not found; check kube.argo_namespace and the repo's
Application wrappers` is configuration: the Application lives in a namespace other than
`kube.argo_namespace` (default `argocd`), or was renamed.

**A stale clone.** The promotion works in a worktree from your own clone, so your clone has to
know the base branch:

- `<repo> has no local branch "main", only …` — `git branch main origin/main` in your clone and
  re-run.
- `origin no longer has a branch "main" (a stale … remains …)` — the base name is wrong, or the
  remote branch was deleted; `git fetch --prune origin` and check `--base`.
- `origin/main is already at X, but this promotion's commit is Y — something else moved this branch; refusing to force-push. Delete or fast-forward it manually if that was intentional.`
  — someone pushed to the promotion's branch by hand. hoist never force-pushes. Delete or
  fast-forward the branch yourself if that was intended, then `R`.
- `commit X changes paths beyond this promotion's plan` — the branch's commit touches files the
  plan did not. hoist refuses to treat it as its own. Fix the branch — reset `hoist/<env>/<id>` on
  origin to the base branch, or delete it there — then `R`, and hoist commits again.
- `found PR #N … but it targets base "x", not "main"` / `… was closed without merging` — a PR on
  the promotion's branch that hoist did not open, or one that was closed. Retarget or reopen it
  and `R`; hoist adopts it. If the closed PR should stay closed, open a new one from the same
  branch, carrying the closed PR's title and body so the reviewer still sees hoist's diff
  summary and warnings (`gh pr create --head hoist/<env>/<id> --base main --title "$(gh pr
  view <n> --json title -q .title)" --body "$(gh pr view <n> --json body -q .body)"`), then
  `R` — hoist prefers an open PR over a closed one on the same branch.

One thing that does *not* help in any of these: deleting the state file. A promotion's id is a
hash of its inputs, so the same digests into the same env is the same id, the same branch and the
same PR — hoist will find them again. The state file is only where hoist keeps its notes; the
branch and the PR are the promotion. A different set of digests is a different promotion.

**Another promotion in flight.** `promotion <id> targeting <env> is still in flight (at <step>:
…); run hoist resume <id> instead of starting a second one` — exactly that. The guard holds
until the change lands (merge or direct push), not until rollout.

**A claim file.** If a `promote` was killed in the first moments — after it claimed the target
env, before it saved any state — the next attempt refuses with the claim's age and its path. Only
then, and only when you are sure the owning process is gone, delete that file.

## Resuming

hoist keeps a state file per promotion under `$XDG_STATE_HOME/hoist/promotions/<id>.json` — an
index of where to look, never a record of what happened. Every step re-observes the branch, the
PR, the checks, the comments, the merge, the Application and the Deployments before it acts, so
resuming from any point is safe and never duplicates a branch or a PR.

```bash
hoist promotions              # every state file, its phase re-observed right now
hoist resume <id>             # continue one from wherever it actually is
hoist resume --env <target>   # the same, for the one non-terminal promotion targeting that env
hoist abandon <id> --confirm-abandon=<id>   # retire one that never landed — not a rollback
```

On the matrix, `r` does the same for what the in-flight pane lists. A finished promotion leaves
the pane; its state file stays until you delete it or run `hoist abandon` (refused outright once
the promotion has landed).

Restarts are the exception: they keep no state, so there is nothing to resume, and re-running
one is a new restart.

## Registry credentials

The tag picker and the registry fallback for digests need to read the registry. hoist tries a
chain, in the order `registries[].auth` gives (default `env, keychain, cluster, op`), stops at the
first that works, and names which one it used — or, when every link fails, names every link it
tried and why, and falls back to anonymous. Each link is one config idea:

| Link | Where the credential comes from |
|---|---|
| `env` | `HOIST_GHCR_TOKEN` or `GHCR_TOKEN` (GHCR only), with `HOIST_GHCR_USER`/`GHCR_USER` as the user |
| `keychain` | the docker keychain — whatever `docker login` stored in `~/.docker/config.json` |
| `cluster` | an image pull secret in the cluster: `registries[].cluster: { namespace: …, secret: … }` |
| `op` | 1Password: `registries[].op: op://vault/item/field`, read with `op read` |

So `cluster: not configured` means the chain reached the `cluster` link and there is no
`registries[].cluster` block for that prefix; `op: not configured` the same for `op`. Neither is a
failure of the credential, only its absence.

The gotcha that costs everyone an hour once: **`gh`'s own token cannot list GHCR tags** — it lacks
the `read:packages` scope, and the error is `DENIED: permission_denied: The token provided does
not match expected scopes`. A Docker Desktop keychain entry can fail the same way. Any other link
works: a personal access token with `read:packages` in `GHCR_TOKEN`, or the cluster's own pull
secret, which is what a cluster that pulls from GHCR already has.

Credential *values* never appear in anything hoist prints, commits or writes to a PR; every
adaptor registers what it reads with the redactor the moment it reads it.

## Direct mode

Direct mode commits straight to the base branch: no branch of its own left on origin, no PR, no
CI wait, no approval. Then it converges through Argo and the rollout exactly as a PR would. It is
for the environment where a PR is ceremony — a staging env you own, where the review would be you
approving yourself.

It is never offered for an env listed under `envs.production`, whatever a flag or a key asked
for, and that refusal is in the engine, not the screen: the step that would commit checks the
list itself. On the plan and deploy screens `m` toggles it (behind a confirmation dialog); in the
picker `D` picks a tag straight into it. At the CLI it takes the env's name twice:

```bash
hoist promote --from app-staging --to app-dev --direct --confirm-direct=app-dev
hoist deploy  --env app-dev --image … --direct --confirm-direct=app-dev
```

`--confirm-direct` must repeat `--to`/`--env` exactly, and both flags need a config file: without
one hoist does not know which envs are production and refuses `--direct` rather than guess. The
second acknowledgement exists because a write with no PR should never be one flag or one key.
