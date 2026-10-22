# UX, feedback-wiring and architecture audit (2026-09, refreshed 2026-10)

Refreshed at `79c9280` (main after #258) on 2026-10-22: the next section is the current ledger,
the new findings, the issue triage and the next arc's trains. The original audit follows it
unchanged.

Audited at `9a15feb` (main after #182). Three read-only scouts, one per axis: **UX**
(navigation, keys, visual design), **FB** (feedback wiring: does the screen find out what the
work did), **AR** (architecture). This doc condenses them. Finding IDs are stable: later PRs
cite them, so they are never renumbered. A closed finding is marked, not deleted.

Paths are relative to the repo root. Line numbers are at `9a15feb`.

## Refresh 2026-10-22 (at `79c9280`)

This section is the current state. Everything below "Original audit (2026-09, at `9a15feb`)"
is the snapshot the five trains were planned from; it is kept unchanged so its IDs stay
citable, and its line numbers are those of `9a15feb`, not of today.

### What landed since the audit

| Range | What | PRs |
|---|---|---|
| T0–T4 trains (`9a15feb..0fa1fdb`) | Service layer, session controller, `scope`, keys registry, redesigned screens, docs pass | #185–#236 |
| CLI progress and scan speed-up | Narrated drives, shared in-flight scan, `ci.ignore`, history change-log, stray-positional refusal | #238, #239, #240, #241, #244 |
| Cleanup stack | Landed promotions no longer need their worktree; removal at landing and abandon; `hoist gc` | #250, #251, #252, #255 |
| Since the stack | Post-commit path check expects only changed files; CLI `--`/root-flag swallow refusals | #257, #258 |

The refresh re-verified every finding in the original audit against HEAD and walked the TUI,
the guide and the mockups again. Six read-only diggers ran on a cheaper model, one per axis:

- UX re-verify
- FB and AR re-verify
- Factoring and smells
- A UX walk against `docs/guide.md` and `docs/tui`
- Two issue-triage batches

Synthesis re-read the cited lines for every High finding and every new finding this section
marks ✓. One digger claim did not survive: `watchInterval` cannot return 0, because a missing
config still runs `Normalize` (`internal/config/config.go:255`). It is not listed.

*Method note:* the repo's `quality-audit-refresh` skill expects a tranche-and-ledger doc and its
workflow script. This doc has neither: it is a by-axis audit. So the refresh ran the same
re-verify step by axis, through individual agents.

### Ledger: original findings at HEAD

| Axis | Fixed | Partial | Still open |
|---|---|---|---|
| UX (33) | 23 | 8 | 2 |
| FB (21) | 20 | 1 | 0 |
| AR (13) | 5 | 7 | 1 |

**Fixed:**

- UX-H1–H11, plus UX-M3, M5–M9, M13, M16 and UX-L1–L3: #191, #205–#207, #217, #219,
  #220, #223–#227, #231.
- FB-H1–H3, FB-M1–M8 and FB-L1–L8, L10: #198, #205–#213, #220, #224, #225. FB-M1 also closes
  #184.
- AR-H1–H3, AR-M1 and AR-L3: #190, #192, #193, #195–#198, #230.

The "Train" column of the original tables is history. The open remainder is below; each item is
assigned to a train in [Next arc](#next-arc-trains-t5t11).

| ID | Status | What remains (file:line at HEAD) | Train |
|---|---|---|---|
| UX-H12 | partial | `esc back` now survives, but the left status is still cut to a fragment (`blocked: no c…` in `flight-ci-none-120x40`; `keys/footer.go:35-47` drops the status only under 8 cells) | T8d |
| UX-M1 | partial | The flight step list keeps its own glyphs (`flight/rows.go:23-29`). `⟳` remains in the compact pane (`matrix/inflight.go:262`) and `✔` in restart (`restart/model.go:450`) | T8b |
| UX-M2 | partial | The row detail still says to run `hoist resume … --override-ci-none` (`engine/steps_review.go:231`) above "press shift+c" | T9 |
| UX-M4 | partial | Times are relative now, but the history column still reads `acted`, `already satisfied: …`, `waiting: …` | T8b |
| UX-M10 | partial | Columns are content-sized (`maxCellWidth=44`). At 120×40 about 50 columns sit empty, and the mockup's detail column is not built | T8a |
| UX-M11 | open | `pinned`/`external` print in every cell (`matrix/model.go:994-1002`) | T8a |
| UX-M12 | partial | In yaml mode the plan header says "d back to impact" while the footer says "d yaml" (`plan/model.go:1269`, `:1321`) | T8c |
| UX-M14 | open | The in-flight pane says `deploy → <target>` with no image or tag (`matrix/inflight.go:286`) ✓ | T8a |
| UX-M15 | partial | Notice and Warn are still one amber (`ui/styles.go:58`, `:61`) | T8d |
| UX-M17 | partial | The bare `!` marker remains (`plan-80x24.txt:7`) | T8c |
| UX-M18 | partial | The tag picker's "no commit history … so …" line is cut at 80 columns (`tags/model.go:1458`) | T8c |
| Terminology | partial | Five refresh verbs, "environments" vs "envs", and `resolving…` in cells | T8d |
| FB-L9 | partial | Unread `plan.StartMsg.Outcome/Source/Target` and `deploy.StartMsg.Target/Image`. Comments still name `flight.DriveFunc` | T7 |
| AR-M2 | partial | Three walks remain (`engine.go:162`, `:297`, `:421`), plus seven list constructors (`lists.go`). Observe still mutates state, and `StateDir()` is process-global (`state.go:163`) | T9 (StateDir), rest ticketed |
| AR-M3 | partial | The controller is extracted, but `app.go` grew from 1310 to 1656 lines. `Update` alone is 433 lines (`app.go:468-901`) | T7 |
| AR-M4 | partial | "Straight to production, skipping staging" is a screen-only string (`plan/rows.go:437`), so `hoist plan` and the PR body never carry it. This also violates §4.5's own rule for warnings | T10 |
| AR-M5 | partial | Kube-context fallbacks are down from 7 to 2. Five settings structs overlap (`selection`, `effective`, `resolveFlags`, `service.Settings`, `service.ResolveOptions`) | T6 |
| AR-M6 | partial | cmd/hoist has 11 package-level seams, assigned 38 times in tests (`deps.go:20` claims "about 108"). `t.Parallel` is used 0 times ✓. Git test helpers are copied across 4 packages, and the §9-entry-8 gitconfig across 5 | T6 (seams), T11 (fixtures) |
| AR-M7 | partial | `planFn` runs synchronously inside `Update` for up to 120s (`app.go:627`, `:1396`) ✓. Matrix reads use deadline-only `scope.Timeout`, so they are not cancelled on pop | T7 |
| AR-L1 | open | The big files still grow: tags 1595, plan 1556, matrix 1329, controller 1174 lines. `huh.NewConfirm` is copied at 7 sites, `dialogWidth` ×5, `plural` ×6 and short-sha ×9 (some 7 characters, some 12) | T7 |
| AR-L2 | partial | After the #234 diet, about 207 review-history comment lines remain in 63 non-test files | T11 |

### New findings at HEAD

✓ means synthesis re-read the cited lines. The rest are a digger's reading, with its file:line;
the digger reports are kept with the planning lane's notes, not in this repo. A finding is a
claim (AGENTS.md §8): each train re-reads its items at its own base before fixing them.

**Safety and correctness**

| ID | Sev | Where | Problem | Train |
|---|---|---|---|---|
| AR-H4 ✓ | H | `cmd/hoist/main.go:682-684`, `app.go:1358`, `restart/model.go:329` | A flags-only TUI run passes a zero `EnvsConfig`, so every env reads as non-production and `shift+r` → `enter` restarts a production env with no confirm. The CLI fails closed in the same situation (`cmd/hoist/restart.go:157-171`). One gate, two answers (§4.5, §8 layered checks) | T5 |
| FB-H4 ✓ | M | `app.go:1317`, `cmd/hoist/main.go:736-742` | A confirmed `q` quit calls `StopAll()` before `main` reads `InFlightIDs()`, so no `hoist resume <id>` lines print. The comment claims both quit paths print them (principle 1) | T5 |
| UX-H13 ✓ | M | `matrix/model.go:721`, `app.go:524-552` | With the action menu open, `q` quits (or asks), `l` pushes the activity log over the menu, and `?` shows the matrix help. `CapturesText` ignores `menuOpen` | T5 |
| CS-1 | M | `pkg/gitops/plan.go:77-96` | A `--promotable` that matches no source image reports "already current" and exits 0. That is a false success (#237 item 2) | T5 |
| CS-2 | M | `internal/service/fresh.go:283-316` | Direct mode with an unticked repo rebuilds the freshness plan over every promotable repo. It then reports the unticked repo's occurrences as unseen, or refuses (#201 item 6). Read from code only: the first PR writes the failing test | T5 |
| CS-3 | M | `cmd/hoist/history.go:183-210`, `main.go:188`, `:216-224` | The history adaptor memoises errors for the whole session and calls `newForge` directly, bypassing `svc.ForgeFor`. A `gh` login fixed mid-session never recovers history or live-age. `gitTagsFn` swallows the forge error | T6 |
| CS-4 | L | `cmd/hoist/gc.go:44-53` | `gc` prints errors without redaction, unlike every other command (§4.10) | T6 |
| CS-5 | L | `engine/claim.go:158-190`, `state.go:326-350`, `registry/cache.go:206-235` | Three atomic-write idioms, none of which calls `Sync`. The claim comment says "durably written" | T9 |

**Factoring**

| ID | Sev | Where | Problem | Train |
|---|---|---|---|---|
| AR-H5 ✓ | H | `cmd/hoist/wiring.go:140`, `:187`, `history.go:40-285`, `main.go:131-224` | Five use cases are still closures in `cmd/hoist`: watch, restart, history, tags and drift (§4.8 says so). `runWatch` and `runRestart` re-derive the same reads with direct clients | T6 |
| AR-M8 | M | `cmd/hoist/promote.go:264-327` vs `service/policy.go:61-92` | Direct-mode preflight runs the same four checks twice, with different wording (#201 item 5) | T6 |
| AR-M9 | M | `cmd/hoist/watch.go:171`, `internal/restart/restart.go:54`, `engine/steps_converge.go:453` | Workload derivation exists three times | T6 (two copies), T9 (the engine copy) |
| AR-M10 | M | `internal/app/tags`, `internal/app/matrix` | Four process-wide generation counters remain beside `scope`. tags is a small migration; matrix is medium, because the root routes `DriftMsg` by name | T7 |
| AR-M11 | M | `deploy/model.go:31,74-75` | The deploy screen imports the plan screen for `RenderDiff` and the Mode constants | T7 |
| AR-M12 | M | `internal/copycheck` | Nothing enforces "no `context.Background()` in internal/app" (`scope.go:126` says a grep does) or the screen import rules. Only watch has an import test | T7 |
| AR-L4 | L | `flight/model.go:25-40`, `wiring.go:29-38`, `wiring.go:66` | Dead plumbing: `PollDurations` CI/Argo/Rollout, `NewAttached`'s `_ = poll`, and the Windows browser branch (goreleaser builds only darwin and linux) | T6, T7 |
| AR-L5 | L | `matrix/model.go:656`, `:664` | The resume chooser branch is unreachable since the pane cursor replaced it | T7 |
| AR-L6 | L | `cmd/hoist/*` | 36 hand-rolled `Fprintf(stderr, "hoist X: …")` sites, about 27 of them with manual `redact.Strings` | T6 |

**TUI UX** (from the walk; issue numbers in the walk report are ranks by operator pain)

| ID | Sev | Where | Problem | Train |
|---|---|---|---|---|
| UX-H14 ✓ | H | `flight/summary.go:116-123`, `matrix/inflight.go:262` | A healthy wait for approval reads "blocked on approval", in the same amber as a real block | T8b |
| UX-H15 | H | `matrix/inflight.go:151-155`, `:203-212` | A promotion that is only listed (after a crash, not driven here) looks like a driven one. Nothing merges until `tab enter` | T8a |
| UX-H16 ✓ | H | `flight/model.go:593-598`, `service/promotions.go:511` | Abandoning a landed-but-stuck promotion is refused only after the confirm, and there is no way to clear it | T8b + D5 |
| UX-H17 | H | `flight/model.go:1060`, golden `flight-ci-none-80x24` | At 80 columns a blocked promotion's footer drops `shift+x abandon` and `r`. "Leaving keeps it running" survives only at 120 | T8b |
| UX-H18 | H | `app.go:149-155`, `service/policy.go:64` | A missing config is invisible on the matrix. The cursor starts on an unmarked production column, and promote fails at the end with "no github configured" | T8a |
| UX-M19 | M | `matrix/model.go:1110-1137`, `:297-305` | "Cluster unreachable" shows only for the cursor column. Stale drift is kept after a failed refresh | T8a |
| UX-M20 | M | `plan/model.go:1302-1363` | The plan's error state advertises dead keys and suppresses notices | T8c |
| UX-M21 | M | `app.go` `requestMatrixRefresh` | `r` on the matrix does not relist the in-flight pane. After approving on GitHub it stays stale for up to 30s | T8a |
| UX-M22 | M | `session/controller.go:230`, `service/errors.go:54` | Start refusals use internal voice ("session: …") and give CLI instructions inside the TUI | T8b |
| UX-M23 | M | `flight/model.go:800-829`, golden `flight-done-80x24` | Finished steps lose their evidence (PR, commit, CI). The done screen has no result block, unlike mockup `v2-flight-done` | T8b |
| UX-M24 | M | `matrix/menu.go:36-52`, `model.go:618` | Disabled menu items are silent no-ops | T8a |
| UX-M25 | M | `flight/model.go:1029` | Blocked guidance is generic ("resolve the conflict, then press r") for every kind of block | T8b |
| UX-M26 | M | goldens `plan-80x24:3`, `deploy-commits-80x24:4`, `config-nofile-80x24:1` | Key summaries are truncated, with no way to read the rest | T8d |
| UX-M27 | M | `tags/mockup_test.go:117` | The registry-denied remedy (`read:packages`, `GHCR_TOKEN`) exists only in a test fixture, never in the app | T8c |
| UX-M28 | M | watch screen | The watch screen cannot say why a rollout is stuck: no pods, no events. Digests hard-wrap mid-token | T8d |
| UX-L4 | L | goldens | Ragged history and activity columns. The 120-column matrix shows the production warning twice. The menu footer ignores the menu. A dead `↑/↓ commits` hint. `app-\nstaging` hard-wraps. The frame never pads short content. The help overlay shows the caps note on read-only screens. Mixed units on watch. `l: activity (1)`. Two stacked rules at 80 columns (ux-reverify N1–N17) | T8 |
| UX-L5 | L | `internal/ui/keys/registry.go` | Divergences from the approved keymap: the tag picker has no `o` and no `pgup`/`pgdn`. The reader's `l` has no row. The restart footer lacks `r`. `ctrl+d`/`ctrl+u` are bound but documented nowhere | T8d |
| UX-L6 | L | fixtures | The deploy and restart goldens used as guide pictures come from fixtures without production config (`shift+d` offered on production, a `production` badge on staging). `testdata/golden/plan.txt` is orphaned and shows retired keys | T8f |

**Docs drift:** the guide disagrees with the build in 11 places and has no section for the watch,
config or activity screens, the dialogs, `window too small`, or the first run. The walk report
lists them with line numbers. `ui/frame.go:10` and AGENTS §4.8 say the footer is always the last
line, but with an activity entry it is second-to-last (`app.go:1237-1260`).

### Open issues: triage at `79c9280`

#168, #245, #248 and #254 are excluded because lanes are fixing them tonight. #245, #248 and
#254 have since landed in #258.

| Issue | Verdict | Train / action |
|---|---|---|
| #260 caps lock + r opens restart (Alacritty) | `Binding.Matches` rejects `ModCapsLock` without shift (`keys/keys.go:108`), so the cause is probably the terminal (not granted, or no caps bit). **Diagnose first:** log `KeyboardEnhancementsMsg` and the raw `Key.Mod` behind a debug env var. Then fix the code, or narrow §9.13 and the guide's claim to the terminals it holds for | T8e |
| #256 cleanup review notes | Item 1 (`engine/direct.go:190`, an `IsAncestor` error is read as "not landed"): quick fix. Item 2 (`pkg/git/git.go:402`, the absent-path branch reaches the force helper): quick fix | T9, T5 |
| #249 cleanup leaves no activity entry | Quick fix | T8b |
| #247 repo-view cache for an unconfigured clone | Quick fix, needs a stated rule (remove when no configured clone hashes to it) | T10 |
| #246 TUI counterpart for gc | Design: recommend CLI-only. Record it in the parity row and close | D4 |
| #243 history/CI deferred findings | H1 and H2 are quick fixes in the engine. H3 is stale, by design. H4 is a quick fix (`remergeInFlight` on `ChangeProgress`). C1–C4: recommend close, since each limit is already stated. C5 (commit statuses not paged, `pkg/forge/github/github.go:317`) is a quick fix | T9, T8a, T5 |
| #237 promote refuses a no-op path | Item 1 fixed by #257. Item 3 fixed by #180. Item 2 (promotable matches nothing yet reports "already current") is still real | CS-1 in T5, then close |
| #235 "AGENTS.md invariant N" comments | Still real: 79 lines in 34 non-test files (113 lines including tests) | T11 |
| #216 T2 deferred findings | Items 1, 4, 5 and 7 stale. Item 6 (`ChangeRefused` is dropped by an empty case, `app.go:972`) is a quick fix. Items 2, 3 and 8: recommend leave | T8b, close remainder |
| #204 concurrent same-id starts | Needs design. More urgent now that worktrees are removed at landing | D2, T9 |
| #201 T1 deferred findings | Items 1, 2 and 7 stale. Item 4 fixed by #241. Items 3, 5, 6, 8, 9 and 11 are quick fixes. Item 10: leave | T6, T5, T9 |
| #184 boot-time repo in the start path | **Fixed** by #197 and #211 (`service/start.go:80-86`) ✓ | close |
| #173 envs.pairs unused by the CLI | Still real ✓. `--to` alone takes `SourcesOf(to)` when unambiguous; `--from` alone takes `Pairs[from]` | T6 |
| #172 registry env source hardcoded to ghcr.io | Small feature. Add an explicit `registries[].env: VAR` with no default | T10 |
| #171 restart only targets Deployments | Small feature. The issue understates it: `RolledOutStep`'s `groupEditsByWorkload` silently ignores StatefulSet and DaemonSet edits, a principle-1 gap. That half goes to the #168 lane or T9 | T10 (restart), T9 (rollout) |
| #170 kustomize/Helm/CRD image fields | Large feature. Start with the kustomize `images:` transformer if ever | backlog |
| #169 merge-gate doc can't see Codex's verdict | Docs quick fix in `docs/pr-review-machinery.md:233-292`; sync to the keel template too | T11 |
| #161 override rebuild leaves a new row unticked | Still real (`plan/model.go:623`) | T5 |
| #153 refs/-qualified `--base` | Code moved to `service/fresh.go:64`. Quick fix: refuse a `refs/` prefix up front | T5 |
| #126 Homebrew tap | Needs operator steps (a tap repo and a token) | backlog, operator |
| #107 GitLab history adaptor | Large. There is no GitLab adaptor at all | backlog |
| #106 first-run wizard | Large. Builds on UX-H18's banner | backlog |
| #90 hoist cannot approve its own PRs | Design: recommend documenting it as is (option 3), and splitting the 405-merge wedge into its own issue | D1, T11 |
| #74 tag picker compares tag strings | Still real. The design is in the issue | T8c |
| #53 Argo refresh anchor is a local clock | Design: recommend decline. The cost is bounded and it never yields a false satisfied | D4 |
| #41 A→B→A re-apply reads as done | Core bug still live ✓: the merged branch of `MergedStep.Observe` never compares the PR's head with `CommitSHA` (`steps_review.go:452`); only `landedGuard` does (`landed_guard.go:120`) | D3, T9 |
| #24 warn when the target is newer | Cheap version: a `WarnTargetNewer` for timestamped tags | T10 |
| #7 M7 umbrella | Items 1, 3, 4 and 5 are done. Item 2 ("skipping staging" as a real warning) = AR-M4 | split 7.2, close |

**Proposed closures** (for Alex; this lane edits no issue):

- Close now: #184.
- Close after the residual is split into its own issue: #7 (item 2), #237 (item 2).
- Close on the decision: #246, #53, and #243 items C1–C4.
- Close the bucket issues #201, #216, #243 and #256 once their remaining items are moved into
  the train PRs that fix them.

### Next arc: trains T5–T11

Starting state: the #168 lane has merged and v0.2.0 is tagged. Effort is in agent-hours of
build time, review rounds excluded.

| Train | Scope | Closes | Footprint | After | Risk | Effort |
|---|---|---|---|---|---|---|
| **T5** safety quick fixes | One small PR each: AR-H4 (the TUI restart gate fails closed with no config); FB-H4 (record the ids before `StopAll`); UX-H13 (menu `q`/`l`/`?`); CS-1; CS-2 (failing test first); #256.2; #243.C5; #153; #161 | AR-H4, FB-H4, UX-H13, #161, #153, #237.2, #201.6, #256.2, #243.C5 | `internal/app/app.go`, `internal/app/matrix/model.go`, `internal/app/plan/model.go`, `internal/service/fresh.go`, `pkg/gitops/plan.go`, `pkg/git/git.go`, `pkg/forge/github/github.go` | — | Low | 8 |
| **T6** service owns the rest | `service/{watch,restart,history,tags,drift}.go`, with method signatures matching today's screen func types so `internal/app` does not change; `runWatch`/`runRestart` call them; history goes through `ForgeFor`/`RegistryFor` (CS-3); one workload derivation in `pkg/gitops` (`Family.Workloads`); one direct preflight (AR-M8); fewer settings structs (AR-M5); `fail()` and one bounded-context helper (CS-4, AR-L6); smaller seams (AR-M6); dead PollDurations and Windows branch; F5 fallback notice (#201.11); #201.3; #173; AGENTS §4.8 bullet | AR-H5, AR-M5, AR-M8, AR-M9 (2 of 3), CS-3, CS-4, AR-L6, #173, #201.3/.5/.11 | `cmd/hoist/*` except the parse functions, `internal/service/*` except `fresh.go`/`start.go`, `internal/restart`, `pkg/gitops` (a new `family.go`), `internal/parity`, AGENTS §4.8 | — (parallel with T5 and T9) | Medium: wide in cmd/hoist; behaviour pinned by the existing CLI tests | 16 |
| **T7** TUI factoring | Pure refactor, goldens byte-identical: split `app.Update` (keys / navigation / session events); `ui.ConfirmDialog` for the 7 `huh.Confirm` copies; shared helpers in `internal/ui`; tags and matrix onto `scope`; deploy stops importing plan; split tags/plan/matrix/controller files by concern; copycheck tests for `context.Background()` and screen imports; `planFn` off the `Update` path; dead `StartMsg` fields and the chooser branch. Last PR: an `app.Deps` struct, which touches `main.go`'s composition root | AR-M3, AR-M7, AR-M10, AR-M11, AR-M12, AR-L1, AR-L4 (flight), AR-L5, FB-L9 | `internal/app/**`, `internal/ui/**`, `internal/copycheck`; last PR also `cmd/hoist/main.go` `runTUI` | T5; last PR after T6 | Medium: large moves. The golden suite is the gate | 14 |
| **T8** TUI UX | Sub-stacks: **8a** matrix and pane; **8b** flight and activity; **8c** plan, deploy and tag picker; **8d** chrome, palette, copy and watch; **8e** #260 diagnose; **8f** guide and mockup sync. 8d goes first (shared `internal/ui`), then 8a, 8b and 8c in parallel, 8f last | UX-H12, UX-H14–H18, UX-M1, M4, M10–M12, M14, M15, M17–M28, UX-L4–L6, terminology, #74, #249, #260, #216.6, #243.H4 | 8a `internal/app/matrix`, 8b `internal/app/{flight,activity}` and `session` copy, 8c `internal/app/{plan,deploy,tags}`, 8d `internal/ui`, `internal/app/{watch,restart,config}`, 8f `docs/guide.md`, `docs/tui`; all regenerate goldens | T7 | Medium: visible behaviour. Approve D5 before 8a and 8b | 24 |
| **T9** engine semantics | #41 phase 1 (a merged PR whose head is not this run's commit is Blocked, not done); #256.1; #243.H1/H2; UX-M2's string; CS-5 (fsync or narrow the comment); the rollout half of #171 and the engine copy of the workload derivation; `StateDir` injected. Then #204 per D2 (touches `service/start.go`) | #41 (phase 1), #204, #256.1, #243.H1/H2, UX-M2, CS-5, AR-M2 (StateDir), #171 (rollout), #201.8/.9 | `internal/engine/*`; #204 also `internal/service/start.go`, `inflight.go` | #168 merged; #204 after T6 | High: landing semantics, §4.1. Independent review and a mutant for each fix | 17 |
| **T10** features and warnings | #24 `WarnTargetNewer`; AR-M4 / #7.2 "skipping staging" as a `gitops.Warning` (pkg and service first, the plan screen switch after 8c); the restart half of #171 (StatefulSet and DaemonSet); #172 `registries[].env`; #247 | AR-M4, #24, #7.2, #171 (restart), #172, #247 | `pkg/gitops/plan.go`, `internal/service/policy.go`, `internal/restart`, `pkg/rollout`, `internal/app/restart`, `pkg/registry`, `internal/config`, `internal/service/gc.go` | T5 (`plan.go`), T6 (`internal/restart`), 8c (plan screen) | Low–medium | 12 |
| **T11** docs, comments, test infra | #235 sweep; AR-L2 review-narration diet; stale comments (`resume.go:151`, `deps.go:20`); #169; #90 docs (D1); `internal/gittest` shared git fixtures with the no-background gitconfig; a `t.Parallel` pilot in `pkg/` | #235, #169, #90 (docs), AR-L2, AR-M6 (fixtures) | Comments across ~60 files, `*_test.go` across packages, `docs/` | Everything else merged | Low, but wide: it must run alone | 8 |

**Total:** about 99 agent-hours of build, before review.

**Parallel waves:**

1. **Wave 1:** T5 ∥ T6 ∥ T9 (#204 waits for T6). These footprints are disjoint. T5 touches
   `service/fresh.go`, which T6 does not, and T9 owns `start.go`.
2. **Wave 2:** T7 (after T5), with T10's pkg/service parts alongside it.
3. **Wave 3:** T8, starting with 8d, then 8a, 8b and 8c in parallel. T10's plan-screen switch
   follows 8c.
4. **Wave 4:** T11, alone.

**Collisions that cannot be designed away:**

- `internal/app/app.go` is in T5, T7 and T8. They run in sequence.
- `cmd/hoist/main.go` `runTUI` is in T6 and in T7's last PR. That PR waits for T6.
- `internal/service/start.go` is in T9 (#204, #201.8/.9) only. T6 does not touch it.
- Goldens are regenerated by T5's plan PR, then T8. T7 must leave them byte-identical, which is
  the check that it is a pure refactor.

### Decisions for Alex

| # | Question | Recommendation | Refs |
|---|---|---|---|
| D1 | Can hoist approve its own PRs? | No. Document it as is: README, R-001 and one §4.5 sentence say four eyes need a second person or `approval: auto`. Split the 405-merge wedge into its own issue | #90, `README.md:165`, `docs/repo-map.md:161` |
| D2 | Same-id concurrent starts | Keep `DeriveID` unchanged. Refuse a start whose mode differs from the existing state with that id, and hold an advisory flock for the life of the drive (released if the process dies) | #204, `engine/claim.go`, `service/start.go` |
| D3 | A→B→A re-apply | Phase 1 only: `MergedStep.Observe` returns Blocked, with a plain sentence, when the merged PR's head is known and differs from this run's commit. Defer a generation in the id | #41, `steps_review.go:452`, `landed_guard.go:120` |
| D4 | Accept these as stated limits and close them | Yes: `gc` stays CLI-only (#246); the Argo anchor is a local clock (#53); the `ci.settle`/`ci.ignore` limits (#243 C1–C4) | #246, #53, #243 |
| D5 | A landed-but-stuck promotion has no exit | `shift+x` on a landed promotion offers **archive** instead of abandon: it moves the state file to `archive/`, touches nothing remote, and the dialog says so before the confirm. Decide after #168, which removes the commonest cause (superseded) | UX-H16, `service/promotions.go:511`, R-011 |

The rest of T8 implements the v2 mockups and keymap that T0 already approved, so it needs no new
decision.

# Original audit (2026-09, at `9a15feb`)

## Summary: root causes

| Axis | Root cause | Consequence |
|---|---|---|
| AR | The application layer (use cases shared by CLI and TUI) lives in `cmd/hoist`, package `main`, ~4.9k lines, which nothing can import. | The TUI reaches it through 17 injected func types. Where that was not enough the logic was copied (drive policy, start-a-promotion, resume), and the copies have drifted (AR-H2: `hoist promote` PRs lose resolution warnings). |
| FB | The root model is a router that also runs drives, and no event flows back when a drive finishes. Async results carry no screen-instance identity. | After hoist's own merge the matrix is stale and the next plan is refused with "origin/main has moved" (FB-H1). Esc from flight lands on a stale confirm screen and silently stops the drive (FB-H2). An earlier plan screen's resolve can paint onto a newer one (FB-H3). |
| UX | Keys mean different things per screen. The matrix column cursor means *source* for `p` and *target* for everything else. `enter` does nothing on the matrix. The shared frame has two rendering bugs. | Operators cannot build a model of the keyboard, the most natural gesture (cursor on prod, `p`) asks a question instead of promoting, and screens look broken at 80×24. |

The three share one root: the missing application layer is why nothing flows back (FB), and why
each screen invented its own keys and wording (UX).

## Verification

Every High finding was re-read against the cited lines at `9a15feb`, or against the golden for
rendering findings. All hold. Corrections:

- **UX-H1**: the guide sentence is at `docs/guide.md:74`, and it describes the opposite of the
  code ("promotes the cursor column's paired source into it"). The code treats the column as
  the source (`internal/app/app.go:540-545`, `matrix/model.go:385-391`). The guide is wrong
  about today's code, but it states the direction the operator decided on (plan, Context).
- **AR-H2** was verified before this audit: `cmd/hoist/promote.go:407` builds the plan without
  `resolve.Warnings`, but `cmd/hoist/main.go:385` and `internal/app/plan/model.go:475` prepend
  them. No test covers it.
- **FB-H2 = UX-H6**, **FB-L7 = UX-M7** and **FB-L8 ⊂ UX-H4** are the same defect seen from two
  axes. Both IDs are kept so the scout reports stay citable. A train that fixes one closes both.
- **AR "§4.3 disagreement"** was flagged as uncertain by the scout. It is recorded here as AR-L3,
  a doc correction and not a code finding.

Nothing was dropped.

## Screen graph (today)

The matrix is always at the bottom of the stack and is never popped (`internal/app/app.go`).

```
MATRIX (root)
 ├─ p ──────────► PLAN  (target = envs.pairs[cursor column]; the cursor column is the SOURCE)
 │                 └─ no pair for that column (cursor on prod) ─► plan asks "promote <col> to…"
 ├─ P ──────────► PLAN  (always asks for a target)
 ├─ d ──[image chooser if >1]──► TAGS
 ├─ R ──────────► RESTART   (or a notice if no cluster)
 ├─ w ──────────► WATCH     (or a notice if no cluster)
 ├─ r / enter ──[chooser if >1]──► (background build) ► FLIGHT  (enter, nothing in flight: silent)
 ├─ o ──[chooser if >1]──► PR in the browser (or a notice with the URL)
 ├─ C ──────────► CONFIG
 ├─ ? ──────────► one-line help, inside the notes area
 └─ F5 / ctrl+r   re-read the cluster and the repo

PLAN   enter ─► FLIGHT pushed ON TOP of PLAN        esc ─► matrix
       dialogs: m (direct mode), o (digest override, text input)
TAGS   space ─► DEPLOY (picker popped)   D + confirm ─► DEPLOY, direct mode
       enter ─► commit reader            esc ─► matrix (esc in the D dialog also leaves the picker)
DEPLOY enter ─► FLIGHT pushed ON TOP of DEPLOY      esc ─► matrix (not the picker)
FLIGHT esc ─► pops to the stale PLAN/DEPLOY, and CANCELS the drive
       x ─► matrix, cancels the drive     X + confirm ─► matrix, abandon
       o browser · R re-observe · c ci.none override · l log
RESTART / WATCH / CONFIG   esc ─► matrix
GLOBAL q ─► quit from ANY screen unless it is taking text (app.go:465). ctrl+c always quits.
```

## Keypress counts (today)

Two envs, cursor at column 0 and row 0. Columns sort alphabetically, so `APP-PRODUCTION` comes first.

| Task | Path | Keys |
|---|---|---|
| Deploy the latest release to staging | `→` · `↓×N` · `d` · [chooser] · `space` (after metadata loads) · `enter` | **4+N** (6+N with several images; direct mode +2–3) |
| Promote staging → prod | `→` (cursor on the **source**) · `p` · wait · `enter` | **3** if you know the rule. The intuitive path (cursor on prod, `p`) asks for a target: **6+** |
| See what's in flight and get it approved | pane (0) · `r`/`enter` · `o` · leave the TUI for an approver's comment · come back · `r` | **2–4 + leaving the TUI**. Leaving flight stopped the drive, so nothing merges until it is resumed |
| Check why a rollout is stuck | from flight: `esc` (stale confirm) · `esc` · `←/→×k` · `↓×N` · `w` | **3+k+N** (≈5–8). There is no flight→watch link |

## Deploy-confirm `enter`, end to end (today)

```
Operator   deploy.Model         tea runtime       app.Model (root)              cmd/hoist closure            flight.Model
  | Enter      |                     |                  |                               |                          |
  |----------->| onKey enter (deploy/model.go:213)      |                               |                          |
  |            | start(mode) -> Cmd  |                  |                               |                          |
  |            |-------------------->| StartMsg ------->| case deploy.StartMsg (:971)   |                          |
  |            |                     |                  | buildGen++; cancel old build  |                          |
  |            |                     |                  | ctx = deadline + cancel       |                          |
  |            |                     |                  | push flight.NewBuilding ------------------------------->| building
  |            |                     |-- buildCmd ---------------------------------------->| buildStartPromotion      |
  |            |                     |                  |                               | (wiring.go:51)           |
  |            |                     |                  |                               | checkRepoViewCurrent     |
  |            |                     |                  |                               | ArgoAppNames(BOOT repo)  |
  |            |                     |                  |                               | claim · save · release   |
  |            |                     |-- progressCh -> progressMsg -> top screen ----------------------------------->| buildLog
  |            |                     |<-- promotionBuiltMsg{gen,state,driveFn,err} ----------|                          |
  |            |                     |----------------->| gen stale? DROP silently      |                          |
  |            |                     |                  | err? pop; notice (cleared on next key)                   |
  |            |                     |                  | AdoptBuilt ------------------------------------------->| busy
  |            |                     |-- driveCmd: engine.Drive + engine.Status ---------------------------------->| onDriveResult
  |            |                     |-- tea.Tick(poll) -> tickMsg -> top ---------------------------------------->| driveCmd again
  | (done)     |                     |                  | NO message to root: matrix, drift, in-flight stay STALE  |
  | Esc        |                     |-- flight.BackMsg>| Cancel(); pop -> lands on DEPLOY CONFIRM, no relist      |
```

## Findings: UX

Train column: **T1** service layer · **T2** feedback wiring · **T3** UX and visual · **T4** docs
(see Trains).

| ID | Sev | Where | Problem | Operator sees | Train |
|---|---|---|---|---|---|
| UX-H1 | H | `app.go:540-545`, `matrix/model.go:385-398`, `docs/guide.md:74` | The column cursor is the SOURCE for `p` and the TARGET for `d`/`R`/`w`. The guide says the opposite | With the cursor on prod, `p` asks "promote app-production to…" | T3 |
| UX-H2 | H | `matrix/cells.go:125` | Columns sort alphabetically, not in pipeline order | Production comes first, promotion reads right to left, and the cursor starts on prod | T3 |
| UX-H3 | H | `matrix/model.go:423-429` | `enter` on the matrix does nothing unless something is in flight (only `r` gets a notice) | A dead key where the most natural gesture should be | T3 |
| UX-H4 | H | keymap conflict table (scout) | `x d o R r m/D enter/space` each mean different things per screen | No transferable muscle memory. `R` is a write on the matrix and a read on flight | T3 |
| UX-H5 | H | `app.go:465-474` | `q` quits from any screen, including mid-drive, with no confirmation | One key ends a promotion that is driving | T3 (T2 makes drives survive) |
| UX-H6 | H | `app.go:774-803`, `839-870` | Leaving flight (`esc` or `x`) cancels the drive. `esc` lands on the stale confirm screen | An approved PR never merges. `enter` on the stale confirm starts the build again | T2 |
| UX-H7 | H | `ui/frame.go:61-63` | Frame truncates assembled lines, so the bottom border is dropped on overflow | `flight-approval-80x24`, `flight-ci-none-80x24` etc. have no `╰`. Content runs into the footer | T1 |
| UX-H8 | H | goldens `tags-80x24`, `tags-120x40` | The picker's box is 10 lines, then blank terminal rows, then the footer | The screen looks unfinished, unlike every other screen | T1 |
| UX-H9 | H | `watch/model.go:380` | The idle footer says `read-only · never refreshes` while the header says `every 5s` | The screen contradicts itself | T3 |
| UX-H10 | H | goldens `flight-approval-*`, `matrix-inflight-*` | "blocked on you — comment on PR #103 to release it: hoist approve …" is laid out like a shell command, and hoist cannot approve as the operator | The operator tries to run it, or thinks they can approve their own PR | T3 |
| UX-H11 | H | golden `matrix-help-80x24` | `?` is one line in the notes, truncated at 80 and 120 columns. No other screen has help | `R w r o F5 C ? q` cannot be discovered | T3 |
| UX-H12 | H | goldens `flight-ci-none-80x24`, `flight-blocked-80x24` | The footer truncates from the right, so `esc back` is lost. The status is cut to `blocked: reso…` | The way back is not shown | T3 |
| UX-M1 | M | goldens `matrix-inflight-*` vs `flight-*` | Two step-glyph sets (`● ◍ ○` and `✓ … · ✗`, plus `⟳`) | The same state looks different on two screens | T3 |
| UX-M2 | M | golden `flight-ci-none-80x24` | The blocked reason is printed twice and names the CLI (`hoist resume … --override-ci-none`) while the screen offers `c` | Noise, and the wrong instruction | T3 |
| UX-M3 | M | golden `flight-ci-none-80x24` | Wrapped detail loses its indent | Ragged, hard-to-read reason | T3 |
| UX-M4 | M | flight history section | Raw RFC3339 timestamps and jargon (`branched acted`) next to `started 4 days ago` | Two time formats on one screen | T3 |
| UX-M5 | M | `tags/model.go:219-220` | `space` is primary (review) and `enter` is secondary (read commit), unlike plan and deploy | `enter` does the unexpected thing | T3 |
| UX-M6 | M | tags `D`, plan/deploy `m` | Direct mode has two gestures | Two things to learn for one concept | T3 |
| UX-M7 | M | `app.go:548-559`, `openDeploy` `:1125` | `esc` from deploy skips the picker and returns to the matrix | Changing the tag means `d` and a reload | T3 |
| UX-M8 | M | `tags/model.go:631-634` | `esc` in the picker's `D` dialog leaves the whole picker | Everywhere else, esc closes the dialog only | T3 |
| UX-M9 | M | `ui/styles.go` Selected | The cell cursor is bold text only, and the column cursor is `▸` in the header | Hard to see where row and column meet | T3 |
| UX-M10 | M | golden `matrix-120x40` | 9 of ~34 body rows used, columns don't widen, and the in-flight pane is outside the frame | A big terminal is wasted | T3 |
| UX-M11 | M | golden `matrix-80x24` | `pinned`/`external` repeat in almost every cell | Exceptions don't stand out | T3 |
| UX-M12 | M | goldens `plan-80x24`, `deploy-commits-80x24`, `plan-yaml-120x40` | Hints appear in both the header and the footer, and they disagree | Contradictory hints | T3 |
| UX-M13 | M | golden `deploy-commits-80x24` | The commit list is cut at 11 of 14 with no count | Silent truncation | T3 |
| UX-M14 | M | `flight/model.go:1098` | The in-flight pane shows `deploy → app-staging` with no image or tag | You can't tell what is deploying | T3 |
| UX-M15 | M | `ui/styles.go` | Notice, Warn and Production share one amber, and Status/Hint/Help/Dim share one grey | An info notice looks like a warning | T3 |
| UX-M16 | M | matrix footer (bubbles help) | `•` separator on the matrix, `·` elsewhere | Visual inconsistency | T3 |
| UX-M17 | M | golden `plan-collide-80x24` | A bare `!` glyph, and no-op rows shown as `vX → vX` | Warnings without words; no-ops look like changes | T3 |
| UX-M18 | M | goldens `matrix-empty-80x24`, `tags-80x24` | Empty and error states give no guidance, and the gap message is cut with `…` | Dead ends | T3 |
| UX-L1 | L | goldens `config-80x24`, `flight-done-80x24` | Titles follow no pattern (a path, `confirm deploy`, `promotion · in flight` when done) | Hard to tell where you are | T3 |
| UX-L2 | L | restart/watch goldens | `1 Deployment(s)`, `replica(s)` | Sloppy copy | T3 |
| UX-L3 | L | `docs/guide.md:39`, `:103` | The guide's example footers are out of date | The docs disagree with the screen | T4 |

Terminology drift (fixed by UX-L1's sweep in T3): promote/promotion/deploy · env/environment ·
three refresh verbs (re-read, re-observe, poll now) · abort/abandon/cancel ·
ticked/toggle/review · declares/live/running · mode: PR/direct/direct commit/no PR ·
resolving…/asking the cluster · "occurrence" is never defined.

## Findings: feedback wiring

| ID | Sev | Where | Problem | Operator sees | Train |
|---|---|---|---|---|---|
| FB-H1 | H | `matrix/model.go:369-372`, `flight/model.go:631-632`, `app.go:452-1020` | Nothing tells the root a drive finished. The matrix only re-reads on F5 | Stale cells and drift after a landed deploy. The next confirm says "origin/main has moved … refresh (F5…)" (`cmd/hoist/repoview.go:116-117`) because hoist's own merge moved it | T2 |
| FB-H2 | H | `app.go:774-803`, `1182-1188`, pinned by `app_test.go:433-435` | = UX-H6. Esc cancels the drive and pops onto the confirm screen without relisting | "confirm deploy" for something already running or landed | T2 |
| FB-H3 | H | `plan/model.go:135-145`, `442`, `541-552` | `loadedMsg` has no generation, and resolve runs on `context.Background()` and survives esc | Esc mid-resolve, then `p` elsewhere: the header says one pair, the rows describe another, and enter sends the other plan | T2 |
| FB-M1 | M | `cmd/hoist/main.go:672`, `wiring.go:149-156`, `514-519`, `main.go:683-686` | Start, watch and history closures capture the boot-time repo. An unreachable origin at boot skips the freshness check all session | After F5 adds a family: "no Application…", `w` says "no family", stale blame (#184) | T2 |
| FB-M2 | M | `matrix/model.go:183`, `430-432`, `app.go:507-539`, `wiring.go:315`, `356-363` | `r`/`enter` resume with no building screen. `Resume` ignores ctx and wires no progress/onWaiting (the CLI does, `resume.go:432-455`) | A drive that can merge starts with no visual change, no log lines, and no "waiting for signing approval" | T1 (parity) + T2 |
| FB-M3 | M | `plan/model.go:282-287`, `487`, `app.go:698` | After a refused start the plan screen returns with its ctx cancelled | The next digest override shows every repo's history as "context canceled" | T2 |
| FB-M4 | M | `watch/model.go:78-82`, `103`, `136` | `snapshotMsg` has no generation, and `tickGen` restarts at 0 | Family A's snapshot painted under family B's title | T2 |
| FB-M5 | M | `restart/model.go:62-73`, `142-152` | Restart messages carry no instance id | An old result flips a new restart screen to "rolling" | T2 |
| FB-M6 | M | `flight/model.go:536`, `1126`, `1226` | Nothing animates between polls (up to `poll.approval`, 30s) | The screen looks frozen | T2 |
| FB-M7 | M | `matrix/model.go:299`, `320`, `131`, `313`, `357` | Drift and refresh have no timeout. `refreshingRepo` is never rendered, and success is silent | "asking the cluster" forever. A second F5 is silently skipped | T2 |
| FB-M8 | M | `app.go:417-426`, `501-506`, `548-559` | The in-flight pane relists only when a pop lands on the matrix, or on a ≥30s tick. `deploy.BackMsg` doesn't relist | A landed promotion lingers at its old step | T2 |
| FB-L1 | L | `app.go:461`, `815`, `834`, `699`, `913-916`, `ui/frame.go:148-157` | Results only appear as a root notice, cleared on the next key and capped at `NoticeMaxLines` | The PR URL vanishes, and long errors are cut with `…` | T2 |
| FB-L2 | L | `flight/model.go:814-825`, `app.go:839-870` | `x` "abort" stops watching with no confirmation and no notice | "abort" sounds destructive, but the promotion silently stays | T2 (x retired) |
| FB-L3 | L | `restart/model.go:123`, `204-205`, `264`, `280-284` | No spinner or timeout. Esc while rolling has no note | You can't tell the rollout continues | T2 |
| FB-L4 | L | `deploy/model.go:284-290`, `plan/model.go:620`, `app.go:633`, `1000` | A double enter can emit two starts | The second cancels the first mid-claim, leaving a residual state file | T2 |
| FB-L5 | L | `flight/model.go:118`, `405-408`, `app.go:645-659` | Progress and tick messages carry no generation. One listener goroutine leaks per building screen | An old attempt's lines appear on a new flight | T2 |
| FB-L6 | L | `flight/model.go:804-810`, `app.go:334-337`, `903-905` | `R` during the abandon wait renews the drive ctx, and abandon doesn't cancel it | A drive can race the abandon | T2 |
| FB-L7 | L | `app.go:1125` | = UX-M7 | — | T3 |
| FB-L8 | L | `tags/model.go:219-220`, `matrix/model.go:183`, `426` | ⊂ UX-H4 / UX-M5: `enter` means confirm, read or resume | — | T3 |
| FB-L9 | L | `deploy/model.go:51-52`, `main.go:724-727`, `screen.go:84-89`, `docs/guide.md:55` | Unused `StartMsg` fields, and stale docs (resolve "degrades", nil DriveFunc, F5) | Misleading comments and guide text | T1 (code) + T4 (guide) |
| FB-L10 | L | `main.go:669-670`, `wiring.go:515`, `549` | Argo/rollout client errors are captured once at boot | `R` and `w` stay disabled all session, even after the cluster returns | T2 |

## Findings: architecture

| ID | Sev | Where | Problem | Operator sees | Train |
|---|---|---|---|---|---|
| AR-H1 | H | `flight/model.go:676`, `688`, `1307` vs `cmd/hoist/drive.go:187`, `191`, `26` | Drive policy is copied because `main` can't be imported. The copies diverge (flight falls back to 2s on ≤0) and their comments are stale (they say only CIGreen/Approved retry; five steps do) | TUI and CLI poll and retry differently | T1 |
| AR-H2 | H | `promote.go:333-552`, `deploy.go:39-240`, `wiring.go:50-241` | "Start a promotion" exists three times, and the claim-release order and `s.Direct` timing differ. **Drift, verified:** `promote.go:407` omits `resolve.Warnings` (`main.go:385` and `plan/model.go:475` include them) | A PR opened by `hoist promote` lacks the resolution warnings a TUI-opened PR carries | T1 (fix is T1 base) |
| AR-H3 | H | find-by-id `abandon.go:48-57`, `resume.go:278-289`, `wiring.go:320-329`; clients+`ensureArgoApps` ×5; `AllSteps` vs `AllDirectSteps` ×5 | Resume, list and abandon are rebuilt at every call site. There is no drive-side `StepsFor(state)` | Behaviour differs by entry point (TUI resume has no onWaiting) | T1 |
| AR-M1 | M | `drive.go:58`, `flight/model.go:491-760`, `wiring.go:273-279` | Two drivers. Each TUI tick runs Drive and then Status, so it observes twice | Double API traffic per poll | T1 |
| AR-M2 | M | `engine.go:136-205`, `258-329`, `376-444`, `steps_m4.go`, `steps_m5.go` | Three copies of the same walk, seven step-list constructors, milestone-named files, Observe mutates state, the one-in-flight rule is split between `main` and the engine, a global `StateDir` | Hard to change safely | T1 (split + `StepsFor`); rest ticketed |
| AR-M3 | M | `app.go:300-430`, `507-1019` | The root (1310 lines) is also a session controller, with the ctx/gen/deadline block repeated three times | The source of FB-H1/H2 | T2 |
| AR-M4 | M | `plan/model.go:438-477`, `app.go:1114`, `plan/rows.go:370`, `387`; `cmd/hoist/deploy.go:109` | Plan building and the production rule live in screen packages. The CLI imports a screen for a domain rule | — | T1 |
| AR-M5 | M | kube-ctx fallback ×7 (`main.go:304`, `resolution.go:82`, `deploy.go:155`, `restart.go:98`, `history.go:71`, `main.go:836`, `resume.go:32`) | Config is threaded by hand: `(client, err)` pairs, 9-parameter builders, four overlapping settings structs | — | T1 |
| AR-M6 | M | `promote.go:29-32`, `resolution.go:24-29`, `main.go:609`, `wiring.go:417`, `drive.go:121` | Global test seams are overridden ~44 times, there are zero `t.Parallel` calls, and git test helpers are copied ×3 | Slow suite, order-sensitive tests | T1 (seams shrink); rest ticketed |
| AR-M7 | M | `plan/model.go:442`, `matrix/model.go:299`, `320`, `restart/model.go:123`, `264`, `284`, `watch/model.go:129`, `app.go:346`, `promote.go:400` vs `480` | Screen commands run on `context.Background()` | Hangs can't be cancelled | T2 |
| AR-L1 | L | tags 1594 lines, flight 1333, plan 1324, matrix 1036 | God screen files. Confirm-dialog code is in 6 screens, `dialogWidth` ×5, and `orNone`/`short`/`plural` ×3 | — | T3 |
| AR-L2 | L | 137 review-history refs in non-test code (`wiring.go:27-49`, `promote.go:54-85`) | Comments are 30–42% of big files and go stale across files | — | T4 |
| AR-L3 | L | AGENTS.md §4.3, §4.8 | §4.3 describes `pkg/` as `func(ctx, In) (Out, error)`, but `git.Git`, `forge.Forge` etc. are stateful interfaces. §4.8 says screens never import `config`, but four do | The rules doc disagrees with the code (principle 1) | T4 |

Target layering (the code already points this way; `internal/restart` is the model):

```
cmd/hoist        flags → request structs, CLI rendering, exit codes
internal/app     root router + session controller + screens → service only
internal/service Settings · Deps (lazy clients) · Plan · StartPromotion · Find/List/Resume/Abandon
                 · Driver{Step, Run} · policy (production warning, StepsFor, one-in-flight)
internal/engine  steps + one walk + state store; files split by concern
pkg/*            unchanged (§4.3 import rule holds today)
```

## Trains

| Train | Scope | Closes |
|---|---|---|
| **T0** | This doc, the proposed keymap, and v2 mockups (`docs/tui`). Docs only. **Gate:** the operator approves the mockups and keymap before T3 | — |
| **T1** service layer | Base fixes first: `promote` warnings (AR-H2), frame border and picker fill (UX-H7, UX-H8). Then the engine file split + `StepsFor`, drive policy into the engine, and `internal/service` (`Settings`, `Deps`, `Plan`, `StartPromotion`, `Find/List/Resume/Abandon`, `Driver`). `cmd/hoist` slims down | AR-H1–H3, AR-M1, AR-M2 (part), AR-M4, AR-M5, AR-M6 (part), UX-H7, UX-H8, FB-M2 (parity), FB-L9 (code) |
| **T2** feedback wiring | `internal/app/session` controller owns drives. Background drives (esc never cancels, `x` retired). Generation stamp on every async result. A `PromotionChanged` event relists and re-reads the repo. Closures read current state. An owned ctx and deadline per command. Visible waiting (countdowns, spinners). A persistent activity log | FB-H1–H3, FB-M1–M8, FB-L1–L6, FB-L10, UX-H6, AR-M3, AR-M7 |
| **T3** UX + visual | Keymap registry + footer helper. Keymap migration (below). Matrix: pipeline order, `p` into the cursor column, cell cursor, `enter` menu, detail pane. Help overlay. Palette and glyph set. Flight, confirm and picker redesigns. Copy sweep | UX-H1–H5, UX-H9–H12, UX-M1–M18, UX-L1, UX-L2, FB-L7, FB-L8, AR-L1 |
| **T4** docs | AGENTS.md §6 compression and §4.3/§4.8 corrections, guide rewrite, repo-map service/session boundaries, `docs/tui` README, a comment diet | UX-L3, FB-L9 (guide), AR-L2, AR-L3 |

## Out of scope

Tracked separately and not built by these trains:

- **#90**: approve from the TUI (hoist would act as the operator, which is a trust question
  first). UX-H10 fixes only the *copy*.
- **#168**: `RolledOutStep` per-occurrence landed verdict (AGENTS.md §4.1 interim state).
- **#41**, **#53**: engine semantics questions flagged in the tracker.
- **#170–#173**: coverage gaps.

## Proposed keymap

This is the spec that T3 migrates to. It answers UX-H3, UX-H4, UX-H5, UX-H11, UX-M5, UX-M6,
UX-M8 and FB-L8. **Status: proposal. The operator approves it before T3 starts.**

### Rules

1. **One semantic class per key, across every screen.** `enter` is always the screen's primary
   action, `esc` is always back, and the arrow keys are always spatial movement — what "primary"
   or "back" resolves to on a given screen varies (rules 2 and 3 say how), but the key's *class*
   never does. A letter key carries one meaning on every screen it is bound on. A key that does
   not apply on a screen is unbound there, never reused for something else.
2. **`enter` is the screen's primary action.** On a confirm screen that is the write, and it
   stays unshifted because the confirm screen *is* the deliberate step (the diff is on it). A
   screen whose job is watching (flight, watch, config) has no primary action, so `enter` is
   unbound there. That also means a stray second `enter` from the confirm screen that just
   pushed the flight screen does nothing (FB-L4).
3. **`esc` is back.** It never quits, never cancels a drive, and inside a dialog it closes the
   dialog only. On the matrix it closes the menu or overlay, and otherwise does nothing.
4. **`q` quits, and only from the matrix.** If a drive is running it asks first ("2 promotions
   are driving; they stop until resumed — quit?"). On every other screen `q` is **unbound**: it
   shows the notice "q quits from the matrix · esc goes back", and a text field gets the
   letter. *Why unbound and not "back":* `q`-as-back gives `q` two meanings, and pressing `q`
   repeatedly to back out quits the app the moment the matrix is reached. `esc` is already
   back, so one key for one meaning. `ctrl+c` always quits at once as the escape hatch. On the
   way out it prints the ids still in flight and `hoist resume <id>`.
5. **Writes use the shift modifier, and only writes do.** `shift+r` restart, `shift+x` abandon,
   `shift+d` direct-mode toggle, `shift+c` treat "no checks" as green. `shift+d` does not itself
   commit anything — it flips which mode the confirm screen's own `enter` will write in — but the
   write it gates is real, so it carries the modifier along with the keys that write directly.
   Displayed as `shift+<key>`
   everywhere a key is shown — footers, the help overlay, docs — never as the bare capital letter,
   because a capital in a footer reads as "press this letter" and invites the caps-lock press
   that a legacy terminal cannot tell apart from shift (see "Modifier" below). Every write is
   still followed by a confirmation (a dialog or the confirm screen) — that confirmation, not the
   key, is the actual safety; the modifier only keeps a write from firing on an unmodified
   letter someone was typing for another reason. So `shift+p`, `shift+c`-for-config, flight's
   `shift+r` re-observe and the `G` (bottom) navigation key retire.

   **Modifier.** hoist requests no keyboard enhancements today (no screen sets
   `tea.View.KeyboardEnhancements`; verified against `charm.land/bubbletea/v2` v2.0.9 — see T3
   below), so every terminal runs the legacy path: a printable letter arrives as a single byte,
   and ultraviolet's decoder (`decoder.go`'s `parseUtf8`) sets `ModShift` on it whenever the byte
   is uppercase — the terminal never tells hoist *why* the byte is uppercase. `shift+c` and
   caps-lock-then-`c` produce the identical `Key{Code: 'c', Text: "C", Mod: ModShift}` on every
   terminal in this state, keyboard-enhancement-capable or not, because the enhancement was never
   asked for. This is true today on kitty, WezTerm, Ghostty, foot, and recent iTerm2 exactly as
   much as on Terminal.app, Termius (iOS), and a default SSH/tmux session — the terminal's own
   capability is irrelevant until hoist opts in. Only a terminal that (a) supports the Kitty
   keyboard protocol and (b) is asked for `KeyboardEnhancements.ReportAllKeysAsEscapeCodes` (which
   hoist does not currently request) can report a real, separate `ModCapsLock` bit distinct from
   `ModShift` (`decoder.go`'s `fromKittyMod`, bits `kittyShift`/`kittyCapsLock`) — and note that
   `KeyPressMsg.String()` still prefers `Key.Text` ("C") over the modifier-qualified keystroke, so
   even bubbles' `key.Matches(msg, key.WithKeys("shift+c"))` never fires for a printable letter;
   only reading `msg.Key().Mod.Contains(tea.ModShift)` directly (with `ModCapsLock` excluded)
   distinguishes shift from caps lock, and only on that opted-in, protocol-capable path.

   **Update, shipped (T3-01, T3 review P1-1):** hoist now requests
   `ReportAllKeysAsEscapeCodes | ReportAlternateKeys | ReportAssociatedText` on every render
   (`app.go`'s `View`) — not flag 8 alone. Flag 8 by itself was tried first and shipped a real
   bug: without also asking for the alternate-key and associated-text components, a Kitty-capable
   terminal reports a shifted punctuation key with no shifted-key or text component at all, so the
   decoder's fallback (upper-casing the base rune) is a no-op for punctuation — shift+/ still
   reads as `/`, never `?`, and shift+; as `;`, never `:`. All three flags together is what
   AGENTS.md §9 entry 13 and `internal/app/kitty_decode_test.go` now cover; a legacy terminal
   that doesn't support the request is unaffected either way, exactly as this paragraph
   describes.
   Principle 1: hoist does not claim a mechanism it does not run. So: matching stays "the
   uppercase letter" everywhere (`key.WithKeys("C")`, unchanged from today) — legacy terminals
   have no other signal, and asking every screen to special-case an enhancement most sessions
   (SSH, tmux, Termius) will never grant is not worth it for a signal the confirmation screen
   already makes safe. If T3 later opts into `ReportAllKeysAsEscapeCodes` for the matrix and finds
   it does not regress non-kitty terminals (Termius, Terminal.app, tmux defaults), the write
   bindings can additionally require `Mod.Contains(ModShift) && !Mod.Contains(ModCapsLock)` on
   that path without changing what is displayed — but that is future work, not this amendment.
6. **One verb per concept.** `r` refresh (the cluster, the repo, or the promotion's remote,
   whichever the screen shows), with `F5` and `ctrl+r` as aliases wherever `r` is bound. `o`
   opens in the browser. `d` toggles diff/yaml. `w` watch. `l` activity log. `?` help overlay.
   `space` toggles a selection. `/` filters. `tab` moves focus between panes.
7. **Navigation keys are shared.** `↑/↓` (and `j/k`), `←/→`, `pgup/pgdn`, `home/end`. `h/l` retire
   as aliases because `l` is the log.

### Screen × key

`·` means unbound. "back" means pop to the screen below.

| Key | matrix | action menu | plan confirm | deploy confirm | tag picker | flight | watch | restart | config | help overlay |
|---|---|---|---|---|---|---|---|---|---|---|
| `enter` | open the action menu for the cell. On the in-flight pane: open flight | run the highlighted item | **start promotion** | **start deploy** | review this tag → deploy confirm | · | · | **restart** (production: confirm dialog) | · | close |
| `esc` | close the menu or overlay, else nothing | close | back | back to the **picker** | back (in the reader: back to the list) | back to the matrix, **drive keeps running** | back | back (a rollout in progress continues, and the screen says so) | back | close |
| `q` | quit (confirm if driving) | · | · | · | · (filter gets the letter) | · | · | · | · | · |
| `?` | help overlay | help overlay | help overlay | help overlay | help overlay | help overlay | help overlay | help overlay | help overlay | close |
| `r` `F5` `ctrl+r` | re-read the cluster and the repo | · | rebuild the plan at fresh origin | rebuild the diff at fresh origin | reload tags | re-observe now | poll now | re-read the targets | · (static text, §4.8) | · |
| `o` | open the PR of the in-flight row (chooser if several) | · | · | · | open the commit under the cursor on the forge | open the PR | · | · | · | · |
| `d` | · | · | toggle yaml diff | toggle yaml diff | · | · | · | · | · | · |
| `p` | promote **into** the cursor column (source from the reverse pair, else asks) | promote into | · | · | · | · | · | · | · | · |
| `t` | deploy a tag (tag picker for the cell) | deploy a tag | · | · | · | · | · | · | · | · |
| `w` | watch the cell's family | watch | · | · | · | watch this promotion's family and target | · | · | · | · |
| `shift+r` | restart the cell's family → restart screen | restart | · | · | · | · | · | · | · | · |
| `shift+x` | abandon the in-flight row (confirm) | · | · | · | · | abandon (confirm) | · | · | · | · |
| `shift+d` | · | · | toggle direct mode (confirm when turning on; never offered for production) | same | · | · | · | · | · | · |
| `shift+c` | · | · | · | · | · | treat "no checks" as green (confirm; only when offered) | · | · | · | · |
| `e` | · | · | override the hovered repo's digest (input dialog) | · | · | · | · | · | · | · |
| `space` | · | · | tick / untick repo | · | · | · | · | · | · | · |
| `/` | · | · | filter | · | filter | · | · | · | · | · |
| `tab` | table ⇄ in-flight pane | · | repos ⇄ impact pane | · | list ⇄ commits | · | · | · | · | · |
| `→` `←` | move column | · | · | · | → open the commit reader, ← back to the list | · | · | · | · | · |
| `c` | config view | · | · | · | · | · | · | · | · | · |
| `l` | activity log | · | activity log | activity log | activity log | activity log (this promotion's lines first) | activity log | activity log | activity log | · |
| `↑↓` `j/k` | move row | move | move | scroll commits | move (reader: switch commit) | scroll log | scroll | scroll | scroll | · |
| `pgup/pgdn` `home/end` | page | · | page | page | page (reader: scroll body) | page | page | page | page | · |
| `ctrl+c` | quit now, printing what is in flight | ← | ← | ← | ← | ← | ← | ← | ← | ← |

Decisions the rules left open:

- **Tag picker: `enter` reviews the tag** (primary, which goes to deploy confirm). Commit
  reading moves to `→` (or `tab` into the commits pane, then `→`). `space` is no longer bound
  there. *Why:* `enter` = primary is the rule. Reading commits is secondary and spatial (the
  pane to the right), so `→` is the key.
- **Matrix deploy shortcut is `t`** ("tag"), because `d` is now diff everywhere. The action
  menu lists the shortcut next to each item, so the menu teaches the shortcuts.
- **Digest override is `e`** ("edit the digest"). It is not a remote write (it rebuilds the
  plan), so it is lowercase. `o` is open-in-browser everywhere.
- **Config is `c`**. It is a read, so it is lowercase. `shift+c` is the ci.none override, a
  write, on flight.
- **Re-attaching to a drive** is `enter` on the in-flight pane (`tab` focuses it), or the
  action menu's "resume in-flight" item when the cell's env has one. `r` is refresh only.
- **`shift+p` retires.** "Promote into this env from a different source" is an action menu item.
  When the cursor column has no reverse pair, `p` asks for the source.

### Migration checklist (old → new)

| Screen | Old | New | Finding |
|---|---|---|---|
| global | `q` quits from any screen | `q` quits from the matrix only (confirm if driving); unbound elsewhere with a notice | UX-H5 |
| global | `?` one-line help on the matrix only | `?` help overlay on every screen | UX-H11 |
| global | three refresh verbs (`F5` re-read, `R` re-observe, `r` poll now) | `r` refresh everywhere, `F5`/`ctrl+r` aliases | UX-H4 |
| global | per-screen notice only | `l` activity log (T2) | FB-L1 |
| matrix | `enter` resume (silent no-op otherwise) | `enter` action menu; on the in-flight pane, open flight | UX-H3, FB-M2 |
| matrix | `r` resume | `r` refresh; resume = `enter` on the pane or the menu item | UX-H4 |
| matrix | `p` promote the cursor column (as the source) into its pair | `p` promote **into** the cursor column | UX-H1 |
| matrix | `P` promote to… | retired → action menu "promote into <env> from…" | rule 5 |
| matrix | `d` deploy a tag | `t` deploy a tag | UX-H4 |
| matrix | `C` config | `c` config | rule 5 |
| matrix | `R` restart the cell's family → restart screen | `shift+r` restart the cell's family → restart screen | rule 5 |
| matrix | `h/l` column aliases | retired (`←/→` only) | rule 7 |
| matrix | — | `tab` focus the in-flight pane; `shift+x` abandon from the pane | — |
| plan | `x` toggle repo | `space` toggle repo | UX-H4 |
| plan | `m` direct mode | `shift+d` direct mode | UX-M6 |
| plan | `o` digest override | `e` digest override | UX-H4 |
| plan | — | `r` rebuild at fresh origin | FB-H1 |
| deploy | `m` direct mode | `shift+d` direct mode | UX-M6 |
| deploy | `space` scroll | `pgdn` / `↓` | UX-H4 |
| deploy | `esc` → matrix | `esc` → picker (the picker stays on the stack) | UX-M7, FB-L7 |
| tags | `space` review the change | `enter` review the change | UX-M5 |
| tags | `enter` read commit | `→` read commit (`←` back) | UX-M5, FB-L8 |
| tags | `D` + confirm → direct deploy | retired; `shift+d` on the deploy confirm | UX-M6 |
| tags | `esc` in the D dialog leaves the picker | retired with `D`; every dialog closes on esc | UX-M8 |
| tags | `g/G` top/bottom | `home/end` | rule 7 |
| tags | — | `r` reload, `o` open commit | — |
| flight | `esc` back to the confirm screen + **cancel drive** | `esc` → matrix, drive keeps running (T2) | UX-H6, FB-H2 |
| flight | `x` abort (stop watching) | retired: esc already leaves without stopping | FB-L2 |
| flight | `X` + confirm → abandon | `shift+x` abandon (confirm) | rule 5 |
| flight | `R` re-observe | `r` re-observe | UX-H4 |
| flight | `c` ci.none override | `shift+c` ci.none override | rule 5 |
| flight | `G` log bottom | `end` | rule 5 |
| flight | — | `w` watch | UX keypress table |
| watch | `r` poll now | `r` (same key, "refresh" wording); footer shows `next poll in Ns` | UX-H9 |
| config | `g/G` top/bottom | `home/end` (`internal/app/config/model.go:30-31`) | rule 7 |
| restart | `esc` pops silently mid-rollout | `esc` pops; notice "rollout continues" | FB-L3 |

### `internal/parity` impact

The registry cites keys in its `TUI:` strings, and it parses the navigation messages the root
switches on. The T3 keymap PR updates these rows in the same change:

- **Key text changes:** "promote" (`p`/`P` → `p` + menu), "direct mode" (`m`/`D` → `shift+d`),
  "deploy" (`d`, `space` → `t`, `enter`), "resume" (`r` → `enter` on the pane), "re-observe"
  (`R` → `r`), "digest override" (`o` → `e`), "ci.none override" (`c` → `shift+c`), "config"
  (`C` → `c`).
- **Rows removed or reshaped:** "stop watching a promotion" (`flight.AbortMsg x`). Its TUI
  side becomes "esc from flight; the drive keeps running in the session" (T2), still two-sided
  with the CLI's ctrl-c.
- **New navigation messages** that need rows: flight → watch (`w`), the matrix action menu if
  it is a root-handled message, and the help overlay if the root owns it. Keeping the menu and
  the overlay inside the matrix and `internal/ui` avoids new root cases.
- **Keymap test (T3):** the `internal/ui/keys` registry asserts every key resolves to at most one
  semantic class (primary / back / spatial) across screens, and that a letter key means the same
  thing on every screen it is bound on, so rule 1 is enforced by a test rather than by review
  (AGENTS.md §10 meta-rule 5).
- **One write-binding helper (T3):** `internal/ui/keys` builds every write binding through one
  constructor that takes keys `"shift+c"` and, on the legacy input path, `"C"` (see "Modifier"
  above — the uppercase letter is what a legacy terminal actually sends for both shift and caps
  lock, and hoist runs the legacy path everywhere today), and whose help text always renders
  `"shift+c"`. A test asserts no footer or help string in the built binaries shows a bare capital
  write key.
