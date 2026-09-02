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

type prResponse struct {
	Number         int       `json:"number"`
	HTMLURL        string    `json:"html_url"`
	Body           string    `json:"body"`
	Merged         bool      `json:"merged"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	CreatedAt      time.Time `json:"created_at"`
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
		Merged:     r.Merged,
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

type checkRunsResponse struct {
	CheckRuns []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

// Checks implements forge.Forge: a minimal rollup over check-runs. M4 turns this into a gate.
func (c *Client) Checks(ctx context.Context, sha string) (forge.CheckSummary, error) {
	var resp checkRunsResponse
	path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100", c.owner, c.repo, sha)
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return forge.CheckSummary{}, translateErr("listing checks", err)
	}
	var s forge.CheckSummary
	s.Total = len(resp.CheckRuns)
	for _, r := range resp.CheckRuns {
		switch {
		case r.Status != "completed":
			s.Pending++
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
			s.Success++
		default:
			s.Failure++
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

// Comments implements forge.Forge: PR conversation comments (GitHub models these as issue
// comments), newer than since. Bots (Type != "User") are excluded here as informational
// filtering only — R-001's actual author check is M4's job, done against the API's own login
// field, never the comment body.
func (c *Client) Comments(ctx context.Context, prNumber int, since time.Time) ([]forge.Comment, error) {
	q := url.Values{"since": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?%s", c.owner, c.repo, prNumber, q.Encode())
	var resp []commentResponse
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, translateErr("listing comments", err)
	}
	out := make([]forge.Comment, 0, len(resp))
	for _, r := range resp {
		if r.User.Type != "" && r.User.Type != "User" {
			continue
		}
		out = append(out, forge.Comment{ID: r.ID, Author: r.User.Login, Body: r.Body, CreatedAt: r.CreatedAt})
	}
	return out, nil
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
