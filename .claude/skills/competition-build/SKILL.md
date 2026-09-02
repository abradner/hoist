---
name: competition-build
description: Build a high-stakes change by running 2–3 independent implementations of the same brief in parallel, judging each adversarially, and shipping only the survivor — escalating to a more capable model tier only when every attempt has blocking holes. Use when the user asks to "orchestrate" rather than implement, hands off a body of trust-boundary work to run unattended, asks for competing or independent attempts at a problem, or when a change sits somewhere a single implementer's blind spot would ship a bypass. Hands a lone survivor to `.claude/skills/single-pr`, or several that ship together to `.claude/skills/batch-review`; where survivors genuinely combine, verify them with `.claude/skills/stack-integration-check` before anything is pushed.
---

# Competition build

For work where a bug is a calamity rather than a nuisance, **redundancy beats depth**. Three implementers who never saw each other's code fail in different places; one implementer plus one reviewer fail in the same place.

**The bargain: nothing ships until it has survived a judge that was trying to kill it.** Attempts are cheap and disposable. Only the survivor becomes a PR.

**This is opt-in.** The default in this repo is one implementation shipped as one PR at a time through `.claude/skills/single-pr` (`AGENTS.md` §8 *Which shipping flow*). An extra per-commit pass via `.claude/skills/independent-commit-review` is available on top of that where the repo kept it, but it is optional and prunable — it is not the default, and competition is not a substitute for the default flow, only a decision about what enters it. Reach for competition when the change sits on a trust boundary — auth, key custody, scope or permission matching, tenancy, anything where *"who is calling and what if they lie?"* has a non-obvious answer — or when a large body of such work must run unattended. It is not a way to make ordinary work feel thorough; three attempts at a CRUD endpoint is waste.

## Terminology

| Term | Meaning |
|---|---|
| **Brief** | The single prompt every attempt receives. Identical across attempts, verbatim. |
| **Attempt** | One independent implementation, isolated. Disposable. |
| **Judge** | An adversarial reviewer of one attempt, trying to find a bypass. |
| **Blocking hole** | A finding that is reachable, not hypothetical, and breaks correctness, security, or a stated invariant. |
| **Survivor** | The single attempt that ships. Exactly one per feature, or none. |
| **Escalation** | One run at the expensive model tier, given all attempts plus all findings, when no attempt is clean. |
| **Hold-out** | A feature with no clean survivor even after escalation. Ships nothing; goes to the operator with attempts and findings attached. |

## Hard constraints

Not negotiable, and **every subagent prompt must restate them** — a constraint you know but the subagent doesn't is not a constraint:

- **No subagent pushes. Ever.** Attempts stay local.
- **No subagent checks out, commits to, or pushes the default branch**, and none opens, comments on, or modifies a PR or issue. Reading it is fine and necessary — `git diff main..$b`, `git show main:<path>`, and the Phase 5 comparison all inspect the baseline. The constraint is on writing, not on looking.
- **Only the survivor is pushed.** Losing attempts stay local — do not open a PR "for comparison".
- **Merging needs the operator's explicit go-ahead**, per batch, even in auto mode (`AGENTS.md` §8 *Destructive & outward-facing actions*). Opening a PR is not merging.
- **Restate the repo's real test invocation** from `AGENTS.md` §6, verbatim, including any version-manager prefix.
- **Restate the applicable entries from `AGENTS.md` §6.1** (Known Environment Gotchas) into every attempt and judge prompt, reading them from §6.1 **at run time**. §6.1 is the source of truth and it accretes; do not copy a snapshot of it into this file, or the prompts silently go stale as new gotchas are added. This is what stops each attempt rediscovering the same host quirk separately and burning a cycle each — a test flag that doesn't work on this host, a port that collides with a sibling stack, a runner that needs isolation.

## Phase 1 — Spike, only if the design has an unknown

Only when a design decision turns on how something actually behaves. Skip it when the mechanism is understood.

**Ask for a running prototype, not a memo.** A spike that reasons about a race lands as an opinion; a spike that *exploits* it changes the design. (Sibling-repo instance: a socket-authorization spike built an `execve`-on-the-same-PID shapeshifter and demonstrated that a `/proc/<pid>/exe` check after `SO_PEERCRED` reads attacker-mutable state. Reasoning alone would not have moved the design.)

