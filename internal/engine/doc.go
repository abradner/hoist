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
