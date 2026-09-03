// Package github implements pkg/forge.Forge against the real GitHub REST API, using
// github.com/cli/go-gh/v2 (AGENTS.md §4.7's one sanctioned new dependency for M3 — not
// google/go-github, not a hand-rolled REST client, not the full gh CLI as a library). New
// resolves credentials exactly the way the gh CLI itself would (its own keyring/config, via
// go-gh's api.DefaultRESTClient): this package never reads a token from a flag or an
// environment variable of its own invention, and never sees the token value directly —
// go-gh sets the Authorization header inside its own HTTP transport (AGENTS.md §4.3, R-002).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// maxSearchPages bounds FindPR's fallback scan of open, then closed, PRs when nothing
// matches by head branch: 3 pages of 100 is generous for a single-operator tool's repo and
// keeps a renamed-branch lookup from becoming an unbounded crawl of PR history.
const maxSearchPages = 3

// Client implements forge.Forge against api.github.com (or a GitHub Enterprise host, via the
// gh environment's own resolution — this package does not special-case it).
type Client struct {
	rest        *ghapi.RESTClient
	owner, repo string
}

// New builds a Client for ownerRepo ("owner/name", RepoConfig.GitHub), authenticating
// through the user's own `gh` login exactly as the gh CLI would.
func New(ownerRepo string) (*Client, error) {
	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("github: %q is not owner/name", ownerRepo)
	}
	rest, err := ghapi.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("github: resolving gh auth: %w", err)
	}
	return &Client{rest: rest, owner: owner, repo: repo}, nil
}

// newWithClient is the seam pkg/forge/github's own tests use: a *ghapi.RESTClient built with
// an explicit dummy token and a fake http.RoundTripper (ghapi.ClientOptions{AuthToken,
// Transport}), so a test here never touches gh's real keyring or a real network call
// (AGENTS.md hard constraints) while still exercising this package's own request-building
// and response-parsing.
func newWithClient(rest *ghapi.RESTClient, owner, repo string) *Client {
	return &Client{rest: rest, owner: owner, repo: repo}
}

type prPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// prResponse decodes both the single-PR endpoint (GET .../pulls/{n}) and the list endpoint
// (GET .../pulls, used by FindPR) — but GitHub's two endpoints do not report merge state the
// same way: the single-PR response has a real "merged" boolean, while a list-endpoint item has
// none at all (decoding it always leaves Merged at its zero value, false) and instead carries
// "merged_at", a nullable timestamp, non-nil exactly when the PR is merged. toPR below treats
// either signal as authoritative — never trusting "merged" alone, since FindPR's callers (every
// re-observation of an already-open PR) go through the list endpoint, and a merged PR read that
// way would otherwise always report Merged=false. Round-6 finding: after a successful merge
// deletes the branch, the next Observe re-read the PR via FindPR, saw Merged=false from this
// exact gap, and — with the branch now gone — misclassified a completed promotion as still
// stuck at Pushed, blocking every future promotion to that env.
type prResponse struct {
	Number         int        `json:"number"`
	HTMLURL        string     `json:"html_url"`
	Body           string     `json:"body"`
	Merged         bool       `json:"merged"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	CreatedAt      time.Time  `json:"created_at"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func toPR(r prResponse) forge.PR {
	return forge.PR{
		Number:     r.Number,
		URL:        r.HTMLURL,
		HeadBranch: r.Head.Ref,
		HeadSHA:    r.Head.SHA,
		Base:       r.Base.Ref,
		Merged:     r.Merged || r.MergedAt != nil,
		MergeSHA:   r.MergeCommitSHA,
		CreatedAt:  r.CreatedAt,
	}
}

// CreatePR implements forge.Forge.
func (c *Client) CreatePR(ctx context.Context, spec forge.PRSpec) (forge.PR, error) {
	body, err := json.Marshal(prPayload{Title: spec.Title, Body: spec.Body, Head: spec.Head, Base: spec.Base})
	if err != nil {
		return forge.PR{}, err
	}
	var resp prResponse
	path := fmt.Sprintf("repos/%s/%s/pulls", c.owner, c.repo)
	if err := c.rest.DoWithContext(ctx, http.MethodPost, path, bytes.NewReader(body), &resp); err != nil {
		return forge.PR{}, translateErr("creating the PR", err)
	}
	return toPR(resp), nil
}

// FindPR implements forge.Forge: first the exact head-branch match GitHub's own "head"
// filter gives for free, then — when a branch was renamed or recreated, so nothing shares
// its name anymore — a bounded scan of open, then closed, PRs for the body marker (Known bug
// classes: "Finding an existing PR only by branch name and missing one whose branch was
// renamed").
func (c *Client) FindPR(ctx context.Context, headBranch, bodyMarker string) (forge.PR, bool, error) {
	q := url.Values{"head": {c.owner + ":" + headBranch}, "state": {"all"}}
	path := fmt.Sprintf("repos/%s/%s/pulls?%s", c.owner, c.repo, q.Encode())
	var byHead []prResponse
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &byHead); err != nil {
		return forge.PR{}, false, translateErr("listing PRs by head branch", err)
	}
	if len(byHead) > 0 {
		return toPR(byHead[0]), true, nil
	}
	if bodyMarker == "" {
		return forge.PR{}, false, nil
	}
	for _, state := range []string{"open", "closed"} {
		prs, err := c.listPRs(ctx, state, maxSearchPages)
		if err != nil {
			return forge.PR{}, false, err
		}
		for _, p := range prs {
			if strings.Contains(p.Body, bodyMarker) {
				return toPR(p), true, nil
			}
		}
	}
	return forge.PR{}, false, nil
}

