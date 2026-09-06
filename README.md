# hoist

> 🤖 **AI Agents:** read [`AGENTS.md`](AGENTS.md) instead. This README is for humans.

A terminal UI that promotes container images between environments in an Argo CD GitOps repo.

hoist reads a repo laid out as `<apps-root>/<env>/<family>/*.yaml`, shows every environment's
images side by side, and moves an environment's image set to the next environment **as a block** —
`staging → production` for web, worker, queue and friends in one reviewable PR. It can also pick a
fresh image straight from the registry for a single environment.

The interesting part is what happens after you press enter: hoist commits to a worktree, opens the
PR, waits for CI, waits for a human to comment `hoist approve <id>` (production only, by default),
squash-merges, pokes Argo CD to refresh, and follows the rollout — telling you at every step what
it is doing and what it is waiting for. Kill it at any point and `hoist resume` picks up where the
world actually is, not where a log file says it was.

## Status

Pre-alpha. Being built milestone by milestone; nothing is usable yet. Watch the
[issues](https://github.com/abradner/hoist/issues) for progress.

## Design in one paragraph

Every wait in a promotion is on something observable — a branch on origin, a PR's checks, a
comment, a merge SHA, an Argo `Application`'s synced revision, a Deployment's rollout status. So
hoist keeps no durable event log: each step *observes* the remote before it *acts*, acts are
idempotent, and a promotion's identity is a hash of its inputs, which names the branch, the PR
marker and the approval token. Digests are promoted, never tags; edits are byte-minimal rewrites
of the image scalar; production is gated by a person. The full rationale, including why this is
not built on a workflow engine, lives in `AGENTS.md` and `docs/repo-map.md`.

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
  unpinned tag can pull a different build when the replacement pod lands.

The reasoning behind each, and why hoist is not built on a workflow engine, is in `AGENTS.md`
and `docs/repo-map.md` — those are for people working on hoist, not for people using it.

## Install

```bash
go install github.com/abradner/hoist/cmd/hoist@latest
```

That builds the newest tagged release (or `main`, before the first tag exists). Prebuilt binaries
for macOS and Linux, amd64 and arm64, are attached to every
[release](https://github.com/abradner/hoist/releases) with a `checksums.txt`; `hoist --version`
names what you are running. hoist shells out to `git` and `gh` — both on `PATH`, and `gh auth
status` logged in, is the whole prerequisite.

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
registries:
  - prefix: ghcr.io/me/
    auth: [env, keychain]
```

## License

Apache-2.0. See [`LICENSE`](LICENSE).
