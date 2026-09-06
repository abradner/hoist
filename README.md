# hoist

> 🤖 **AI Agents:** read [`AGENTS.md`](AGENTS.md) instead. This README is for humans.

A terminal UI that promotes container images between environments in an Argo CD GitOps repo,
and drives the whole path from edit to rollout.

hoist reads a repo laid out as `<apps-root>/<env>/<family>/*.yaml`, shows every environment's
images side by side, and moves an environment's image set to the next environment **as a block**
— `staging → production` for web, worker, queue and friends in one reviewable PR. It can also
write one fresh build straight from the registry into a single environment, or roll an
environment's Deployments without changing anything they declare.

The interesting part is what happens after you press enter: hoist commits to a worktree, opens the
PR, waits for CI, waits for a person to comment `hoist approve <id>` (production only, by default),
squash-merges, asks Argo CD to refresh, and follows the rollout — telling you at every step what
it is doing and what it is waiting for. Kill it once it is under way and `hoist resume <id>` (or
`hoist resume --env <target>`) picks up where the world actually is, not where a log file says it
was. (The one narrow exception is the first second or so before its state file exists: a process
killed there leaves a claim file naming the target env, and the next attempt tells you where it
is so you can delete it.)

## What it looks like

The matrix is the entry screen: one row per family, one column per environment, every cell the
reference that environment declares and its state as a word. Under it, whatever is promoting right
now — re-observed against GitHub and the cluster, not read from a log — with the one thing that is
yours to do.

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
│                                                                              │
╰──────────────────────────────────────────────────────────────────────────────╯
╭─ in flight (1) ──────────────────────────────────────────────────────────────╮
│5pr6sd333t   app-staging → app-production   started 12m ago                   │
│● branch  ● commit  ● push  ● PR #103  ● CI  ◍ approval  ○ merge              │
│○ argo refresh  ○ argo sync  ○ rollout                                        │
├──────────────────────────────────────────────────────────────────────────────┤
│blocked on you — comment on PR #103 to release it:    hoist approve 5pr6sd333t│
╰──────────────────────────────────────────────────────────────────────────────╯
env a                          p promote • d deploy • r resume • ? help • q quit
```

(Rendered from the test fixture, which is why the environments are called `A`, `B` and `C`; a
real repo's columns are its Argo destination namespaces, and a production column is marked `⚠`.)

Before any write, the confirm screen leads with what is being shipped — the commits between the
build an environment declares and the one about to be written, and which of them migrate the
database — with the YAML diff one key away:

```
╭─ hoist · confirm deploy ─────────────────────────────────────────────────────╮
│ghcr.io/example/web:v9   →   app-production              mode: PR · production│
├──────────────────────────────────────────────────────────────────────────────┤
│rolling out 14 commits · 2 migrations · replacing v202601010101, live 4 weeks │
├──────────────────────────────────────────────────────────────────────────────┤
│  4a1c2ef  Add rate limiting to the public API                                │
│  e9b0d31  Fix N+1 query when resolving digests                               │
│  77c0ffe  db: add index on events.created_at  migration                      │
│  1b2d3e4  Bump temporal SDK to 1.31                                          │
├──────────────────────────────────────────────────────────────────────────────┤
│2 migrations run on this deploy:                                              │
│  db/migrate/20260225T101500_add_events_created_at_index.rb                   │
│  db/migrate/20260301T090200_backfill_events_tenant_id.rb                     │
├──────────────────────────────────────────────────────────────────────────────┤
│writes 3 occurrences in 1 file · digest dddddddddddd           d  see the yaml│
╰──────────────────────────────────────────────────────────────────────────────╯
                              enter deploy · ↑/↓ scroll · d yaml diff · esc back