**Spike evidence evaporates when the session ends — land it in `docs/` in the same batch.** Findings that record a *negative* result matter most: *"this host happens to block the cheap PID-reuse race, but that is a local kernel-config accident and must not be designed against"* is unrecoverable once lost, and its absence invites the next session to design against the accident.

## Phase 2 — Write the brief

One brief, issued verbatim to every attempt. It needs, in this order:

1. **The invariant in the imperative, not the feature in the abstract.** *"An untargeted grant must not cover a targeted request"* beats *"implement scope targets"*. The abstract version invites three different guesses at what you meant; the imperative version is falsifiable.
2. **A pointer to `AGENTS.md`** and the instruction to judge the design against *this project's* stated invariants rather than generic best practice.
3. **The named adversary.** Who is attacking this, with what access? "A competent but literal agent in a repo none of us have seen" is a real adversary for a doc; "an attacker" is not.
4. **Known bug classes from earlier attempts at adjacent work, spelled out by name.** This is the cheapest quality lever in the whole skill — see Phase 4.
5. **The hard constraints above**, restated in full.
6. **What "done" means**: which tests, and run how. **For a bug-fix competition**, `AGENTS.md` §8 *Testing & verification* requires revert-and-confirm — write the regression test, see it pass, revert *only* the fix, see it fail for the right reason, restore. Keep that condition where the canonical rule puts it: on a greenfield feature, reverting the implementation leaves the new test referencing symbols that no longer exist, so it fails to compile, and the same rule says a build-failed mutant proves nothing. For new functionality, ask instead for a demonstrated failure the test *can* detect — the test run against the pre-feature baseline, or against a deliberately wrong implementation of the same interface. Either way the requirement is the same in substance: **a test nobody has watched fail has proven nothing.**

**Brief the merge, not just the code.** When an attempt builds on another branch, name the paths explicitly and require it to `git diff` against the branch below and justify every deletion. (Sibling-repo instance: an attempt took one package wholesale as a file-level copy and silently dropped a security control the branch below had introduced. The merged result was correct; the individual PR was not — which is exactly the shape a reviewer catches and loses trust over.)

## Phase 3 — Get the plan approved, then run 2–3 independent attempts

**Before any attempt writes code, put the plan in front of the operator and get it approved** (`AGENTS.md` §8 *Planning & approval*: propose an implementation plan for any moderately or highly complex change and get it approved before making edits). Competition is reserved for exactly the complex, trust-boundary work that rule targets, so it never exempts itself from it — and "orchestrate this" or an unattended hand-off approves the *objective*, not the approach. Present the brief's invariant, the named adversary, and the intended approach; if the operator already approved a concrete plan, say so and proceed. Skip this only when the plan itself was the thing they approved.

The cost asymmetry makes this obvious: an approach corrected before the fan-out costs one message, and after it costs 2–3 wasted attempts plus their judges.

Then: parallel, isolated, same brief.

**Independence is the whole product.** Do not let attempt 2 see attempt 1's code, do not summarise attempt 1's approach into the brief, and do not review attempt 1 yourself and hand the judges your opinion. Every one of those collapses three samples into one.

**Match the isolation to the artifact.** For code that touches shared paths, give each attempt its own git worktree — they will collide otherwise. For a single-file or docs-only competition, worktrees are overhead with a hazard attached: have attempts write to distinct scratch paths instead and keep the repo untouched. (Instance, in the keel template itself: a docs competition run from a session already mid-review on two branches, where branch juggling was the very risk the competition existed to write about.)

## Phase 4 — Judge each attempt adversarially

One judge per attempt minimum; more lenses for the highest-risk work.

**Every attempt gets the same blocking checklist. Lenses are added on top, never carved out of it.** Give each judge a **distinct lens** — correctness, bypass-hunting, does-it-actually-run — *in addition to* the common set every attempt is checked against, rather than N identical reviewers. Diversity catches failure modes redundancy cannot, but partitioning the checks across candidates destroys the only thing Phase 5 can decide on: if attempt A's judge hunted bypasses and attempt B's only checked that it runs, B can come back "clean" purely because nobody looked. Blocking-hole counts are comparable only when every attempt faced the same bar. This matters most for exactly the trust-boundary work the skill exists for, where the unexamined lens is the one that ships the bypass.

