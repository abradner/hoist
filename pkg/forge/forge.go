// Package forge is the code-host boundary (GitHub today; AGENTS.md §11 leaves GitLab as an
// interface with no adaptor until a GitLab repo needs one). Forge is activity-shaped
// (AGENTS.md §4.3): every method is func(ctx, In) (Out, error) with JSON-serialisable,
// secret-free inputs and outputs — the credential itself never crosses this boundary, only
// its effect. pkg/forge/github resolves auth from the user's own `gh` login via go-gh; this
// package never reads a token, an env var, or a flag naming one.
package forge

import (
	"context"
	"errors"
	"time"
)

// ErrStaleHead is returned (wrapped) by MergePR when the PR's current head sha no longer
// matches expectedHeadSHA: something else moved the branch since this promotion last observed
// it pushed, and merging blind could squash-merge content this promotion never verified
// (AGENTS.md named adversary; R-003's neighbor). Callers distinguish this from a plain
// transport error with errors.Is.
var ErrStaleHead = errors.New("forge: PR head does not match the expected sha")

// PRSpec is what CreatePR needs to open a pull request. Head and Base are branch names, not
// refs (no "refs/heads/" prefix).
type PRSpec struct {
	Title, Body, Head, Base string
}

// PR is a pull request as this package cares about it: identity, where it points, and its
// merge state. Body is deliberately not part of PR — only PRSpec carries it in, since nothing
// downstream of CreatePR/FindPR needs to read it back (FindPR searches it internally).
type PR struct {
	Number     int
	URL        string
	HeadBranch string
	HeadSHA    string
	Base       string
	Merged     bool
	MergeSHA   string
	CreatedAt  time.Time
	// Closed is true for a PR someone closed WITHOUT merging (GitHub's own "state": "closed"
	// with Merged still false) — a dead PR a merge call can never succeed against (a real 405).
	// FindPR's query (state=all, so a renamed-branch/body-marker fallback can still find a
	// promotion's own history) can return one of these, so a caller adopting a found PR must
	// check this explicitly rather than assuming "found" means "usable."
	Closed bool
}

// CheckSummary is the combined check-run AND commit-status rollup for one commit sha (GitHub
// reports CI through two genuinely separate mechanisms — check-runs and the older Statuses API
// — and a repository can use either or both; an implementation must query and fold in both,
// never just one). FailedNames lists the name of every check-run or status context (not empty
// run titles) that concluded in something other than success/neutral, so CIGreenStep can name
// which checks failed rather than just reporting a count (M4). Skipped has no equivalent among
// commit statuses (the Statuses API has no "skipped" state) — SkippedNames only ever names
// check-runs.
//
// Skipped is its own bucket, not folded into Success: a `skipped` conclusion means the check
// never actually ran (a path filter, a conditional job) — GitHub reports it the same way whether
// the check was optional or required, and this type carries no required-vs-optional distinction
// at all, so there is no way to tell "safely skipped" from "a required gate that silently never
// ran" at this layer. CIGreenStep therefore treats any Skipped>0 as blocking, the same as
// Failure>0 (AGENTS.md §2 principle 5: "warn, don't block, except where the runbook blocks" — a
// required check that never ran is exactly that exception). SkippedNames mirrors FailedNames.
type CheckSummary struct {
	Total, Pending, Success, Failure, Skipped int
	FailedNames, SkippedNames                 []string
}

// Comment is one issue/PR comment. AuthorType is the GitHub account "type" of the commenter
// ("User", "Organization", "Bot", …) — added in M4 so a caller can tell a bot apart from a
// legitimate non-"User" account (an org-owned login is real, per the precedent in
// pkg/forge/github's Comments doc comment) without trusting the comment body for anything.
type Comment struct {
	ID         int64
	Author     string
	AuthorType string
	Body       string
	CreatedAt  time.Time
}

// GitTag is one git tag on the forge as Tags reports it: the tag name and the date of the
// commit it points to. Date is deliberately the *commit's* date, not an annotated tag
// object's own creation date — "when was this released" (M6's tag-ordering use, AGENTS.md
// invariant 3) means when the code was committed, not when someone got around to tagging it,
// and a lightweight tag has no object date to read anyway.
type GitTag struct {
	Name string
	Date time.Time
}

// Forge is the seam between internal/engine and the code host. Every adaptor (pkg/forge/github,
// and Fake for tests) implements the same interface, so internal/engine never knows which one
// it is talking to.
type Forge interface {
	// CreatePR opens a new pull request from spec.Head into spec.Base. The caller (internal/
	// engine's PROpened step) always calls FindPR first — CreatePR is not itself required to
	// deduplicate, though implementations may refuse an obviously duplicate head branch as a
	// safety net (Fake does; the real GitHub API does this on its own, returning 422).
	CreatePR(ctx context.Context, spec PRSpec) (PR, error)
	// FindPR looks for an existing pull request for this promotion: first by headBranch, then
	// — so that a PR survives its branch being renamed or the branch being deleted and
	// recreated — by searching bodyMarker (the exact "<!-- hoist:id=... -->" line) across
	// open PRs and, if that search is empty, recently closed/merged ones within a bound.
	// ok=false means no matching PR exists yet, not an error.
	FindPR(ctx context.Context, headBranch, bodyMarker string) (PR, bool, error)
	// Checks reports the check-run rollup for sha. Stubbed/minimal is fine for M3; M4 extends
	// it into a gate.
	Checks(ctx context.Context, sha string) (CheckSummary, error)
	// Comments lists comments on prNumber posted at or after since. Stubbed/minimal is fine
	// for M3; M4 scans these for the approval magic comment.
	Comments(ctx context.Context, prNumber int, since time.Time) ([]Comment, error)
	// IsAllowedAuthor reports whether login is a collaborator with write (or higher) permission
	// on the repo — the second way (besides RepoConfig.Approvers) an Approved comment's author
	// can be accepted (R-001). A permission-scope error (the gh token lacking what this needs)
	// must be returned as an error, never silently folded into false — AGENTS.md §6.1's "the gh
	// token may be missing the repo scope this needs" gotcha applies here exactly as it does to
	// every other adaptor call.
	IsAllowedAuthor(ctx context.Context, login string) (bool, error)
	// MergePR squash-merges prNumber, but only if the PR's current head sha still equals
	// expectedHeadSHA — using the forge's own atomic "merge iff head is X" primitive, never a
	// client-side check-then-merge (a race the caller cannot close on its own). A stale head is
	// reported as an error satisfying errors.Is(err, ErrStaleHead). Merging an already-merged PR
	// is not an error the caller must avoid causing — MergePR may report it either way, and a
	// caller must re-check FindPR before concluding a merge failed outright (a killed process
	// cannot always tell whether its own call landed server-side).
	MergePR(ctx context.Context, prNumber int, expectedHeadSHA string) (PR, error)
	// Tags lists every git tag on this forge's repo, each with the date of the commit it
	// points to. Added for M6: the tag picker prefers the app repo's own git tags for
	// ordering registry tags by recency (AGENTS.md invariant 3) over the registry's own
	// unordered, timestamp-free tag list (AGENTS.md §6.1 item 3). Order is unspecified —
	// callers sort by Date themselves; a repo with no tags returns an empty slice, not an
	// error.
	Tags(ctx context.Context) ([]GitTag, error)
}
