# [KEEL:FILL project-name] — Agent Onboarding Guide

> Read this file first. It is the source of truth for AI agents working in this codebase.
> Plans, status, and decision rationale live in [KEEL:FILL external tracker — name it explicitly,
> e.g. "the <project> page in Notion"], not in a repo file. When this file and the tracker disagree
> on architecture or direction, the tracker wins — update this file to match, never the reverse.
> This file is operational: how to build, run, and write code here.

## 1. What Is This?

[KEEL:FILL 2–5 sentences: what the project does, who it's for, current status. Name the domain
nouns an agent will encounter and disambiguate any overloaded terms — if a word in this repo means
something narrower or different than its plain-English reading (a CRD kind, a framework concept, a
domain entity), define it here before it's used anywhere else.]

## 2. Purpose & Principles

**Purpose:** [KEEL:FILL one sentence — the project's thesis, the thing every design decision
should serve. If the project is too young to have one, write "no settled thesis yet — revisit
when the first values conflict appears" rather than inventing one.]

Principles: short, numbered, memorizable. Each one must be able to change a real decision — a
principle that never changes a decision is decoration.

1. **Claims must hold.** A doc, comment, or commit message claiming a property the mechanism
   doesn't deliver is a bug, not documentation. Interim states say so in-place ("stated, not
   enforced — landing in E7"); stopgaps record the bar they miss. *(Portable seed — keep, adapt,
   or replace. It has caught real security bugs, twice, in a sibling repo, by being invoked
   against the project's own earlier claims.)*
2. [KEEL:FILL 2–7 project principles, or delete the slot if deferring.]

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
| Language | [KEEL:FILL] | [version manager, e.g. managed via `mise`] |
| Framework | [KEEL:FILL] | |
| Database | [KEEL:FILL] | |
| Testing | [KEEL:FILL] | |
| Deployment | [KEEL:FILL] | See §7 |

[KEEL:FILL add/remove rows to match reality. Every row should earn its place — a stack table an
agent can't act on is decoration.]

## 4. Critical Architectural Rules

These are non-negotiable. Violating them will break things or be rejected in review.

[KEEL:FILL the project's own invariants. Authoring guidance — delete after filling:
- One `###` subsection per rule, with a code example of the right shape where one helps.
- Shape conventions belong here too, not just break-things invariants: per-surface architectural
  standards (UI component architecture — e.g. container/presenter segregation, layout vs
  scaffolding vs domain components; service layering; error-handling taxonomy) are exactly what
  agents need stated to avoid inventing a different shape per session. "Rejected in review" is a
  real cost even when nothing breaks. Empty is a valid answer at init, but §8's
  no-stated-convention rule then applies: agents propose, the operator codifies.
- State the rule, then the incident or reasoning that motivated it. A rule grounded in "this
  happened, here's what it cost" is far harder for an agent to rationalize around than a bare
  imperative — and if the incident happened in a sibling repo, cite it anyway.
- Scope absolutes with named exceptions. "All tables use X" fails the first time a framework-owned
  table legitimately doesn't; write "all application/domain tables use X; framework-owned schemas
  (name them) are exceptions — don't hand-edit those to match."
- If a rule depends on unshipped work, say so in-place ("stated, not enforced — landing in PR #N")
  so an agent reading mid-transition doesn't conclude the system is broken.]

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

[KEEL:FILL setup, dev server, test commands, lint/format commands — the exact invocations,
including any version-manager prefix (`mise exec --`, etc.) a fresh shell needs. A command that
works in CI's provisioned container but silently resolves the wrong toolchain on a dev machine is
a known failure mode; give the dev-machine form.]

### 6.1 Known Environment Gotchas

Things that cost real time to discover once — don't rediscover them. Add entries as they're found;
add rather than rewrite.

[KEEL:FILL — starts empty. Entry shape: the symptom, the actual cause, the workaround. Host
quirks, port collisions with sibling projects, toolchain path issues, platform limitations.]

## 7. CI & Deployment

[KEEL:FILL what CI runs, on what triggers, and what deployment looks like — or state plainly that
deployment is undecided and nothing should be built for it until asked.]

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
  deployable artifact until a tag exists. [KEEL:FILL or delete if not applicable.]

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

[KEEL:FILL — starts empty. The first entry usually arrives within days.]

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

[KEEL:INIT — the init procedure fills this section, then this marker line is removed.]

### Template lineage

[KEEL:INIT — private repos: fill this block. Public repos: delete the whole block and rely on the
fleet registry (`abradner/fleet`, private) instead — a pointer to a private template is
unresolvable for outside contributors and mis-steers their agents.]

- Template: `abradner/keel` (private)
- Synced through: [KEEL:INIT commit sha] ([date])
- Last checked upstream: [KEEL:INIT date]

At the start of a substantive session, if "last checked" is more than about a month old, offer a
sync pass: list template commits since the synced SHA
(`gh api 'repos/abradner/keel/commits?since=<date>' --jq '.[].sha + " " + .commit.message'`),
review the delta, port with judgment (maintenance meta-rule 7 — adopt, adapt, or reject
explicitly), and update both lines above — including when the answer is "nothing to take"; the
date records attention, not just change. The flow is two-way: when a rule or lesson in this repo
proves general, flag it for backport to keel and record it in the fleet registry
(`abradner/fleet`).