func (c *Client) listPRs(ctx context.Context, state string, maxPages int) ([]prResponse, error) {
	var out []prResponse
	for page := 1; page <= maxPages; page++ {
		q := url.Values{"state": {state}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		path := fmt.Sprintf("repos/%s/%s/pulls?%s", c.owner, c.repo, q.Encode())
		var batch []prResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, translateErr("listing "+state+" PRs", err)
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type checkRunsResponse struct {
	CheckRuns []checkRun `json:"check_runs"`
}

// commitStatus is one entry of GitHub's older Statuses API (repos/.../commits/{sha}/status) —
// a separate rollup from check-runs above, used by CI systems that predate (or simply still
// use) the Statuses API instead of Checks. State is one of "success", "pending", "failure" or
// "error"; Context is the status's own name (equivalent to a check-run's Name).
type commitStatus struct {
	State   string `json:"state"`
	Context string `json:"context"`
}

type combinedStatusResponse struct {
	Statuses []commitStatus `json:"statuses"`
}

// maxCheckRunPages bounds Checks' pagination: 10 pages of 100 (1000 check runs) is far beyond
// any real CI matrix this tool will ever gate on, while still keeping a pathological rollup from
// paging forever — the same defensive-bound shape as maxSearchPages for FindPR's fallback scan.
const maxCheckRunPages = 10

// Checks implements forge.Forge: the check-run rollup CIGreenStep gates on (M4), paged through
// every page of check-runs GitHub reports for sha (Known bug classes: "only page 1 of a
// check-run set is fetched" — a commit with more than 100 check runs would otherwise silently
// hide a pending or failed run past the first page, and CI could appear green when it isn't).
// FailedNames names every check-run that concluded in something other than
// success/neutral/skipped; SkippedNames separately names every run that concluded `skipped`,
// which forge.CheckSummary's own doc comment explains is never folded into Success. A run's own
// name is upstream text nothing here wrote (a CI system can name a job anything), so both name
// lists go through redact.Strings at this adaptor boundary the same way translateErr already
// does for GitHub's own free-form error messages (AGENTS.md invariant 6).
func (c *Client) Checks(ctx context.Context, sha string) (forge.CheckSummary, error) {
	var runs []checkRun
	// lastPageFull tracks whether the loop's final iteration returned a full 100-row page:
	// true only when the loop ran out of pages (maxCheckRunPages) without ever seeing a
	// short page, which is exactly "there may be more beyond the bound" (Known bug classes,
	// the P1-adjacent hardening this pagination fix's own bound can reintroduce: silently
	// truncating at maxCheckRunPages is the identical failure mode as never paginating at
	// all, just moved further out — a later pending/failed run past the bound would make CI
	// appear green when it isn't).
	lastPageFull := false
	for page := 1; page <= maxCheckRunPages; page++ {
		var resp checkRunsResponse
		path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", c.owner, c.repo, sha, page)
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return forge.CheckSummary{}, translateErr("listing checks", err)
		}
		runs = append(runs, resp.CheckRuns...)
		if len(resp.CheckRuns) < 100 {
			lastPageFull = false
			break
		}
		lastPageFull = true
	}
	if lastPageFull {
		// A full last page alone cannot distinguish "there are more check-runs beyond the bound"
		// from "this commit has exactly maxCheckRunPages*100 check-runs and the last page just
		// happens to be completely full" — the latter is not truncation at all, and erroring on
		// it would falsely block a promotion on an unusually large but entirely finite CI
		// matrix. Resolve it with one more request for the page immediately past the bound, at
		// per_page=100 — the SAME page size every other page in the loop above used — for page
		// maxCheckRunPages+1: this is still a single bounded extra call, never a further
		// unbounded scan. per_page must match, not shrink to 1: GitHub's pagination offset is
		// (page-1)*per_page, so a smaller per_page on this one request would ask for a
		// different, earlier slice of items than "the 100-item batch right after what's already
		// fetched" — page=maxCheckRunPages+1 at per_page=1 asks for the single item at offset
		// maxCheckRunPages*1, not maxCheckRunPages*100, and almost always finds something,
		// falsely reporting truncation for any commit with more than ~maxCheckRunPages check-runs
		// (a real regression from an earlier round of this exact fix).
		sentinelPath := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100&page=%d", c.owner, c.repo, sha, maxCheckRunPages+1)
		var sentinel checkRunsResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, sentinelPath, nil, &sentinel); err != nil {
			return forge.CheckSummary{}, translateErr("listing checks", err)
		}
		if len(sentinel.CheckRuns) > 0 {
			return forge.CheckSummary{}, fmt.Errorf(
				"github: checking checks for %s: more than %d check-runs (the %d-page bound at 100 per page) — refusing to report a possibly truncated result rather than silently hide a pending or failed run past the bound",
				sha, maxCheckRunPages*100, maxCheckRunPages,
			)
		}
	}
	// Commit statuses (the older Statuses API) are a genuinely separate CI reporting mechanism
	// from check-runs above — a repository can report through either, or both at once, and
	// they are never merged server-side. Fetched here as a second, independent rollup and
	// folded into the same CheckSummary: a repo whose real CI reports only through statuses
	// (or mixes a green check-run with a pending/failing status context) must gate on it too,
	// not just whichever one this adaptor happened to query first (Known bug classes:
	// "querying check-runs alone, so a status-only or mixed CI reports green/none when the
	// repository's real gate is still pending or failing").
	var combined combinedStatusResponse
	statusPath := fmt.Sprintf("repos/%s/%s/commits/%s/status", c.owner, c.repo, sha)
	if err := c.rest.DoWithContext(ctx, http.MethodGet, statusPath, nil, &combined); err != nil {
		return forge.CheckSummary{}, translateErr("listing commit statuses", err)
	}

	var s forge.CheckSummary
	s.Total = len(runs) + len(combined.Statuses)
	for _, r := range runs {
		name := r.Name
		if name == "" {
			name = "(unnamed check)"
		}
		switch {
		case r.Status != "completed":
			s.Pending++
		case r.Conclusion == "success" || r.Conclusion == "neutral":
			s.Success++
		case r.Conclusion == "skipped":
			s.Skipped++
			s.SkippedNames = append(s.SkippedNames, redact.Strings(name))
		default:
			s.Failure++
			s.FailedNames = append(s.FailedNames, redact.Strings(name))
		}
	}
	for _, st := range combined.Statuses {
		name := st.Context
		if name == "" {
			name = "(unnamed status)"
		}
		switch st.State {
		case "pending":
			s.Pending++
		case "success":
			s.Success++
		default: // "failure", "error", or anything else unrecognized — never silently green
			s.Failure++
			s.FailedNames = append(s.FailedNames, redact.Strings(name))
		}
	}
	return s, nil
}

type commentResponse struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

// maxCommentPages bounds Comments' pagination: 10 pages of 100 (1000 comments) comfortably
// covers a real promotion PR's conversation while keeping a pathological one from paging
// forever — the same defensive-bound shape as maxCheckRunPages and maxSearchPages.
const maxCommentPages = 10

// Comments implements forge.Forge: PR conversation comments (GitHub models these as issue
// comments), newer than since, with AuthorType carrying the API's own account "type" through
// (M4: internal/engine.ApprovedStep is what actually enforces R-001's author check against it,
// against the API's login field, never the comment body — see that package's doc comment for
// why enforcement lives there and not here). Bots (Type == "Bot") are additionally excluded
// here too, as a politeness layer only (AGENTS.md §8 "layered checks": deleting this filter
// would not change what a correctly-written ApprovedStep accepts, only which layer reports it).
// This deliberately excludes only "Bot", not "anything other than User": GitHub's account
// "type" field also has legitimate non-"User" values such as "Organization" (an org-owned
// account, not a bot), and the set of values isn't closed, so filtering on Type != "User" would
// silently drop a real, non-bot commenter along with actual bots.
//
// Paged through every page GitHub reports (Known bug classes: "only page 1 of a PR's comments
// is fetched" — a PR with more than 100 comments could otherwise hide an approval or a later
// reject past the first page, so ApprovedStep's newest-match scan would never see it).
func (c *Client) Comments(ctx context.Context, prNumber int, since time.Time) ([]forge.Comment, error) {
	var resp []commentResponse
	// lastPageFull mirrors Checks' own bound-vs-truncation tracking (Known bug classes, the
	// P1-adjacent hardening this pagination fix's own bound can reintroduce): true only when
	// every page up to maxCommentPages came back full, meaning there may be more comments
	// beyond the bound this loop never saw — silently truncating here could hide a later
	// approve or reject exactly as the original unpaginated bug did, just moved further out.
	lastPageFull := false
	for page := 1; page <= maxCommentPages; page++ {
		q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?%s", c.owner, c.repo, prNumber, q.Encode())
		var batch []commentResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, translateErr("listing comments", err)
		}
		resp = append(resp, batch...)
		if len(batch) < 100 {
			lastPageFull = false
			break
		}
		lastPageFull = true
	}
	if lastPageFull {
		// As in Checks: a full last page alone cannot distinguish "more comments exist beyond
		// the bound" from "this PR has exactly maxCommentPages*100 comments and the last page
		// just happens to be completely full" — the latter isn't truncation, and erroring on it
		// would falsely block a promotion on a long-but-finite PR conversation. One more
		// per_page=100 request (the SAME page size as every other page above) for the page past
		// the bound resolves it without any further unbounded scanning. per_page must match, not
		// shrink to 1, for the same reason Checks' own sentinel does (see its comment): GitHub's
		// offset is (page-1)*per_page, so a smaller per_page here would ask for a different,
		// earlier slice than "the next 100-item batch" and almost always find something, falsely
		// reporting truncation.
		q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}, "page": {fmt.Sprint(maxCommentPages + 1)}}
		sentinelPath := fmt.Sprintf("repos/%s/%s/issues/%d/comments?%s", c.owner, c.repo, prNumber, q.Encode())
		var sentinel []commentResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, sentinelPath, nil, &sentinel); err != nil {
			return nil, translateErr("listing comments", err)
		}
		if len(sentinel) > 0 {
			return nil, fmt.Errorf(
				"github: listing comments for PR #%d: more than %d comments (the %d-page bound at 100 per page) — refusing to report a possibly truncated result rather than silently hide a later approve or reject past the bound",
				prNumber, maxCommentPages*100, maxCommentPages,
			)
		}
	}
	out := make([]forge.Comment, 0, len(resp))
	for _, r := range resp {
		if r.User.Type == "Bot" {
			continue
		}
		out = append(out, forge.Comment{ID: r.ID, Author: r.User.Login, AuthorType: r.User.Type, Body: r.Body, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// permissionResponse is GET .../collaborators/{username}/permission's body: the caller's own
// effective permission on the repo (admin, write, maintain, triage, read, or none). GitHub
// returns 404, not 200, when login is not a collaborator at all — a routine, expected outcome
// for anyone commenting on a public PR, never an error (IsAllowedAuthor folds it into a plain
// "not allowed" below). A 403 here still means the repo itself or the token's scope couldn't
// resolve the query (translateErr's "gh token may be missing the repo scope this needs" message
// applies unchanged to that case).
type permissionResponse struct {
	Permission string `json:"permission"`
}

// IsAllowedAuthor implements forge.Forge: true for "admin", "maintain" or "write" — R-001's
// stated bar is "collaborator with write (or higher) permission" (forge.Forge's own doc
// comment), and GitHub ranks maintain strictly above write (admin > maintain > write > triage >
// read), so a maintain-level collaborator qualifies exactly like write and admin do; only
// "triage" and "read" fall below the bar. A 404 (login is not a collaborator — the ordinary
// case for a drive-by commenter on a public PR) is folded into (false, nil) rather than
// propagated as an error: treating it as fatal would make any non-collaborator's comment error
// and block the whole promotion whenever collaborators=true, rather than simply not counting
// as an approval.
func (c *Client) IsAllowedAuthor(ctx context.Context, login string) (bool, error) {
	var resp permissionResponse
	path := fmt.Sprintf("repos/%s/%s/collaborators/%s/permission", c.owner, c.repo, url.PathEscape(login))
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var herr *ghapi.HTTPError
		if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, translateErr("checking collaborator permission for "+login, err)
	}
	return resp.Permission == "admin" || resp.Permission == "maintain" || resp.Permission == "write", nil
}

// mergeResponse is PUT .../pulls/{n}/merge's body on success; its fields aren't otherwise
// trusted — MergePR re-fetches the PR fresh via getPR immediately after, so the returned
// forge.PR reflects the API's own current state rather than this response's own snapshot.
type mergeResponse struct {
	Merged bool `json:"merged"`
}

type mergePayload struct {
	SHA         string `json:"sha,omitempty"`
	MergeMethod string `json:"merge_method"`
}

// MergePR implements forge.Forge: a squash merge, gated atomically on the server's own "sha"
// parameter (Known bug classes: "don't roll your own check-then-merge race") — a head that has
// moved since expectedHeadSHA was observed is refused with a 409, translated to
// forge.ErrStaleHead. Squash is always used, so a multi-commit branch's title would normally
// concatenate every commit subject; this promotion is guaranteed exactly one commit (M3), so
// that default composes to that one commit's own message, never a leak of unrelated history
// (Known bug classes, confirmed here rather than assumed).
func (c *Client) MergePR(ctx context.Context, prNumber int, expectedHeadSHA string) (forge.PR, error) {
	body, err := json.Marshal(mergePayload{SHA: expectedHeadSHA, MergeMethod: "squash"})
	if err != nil {
		return forge.PR{}, err
	}
	var mr mergeResponse
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/merge", c.owner, c.repo, prNumber)
	if err := c.rest.DoWithContext(ctx, http.MethodPut, path, bytes.NewReader(body), &mr); err != nil {
		var herr *ghapi.HTTPError
		if errors.As(err, &herr) && herr.StatusCode == http.StatusConflict {
			return forge.PR{}, fmt.Errorf("github: merging PR #%d: HTTP 409 %s: %w", prNumber, redact.Strings(herr.Message), forge.ErrStaleHead)
		}
		return forge.PR{}, translateErr(fmt.Sprintf("merging PR #%d", prNumber), err)
	}
	// The merge response itself carries no head/base/branch info — re-fetch the PR so the
	// returned forge.PR is the API's current, complete state (Known bug classes: "did the merge
	// actually happen server-side" is answered by asking the forge, never by trusting a call
	// that may itself have been the one whose response got lost).
	return c.getPR(ctx, prNumber)
}

// getPR fetches prNumber fresh: used by MergePR (whose own response is minimal) and available
// for a caller re-checking "did this already merge" after a call whose response never arrived.
func (c *Client) getPR(ctx context.Context, prNumber int) (forge.PR, error) {
	var resp prResponse
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", c.owner, c.repo, prNumber)
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return forge.PR{}, translateErr(fmt.Sprintf("getting PR #%d", prNumber), err)
	}
	return toPR(resp), nil
}

// translateErr turns a go-gh HTTPError into a message actionable for AGENTS.md §6.1's "gh
// CLI's own token scopes" gotcha: a 403/404 here is exactly what a `repo`-scope gap looks
// like, indistinguishable at the wire level from "doesn't exist" — surface both possibilities
// rather than a bare status code.
func translateErr(op string, err error) error {
	var herr *ghapi.HTTPError
	if errors.As(err, &herr) {
		// herr.Message is free-form text from GitHub's own response body — untrusted in the
		// same sense pkg/registry's error codes are (AGENTS.md §4.10): it is not attacker
		// content in the normal case (this only talks to the operator's own configured
		// repo), but it is still upstream text nothing here wrote, so it goes through
		// redact.Strings before reaching the operator like every other adaptor's output does.
		msg := redact.Strings(herr.Message)
		if herr.StatusCode == http.StatusForbidden || herr.StatusCode == http.StatusNotFound {
			return fmt.Errorf("github: %s: HTTP %d %s (the gh token may be missing the repo scope this needs — check `gh auth status`, and `gh auth refresh -s repo` if so)", op, herr.StatusCode, msg)
		}
		return fmt.Errorf("github: %s: HTTP %d %s", op, herr.StatusCode, msg)
	}
	return fmt.Errorf("github: %s: %w", op, err)
}
