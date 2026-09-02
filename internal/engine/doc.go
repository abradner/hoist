package engine

// Assumption this milestone rests on, stated rather than enforced: BuildPlan's Edits carry
// the Occurrence.Line/Col/Raw that gitops.Discover recorded from the *user's own clone*
// (--repo), which reads whatever is on disk there — including any uncommitted change. This
// package's worktree is created fresh from Base's committed tip (invariant 1: the user's own
// checkout is never touched, so nothing here can read the user's uncommitted edits even if it
// wanted to). If the user's clone has uncommitted changes to a file this promotion edits, the
// worktree's copy of that file will differ from what BuildPlan planned against, and
// gitops.Apply's own re-verification (invariant 3) will refuse the mismatch loudly — a safe
// failure, not a silent wrong-content commit — rather than this package detecting "the clone
// is dirty" ahead of time (that would need a `git status --porcelain` primitive pkg/git's
// brief-specified interface does not include). Closing this gap, if it is ever worth closing,
// means either adding such a primitive or documenting "run hoist promote against a clean
// checkout" as a hard requirement enforced by the CLI before the engine ever starts.
