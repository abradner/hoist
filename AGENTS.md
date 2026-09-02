# hoist — Agent Onboarding Guide

> Read this file first. It is the source of truth for AI agents working in this codebase.
> Plans, status, and decision rationale live in GitHub issues on `abradner/hoist`, not in a repo
> file. When this file and the tracker disagree
> on architecture or direction, the tracker wins — update this file to match, never the reverse.
> This file is operational: how to build, run, and write code here.

## 1. What Is This?

hoist is a Go terminal UI that promotes container images between environments in an Argo CD
GitOps repository, and drives the whole path from edit to rollout: commit, PR, CI, human approval,
merge, Argo refresh, Deployment watch — with resume after interruption. It is a single-operator
tool for the author's own GitOps repo first, written so other repos with the same shape can use it.
Status: greenfield, pre-alpha; milestones M0–M7 are tracked in issues.

Domain nouns, as this repo uses them:

- **Env** — one environment, identified by a directory under the apps root (`cluster/apps/<env>/`)
  which is also the Kubernetes namespace. Not "environment variable".
- **Family** — one deployable unit inside an env (`<env>/<family>/*.yaml`), backed by exactly one
  Argo CD `Application`. A family may hold several containers on the same image.
- **Occurrence** — one `image:` scalar in one manifest (file, document, YAML path). Promotion
  rewrites occurrences, grouped by image repo.
- **Image repo** — the registry path without tag or digest (`ghcr.io/org/app`). "Repo" alone means
  the *git* repo; say "image repo" when you mean the registry.
- **Promotion** — moving every occurrence of an image repo in a target env to the `tag@digest` the
  source env is running. Identified by a deterministic id (§4.1).
- **Block** — the set of image repos promoted together in one PR.
- **Forge** — the code host (GitHub today; the interface exists so GitLab can follow).
- **Magic comment** — the human approval token `hoist approve <id>` posted on the PR.
- **Direct mode** — commit straight to the default branch, no PR. Non-production envs only.

## 2. Purpose & Principles

**Purpose:** The world is the state — hoist re-observes git, the forge, Argo CD and the cluster,
then makes the smallest idempotent change that moves a digest.

Principles: short, numbered, memorizable. Each one must be able to change a real decision — a
principle that never changes a decision is decoration.

1. **Claims must hold.** A doc, comment, or commit message claiming a property the mechanism
   doesn't deliver is a bug, not documentation. Interim states say so in-place ("stated, not
   enforced — landing in E7"); stopgaps record the bar they miss. *(Portable seed — keep, adapt,
   or replace. It has caught real security bugs, twice, in a sibling repo, by being invoked
   against the project's own earlier claims.)*
2. **Re-observe, never remember.** Every engine step derives truth from the remote before it
   acts; the state file is an index of what to look at, never evidence of what happened. A step
   that trusts its own record will one day open a second PR.
3. **Promote digests, not tags.** Registry tags are mutable and `imagePullPolicy: IfNotPresent`
   makes a moved tag invisible. Nothing hoist writes to a manifest is ever a bare tag.
4. **Byte-minimal edits, verified before commit.** Only the image scalar changes; the file is
   re-parsed and diffed structurally before anything is committed. A reviewer should see one line
   per occurrence and nothing else.
5. **Warn, don't block** — except where the source repo's own runbook blocks. hoist surfaces the
   digest mismatch, the migration delta, the "straight to production" jump; the operator decides.
   The exception is stated at the rule (§4.5), not improvised at the call site.
6. **Lean on community libraries; own as little code as possible.** Every dependency is weighed
   (§4.7) but the default answer to "should we write this" is no.
7. **Nothing internal leaks into a public surface.** Commits, PR bodies, logs, fixtures and docs
   carry no cluster addresses, context names or internal hostnames. Enforced by CI, not by care.

### The tripwire protocol

A conflict with a principle is a stop-and-check, never something to quietly work around:

- If **your own approach** conflicts with a principle, treat that as a strong signal the approach
  is wrong. Re-derive it before proceeding — the principle has usually seen more failures than
  this session has.
- If the **operator's request** conflicts, say so explicitly, naming the principle. Then exactly
  one of two things is true:
  - **The principles have evolved.** Update the principle in the same change — and demand the
    justification: the *why* gets recorded next to the amendment, because an unjustified
    amendment is indistinguishable from erosion.
  - **The principles stand.** Then the request should be reconsidered — propose the nearest
    conforming alternative and let the operator choose with the conflict in view.
- Never proceed silently with a violation. An unremarked exception teaches every future reader —
  human or agent — that the principles are decorative (see §10, meta-rule 1).