Judge prompts need:

- **The exact artifact, named.** State whether the target is *the branch as it would be reviewed as a PR* or *the merged result on top of its base*, and why. (Instance: a judge aimed at a feature branch reported CRITICALs already fixed on the merged stack — not wrong, just pointed at the wrong thing.)
- **The known bug class, by name.** Independent attempts converge on the same blind spot far more than intuition suggests. (Instance: three independent WebAuthn implementations shipped the *same* critical bug — an "ever-enrolled" bootstrap gate counting TOTP rows but not passkeys, leaving self-mint open in a passkey-only deployment.) Once one attempt's bug class is known, name it in every later judge's prompt and they hunt for it.
- **Scoring that cannot drift.** Ask for an integer 0–10 and say "0-10 ONLY, not a percentage" in the schema description — judges asked for 0–10 have returned 93 and 95. Then **do not threshold on the score**: threshold on blocking-hole count. The count decides; the score is colour.
- **A length cap, in both the prompt and the schema description**: "UNDER 1500 chars, do not paste diffs or logs". (Instance: a workflow died on `StructuredOutput retry cap (5) exceeded` because a judge kept emitting an oversized report — while the work itself was already committed and correct.)
- **A baseline-regression check, named explicitly.** A judge told to hunt for defects *in the candidate* will not notice what the candidate **deleted**. Give it the prior version and the enumerated list of properties the baseline already had, and require a verdict on each. (Instance: in a rewrite competition, one attempt silently dropped a safety rule the baseline carried — a class of change that had to stack and wait for a human regardless of any batch grant. Its judge returned zero blocking holes and the highest score in the round, because the prompt asked it to find ungated actions in the body and never to diff. The prompt's blind spot became the judge's.)
- **Verbatim quotes with line numbers, or the finding is discarded.** Say so in the prompt. This is what makes a judge's claim checkable at a glance instead of requiring you to re-derive it. (Instance: a judge returned five blocking holes on a document, every one false — it reported a buggy command form the file did not contain and three missing gates that were present and quotable. It had judged a reconstruction rather than the artifact. A quote requirement would have caught that in the judge, not in the reviewer.)
- **Where a prior judge on the same artifact failed, tell the next one so.** Naming the specific false findings turns a second pass from a coin-flip into a targeted re-check.

## Phase 5 — Compare the attempts, then pick the survivor or escalate

**Compare the attempts against each other before you select.** Per-attempt judging is structurally blind to the thing a competition most reliably produces: two implementations of one brief that diverge on a case neither of them tested. `.claude/skills/stack-integration-check` handles genuinely independent candidates — its parent map yields `main` for each, and its shared-file pass reduces to `for b in …; do git diff --name-only main..$b; done | sort | uniq -d`. For every file two attempts both touched, read the competing implementations side by side and build the truth table. **Do not compare test results, and do not ask whether both suites are green — green suites are what hide a semantic fork**, since each attempt's tests were written to match its own semantics. Concretely, that is its **Step 1** (topology — which yields `main` for every independent candidate) and **Step 3** (find the shared surface and compare implementations directly), plus **Step 4**'s insistence that each artifact be correct in its own right. **Skip Steps 2, 5 and 6** — justifying deletions against the branch below, testing the combination, and enforcement drift across a stack all presuppose a stack, and mutually exclusive attempts are not one.

**When attempts wrote to scratch paths rather than branches** — the single-file/docs mode in Phase 3 — the git-based shared-file discovery has nothing to work on: there are no candidate branches and no shared filenames for `uniq -d` to find. The comparison is not optional in that mode, only mechanically different: every attempt produced *the same artifact*, so the whole artifact is the shared file. Diff them directly (`diff attempt-a.md attempt-b.md`), and where they disagree on a rule, a command, or a claim, treat that disagreement exactly as a truth-table divergence above — resolve it against the invariant and convert the wrong side into a blocking hole. A docs competition where two attempts state opposite rules has the same failure mode as two branches with opposite semantics, and the same remedy.

