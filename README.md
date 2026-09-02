# keel

> 🤖 **AI Agents:** if this repo was created from the keel template and hasn't been initialized
> yet, read [`TEMPLATE_INIT.md`](TEMPLATE_INIT.md) and run the init procedure. Otherwise read
> [`AGENTS.md`](AGENTS.md) — this README is for humans.

A template repository providing a baseline for **agentic steering**: the documents, skills, and
hygiene files that make a repo a good working environment for AI coding agents from the first
session.

Synthesized from steering docs iterated across a fleet of real repos — the patterns here survived
months of near-daily agent-driven development, and the load-bearing rules cite the incidents that
motivated them.

## What's in the box

| Path | What it is |
|---|---|
| `AGENTS.md` | The canonical steering doc: purpose & principles (with a tripwire protocol for conflicts), portable working rules (pre-filled), and repo-specific slots (`[KEEL:FILL]` placeholders). |
| `CLAUDE.md` | 8-line import stub pointing at `AGENTS.md` — one canonical file, no drift. |
| `TEMPLATE_INIT.md` | Agent-driven init procedure: interview, fill, prune, record, self-delete. |
| `docs/repo-map.md` | Living boundary map: surfaces, trust boundaries, cross-cutting flows, risk register. Opt-out (keep by default) — its value compounds with complexity and nobody retrofits one later. |
| `.claude/skills/single-pr/` | The default flow made rigorous: self-review before opening, solicit the reviewers that don't fire on their own, react immediately under a round cap, verify before claiming green. |
| `docs/pr-review-machinery.md` | Canonical shared reference both PR skills read — reviewer triggers, three-surface harvest, triage, round cap, green-signal traps. Kept in one place so the two flows can't drift. |
| `docs/rails-prometheus-metrics.md` | Day-one Prometheus `/metrics` recipe for Rails services: the yabeda stack, the cardinality/exposure footguns pre-solved, Temporal SDK metrics, and the vmagent scrape half. Rails-only — delete at init otherwise. |
| `.claude/skills/batch-review/` | Multi-PR fan-out shipping workflow (write-only feedback until synthesis, hard round cap, operator-gated merge). Parameterized — merge strategy and bot roster are fill-ins. |
| `.claude/skills/independent-commit-review/` | Adversarial fresh-eyes pre-push review: one cold subagent per commit, revert-and-confirm verification. |
| `.claude/skills/dependabot-sweep/` | Triage and merge open dependency-update PRs (Dependabot and/or Renovate), operator-gated per batch: CI/mergeability checks, peer-locked-pair handling for each bot's grouping model, cross-manager version-guard gaps, lockfile-conflict rebase mechanics. |
| `.claude/skills/competition-build/` | Opt-in redundancy for trust-boundary work: 2–3 independent attempts at one brief, adversarial judges with distinct lenses, ship only the survivor, escalate one model tier when every attempt has a blocking hole, and ship nothing when none survives even that. Depends on `single-pr` (where a survivor ships) and `stack-integration-check` (attempt comparison) — keep those if you keep this. |
| `.claude/skills/stack-integration-check/` | Verify candidate branches actually *combine* before any is pushed: semantic forks, silently deleted controls, content-without-ancestry, enforcement drift. Required by `batch-review` (multi-branch combinations) and by `competition-build` (attempt comparison) — keep it if you keep either. |
| `.claude/skills/park-context/`, `.claude/skills/resume-context/` | Planned-compaction handoff: park the irrecoverable context (intent, rejected alternatives, verified-vs-assumed state) under a strict no-exploration budget, then resume by verifying the handoff's claims before trusting them. |
| `.cursor/`, `.windsurf/`, `.clinerules/`, `.opencode/`, `.github/copilot-instructions.md` | Optional tone module ("caveman mode") fanned out to each tool's convention path, each also pointing back at `AGENTS.md`. Prunable at init. |
| `LICENSE` | Apache-2.0 (swap or remove at init). |
| `.gitignore`, `.editorconfig`, `mise.toml` | Universal hygiene baselines; init extends them per stack. |

## Creating a repo from this template

**Preferred:** GitHub's *Use this template* → creates a fresh repo with a single initial commit
and no shared history. Note: this template is private; the "generated from" attribution on a
public downstream repo is only visible to users with read access to the template, so nothing
leaks — but if you want zero pedigree, use the copy path.

**Alternative:** plain file copy into a fresh `git init` (optionally omitting modules by hand,
though the init procedure handles pruning either way).

Then open the new repo in an agent session and say: **"run the template init"** — the agent
follows `TEMPLATE_INIT.md`, interviews you, fills every placeholder, prunes what doesn't apply,
records the pruning rationale in `AGENTS.md`'s Adaptation Record, and deletes the init file.

## Maintaining the template

Improvements discovered downstream should flow back here — but as judgment, not copying: verify
each candidate against what the template actually claims, and keep `AGENTS.md`'s maintenance
meta-rules (especially "a rule everyone knows is wrong is worse than no rule") applying to this
repo too.