## 3. Stack at a Glance

| Layer | Technology | Notes |
|---|---|---|
| Language | Go 1.26 | Managed via `mise` (`mise.toml`); use `mise exec -- go …` in a fresh shell |
| TUI | Bubble Tea v2 (`charm.land/bubbletea/v2`), Bubbles v2, Lip Gloss v2, huh v2 | Hand-rolled panes; no layout library |
| Manifests | `gopkg.in/yaml.v3` node API | Scan, byte-minimal edit, structural verify. No kustomize/helm libraries |
| Registry | `github.com/google/go-containerregistry` | Tag list, manifest HEAD, config blobs; `authn` keychain chain |
| Forge | `github.com/cli/go-gh/v2` + `exec gh` | Reuses the user's `gh` login. `Forge` interface; GitHub only today |
| Git | `exec git` | Worktree per promotion; inherits the user's signing config. Not go-git (§4.6) |
| Kubernetes | `k8s.io/client-go` (+ `api`, `apimachinery`) | Pods, secrets, dynamic client for Argo `Application` CRs, Deployment watches |
| State | JSON files under `$XDG_STATE_HOME/hoist/` | No database, no workflow engine (§4.1) |
| Testing | `go test` with golden files, `client-go` fakes, ggcr's in-memory registry, real `git` in temp dirs | See §6 |
| Lint | golangci-lint v2 | Pinned in `mise.toml` and CI |
| Deployment | `go install …/cmd/hoist@latest`; tagged releases later | See §7 |

## 4. Critical Architectural Rules

These are non-negotiable. Violating them will break things or be rejected in review.

### 4.1 The world is the state

Every engine step is a pair: `Observe(ctx, state)` derives whether the step is already satisfied
from the remote (branch on origin, PR on the forge, check-runs, comments, merge SHA, Argo
`status.sync.revision`, Deployment status); `Act` runs only when `Observe` says not satisfied and
not waiting. No step reads the state file to decide truth — the file records where to look and what
was planned, and `hoist resume` re-observes every step from the top regardless of the recorded
phase. Promotion identity is deterministic: `id = hash(repo, target env, sorted repo@digest set)`,
and it names the branch (`hoist/<env>/<id>`), the PR body marker (`<!-- hoist:id=… -->`), the commit
trailer (`hoist-id:`) and the approval token. *Why:* a promotion waits on a human for hours; the
process will be killed, the laptop will sleep. A durable event log (Temporal was considered and
declined — see §11) would only cache facts GitHub and Argo already hold, and a random id would open
a second PR on restart.

### 4.2 Digests, not tags — and byte-minimal edits

Anything hoist writes to a manifest is `<repo>:<tag>@sha256:<digest>`. The digest comes from what
the source env is *running* (`status.containerStatuses[].imageID`), falling back to a registry
HEAD; a bare tag in the source is pinned on the way through. `gitops.Apply` replaces only the image
scalar at the recorded line/column; `gitops.Verify` re-parses before/after with `yaml.v3` and fails
the promotion if any difference is not an expected image scalar at an expected path, or if the line
count changed. Verify runs before `git add`, always. *Why:* the target repo uses
`imagePullPolicy: IfNotPresent`, so a re-tagged image is silently not deployed; and a
one-line-per-occurrence diff is the whole review surface for a production change.

### 4.3 `pkg/` is activity-shaped

`pkg/*` packages never import `internal/`, never import a workflow engine, and expose functions of
the shape `func(ctx, In) (Out, error)` with JSON-serialisable inputs and outputs that contain **no
secrets** — credentials are resolved inside the adaptor from env, keychain, cluster or `op`. *Why:*
the same functions must be wrappable as Temporal activities by `github.com/abradner/workflow`
without change, and that library's boundary rule is that nothing secret or unbounded crosses a
workflow/activity edge.

### 4.4 Public surfaces carry no cluster identity

PR bodies, commit messages, log lines, `testdata/`, and every doc in this repo contain no private
addresses, internal hostnames, or real kube context names. Example config uses placeholders
(`my-cluster`, `me/my-gitops`). `scripts/public-safety.sh` runs in CI over tracked files; rendered
PR-body and commit templates get a unit test with the same patterns. *Why:* this repo and the
first target GitOps repo are both public; a hostname in a PR body is indexed within minutes.

### 4.5 Production is gated by a person

An env listed in `envs.production` always goes through a PR, always requires the magic comment
unless the operator sets `approval: auto` for that env explicitly, and refuses direct mode outright
— the keypress that enables direct mode on a non-production env is not offered. The "deploying
straight to production" warning on the registry-pick path informs; it does not block (principle 5).
Config *defaults* may never weaken this; only an explicit per-env setting can. *Why:* on the target
repo every `Application` auto-syncs with prune and self-heal, so a merge to `main` is the
deployment, and the app entrypoint runs `db:prepare` — a merge can migrate a production database.