**Resolve every divergence against the brief's invariant, and write the answer into the attempts' blocking status before applying the selection rules below.** For each divergent cell in the truth table, decide which behaviour the invariant actually requires; every attempt on the wrong side of that answer gains a **blocking hole**, whatever its judge concluded. Running the comparison earlier is worth nothing on its own — the selection rules consume blocking-hole counts and test strength, so a divergence that never becomes a blocking hole cannot change which attempt wins. Two attempts with clean reports and opposite semantics are not two clean attempts; at most one of them is.

If a divergence is genuinely ambiguous under the brief, that is a defect in the brief, not a tie to break on test strength: take it to the operator. The competition has just told you the invariant was underspecified, which is worth more than the attempt you were about to pick.

That is the whole reason this sits before selection rather than after: run it later and an attempt with the wrong semantics has already won, with nothing in the procedure revisiting it.

**A clean judge report is not evidence of a clean attempt — it is evidence about the judge.** Before treating zero blocking holes as a pass, ask what that judge's prompt made it *capable* of finding, and spot-check the artifact yourself on any property the prompt did not name. A judge cannot find a failure class it was never pointed at, and its confidence will not betray the gap.

**A blocking class discovered late must be re-run against every attempt already judged.** Phase 4 says to name each newly found bug class in later judges' prompts — that alone leaves the attempts judged *before* the discovery untested against it, so an earlier "clean" report means only that nobody looked. Re-check every prior attempt against the new class before comparing reports, or Phase 4's common-bar guarantee is not true and these selection rules are comparing unlike things.

- **Exactly one attempt clean** ⇒ it is the survivor.
- **More than one clean** ⇒ take the one whose *tests* are strongest, not the one with the most code.
- **Every attempt has blocking holes** ⇒ **escalate once**. Synthesise every finding, hand the expensive tier all attempts plus all findings, have it build a candidate. Do not pre-assign the expensive tier to a whole phase — let the judges decide when it is needed. **The escalation output is a candidate, not the survivor**: it is new code no judge has seen, so put it through the same common blocking checklist every attempt faced, plus a check against each invariant the comparison established. Skipping that is how a trust-boundary hole reaches Phase 7 with nothing having examined it — and without it the hold-out rule below has no basis on which to say the escalation was or wasn't clean. Coming from the expensive tier is not evidence; it is the least-reviewed artifact in the whole procedure.
- **A survivor with a narrow, non-bypass gap** (a missing UI path, an incomplete error case) ⇒ one targeted fix pass, then re-judge. Do not re-run the whole competition for a completeness gap. **If the re-judge still finds the gap, or the fix introduced a blocking hole, the artifact stops being the survivor** — strip the designation there and then, and re-enter these rules from the top with that attempt now counted as holed. Usually that means promoting the next clean attempt, if one remains; if none does, escalation; if escalation is already spent, hold-out. Do not skip straight to escalation while a clean attempt is still sitting there unpromoted — that is how a run ends with zero survivors and a perfectly good candidate nobody selected. One targeted pass, not a repair loop: an artifact that keeps its survivor label through a failed re-judge reaches Phase 7 and ships, which is the whole failure this rule exists to prevent.
- **Judging the escalation candidate.** Before deciding anything about it, put it through the same bar as any attempt — zero blocking holes against the common checklist, no divergence left unresolved against the invariants the comparison established — **and compare it against the original attempts, the same way you compared them against each other.** Do that comparison *first*, not after promoting it: The earlier comparison only established invariants for cases the attempts actually disagreed on; where they agreed, or where none of them implemented a case at all, no invariant exists — so a synthesised candidate can introduce fresh semantics there and pass every check, because nothing was ever written down to contradict. It is new code from a tier that saw all the findings and none of the review, which makes it the most likely artifact in the run to invent a case nobody considered.

  **Only once all of that comes back clean is it the survivor**, proceeding to Phase 7 exactly as a winning attempt would. If the comparison exposes a divergence that is wrong under the invariant, or one the invariant cannot settle, the candidate is **not** clean: it is holed like any other attempt, and since escalation is spent by definition, that is a hold-out — take it to the operator. Say explicitly in the report that a survivor came from escalation: it is the least-reviewed artifact in the run, and whoever reviews the PR should know that.
- **No clean survivor after escalation** ⇒ **hold out.** Ship nothing for that feature; bring the attempts and findings to the operator. Surfacing beats discarding, and the decision to ship a known-holey thing is not an agent's to make.

