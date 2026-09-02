# Template Init — Agent Procedure

You are initializing a repo created from the **keel** template. This file is the procedure; you
are the executor. When you finish, this file gets deleted — the end state is a repo whose steering
docs read as if written for this project, with the adaptation rationale recorded in `AGENTS.md`'s
Adaptation Record section.

Work through the phases in order. Don't skip the interview: filling placeholders from guesses
defeats the point of having an operator in the loop.

## Phase 1 — Interview

Ask the operator (batch the questions, don't drip them one at a time):

1. **Project**: name, one-line purpose, current status (greenfield / port of X / extraction).
2. **Purpose & principles**: does the project have a thesis — a sentence every design decision
   should serve — and 2–7 values/standards behind it? If yes, capture them (short, numbered, each
   able to change a real decision). If not yet, record "no settled thesis — revisit at the first
   values conflict" in §2 rather than inventing filler; the tripwire protocol ships either way.
3. **Stack**: language(s), framework, database, test tooling, anything already decided. If the
   answer is "not decided yet," record that as the answer — `AGENTS.md` should say "undecided,
   don't build for it until asked" rather than a guess.
4. **External tracker**: where do plans/status/decisions live (Notion, Linear, issues, none)?
   If none, the tracker-pointer lines in `AGENTS.md` change to "tracked in-repo until a tracker
   exists" — don't leave a pointer to nothing.
5. **Visibility**: public or private? If public: double-check nothing repo-specific being written
   in (hostnames, internal URLs, tracker IDs) is sensitive, and confirm the license.
6. **License**: keep Apache-2.0, swap, or remove (private/personal repos may want none).
7. **Merge strategy**: squash-merge, merge commits, or rebase — this parameterizes the
   batch-review skill (several of its rules are strategy-dependent and say so).
8. **Review bots**: which automated reviewers will run (Copilot, Codex, a custom action, none)?
   Fills the batch-review skill's bot roster.
9. **Repo map**: keep by default (`docs/repo-map.md` — boundary map + risk register; its value
   compounds as complexity grows, and nobody retrofits one later). Prune only if the project is
   genuinely trivial — a single-file tool, a static site — and record the pruning in the
   Adaptation Record so it's a revisitable decision, not a lost one.
10. **Rails metrics recipe**: if the repo is a Rails service, keep
    `docs/rails-prometheus-metrics.md` and instrument during initial build-out (the doc's point
    is that nobody retrofits observability happily); delete the file for any other stack.
11. **Caveman mode**: keep the terse-tone module or prune it? (It's a tone preference, not a
    correctness concern — pruning removes `.cursor/`, `.windsurf/`, `.clinerules/`, `.opencode/`,
    `.github/copilot-instructions.md`, and the Caveman Mode section if one was added to AGENTS.md.)
12. **Skills**: keep the shipped skills? `single-pr` is the default flow and should be kept by any
    repo that ships through PRs at all; `batch-review` only earns its keep in repos that will ship
    multi-PR bodies of work; `independent-commit-review` is broadly useful;
    `park-context`/`resume-context` are a pair — keep or prune them together, since a handoff with
    no resume procedure is how ratchet items get dropped;
    `dependabot-sweep` only earns its keep in a repo that actually runs Dependabot and/or Renovate
    — prune it for a repo with neither;
    `competition-build` earns its keep only where the repo has genuine trust-boundary work (auth,
    key custody, tenancy, permission matching) — it spends 3–5× the tokens of a normal build by
    design, so prune it for a repo with no such surface.
    **`stack-integration-check` is required by whichever of `batch-review` and `competition-build`
    you keep** — `batch-review` uses it to verify multi-branch combinations, and
    `competition-build`'s Phase 5 uses its comparison stages on *every* competition, not only ones
    whose survivors stack. Keep it if you keep either; prune it only when both are pruned.
    **`competition-build` also requires `single-pr`**, which is where a lone survivor ships. Don't
    keep `competition-build` alongside a pruned dependency and settle for a note about the dangling
    pointer: either keep the dependency, or adapt `competition-build` to name whatever this repo
    actually uses in its place. When in doubt, keep — they're inert until invoked.
    **`docs/pr-review-machinery.md` survives if either PR skill *or* `competition-build` does** —
    both PR skills read it, and `competition-build`'s Phase 5 cites its §4 round cap. A skill
    pointing at a pruned file is worse than a skill that inlined it.

## Phase 2 — Fill

Find every marker: `grep -rn "KEEL:" . --include="*.md" --include="*.toml" --include="*.gitignore" -l` then work file by file.

- `AGENTS.md`: fill every `[KEEL:FILL]` from interview answers. Delete the bracketed authoring-
  guidance blocks once their sections are filled — they're scaffolding, not content. The Known
  Environment Gotchas and Gotchas & Lessons Learned sections stay as empty accreting sections
  with their instructions intact; the tripwire protocol in §2 ships as-is regardless of whether
  principles were captured or deferred.
- `docs/repo-map.md` (if kept): seed the Surfaces table and trust-boundary entries with whatever
  is already true — even a greenfield repo has at least one planned surface. Leave the rest to
  grow by incident; don't pad it with aspirational entries.
- `.claude/skills/batch-review/SKILL.md`: fill the "Repo specifics" section (validation commands,
  which reviewers are actually installed + the expensive one's budget, merge strategy — keep only
  the matching variant of each strategy-flagged rule, delete the other).
- `docs/pr-review-machinery.md`: correct the reviewer table to this repo's real roster from the
  operator's answer, and delete rows for reviewers it doesn't have. A fresh repo has no PR history
  to verify against, so **mark the roster provisional in-place** ("unconfirmed — no PR has been
  reviewed yet; confirm on the first PR and delete this note") rather than deleting rows you cannot
  yet prove. Confirming it is then the first PR's job: a named-but-uninstalled reviewer lets PRs
  satisfy the letter of the rule while getting one pass instead of two, which is exactly how a
  sibling repo lost a review surface for weeks.
- `mise.toml`: uncomment/fill the stack's tools; delete the rest.
- `.gitignore`: uncomment the relevant language block(s); delete the rest. Honour any instruction
  inside a block — some require deleting a universal line above that would otherwise defeat them.
  Then verify rather than assume: after the stack's scaffold exists, `git status --ignored` and
  confirm every credential and build artifact it generated is actually ignored. A generator that
  writes a secret is not obliged to write the rule that hides it.

## Phase 3 — Prune (or wire in) modules

For each module the operator declined, delete its files entirely — no commented-out corpses. **Then
delete the `AGENTS.md` text that instructs agents to use it**, in the same pass: §8's "Which
shipping flow" names both PR skills and `docs/pr-review-machinery.md`, and "Context & compaction"
names the handoff pair. Deleting the files while leaving those subsections standing produces an
unconditional instruction to invoke something that no longer exists — Phase 6's cold read is a
backstop for this, not the mechanism.

Caveman mode specifically:
- **Keep** → append the caveman block (copy the body of `.clinerules/caveman.md`, minus its
  AGENTS.md pointer line) to the bottom of `AGENTS.md` under a `## Caveman Mode` heading — Claude
  Code reads `AGENTS.md`, not the per-tool files, so without this the tone rule never reaches it.
- **Prune** → delete `.cursor/`, `.windsurf/`, `.clinerules/`, `.opencode/`, and
  `.github/copilot-instructions.md`.

## Phase 4 — Stack hygiene

Generate what the template deliberately doesn't ship because it's stack-specific:

- CI skeleton (`.github/workflows/ci.yml`) if the stack is known — lint + test on PR, and honor
  the "a skipped job is not a passing job" caution if using path filters. If the stack is
  undecided, skip; don't scaffold speculative CI.
- README: replace the template's README with a project README. It must keep the agent-redirect
  line (`> 🤖 AI Agents: read AGENTS.md instead. This README is for humans.`) — that line is how
  humans landing on the repo discover the steering doc exists.

## Phase 5 — Record

Write `AGENTS.md`'s Adaptation Record section: date, what was pruned and why, what was declined vs.
adapted, any interview answer that shaped a decision ("no tracker yet — tracker pointers replaced
with in-repo tracking note"). Remove the `[KEEL:INIT]` marker lines. This section exists so the
rationale survives in the doc instead of only in a commit message.

Then the **Template lineage** block, by visibility (from the interview):

- **Private repo** → fill the block: template SHA this repo was created from
  (`gh api repos/abradner/keel/commits --jq '.[0].sha'` at init time is close enough if the
  creation SHA isn't known) and today's date for both fields.
- **Public repo** → delete the block entirely.
- **Either way** → add or update this repo's row in the fleet registry (`abradner/fleet`,
  private — deliberately not a template file, so it can never be copied into a public downstream)
  with relationship, synced-through SHA, and last-checked date. If the fleet repo isn't reachable
  from this session, leave the operator a note to do it — don't skip it silently.

## Phase 6 — Verify & finish

1. `grep -rn "KEEL:" .` must return nothing (this file is about to be deleted, so it doesn't
   count).
2. Read `AGENTS.md` top to bottom once, cold: does it read as written for this project? No
   template voice, no dangling references to pruned modules, no placeholder tables.
3. Delete `TEMPLATE_INIT.md`.
4. Commit everything as the init commit, message along the lines of:
   `Initialize from keel template — <project name>`, with a body summarizing the adaptation
   record. Get operator approval before pushing anywhere.