### 4.6 `exec git`, not go-git

Commits, pushes, worktrees and ls-remote go through the `git` binary in a worktree created under
`$XDG_CACHE_HOME/hoist/worktrees/<id>` from the user's own clone. The user's checkout, branch and
index are never touched. Commit runs with a timeout (120s) and the UI says "waiting for signing
approval" after 5s. *Why:* the user's commits are SSH-signed via 1Password, which needs an
interactive approval and a config chain (`gpg.format=ssh`, `gpg.ssh.program`, `includeIf`) that
go-git would have to reimplement.

### 4.7 Dependency weight is a review criterion

Not imported, and a PR adding them needs a stated reason in this section: `argo-cd/v3` (83 direct
requires, 50 `replace` directives), `k8s.io/kubectl` (copy the ~60 lines of `rollout_status.go`
instead — Apache-2), Temporal SDK or server, go-git, kustomize `api`/`kyaml`, Bubble Tea layout
libraries, cobra (arrives for free if `abradner/workflow`'s `cli` is adopted later). Argo CD is
driven through the Kubernetes API alone: refresh = the `argocd.argoproj.io/refresh` annotation on
the `Application`, status = its `status` subresource. No Argo API server, no Argo token.

## 5. Repository Map

`docs/repo-map.md` is the living boundary map: surfaces, trust boundaries, cross-cutting flows,
and the risk register. It exists because that knowledge lives *between* files and can't be
reconstructed by grep — and because cold-started subagents and future sessions get no
conversation context, only what's written down.

- **Read it before touching anything boundary-sensitive**: auth, tenancy, deletion/visibility,
  serialization, background delivery, routing, any external surface.
- **Update it in the same PR** as any change that moves an edge on it. Never leave it describing
  a stale security boundary — a wrong map is worse than no map, because it is trusted.
- Cite risk-register IDs (R-00N) from rules, PRs, and code comments, so the register stays
  load-bearing instead of decorative.

## 6. Development Essentials

```bash
mise install                                   # Go + golangci-lint, pinned in mise.toml
mise exec -- go build ./...
mise exec -- go test -count=1 -race ./...      # -count=1: no test cache; read the run count
mise exec -- golangci-lint run
./scripts/public-safety.sh                     # the §4.4 grep, same as CI
mise exec -- go run ./cmd/hoist --help
```

Config lives at `$XDG_CONFIG_HOME/hoist/config.yaml`; state under `$XDG_STATE_HOME/hoist/`;
caches under `$XDG_CACHE_HOME/hoist/`. `mise exec -- go run ./cmd/hoist plan --repo <path> --from
<env> --to <env> --dry-run` is the read-only way to run against a real repo: it prints the unified
diff, the untouched images and the warnings, and touches no git state (`--apps-root` defaults to
`cluster/apps`, `--promotable` to `ghcr.io/`; config files are not read yet). Without `--dry-run` it
prints the same and exits 3 — writing lands in a later milestone. `mise exec -- go run ./cmd/hoist
--repo <path>` with no command opens the env × family matrix screen (read-only; `q` quits, `?`
help). Golden files under `testdata/golden/` regenerate with
`mise exec -- go test ./pkg/gitops ./internal/app -update`; the fixture repo is `testdata/repo`
(synthetic, placeholder-only — §4.4).
The dev-machine form matters: the `mise` shim for `go` errors with `No version is set for shim: go`
outside a directory that pins one, so use `mise exec --` or run from inside this repo.

### 6.1 Known Environment Gotchas

Things that cost real time to discover once — don't rediscover them. Add entries as they're found;
add rather than rewrite.

Seeded at init from the design session rather than left empty (deviation recorded in §11):

1. **`gh auth token` cannot list GHCR tags.** Symptom: `DENIED: permission_denied: The token
   provided does not match expected scopes` from `ghcr.io/v2/<repo>/tags/list`. Cause: `gh`'s
   OAuth token lacks `read:packages`; the GitHub *packages* REST API (`/user/packages/…/versions`)
   403s for the same reason. Workaround: any other source in the registry auth chain — a PAT in
   `GHCR_TOKEN`, a docker keychain entry with `read:packages`, or the cluster's pull secret
   (config `registries[].cluster`), which is what worked during design.
2. **A Docker Desktop keychain entry can exist and still fail.** `~/.docker/config.json` with
   `credsStore: desktop` may hold a GHCR credential whose scopes don't cover tag listing; the error
   is the same as (1). Don't conclude the chain is broken from one source failing — hoist tries
   the next and reports which one succeeded.
3. **GHCR tag lists are unordered and carry no timestamps.** `tags/list` returns `v…`, `sha-…`,
   branch names and `latest` interleaved; sorting by date needs the config blob per tag (index →
   platform manifest → config), which hoist caches by digest. Prefer the app repo's git tags for
   ordering when the image repo is mapped.
4. **`mise` shim for `go` fails outside a pinned directory** — see §6. `go version` in a random
   shell prints `No version is set for shim: go`; it is not a broken install.

## 7. CI & Deployment

`.github/workflows/ci.yml` runs on every push to `main` and every pull request, with **no path
filters**: `test` (`go vet`, `go build`, `go test -count=1 -race ./...`), `lint` (golangci-lint v2)
and `public-safety` (`scripts/public-safety.sh`, §4.4). All three are required for merge once
branch protection exists — it does not yet; until then the merge gate is the operator go-ahead in
§8 plus a green rollup you have read job-by-job. Renovate (`renovate.json5`) opens grouped
dependency PRs on a weekly schedule; it is scheduled tooling, never a PR gate.

Deployment is `go install github.com/abradner/hoist/cmd/hoist@latest` from `main`. Tagged releases
with prebuilt binaries (goreleaser, Homebrew tap) are planned for after M7 and nothing should be
built for them until then.

Two universal cautions, whatever the pipeline:

- **A skipped job is not a passing job.** A green run where path filtering skipped half the suite
  means that half never saw the commit. Check what actually ran before trusting the check.
- **A test job that cannot distinguish "everything passed" from "nothing ran" is not a gate.**
  When a test runner is swapped, changing the local command is half the job: read what CI actually
  invokes and confirm the run count it reports is non-zero. (A generated workflow in a sibling
  repo kept running the abandoned runner against an empty directory — green on every push while
  executing zero examples, because "no tests found" was a success to it.) Applies equally to any
  generated pipeline adopted wholesale.
- **A gate fails only for reasons inside the diff.** Dependency currency belongs to scheduled
  tooling (dependabot, audit jobs), not to a PR gate — nothing should be able to redden a PR that
  changed no dependencies, on a schedule set by someone else's release cadence. A gate that cries
  wolf gets ignored, including on the day it is right. (Instance: a generated scanner wrapper
  passed `--ensure-latest`, so an upstream release turned every PR in a sibling repo red while
  reporting a version fact through the channel reserved for security failures.)
- **A merge is not a release** — if images/artifacts build from tags only, merged work has no
  deployable artifact until a tag exists. Today `go install …@latest` builds from `main`, so a
  merge *is* the release for anyone installing that way; when tagged releases arrive, this line
  becomes literally true and the release step gets its own approval (batch-review Phase 8).

## 8. Working Rules

The portable core. These rules are pre-filled from hard-won convention across this repo's sibling
projects; adjust only with reason, and record the reason (see §10).

### Planning & approval

- Propose an implementation plan for any moderately or highly complex change, and get it reviewed
  and approved before making edits. Don't dive into large builds or refactors on your own read of
  the situation.
- For anything genuinely ambiguous or not yet decided by the operator, prefer the reversible
  option and leave a clear marker rather than picking silently. Two-way doors over one-way doors.
- **Building structure where no convention is stated is a decision, not a default.** When a change
  introduces more than trivial structure (a new page/screen, a new service layer, a new module
  shape) and §4 states no convention for that surface, do not silently invent one — and do not
  treat "match the surrounding code" as cover: over undesigned code that instruction *propagates*
  the blur. Adopt a pattern, then name it in the PR body as an explicit proposal ("no stated
  convention for X; this PR uses Y; codify or correct"). The operator's review then decides a
  standard once, instead of re-litigating shape per PR. (Motivating instance: a sibling repo's
  first UI-heavy PR mixed layout, scaffolding, and domain components with no container/presenter
  separation — the agent had nothing to follow, faithfully extended the existing blur, and the
  standard got stated for the first time as a review comment, the most expensive place to state
  it.)

### Landing changes

- **Every change lands through a pull request** — including one-line copy tweaks. Size is not the
  criterion. (Motivating instance: a few commits went straight to `main` during a sibling repo's
  init. Automated reviewers fire on the ready-for-review edge, so a direct push is a change nothing
  reviewed, and `main`'s history stops meaning "reviewed states".)
- Opening a PR is not merging it. Push permission is granted per session — ask once before the
  first push unless already told.

### Destructive & outward-facing actions

- Destructive actions (dropping data, deleting files you didn't just create, rewriting published
  history) and outward-facing actions (publishing, sending, deploying) need explicit approval,
  every time. Approval for one instance is not approval for the next.
- **Merging is gated on a live go-ahead, and every new session starts assuming the manual gate.**
  A completed review, green CI, a finished followup, and an auto-mode session default are none of
  them authorization. Report readiness and stop.
  - A **conditional, forward-looking** go-ahead is still a go-ahead: "merge this train once #38 is
    resolved" names the batch and states the condition, and covers merging while the operator is
    away. It authorizes *that* batch only.
  - A **session-scoped carve-out** ("auto-merge only trivial mechanical PRs tonight") is valid when
    the operator sets it — at session start, as their own policy choice. Treat it as a one-session
    precedent and ask again next time; anything touching auth, scopes, custody, or an API contract
    stacks and waits regardless.
  - Ambiguous continuations ("continue the stack flow") are **not** authorization. Ask briefly,
    citing this rule so it doesn't read as timidity.
  - **Do not propose loosening this.** The operator's position is that they would like to soften it
    eventually but that harnesses are not reliable enough yet. Offering options is fine when they
    are the one setting session policy; advocating for less gating mid-stream is not.
- Never force-push or rewrite published history without an explicit, current go-ahead.
- **Visibility and content are separate axes.** Before making anything public, do not let a narrow
  confirmation (repo name, a visibility toggle) stand in for consent to the *content* — git
  history, comments, and design docs go out too. Separately flag anything describing a
  still-unfixed vulnerability or written more candidly than the operator likely pictured, and ask
  about that specifically. (A sibling repo was pushed public with candid commit messages
  documenting exact vulnerabilities, one of them still live in the code at push time; it had to be
  flipped back to private immediately.)
- If you encounter a violation of a safety rule already committed (a plaintext secret, a
  destructive migration lying in wait), flag it immediately — finding it is not the same as
  having caused it, and silence helps nobody.

### Tooling version floors

- **A version floor is an interrupt, not a workaround.** When a skill or workflow depends on
  tooling at or above some version and the environment is below it, stop and tell the operator
  what to upgrade. Do not silently take a degraded path, reimplement the missing capability by
  hand, or work around it — the operator can fix an install in seconds, and the workaround is
  what ends up load-bearing and unreviewed.
- Distinguish **fixable** from **unavailable**. A missing or outdated tool is fixable: interrupt.
  A capability the platform genuinely doesn't offer here — wrong host, feature not enabled for
  this repo, a documented limit — is unavailable: take the documented fallback and record why.
- Name the floor and the exact upgrade command when you interrupt. "Your `gh` is too old" costs
  the operator a search; "`gh extension upgrade stack` — `merge` landed in v0.1.0" does not.
- The same applies mid-run: if tooling turns out to be below the floor after work has started,
  stop and say so rather than finishing on the degraded path and reporting success.
- **Adding a dependency can raise the project's floor without asking.** Package managers resolve a
  new dep's own requirements by bumping yours. After any dependency add, read the manifest diff for
  the language/toolchain lines specifically; if a dep forced a bump, pin the *dep* to the newest
  version whose floor matches the repo rather than raising the repo. A toolchain bump touches the
  production image and is a deliberate decision, not a side effect of installing something.
  (Instance: `go get <dep>@latest` rewrote `go.mod`'s Go version and deleted the `toolchain` pin,
  breaking the Docker build against a pinned base image. The `test` job still passed — only `build`
  caught it.)

### Configuration

- Fail fast on boot: never provide fallback defaults when reading *required* configuration.
  Missing required config must raise at startup rather than silently degrade. Defaults are fine
  for genuinely optional tuning knobs. (This isn't hypothetical: a sibling repo baked a stand-in
  value into an image-wide ENV to make a build step pass, which would have silently defeated this
  rule in production. If a build step needs a stand-in, scope it to that step, never image-wide.)
- The same explicitness applies to what the code writes: anything persisted that holds data states
  its permissions explicitly rather than inheriting the process umask. (Database dumps in a
  sibling repo landed world-readable because the dump path never asserted a mode.)

### Review feedback

- **A finding is a claim, not a verdict.** Whether it comes from a bot, a human, or your own
  earlier session: trace or reproduce it before acting on it. Ask whether the flagged path is
  actually reachable. When a finding says code and docs disagree, work out which end is wrong
  before "fixing" either. Declining findings has to actually happen — a round that accepts every
  finding is a warning sign, not a good score.
- **Verify a delegated claim against the artifact before relaying or acting on it.** Read the
  diff, grep the branch, run the command — a subagent's report is a claim like any other. (Two
  agents once filed contradictory security reports and *both were correct about different
  artifacts*; only diffing them resolved it, and doing so exposed a real defect — a branch that
  deleted a control introduced by the PR below it, which the final merge accidentally restored.
  Merged-result-correct but per-PR-wrong is exactly what a reviewer catches and loses trust over.
  Separately, an agent has returned a placeholder summary while having done complete, correct work:
  believing the report would have discarded it.)
- Reject suggestions that violate the rules in this file, and say why. Automated reviewers read
  this file too; that's expected — reviewer-side agents should review fully as normal, and rules
  here that bind only author-side agents say so explicitly.

### Testing & verification

- **Verify the output, not the instrument.** Green means nothing threw, and nothing more. Before
  claiming something works, name the artifact it should have produced and go look at it — the
  rendered page, the written file, the actual rows. A tool reporting on itself is not the
  artifact: `git bundle verify` once passed on backup bundles that could not actually be cloned,
  because the verifier checks internal structure, not that the bundle can reconstitute a repo —
  the suite that replaced it performs a real clone.
- **Prove a new test can fail.** For any bug fix: write the regression test, confirm it passes
  with the fix, revert just the fix, confirm the test fails for the right reason, restore the fix.
  A test that was never seen to fail hasn't proven anything. This is part of writing the fix, not
  review debt to defer — one reviewer's "add regression coverage for your own fix" finding recurred
  three times across a single PR series and was right every time. And the mutated code must
  *compile*: a build-failed mutant proves nothing (re-learned three times in one night).
- **For any isolation or authorization test, name the attacker and write *their* request.** A spec
  that asserts the mechanism's own definition back at itself cannot fail for any input — it reads
  like coverage and gates nothing. If you cannot describe an input that would make the assertion
  fail, the test is documentation, not a gate. (A tenancy spec built its record through the very
  scope it claimed to test; the actual cross-tenant hole sat unnoticed until a reviewer read the
  middleware and the controller together.)
- **Inference from a plausible nearby cause is not diagnosis.** During a platform outage, a red
  main was attributed to the outage's known error; the outage was real but unrelated, and the
  actual job failure had been there all along. Wait for the real signal and read the actual
  failure before naming a cause.
- **Watch the setup, not just the assertion.** Four tests in one feature passed against genuinely
  broken code because their setup could never exercise the branch — an assertion on node count
  only, a fixed `now` so the fade never completed either way, a payload rejected as malformed
  before its size mattered. Also beware a test that captures current behavior so faithfully it
  archives the defect.
- Ask what else satisfies your assertion — a count-based check that an empty-state row also
  matches, a visibility check that passes for a scrolled-away element. When asserting absence,
  include a positive control so a broken probe can't read as success.
- Every conditional branch that encodes real logic gets a test that exercises it — especially the
  rare/edge branches, not just the happy path.

### Git hygiene in shared checkouts

- **Create branches only from an explicit start point** (`git checkout -B <name> origin/main`) when
  anything else might be operating in the same checkout, and check `git branch --show-current`
  before any operation that depends on HEAD. (A "one-line docs PR" silently inherited five feature
  commits from an in-repo builder agent's branch and was reviewed in that state. "Only one builder
  running" is not "only one git user"; prefer worktree isolation for delegated builders.)
- **Never pair `git stash` with a later `pop` unless the stash verifiably created an entry.** A
  stash on a clean tree stashes nothing, and the paired pop then pops whatever stranger's entry was
  on top of a stack you don't control. For "test against a clean copy", use `git show REV:path` or
  a scratch worktree instead of stashing at all.

### Layered checks

- Apply the **deletion test** to any validation that exists in more than one place: enforcement
  exists exactly once; anything layered on top must, if deleted, change only politeness (a
  friendly error instead of an ugly one), never possibility. If deleting either check would make a
  new state possible, you have enforcement in two shapes, and they will drift.

### Documentation & discovered work

- Docs describing a boundary or behavior change in the same PR as the change. A doc describing a
  boundary that no longer exists is worse than none, because it is trusted.
- Log discovered work (bugs found in passing, deferred improvements) in the external tracker —
  never a PR body or a code comment. A triage table in a PR body goes stale between rounds and
  vanishes on merge.
- When you fix a subtle bug or get burned by a non-obvious behavior, write the general form of the
  lesson into §9 before moving on — same session, not later.
- **A review comment that names a missing standard forks.** "We need a convention for X" /
  "this pattern was never established" is not an instance finding — resolving it means both
  fixing the instance *and* codifying the standard into §4 (or the doc §4 points to) in the same
  round; if the standard needs real design time, ticket it in the tracker with the review link.
  A convention stated only in a review thread doesn't exist for the next session — the thread
  dies with the PR, and the next agent re-improvises. (This is §9's ratchet, aimed at
  conventions: incidents accrete into gotchas, review-discovered standards accrete into §4.)

### Tooling

- Use the agent's structured file tools rather than `cat`/`sed`/shell here-docs for inspecting
  and modifying files during a session. (Scope: this governs session file operations; committed
  shell scripts do what shell scripts do.)

### Which shipping flow

- The default is one PR at a time: react to feedback immediately, merge when green. Use
  `.claude/skills/single-pr` — it makes that default rigorous rather than merely simple.
- Several PRs open together as one body of work is a different regime: use
  `.claude/skills/batch-review` (fan out, feedback write-only until synthesis, one followup PR).
  It is opt-in for genuine multi-PR fan-outs, not a replacement for the default. The tell is
  reviewer attention fragmenting across live threads, not the size of the diff.
- Both skills read `docs/pr-review-machinery.md` for the parts that don't differ between them —
  reviewer triggers, the three-surface comment harvest, triage, the round cap, and the green-signal
  traps. It is the canonical copy; don't restate it in a skill, and don't let a skill contradict it.
- When more than one branch is in flight against the same code — or any branch went through an
  agent-performed merge or rebase — run `.claude/skills/stack-integration-check` before opening
  PRs. Per-branch review is structurally blind to what happens between branches; this is the check
  that runs on the combination.

### Context & compaction

- When the operator signals they are near the context limit and about to compact, use
  `.claude/skills/park-context` rather than improvising a summary. Compaction keeps a paraphrase
  and discards the transcript, so the park writes only what compaction destroys — intent, rejected
  alternatives, what was actually verified versus assumed, what was mid-flight when the turn was
  cut — and never the diff, which is reconstructible.
- Do not finish work, commit, or push while parking. Parking is triggered by interrupting a live
  turn, so the tree may hold a partial edit nobody intended; record it as observed and stop.
- Resuming from a handoff uses `.claude/skills/resume-context`. The handoff is a claim, not a
  verdict (see Review feedback above) — the session that wrote it is gone and cannot be asked what
  it meant. Verify its state claims against the repo and report drift before building on them.
- Durable lessons never live in a handoff. They land in §9 / §6.1, or in the external tracker,
  before the work merges — handoff files are gitignored local state and get deleted.

## 9. Gotchas & Lessons Learned

Numbered, accreting. Add to it rather than rewriting it — the numbering is stable so entries can
be cited from code comments and PR descriptions.

Entry shape: **what happened → the actual root cause → the general rule → where the regression
test lives** (if one exists).

*No entries yet.*

## 10. Maintaining This Document

Meta-rules for editing this file — they exist because each was violated somewhere first:

1. **A rule everyone knows is wrong is worse than no rule** — it teaches agents that the rules are
   decorative, which devalues the ones that are load-bearing. When reality has moved, revise the
   rule openly: state what the old rule said, why it was the wrong shape, and what replaces it.
2. **Ground rules in incidents.** When adding a rule, say what happened. When correcting a wrong
   rule, record the correction in-place so the old rule doesn't get quietly reintroduced.
3. **Scope absolutes.** Name the exceptions at authoring time, or agents will either violate the
   rule or over-apply it.
4. **Name the audience** when a rule doesn't bind every reader — author-side agents, reviewer
   bots, and humans all read this same file.
5. **Prefer structural guarantees to written claims.** If a rule can be made mechanically true (a
   build-context exclusion, a CI gate, an import stub instead of a second copy), do that and
   document the mechanism, rather than adding another sentence.
6. **Expect to compress.** First-draft steering prose runs vague and grandiose; cut it to concrete
   nouns and commands the same day. When compressing an established doc, treat it like a refactor:
   diff the imperatives before/after and confirm nothing operative was dropped.
7. **Porting rules between sibling repos is judgment, not copying.** Verify each candidate rule
   against this repo's own files, CI, and conventions; adopt, adapt, or reject explicitly — and
   note rejections in the Adaptation Record below. Watch especially for rules that silently assume
   another repo's merge strategy, bot roster, or deploy shape.

## 11. Adaptation Record

What was pruned or changed from the keel template when this repo was initialized, and why — kept
in the doc (not just a commit message) so future readers don't need git archaeology to know what
was deliberately excluded.

Initialized 2026-09-02 from `abradner/keel` at `0e3a4e80d3defd478a226a2332d547f462eacc1b`, by an
agent session working from a plan the operator had already approved, with the interview answered
up front.

**Kept:** repo map (real trust boundaries: approval comment → production merge, registry credential
chain, kubeconfig, public output surfaces); every shipped skill — `single-pr` (the default flow),
`batch-review` + `stack-integration-check` (milestones may ship as a stack), `competition-build`
(the approval-author check and the credential chain are genuine trust-boundary work; it was not in
the plan because it post-dates the local keel checkout, kept under "when in doubt, keep"),
`independent-commit-review`, `park-context`/`resume-context`, `dependabot-sweep` (Renovate is
configured for gomod + github-actions); `docs/pr-review-machinery.md`; caveman tone module, fanned
out and appended below; Apache-2.0 license.

**Pruned:** `docs/rails-prometheus-metrics.md` — not a Rails service. `[MERGE-COMMIT]` variants in
`batch-review` — squash everywhere. The Template lineage block — public repo; lineage lives in the
fleet registry.

**Adapted:** the reviewer roster in `docs/pr-review-machinery.md` and `batch-review` records the
operator's description of the bots' real triggers (Copilot on ready/re-request, Codex only when
@-mentioned) and is marked unconfirmed until the first PR. §6.1 was **seeded, not left empty** —
the design session had already hit four host/registry traps, and leaving them out would omit the
ones most likely to bite first (asn-infra made the same call at its init). Principle 1 kept
verbatim; principles 2–7 are hoist's own.

**Added beyond the template:** an *Output Style* section (below) adapted from
`ayghri/i-have-adhd` at `58494af57962b2d7a996b4d419474380a299af5e` (MIT), vendored verbatim at
`.claude/skills/i-have-adhd/SKILL.md` and installed as a user-level Claude Code plugin on the
operator's machine. Adaptations: progress restated as milestone + step; the two-minute next action
defaults to a test or dry-run invocation; time estimates aimed at the executing agent; the
five-item cap yields to §9 and tables; an explicit precedence rule against caveman mode. Also
added: `scripts/public-safety.sh` + CI job (§4.4), `renovate.json5`, `.github/workflows/ci.yml`.

**Declined:** building on `github.com/abradner/workflow` (Temporal). Its embedded engine is
in-memory and loses state on exit, which fails the resume requirement outright; external mode makes
a Temporal server and worker a prerequisite for a single-user TUI and pulls the Temporal server
module tree into a public binary. Every hoist wait is on an externally observable fact, so
re-observation gives resume for free. `pkg/` stays activity-shaped (§4.3) so the decision is
reversible. GitLab support: interface only, no adaptor, until a GitLab repo needs it.

## Caveman Mode

Respond terse like smart caveman. All technical substance stay. Only fluff die.

Rules:
- Drop: articles (a/an/the), filler (just/really/basically), pleasantries, hedging
- Fragments OK. Short synonyms. Technical terms exact. Code unchanged.
- Pattern: [thing] [action] [reason]. [next step].
- Not: "Sure! I'd be happy to help you with that."
- Yes: "Bug in auth middleware. Fix:"

Switch level: /caveman lite|full|ultra|wenyan
Stop: "stop caveman" or "normal mode"

Auto-Clarity: drop caveman for security warnings, irreversible actions, user confused. Resume after.

Boundaries: code/commits/PRs written normal.


## Output style

The reader has ADHD. Shape every response so it can be acted on (rules adapted from
https://github.com/ayghri/i-have-adhd, MIT; the canonical skill is vendored at
`.claude/skills/i-have-adhd/SKILL.md`):

1. Lead with the answer or next action: command, path, or snippet first.
2. Number multi-step work; one bounded action per step.
3. End with one next action doable in under two minutes — here that is usually a
   `go test ./pkg/...` or `hoist … --dry-run` invocation.
4. Finish the current issue before raising a new one.
5. Restate progress each turn as milestone and step ("M3, step 2 of 6 done").
6. Give time estimates in concrete units, aimed at whoever executes — usually the agent.
7. After a change, show what now works.
8. Errors: state location, cause, and fix. No drama.
9. Cap lists at 5 items — except AGENTS.md §9 gotchas and tables, where completeness is the point.
10. No preamble, no recaps, no closers.

Exceptions: explain fully when asked to explain. Confirm before destructive or outward-facing
actions (AGENTS.md §8). After three failed fixes, stop and name the doubtful assumption. If the
request is ambiguous, ask one short question.

Precedence with caveman mode: caveman compresses *words*, this shapes *structure*. Where they
collide — caveman's "no tool-call narration" against rule 5 — the one-line progress restatement
wins; everything else stays caveman.
