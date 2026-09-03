package engine

// Assumption this milestone rests on, stated rather than enforced: BuildPlan's Edits carry
// the Occurrence.Line/Col/Raw that gitops.Discover recorded from the *user's own clone*
// (--repo), which reads whatever is on disk there — including any uncommitted change, or
// content that has simply fallen behind --base's real current tip (AGENTS.md M6 gotcha: direct
// mode advances refs/remotes/origin/<base> in the clone without ever touching the clone's own
// checked-out branch or working tree — invariant 1 — so a clean-looking clone can still be
// stale relative to what a prior direct-mode promotion, or anyone else, actually pushed). This
// package's worktree is created fresh from Base's resolved current tip regardless, so if the
// clone's content silently disagreed with it, gitops.Apply's own re-verification (invariant 3)
// would refuse the mismatch loudly — a safe failure, not a silent wrong-content commit.
//
// That gap — this package detecting the disagreement itself, before ever reaching Apply — is
// closed one layer up: cmd/hoist's own runPromote calls checkCloneCurrentForBase right after
// BuildPlan, unconditionally, comparing the clone's on-disk content (and what applying the
// plan's own edits to it would produce) against --base's local remote-tracking content — this
// check never fetches; it trusts refs/remotes/origin/<base> as last updated into this clone,
// the same ref pkg/git.Exec's own resolveBase resolves for worktree creation, and the same
// "as last fetched" limitation every other check in this codebase that reads a remote-tracking
// ref without fetching first already carries. A mismatch on both comparisons refuses clearly,
// naming the file,
// before any worktree exists or any commit is attempted; a match against either — "the clone
// agrees with base" or "base already carries exactly what this promotion would produce" (this
// promotion's own prior push, on resume, or someone else reaching the identical end state) —
// proceeds, so a killed-and-resumed direct-mode promotion is never mistaken for foreign drift.
// This package itself still never enforces the checkout is clean or current — that check lives
// in cmd/hoist, which is what actually knows how to refuse before starting the engine at all.
