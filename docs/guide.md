# hoist — the user guide

The README is the pitch and the reference. This is the thing you read when hoist has just refused
to do something, or stopped, and you need to know whether that is a bug or the point. Every screen
below is rendered from the test fixture, which is why the images are `ghcr.io/example/…` and the
environments have placeholder names; the shapes are exact.

Contents: [keys at a glance](#keys-at-a-glance) · [the matrix](#the-matrix) · [promote a
pair](#promote-a-pair) · [deploy a build](#deploy-a-build) · [restart a family](#restart-a-family)
· [when it stops](#when-it-stops) · [resuming](#resuming) · [registry
credentials](#registry-credentials) · [direct mode](#direct-mode)

Three keys matter everywhere. `?` (help) works on every screen: it opens an overlay naming every
key the current screen honours (`esc`, `?` or `enter` closes it). `q` quits hoist from the matrix
only (with a confirm if a drive is still running); pressed anywhere else it does nothing but
remind you to go back first. `l` opens the activity log — every result and error this session has
recorded, oldest first, nothing truncated — on the screens that list it (see the table below).
`ctrl+c` quits immediately from any screen, no confirm — except while the `q` quit-confirm dialog
is open, where it is swallowed like every other key — and on the way out names every promotion
still in flight with its own `hoist resume <id>`.

## Keys at a glance

Every key hoist honours, one meaning per letter across every screen it appears on
(`internal/ui/keys`, the one registry the help overlay and this table both draw from — a write
that starts or confirms a remote change always shows as `shift+<letter>`, never a bare capital,
since a legacy terminal cannot tell a real shift from caps lock pressed by mistake):

| Key | Meaning | Screens |
|---|---|---|
| `enter` | the one primary action — open the menu, run the highlighted item, start a promotion/deploy/restart, review a tag | every screen except flight, watch, config, activity |
| `esc` | back — never cancels a drive that's running | every screen |
| `?` | help overlay | every screen |
| `l` | activity log | matrix, plan, deploy, tags, flight, watch, restart, config |
| `q` | quit; only from the bare matrix (confirms if a drive is running) | matrix |
| `ctrl+c` | quit immediately, no confirm (swallowed while the `q` confirm is open) | every screen |
| `r` / `F5` / `ctrl+r` | re-observe / refresh / reload / rebuild from origin | matrix, plan, deploy, flight, watch, restart, tags |
| `o` | open the PR (chooser if several are in flight) | matrix, flight |
| `p` | promote into the cursor's column | matrix, menu |
| `t` | deploy a tag | matrix, menu |
| `w` | watch the family/target | matrix, menu, flight |
| `c` | read-only config view | matrix |
| `d` | toggle the yaml diff | plan, deploy |
| `e` | override the digest by hand | plan |
| `/` | filter | plan, tags |
| `space` | tick/untick a repo | plan |
| `tab` | switch pane (table ⇄ in-flight, repos ⇄ impact, list ⇄ commits) | matrix, plan, tags |
| `shift+r` | restart the family (write; the restart screen's own confirm is `enter`) | matrix, menu |
| `shift+x` | abandon (write) | matrix, flight |
| `shift+d` | toggle direct mode (write) | plan, deploy |
| `shift+c` | override a `ci.none: prompt` block (write) | flight |
| ↑↓←→, `pgup`/`pgdn`, `home`/`end` | move / scroll / page | every list or viewport |

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
╭─ hoist · matrix · my-gitops ─────────────────────────────────────────────────╮
│my-gitops · base main                                                         │
├──────────────────────────────────────────────────────────────────────────────┤
│─────────────┬───────────────────────────────┬─────────────────────────────── │
│ FAMILY      │ ▸ APP-STAGING                 │ APP-PRODUCTION ⚠               │
│─────────────┼───────────────────────────────┼─────────────────────────────── │
│ marketing   │ sha256:aaaaaaaaaaaa    pinned │ sha256:aaaaaaaaaaaa    pinned  │
│ ▸ orders    │ v202602201200          pinned │ v202601151010          pinned  │
│ temporal    │ 2 images             external │ 2 images             external  │
│ web         │ 2 versions              split │ v202601010101          pinned  │
│ worker      │ v3                   unpinned │ v2                   unpinned  │
│             │                               │                                │
│             │                               │                                │
│             │                               │                                │
│             │                               │                                │
│             │                               │                                │
├──────────────────────────────────────────────────────────────────────────────┤
│in flight · 1                                                                 │
│  5pr6sd333t   app-staging → app-production   started 12m ago                 │
│✓ branch  ✓ commit  ✓ push  ✓ PR #103  ✓ CI  ⏸ approval  · merge              │
│· argo refresh  · argo sync  · rollout                                        │
│waiting for an approver to comment `hoist approve 5pr6sd333t` on PR #103      │
╰──────────────────────────────────────────────────────────────────────────────╯
  enter actions · p promote into · t tag · w watch · r refresh · q quit · ? more
```

(This and every other frame below is abridged from `testdata/golden/*-mockup-80x24.txt` — the same
`internal/app/plan.mockupPlanModel`-style fixtures the screens' own golden tests render, built with
realistic names for docs rather than the `ghcr.io/example/…` placeholder fixture the walkthrough
below uses, then trimmed or annotated for the page. Not every screen shown has its own mockup
golden yet, so treat these as illustrative rather than byte-exact. A screen's own golden test
failing is a signal this guide may have gone stale, not a guarantee it hasn't.)

The words:

| Word | Meaning |
|---|---|
| `pinned` | every reference in the cell carries a digest (`tag@sha256:…`) |
| `unpinned` | at least one is a bare tag — a moved tag is invisible to `imagePullPolicy: IfNotPresent` |
| `split` | one image repo declared at two different versions in the same env; a promotion out of here picks one (the pinned one if exactly one is pinned, else the most common) and warns that it had to choose |
| `external` | third-party images (outside the `promotable` prefixes); shown, never written |
| `drifted` | the cluster is running a build the manifest does not declare — with the running reference named under the table |
| `resolving…` | the cluster has not answered for that column yet |

Columns come in pipeline order (the chain `envs.pairs` describes, source before target — an
unpaired env sorts alphabetically among the heads), not alphabetically, and the cursor starts on
the first non-production column so a fresh session never opens pointed at a write that asks for
approval. `←`/`→` move the environment cursor (`▸`), `↑`/`↓` the family; the column under the
cursor is what `p`, `t`, `w` and `shift+r` act on. A production column is marked `⚠` in its
header and named under the table. `enter` opens an action menu for the cell under the cursor,
listing what each key would do there — including "promote into … from…" when the source is
ambiguous — so a letter never has to be guessed. `r`/`F5`/`ctrl+r` re-read origin/`<base>` and
ask the cluster again — the same re-read that makes `w`, `shift+r` and `t` see a family or a
cluster fix moments after it lands, not just after a restart. When something is promoting, it is
listed under the table (the "in flight" pane in the mockup above) with its step strip and — when it
is waiting on you — the exact command; the pane is drawn inside the matrix's own frame rather than
a separate box stacked under it, so it ends at the same closing border the table does.

That list is re-observed against GitHub and the cluster at boot and on every poll, never read
from a log; `tab` moves the cursor onto the pane (`↑`/`↓` between several), where `enter` reopens
the one under the cursor on its flight screen and `shift+x` abandons it behind a confirm; `o`
opens its PR and asks which when several are in flight. A promotion this session itself started
or resumed is marked "driving" — it is still running here, in this process, whether or not any
screen is currently watching it; `tab`+`enter` on one of those re-attaches the flight screen to
it without starting anything new.

Whatever last happened — a promotion started, landed, blocked or failed, an abandon, a browser
launch that couldn't find a browser — shows on the terminal's last line: one line, naming the
result and how many entries are on record, for example `abcd1234 landed · l: activity (4)`. It is
never cleared by pressing another key (a real refusal that vanished the moment you pressed `j` used
to be unreadable except by re-running the equivalent CLI command) — only a newer event replaces it,
or `l` opens the full activity log: every entry this session has seen, oldest first, with nothing
truncated — a multi-line transport error is there in full, not just its first line. `esc` closes
it. `h`/`l` used to alias `←`/`→` on the matrix; they no longer do, since `l` now opens this log
everywhere it is bound.

## Promote a pair

`p` promotes INTO the cursor column: the source is the one reverse pair (`envs.pairs` in the
config) when exactly one exists, or the plan screen asks "promote into `<target>` from…" when
there is none or several (a fan-in). The CLI is the same operation:

```bash
hoist plan    --from app-staging --to app-production --dry-run
hoist promote --from app-staging --to app-production
```

**What the CLI prints while it works.** `promote`, `deploy` and `resume` say what they are doing
on stderr, one line as each phase starts and one as each write finishes; stdout stays the final
summary, so a script reading it sees what it always did:

```
hoist: resolving what app-staging runs to digests (pods, manifest, registry)
hoist: checking your checkout against origin/main
hoist: claiming app-production and checking for a conflicting promotion
hoist: saving promotion state
hoist: branched: creating branch hoist/app-production/k3v9q2x7ab in its own worktree
hoist: branched: done
hoist: committed: committing
hoist promote: waiting for signing approval...
hoist: committed: done, commit 4f0c…
hoist: pushed: pushing hoist/app-production/k3v9q2x7ab
hoist: pushed: done
hoist: pr-opened: opening the pull request
hoist: pr-opened: done, https://github.com/me/my-gitops/pull/42
hoist: ci-green: waiting: CI: 1/3 checks complete
```

The PR's URL is on the line that opens it. A phase that runs 15 seconds with nothing else to say
repeats itself with a clock — `hoist: still pushed: pushing … (15s so far)` — so a slow push or
a signing prompt you have not answered never looks like a hang; a wait on someone else (CI, an
approval, Argo, a rollout) prints when its reason changes and then every ten minutes. `--quiet`
drops the progress lines and keeps the waits, the signing notice, the approval instructions and
every error. `promotions` and `abandon` name what they are re-observing the same way, and take
`--quiet` too.

**What it plans.** For every first-party image repo the target already carries, the build the
source environment is running: its pods first, then the manifest's own pin, then a registry lookup
of the tag, in `digest_sources` order. A repo the target does not carry is listed as untouched, not
added. A bare tag that nothing can pin is refused — hoist never writes a tag without a digest.

**The confirm screen** leads with what ships, not with the bytes:

```
╭─ hoist · promotion · confirm ────────────────────────────────────────────────╮
│app-staging  →  app-production   images under ghcr.io/e… mode: PR · production│
│3 repos ticked · 41 commits · 3 migrations · 3 image referenc… d  see the yaml│
├──────────────────────────────────────────────────────────────────────────────┤
│┃   ✓ marketi…  → sha-1a2b3c4d5e6f  │orders                                   │
│┃ > ✓ orders  → v2026022012         │v2026011510 → v2026022012                │
│┃   ✓ web  → v2026021509            │1 image reference · 1 file · digest from │
│┃                                   │pods                                     │
│┃                                   │                                         │
│┃                                   │v2026022012 is 18 commits ahead of       │
│┃                                   │v2026011510 · migrations not tracked for │
│┃                                   │this app                                 │
│┃                                   │                                         │
│  · worker · already current        │                                         │
╰──────────────────────────────────────────────────────────────────────────────╯
    enter promote · space tick · d yaml · e digest · r fresh · esc back · ? more
```

The totals line above the columns names what the ticked set actually ships: repos, commits,
migrations, and the image references and files this promotion would write. A row already at the
target's declared reference is shown greyed with no checkbox at all — `· worker · already
current` — since ticking it would write nothing. Left: the repos, ticked (`space` untick one to
leave it out; `!` marks a repo with a warning). Right: the repo under the cursor — the versions in
full, where its digest came from, its warnings as plain sentences, and, when `repos[].apps` maps
the image to its source repo, the commits between what the target declares and what is about to be
written, with any migration among them — `orders` above has no app mapping to walk, so it reports
"migrations not tracked for this app" rather than guessing. `d` swaps the right pane for the YAML
diff; `enter` means the same from either view. `e` opens a dialog to override the hovered repo's
digest by hand (the CLI's `--digest`, validated the same way). `shift+d` switches to direct mode on
a non-production target (see [direct mode](#direct-mode)); it is not offered at all on a production
target, exactly as this frame's footer shows no `shift+d` at all — the
key does nothing there, rather than refusing with a notice. `r`/`F5`/`ctrl+r` rebuild the plan at
fresh origin, the same fetch `F5` already runs on the matrix.

**What happens after enter**, in order: a worktree under `$XDG_CACHE_HOME/hoist/worktrees/<id>`
from your own clone, a branch `hoist/<env>/<id>`, one commit with only the image lines changed
(verified structurally before `git add`), a push, a PR whose body carries the same warnings, then
waiting — for CI, for approval on a production target, for the merge, for Argo to sync, for every
Deployment the commit touched to roll out. The flight screen shows each step and what the current
one is waiting for:

```
╭─ hoist · promotion · waiting for approval ───────────────────────────────────╮
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
│waiting for an approver to comment `hoist approve abcd1234` on PR #103 · o    │
│opens it                                                                      │
╰──────────────────────────────────────────────────────────────────────────────╯
esc back · o open PR · w watch · l log · shift+x abandon · r re-observe · ? help
```

The title itself names the state (`waiting for approval`, `blocked`, `done`, …), so it never
disagrees with what the body says. `r` re-observes now instead of at the next poll. `w` watches
this promotion's own family and target on the read-only watch screen (the same view `w` on the
matrix opens for a cell) — it asks which family first when the promotion touches more than one.
`esc` leaves this screen — it stops *watching* only; the drive itself keeps running exactly as it
was (branch, commit, push, PR, merge, Argo, rollout — whatever step is next still happens), and
`tab` to focus the matrix's own in-flight pane below, then `enter` on the row, re-attaches to the
exact same running drive later, never starting a second one. `shift+x` (a write, kept out of reach of a mistyped key)
*abandons* it instead: behind a confirm, it retires the state file and, if it opened a PR, closes
it and deletes the branch. It refuses outright if the promotion has already landed — abandoning is
not a rollback, so a landed one needs `hoist deploy` or a fresh promotion to undo, not a
state-file delete — and it is never offered at all once the promotion is done. `l` shows the
activity log here too, not only from the matrix. `q` quits hoist, but only from the matrix:
pressed here it does nothing but remind you ("q quits from the matrix · esc goes back") — `esc`
back to the matrix first, then `q` there. With any drive this session started still Building,
Stepping or Waiting, quitting from the matrix asks first, behind a confirm — nothing is rolled
back, `hoist resume` picks every one back up later, but it is a deliberate step rather than a
silent one. A promotion merely listed on the pane (started earlier, by this session or another)
does not count toward that: nothing here is driving it, so quitting does not interrupt anything
already in flight for it. `ctrl+c` is the one always-immediate quit, from any screen, with no
confirm — state is durable either way. The CLI prints the same steps as lines, with `waiting: …`
naming what it is waiting for; `hoist abandon <id> --confirm-abandon=<id>` is `shift+x`'s own CLI
form (the repeated id is the confirmation, same shape as `--confirm-direct`).

The promotion's id (`abcd1234` above) is a hash of the repo, the target env and the digest set.
It names the branch, the PR body marker, the commit trailer and the approval token — so running
the identical promotion twice finds the first one's branch and PR rather than opening a second.

## Deploy a build

`t` on a cell (a chooser first, when the family runs several first-party images) opens the tag
picker for that image in the cursor column:

```
╭─ hoist · tags · app-staging ─────────────────────────────────────────────────╮
│ghcr.io/example/app  →  app-staging                                           │
│app-staging declares  v1 · 111111111111 · since 4 weeks ago                   │
├──────────────────────────────────────────────────────────────────────────────┤
│   TAG          BUILT          DIGEST                                         │
│▸ v3           3 days ago     333333333333                                    │
│  v2           4 weeks ago    222222222222                                    │
│  v1           2 months ago   111111111111  ◂ declared here                   │
│── digest tags (sha-…): builds named by hash ──                               │
│  sha-3333333  3 days ago     333333333333                                    │
│── moving tags (latest, branches): not releases ──                            │
│  latest       3 days ago     333333333333                                    │
├──────────────────────────────────────────────────────────────────────────────┤
│v3 is 14 commits ahead of v1 · 2 migrations                                   │
│  4a1c2ef  Add rate limiting to the public API                                │
│  e9b0d31  Fix N+1 query when resolving digests                               │
│                                                             ↓ 12 more commits│
╰──────────────────────────────────────────────────────────────────────────────╯
 enter review v3 · → read · / filter · r reload · l activity · esc back · ? help
```

The header says what the environment *declares* (the manifest, dated by when that line last
changed) — never "runs", because this screen does not read the cluster; on a production target the
header instead warns when the tag under the cursor has not been committed in the paired staging
environment yet (`testdata/golden/tags-production-80x24.txt`). The list is grouped since no
registry marks a tag's kind (`tags.Classify`): releases lead with no divider above them, then
digest-named tags (`sha-…`), then moving tags (`latest`, branch names) each under their own
divider — nothing is hidden, and `/` filters across all three. Each tag has its build age and,
under the cursor, the commits between the declared build and that tag with any migration marked.
`enter` reviews the change — this screen's one primary action; `tab` moves focus into the commit
list, then `→` reads one in full (`pgdn`/`pgup` page its body, `home`/`end` jump to its ends; `←`
or `esc` back to the list). `r` reloads the tag list. A
tag whose digest could not be read is not selectable, for the same reason a bare tag is never
written.

The confirm screen then leads with the delta and the migrations:

```
╭─ hoist · deploy · confirm ───────────────────────────────────────────────────╮
│ghcr.io/example/app:v3   →   app-production          mode: PR · shift+d direct│
├──────────────────────────────────────────────────────────────────────────────┤
│rolling out 14 commits · 2 migrations · replacing v1, declared 4 weeks        │
├──────────────────────────────────────────────────────────────────────────────┤
│  4a1c2ef  Add rate limiting to the public API                                │
│  e9b0d31  Fix N+1 query when resolving digests                               │
│  77c0ffe  db: add index on events.created_at  migration                      │
│  1b2d3e4  Bump temporal SDK to 1.31                                          │
│  6f8a90c  Drop the legacy /v1/export endpoint                                │
│  a3e91b2  db: backfill events.tenant_id  migration                           │
│  9d2c4e1  Retry the registry HEAD on 429                                     │
│  c0ffee1  Log the resolved digest at startup                                 │
│  5e6f7a8  Move health checks to /healthz                                     │
│  d4c3b2a  Tidy the Dockerfile layers                                         │
│                                                            ↓ 4 more commits  │
├──────────────────────────────────────────────────────────────────────────────┤
│2 migrations run on this deploy:                                              │
│  db/migrate/20260225T101500_add_events_created_at_index.rb                   │
│  db/migrate/20260301T090200_backfill_events_tenant_id.rb                     │
├──────────────────────────────────────────────────────────────────────────────┤
│writes 3 image references in 1 file · digest 333333333333      d  see the yaml│
╰──────────────────────────────────────────────────────────────────────────────╯
      enter deploy · d yaml · shift+d direct · ↑/↓ · r fresh · esc back · ? more
```

The chip always also names the key that flips the mode — `shift+d direct` (or `shift+d PR` once
direct mode is on) — never a bare capital, since a capital in a footer reads as "press this
letter" and invites the caps-lock press a legacy terminal cannot tell apart from shift. `shift+d`
is not offered at all on a production target: the chip and the footer both drop it, and pressing
it anyway does nothing — the actual enforcement is `internal/engine.DirectCommitGateStep`
(§4.5), never this screen. `esc` returns to the tag picker underneath, cursor and filter intact,
not all the way back to the matrix.

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

`shift+r` on a cell (a legacy terminal's bare capital R still works — there is no way for it
to tell that apart from shift), or:

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
╭─ hoist · restart · web/app-staging ──────────────────────────────────────────╮
│app-staging / web                               production   not yet restarted│
├──────────────────────────────────────────────────────────────────────────────┤
│1 Deployment in app-staging                                                   │
│                                                                              │
│   web  1 replica · RollingUpdate · last restart: never restarted this way    │
│     ! only 1 replica: it keeps serving until the replacement is ready, but   │
│       there is no redundancy if the replacement fails                        │
│     ! no readiness probe: a new pod counts as available the moment it        │
│       starts, before it can serve                                            │
│     ! unpinned image ghcr.io/example/web:v1: a replacement pod can pull a    │
│       different build than the one running now, so this restart may not be a │
│       no-op                                                                  │
╰──────────────────────────────────────────────────────────────────────────────╯
                                  l activity · enter restart · esc back · ? help
```

Pressing `enter` on a production target overlays a confirmation dialog before anything rolls
(`--confirm-production=<env>` is the CLI's own second acknowledgement, below).

Every target is named first, with its replica count, strategy and last restart, and every reason
the restart will not be graceful is a sentence. None of them blocks. A production env takes a
second acknowledgement — a confirmation dialog on the screen, `--confirm-production=<env>` at the
CLI — because nothing is committed and nothing reviews it. `esc` while a confirmed restart is
still starting or rolling still leaves the screen — it never blocks on it — but adds "rollout
continues" to the activity log (`l`), since the restart itself is a live cluster operation this
screen does not cancel.

## When it stops

A stopped promotion says which step and why; this is what to do about each. `r` on the flight
screen (or `hoist resume <id>`) re-observes once you have.

**CI.** `CI: 2/3 checks complete` is waiting, not stopped. `1 of 3 checks failed: <name>` — or
`… were skipped (never ran)`, which blocks the same way — names the check. Do not push a fix to
`hoist/<env>/<id>`: the promotion only ever merges the head it pushed itself, and a branch
someone else moved is refused at the merge. If the failure is transient, re-run the failed
workflow on the same commit in GitHub and `r`. If it needs a real change in the GitOps repo,
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
Healthy again, `r`. `Argo Application <name> not found; check kube.argo_namespace and the repo's
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
  fast-forward the branch yourself if that was intended, then `r`.
- `commit X changes paths beyond this promotion's plan` — the branch's commit touches files the
  plan did not. hoist refuses to treat it as its own. Fix the branch — reset `hoist/<env>/<id>` on
  origin to the base branch, or delete it there — then `r`, and hoist commits again.
- `found PR #N … but it targets base "x", not "main"` / `… was closed without merging` — a PR on
  the promotion's branch that hoist did not open, or one that was closed. Retarget or reopen it
  and `r`; hoist adopts it. If the closed PR should stay closed, open a new one from the same
  branch, carrying the closed PR's title and body so the reviewer still sees hoist's diff
  summary and warnings (`gh pr create --head hoist/<env>/<id> --base main --title "$(gh pr
  view <n> --json title -q .title)" --body "$(gh pr view <n> --json body -q .body)"`), then
  `r` — hoist prefers an open PR over a closed one on the same branch.

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
hoist promotions --repo owner/name    # scope the listing to one repo
hoist promotions --archived           # also list what's already been archived (below)
hoist resume <id>             # continue one from wherever it actually is
hoist resume --env <target>   # the same, for the one non-terminal promotion targeting that env
hoist abandon <id> --confirm-abandon=<id>   # retire one that never landed — not a rollback
```

`hoist promotions` also retires what it finds: a promotion it re-observes as terminal — done —
and older than `state.retain` (default 30 days, measured from when anything last happened to it)
moves from the live promotions dir to a sibling `archive/` subdirectory — still a plain, readable,
deletable JSON file, just out of the default listing. This never happens on age alone: a promotion
has to be re-observed as done first, every time, so a genuinely stuck one (however old) is never
touched — `--repo`/`--archived` only change what's *listed*, never what gets archived.

On the matrix, `tab` then `enter` on the pane does the same for what it lists, and `shift+x`
there abandons the one under the cursor behind a confirm. A finished promotion leaves the pane;
its state file is archived (see above) after `state.retain`, or you can delete it, or run
`hoist abandon` (refused outright once the promotion has landed — that's what retention's own
archiving is for instead).

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
list itself. On the plan and deploy confirm screens `shift+d` toggles it (behind a confirmation
dialog turning it on; turning it back off needs no second confirmation) — not offered at all for
production, so the key does nothing there rather than refusing with a reason for a gesture the
footer never advertised. The tag picker's own direct-commit gesture retired: reviewing a tag
(`enter`) always opens the deploy confirm screen now, and `shift+d` there is the only way to
choose direct mode from the TUI. At the CLI it takes the env's name twice:

```bash
hoist promote --from app-staging --to app-dev --direct --confirm-direct=app-dev
hoist deploy  --env app-dev --image … --direct --confirm-direct=app-dev
```

`--confirm-direct` must repeat `--to`/`--env` exactly, and both flags need a config file: without
one hoist does not know which envs are production and refuses `--direct` rather than guess. The
second acknowledgement exists because a write with no PR should never be one flag or one key.