**A competition does not exempt the result from the round cap.** `docs/pr-review-machinery.md` §4 governs the survivor exactly as it governs anything else: three reactive rounds, then ticket — and note the revert-and-ticket rule there is scoped to *one small change* drawing three or more findings, which a substantial survivor usually isn't. Don't broaden it into "any survivor with three findings gets reverted"; read §4 and apply what it actually says. Competition decides what enters the flow; it does not buy extra laps inside it, and it does not change the rules of the flow.

## Phase 6 — Verify the survivors combine, early

Only when **more than one survivor will ship together as a stack** — several features competed separately and their survivors now stack. Run the whole of `.claude/skills/stack-integration-check` as soon as those branches exist, not as a last gate before push. Per-branch review structurally cannot catch two branches implementing the same shared abstraction in opposite directions, and running it early makes a semantic fork a cheap fix instead of post-push archaeology.

(The attempt-level comparison lives in Phase 5, deliberately: it is an input to selection. Getting this boundary right took three tries. The first draft pointed the whole skill at competition attempts, inherited from a source repo where survivors always stacked; the correction excluded the skill from attempts entirely and discarded the comparison stages that *do* apply; the version after that restored them but left the comparison running *after* a survivor had already been picked, where a divergence it surfaced could no longer change anything. Meta-rule 7 in three parts — port nothing without reading this repo's copy of what you are naming; when a reviewer says a rule is too broad, check whether the fix is narrower scope rather than exclusion; and a check whose output cannot change a decision is decoration, however correct it is.)

## Phase 7 — Hand off to the repo's normal shipping flow

Route by how many survivors there are:

- **One survivor** ⇒ `.claude/skills/single-pr`. This is the common case, and it is the repo's default flow.
- **Several survivors shipping together** ⇒ they become the interstitials of a normal stack, and `.claude/skills/batch-review` owns the process: feedback write-only, one synthesis pass, one followup PR, merge on the operator's explicit signal.

Competition does not usurp either flow — it decides *what* enters one.

## Never trust a subagent's report

The report and the artifact are different objects, and the artifact is the one that ships. `AGENTS.md` §8 *Review feedback* requires verifying a delegated claim against the artifact before relaying or acting on it. Before relaying any agent claim that would change what ships: read the diff, grep the branch, run the command — then say which of the two you are relaying.

| Signal | Why it lies | The check |
|---|---|---|
| A judge's CRITICAL | May be true of a different artifact than the one shipping | `git diff` the branch against the one it stacks on |
| `summary: "test"` or other garbage structured output | An agent can do correct work and fail only at reporting | `git branch` / `git log` on the branch it claimed |
| A crashed workflow | Same — commits survive the crash that killed the report | `git log` before assuming work was lost |
| Two agents contradicting each other | Both are often right about *different artifacts* | Diff the two artifacts against each other |
| A green suite on both of two branches | Each may test only cases that pass under either semantics | Compare the shared code path directly, not the suites |

## Model tiering

Cheap-first, escalate on evidence. Attempts and judges run at the standard implementation tier; the expensive tier is spent only where Phase 5 says every attempt failed. Bulk mechanical work goes to subagents or plain scripts, never to a capable main thread.

If the operator is fighting the model selector, discuss tiers by role ("the escalation tier", "the main thread") rather than by model name — naming models in conversation can trip a classifier that flips the session model.

## Where these rules came from

Backported from a sibling repo's capability-broker build (overnight, 2026-08-09): two research spikes, three simple builds, two three-way competitions with adversarial judging, one escalation, fresh-eyes review and fix passes, stack-integration verification. Six PRs, none merged without the operator.

What redundancy caught that depth would not have:

- All three WebAuthn attempts shipped the identical bootstrap-gate bug. A single implementer ships it; a single reviewer probably misses it.
- Two branches wrote opposite `scope.Covers` semantics for the same case, both suites green, because each only tested cases that pass identically either way.
- One branch deleted a security control introduced by the branch below it in the stack, while its own tests stayed green.

What cost time on that run and is now prevented above: judges drifting off the scoring scale, a workflow dying on an oversized structured report, a judge aimed at the wrong artifact, and spike evidence that lived only in `/tmp`.

Append this repo's own competition outcomes here as they accumulate.
