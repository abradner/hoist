package engine

// Assumption this milestone rests on, stated rather than enforced: BuildPlan's Edits carry
// the Occurrence.Line/Col/Raw that gitops.Discover recorded from the *user's own clone*
// (--repo), which reads whatever is on disk there. This package's worktree is created fresh
// from Base's resolved current tip regardless (pkg/git.Exec.Worktree's own resolveBase: prefer
// origin/<base> whenever that ref exists, fall back to the local branch of the same name
// otherwise) — if the clone's content silently disagreed with that, gitops.Apply's own
// re-verification (invariant 3) would refuse the mismatch loudly, a safe failure rather than a
// silent wrong-content commit.
//
// That gap — this package detecting the disagreement itself, before ever reaching Apply — is
// closed one layer up: cmd/hoist's own runPromote calls checkCloneCurrentForBase right after
// BuildPlan, unconditionally. It fetches origin/<base> fresh itself, first — never "as last
// fetched into this clone", which used to be this check's own gap (round 5: nothing ever
// fetched before it ran, so staleness was unbounded, not merely a documented limitation) — then
// compares the clone's on-disk content, and what applying the plan's own edits to it would
// produce, against the SAME revision Worktree's own resolveBase would actually build a new
// promotion from: origin/<base> whenever that ref exists at all, unconditionally, never a
// special case for "local happens to be ahead" (round 5's other finding: Worktree does not treat
// that specially either, so this check must not). A mismatch refuses clearly, naming the file
// and both revisions, before any worktree exists or any commit is attempted; a match — "the
// clone agrees with what the worktree will actually be built from" or "that content already
// carries exactly what this promotion would produce" (this promotion's own prior push, on
// resume, or someone else reaching the identical end state) — proceeds, so a killed-and-resumed
// direct-mode promotion is never mistaken for foreign drift. This package itself still never
// enforces the checkout is clean or current — that check lives in cmd/hoist, which is what
// actually knows how to refuse before starting the engine at all.
//
// checkCloneCurrentForBase only validates files plan.Edits already names — it has nothing to
// compare a file against if gitops.Discover never saw that file to begin with. Direct mode's own
// prior pushes are the one route that can put origin/<base> ahead of the clone's local disk in a
// way the clone itself never observes (PushHeadTo only ever moves origin's ref; §4.6 forbids
// touching the clone's own checked-out branch directly, and nothing else refreshes it), so a
// promotable image repo can gain a whole new occurrence on origin/<base> that the clone's disk
// has no record of at all — silently left on the old image, since nothing ever asked about it.
// cmd/hoist's own checkNoMissingOccurrenceAtFreshBase closes that gap alongside
// checkCloneCurrentForBase, direct mode only: it discovers and plans a second time, from a
// throwaway detached checkout of origin/<base>'s actual current tree (pkg/git.Git.WorktreeAtRef),
// and refuses if that finds an occurrence — by file/line/column, never by its current value, since
// a differing value at an already-known position is exactly what checkCloneCurrentForBase already
// catches — the clone-based plan doesn't already know about. The PR flow's own worktree is always
// built directly from the clone's content, whatever it is, and any staleness there surfaces as an
// ordinary GitHub merge conflict; only direct mode's silent, no-PR write needs this loud a refusal.
//
// DirectPushedStep's own re-observe (direct.go) compares planned blob CONTENT at whatever
// origin/<base>'s current tip is, never mere object-graph ancestry — an earlier revision used
// git.Git.IsAncestor instead, which could not tell "Base advanced with a distinct, later,
// legitimate change that never touches these paths" (content still matches; genuinely still
// satisfied) from "this exact promotion was reverted" (this promotion's commit remains an
// ancestor forever — a revert never removes it from history — but the content it changed is no
// longer at the tip). See DirectPushedStep.Observe's own doc comment for the full reasoning.