```

## Install

```bash
go install github.com/abradner/hoist/cmd/hoist@latest
```

That builds the newest tagged release (or `main`, before the first tag exists). Prebuilt binaries
for macOS and Linux, amd64 and arm64, are attached to every
[release](https://github.com/abradner/hoist/releases) with a `checksums.txt`; `hoist --version`
names what you are running. hoist shells out to `git` and `gh` — both on `PATH`, and `gh auth
status` logged in, is the whole prerequisite.

## The three operations

Everything hoist does is one of these. Each is a subcommand and a key on the matrix, with the
same gates either way, and each has a read-only form that prints what would happen and touches
nothing: `plan` for a promotion, `--dry-run` on `deploy` and `restart`.

**Promote a pair** — copy what one environment runs into the next, every first-party image the
target already carries (the `promotable` prefixes; anything else is listed as untouched):

```bash
hoist plan --from app-staging --to app-production --dry-run   # the diff, nothing written
hoist promote --from app-staging --to app-production           # branch, commit, push, PR, CI …
```

`promote` resolves each image to a digest — by default what the source environment's pods are
running, else the manifest's own pin, else the registry, in the order `digest_sources` gives —
rewrites only the image lines in the target environment, and opens one PR carrying all of them.
Then it waits: for CI, for `hoist approve <id>` on the PR when the target is production, for the
merge, for Argo to sync, for every Deployment it touched to roll out. On the matrix this is `p`.

**Deploy a build** — write one named image into one environment, through the same pipeline:

```bash
hoist deploy --env app-staging --image ghcr.io/me/web:v3@sha256:…
```

The reference must carry its digest; hoist will not resolve a bare tag on the way in. On the matrix
this is `d`: a picker lists the registry's tags with the build age of each, whether the paired
staging environment has committed it, and the commits and migrations between the build the
environment declares and the one under the cursor.

**Restart a family** — roll an environment's Deployments without changing what they declare:

```bash
hoist restart --env app-staging --family web
```

This is the one operation that writes to the cluster instead of to git: it stamps the same
annotation `kubectl rollout restart` does, so there is no branch, no PR and nothing to resume.
Before rolling anything it names every target with its replica count and strategy and warns
where the restart will not be graceful. On the matrix this is `R`.

## Rules hoist enforces

When hoist refuses to do something, it is one of these. They are enforced in code, not by
convention, and the refusal says which one:

- **Production always goes through a PR.** Any env listed under `envs.production` is refused
  direct mode outright, whatever a flag or a keypress asked for.
- **Production waits for a person** — a `hoist approve <id>` comment on the PR — unless that env
  is explicitly set to `approval: auto`.
- **Nothing written is a bare tag.** Every image reference hoist writes is `tag@sha256:…`, and a
  tag with no digest is refused rather than resolved on the fly. The tag picker will not let you
  select a build whose digest it could not read, for the same reason.
- **A direct commit takes the env's name twice** — `--confirm-direct=<env>` at the CLI, a
  keypress and a confirmation in the TUI — so a write with no PR is never one flag or one key.
- **A restart changes no declared reference**, and warns where it may still not be a no-op: an
  unpinned tag can pull a different build when the replacement pod lands. Restarting production
  takes `--confirm-production=<env>`.
- **One promotion per target environment until it lands.** While an env's promotion is still
  before its merge (or its direct push), a second one for that env is refused and named;
  `hoist promotions` lists them and `hoist resume <id>` continues one. Once the change has landed
  the guard lifts, even if Argo is still converging.

These, by contrast, are warnings and never refusals: a digest the source's pods and manifest
disagree on, a promotion that jumps straight to production, a build staging has never committed,
a migration in the delta, a restart that will not be graceful. hoist says so and lets you decide.

## What hoist is not

It is not a controller, an operator, or a platform. It runs on a laptop, holds no state a person
cannot delete (`~/.local/state/hoist/`, an index of where to look, never a record of what
happened), and stops when you close it — a promotion it started is a branch, a PR and Argo doing
what Argo does, all of which are there whether hoist is running or not. It is not Kargo or
Argo Rollouts: it drives *your* GitOps repo through *your* PR review and lets Argo deploy, rather
than replacing either.

## Configuration

hoist reads `$XDG_CONFIG_HOME/hoist/config.yaml` (usually `~/.config/hoist/config.yaml`).
Nothing cluster-specific lives in the GitOps repo itself. A minimal example:

```yaml
repos:
  - path: ~/src/my-gitops
    github: me/my-gitops
    apps_root: cluster/apps
    envs:
      production: [app-production]
      pairs: { app-staging: app-production }
    approvers: [me]
    kube: { context: my-cluster }
    apps: { ghcr.io/me/web: me/web }        # image repo → app repo, for commit history
registries:
  - prefix: ghcr.io/me/
    auth: [env, keychain]
```

`docs/config.example.yaml` is the annotated schema: every key, its default, and why. Unknown keys
are errors, and `hoist config show` prints the effective config with defaults filled in.

## License

Apache-2.0. See [`LICENSE`](LICENSE).
