// Package forge is the code-host boundary (GitHub today; AGENTS.md §11 leaves GitLab as an
// interface with no adaptor until a GitLab repo needs one). Forge is activity-shaped
// (AGENTS.md §4.3): every method is func(ctx, In) (Out, error) with JSON-serialisable,
// secret-free inputs and outputs — the credential itself never crosses this boundary, only
// its effect. pkg/forge/github resolves auth from the user's own `gh` login via go-gh; this
// package never reads a token, an env var, or a flag naming one.
package forge

import (
	"context"
	"time"
)

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
}

// CheckSummary is the check-run rollup for one commit sha. Stubbed/minimal for M3 — M4 reads
// it to decide whether a PR is green.
type CheckSummary struct {
	Total, Pending, Success, Failure int
}

// Comment is one issue/PR comment. Stubbed/minimal for M3 — M4 scans these for the approval
// magic comment.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
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
}
