package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// The M10 half of the adaptor: commit history between two revisions of an app repo, the
// files each commit touched, and the age of one manifest line — everything pkg/migrate needs
// and nothing the promotion pipeline itself calls. Kept in its own file so the M3–M6 surface
// in github.go stays the reviewed thing it was.

// maxComparePages bounds Compare's pagination: 3 pages of 100 (300 commits) is past
// GitHub's own unpaginated default of 250, generous for one promotion's delta, and keeps a
// stale env (months behind) from turning one confirm-screen open into an unbounded crawl.
// Comparison.Truncated says when the bound bit; the adaptor never silently drops the tail.
const maxComparePages = 3

// maxCommitFilesPages bounds CommitFiles and CommitsTouching the same way.
const maxCommitFilesPages = 3

// githubFilesCap is the number of files GitHub reports for a compare or a commit before it
// stops listing them (documented: "up to 300 files"). A response carrying exactly this many is
// read as possibly truncated.
const githubFilesCap = 300

type commitResponse struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
	Files []struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

func toCommit(r commitResponse) forge.Commit {
	subject, body, _ := strings.Cut(r.Commit.Message, "\n")
	body = strings.TrimSpace(body)
	return forge.Commit{
		SHA:     r.SHA,
		Subject: cleanLine(strings.TrimSpace(subject)),
		Body:    clean(body),
		Author:  cleanLine(r.Commit.Author.Name),
		Date:    r.Commit.Author.Date,
	}
}

// ResolveRef implements forge.Forge: GET .../commits/{ref}, the same endpoint Tags already uses
// to date a tag, which accepts a tag, a branch, or a sha prefix. 404 (no such ref) and 422
// (an ambiguous prefix) are ok=false; anything else — a 403 in particular, the scope-gap
// signature — is an error, per the interface's own contract.
func (c *Client) ResolveRef(ctx context.Context, ref string) (string, bool, error) {
	var resp commitResponse
	path := fmt.Sprintf("repos/%s/%s/commits/%s", c.owner, c.repo, url.PathEscape(ref))
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var herr *ghapi.HTTPError
		if errors.As(err, &herr) && (herr.StatusCode == http.StatusNotFound || herr.StatusCode == http.StatusUnprocessableEntity) {
			if herr.StatusCode == http.StatusNotFound {
				// A 404 is also what a private repo the token cannot read answers; only a
				// visible repo's 404 means "no such ref" (repoVisible).
				if verr := c.repoVisible(ctx); verr != nil {
					return "", false, verr
				}
			}
			return "", false, nil
		}
		return "", false, translateErr("resolving "+ref, err)
	}
	return resp.SHA, true, nil
}

type compareResponse struct {
	Status       string           `json:"status"`
	AheadBy      int              `json:"ahead_by"`
	BehindBy     int              `json:"behind_by"`
	TotalCommits int              `json:"total_commits"`
	Commits      []commitResponse `json:"commits"`
	Files        []struct {
		Filename string `json:"filename"`
	} `json:"files"`
}

// Compare implements forge.Forge: GET .../compare/{base}...{head}, paged. GitHub's
// total_commits is authoritative for the whole range regardless of paging, so Truncated is
// len(commits) < total rather than a sentinel request. The file list is only ever on page 1
// and GitHub caps it at githubFilesCap; a full list is reported FilesTruncated. A 404 is
// forge.ErrUnknownRef, wrapped with the same scope hint translateErr gives — an unknown ref and
// a private repo the token cannot see are the same status code.
func (c *Client) Compare(ctx context.Context, base, head string) (forge.Comparison, error) {
	const perPage = 100
	var out forge.Comparison
	fetch := func(page int) (compareResponse, error) {
		q := url.Values{"per_page": {fmt.Sprint(perPage)}, "page": {fmt.Sprint(page)}}
		path := fmt.Sprintf("repos/%s/%s/compare/%s...%s?%s", c.owner, c.repo, url.PathEscape(base), url.PathEscape(head), q.Encode())
		var resp compareResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			var herr *ghapi.HTTPError
			if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
				if verr := c.repoVisible(ctx); verr != nil {
					return resp, verr
				}
				return resp, fmt.Errorf("%w: %w", forge.ErrUnknownRef, translateErr(fmt.Sprintf("comparing %s...%s", base, head), err))
			}
			return resp, translateErr(fmt.Sprintf("comparing %s...%s", base, head), err)
		}
		return resp, nil
	}
	first, err := fetch(1)
	if err != nil {
		return forge.Comparison{}, err
	}
	out.Status, out.AheadBy, out.BehindBy, out.Total = first.Status, first.AheadBy, first.BehindBy, first.TotalCommits
	for _, f := range first.Files {
		out.Files = append(out.Files, cleanLine(f.Filename))
	}
	out.FilesTruncated = len(first.Files) >= githubFilesCap

	// GitHub pages a comparison oldest first. When the range fits in maxComparePages the
	// walk is 1..n; when it does not, the pages that matter are the LAST ones — the caller
	// documents Commits as the newest len(Commits) of Total, and a migration in the newest
	// fifty of a 350-commit range is exactly the one that must not fall off the end.
	lastPage := (out.Total + perPage - 1) / perPage
	firstPage := 1
	if lastPage > maxComparePages {
		firstPage = lastPage - maxComparePages + 1
	}
	pages := []compareResponse{}
	if firstPage == 1 {
		pages = append(pages, first)
	}
	for page := max(firstPage, 2); page <= lastPage && page <= firstPage+maxComparePages-1; page++ {
		resp, err := fetch(page)
		if err != nil {
			return forge.Comparison{}, err
		}
		pages = append(pages, resp)
		if len(resp.Commits) < perPage {
			break
		}
	}
	for _, resp := range pages {
		for _, r := range resp.Commits {
			out.Commits = append(out.Commits, toCommit(r))
		}
	}
	if firstPage > 1 {
		// The last pages alone hold fewer than the bound when the total is not a multiple
		// of the page size; the page just before firstPage fills the gap — page 1 when that
		// is the one, otherwise fetched — so the result is the newest bound, contiguous.
		if need := maxComparePages*perPage - len(out.Commits); need > 0 {
			before := first
			if firstPage-1 != 1 {
				var err error
				if before, err = fetch(firstPage - 1); err != nil {
					return forge.Comparison{}, err
				}
			}
			if need <= len(before.Commits) {
				var fill []forge.Commit
				for _, r := range before.Commits[len(before.Commits)-need:] {
					fill = append(fill, toCommit(r))
				}
				out.Commits = append(fill, out.Commits...)
			}
		}
	}
	out.Truncated = len(out.Commits) < out.Total
	return out, nil
}

// repoVisible answers, once per Client, whether the token can see the repository at all:
// GitHub answers 404 for a missing ref and for a private repository the token cannot read,
// and the Forge contract says a scope failure is an error, never ok=false — pkg/migrate reads
// ok=false as "try the next source" and would otherwise degrade every revision to unknown
// without a word about why. nil means visible; the error carries translateErr's scope hint.
func (c *Client) repoVisible(ctx context.Context) error {
	c.visMu.Lock()
	defer c.visMu.Unlock()
	if c.visKnown {
		return c.visErr
	}
	var resp struct {
		FullName string `json:"full_name"`
	}
	err := c.rest.DoWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s", c.owner, c.repo), nil, &resp)
	if err == nil {
		c.visKnown = true
		return nil
	}
	verr := translateErr(fmt.Sprintf("reading %s/%s (the repository itself is not visible to this token)", c.owner, c.repo), err)
	// Only a definitive answer is remembered: 404 and 403 say the token cannot see the
	// repository. A cancelled context, a 5xx or an exhausted rate limit is the moment's
	// answer, and caching it would replay a stale failure for the rest of the session.
	var herr *ghapi.HTTPError
	if errors.As(err, &herr) && (herr.StatusCode == http.StatusNotFound || herr.StatusCode == http.StatusForbidden) && !rateLimited(herr) {
		c.visKnown, c.visErr = true, verr
	}
	return verr
}

// rateLimited recognises GitHub's two rate-limit shapes — a primary limit (403 with
// X-Ratelimit-Remaining: 0) and a secondary one (403/429 whose message says so) — which are
// the moment's answer, never the repository's visibility.
func rateLimited(herr *ghapi.HTTPError) bool {
	if herr.StatusCode == http.StatusTooManyRequests || herr.Headers.Get("X-Ratelimit-Remaining") == "0" {
		return true
	}
	return strings.Contains(strings.ToLower(herr.Message), "rate limit")
}

// clean strips terminal control sequences from upstream text before it is ever styled or
// rendered: a commit subject is whatever its author typed, and a crafted OSC/CSI sequence in
// one would otherwise reach the operator's terminal verbatim the moment the picker opens.
// ANSI escapes go through ansi.Strip; the remaining C0 controls are dropped except newline
// and tab, which commit bodies legitimately carry. redact.Strings runs after, as before.
// Anything that is one line on a screen — a subject, an author, a file name (git allows
// both bytes in a path) — goes through cleanLine, which drops those two as well.
func clean(s string) string {
	s = ansi.Strip(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return redact.Strings(s)
}

// cleanLine is clean for single-line text: newline and tab go too, so a file name cannot
// add rows to a confirm screen.
func cleanLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return -1
		}
		return r
	}, clean(s))
}

// CommitFiles implements forge.Forge: GET .../commits/{sha}, whose files[] pages at 100 like
// everything else here and is capped by GitHub at githubFilesCap in total.
func (c *Client) CommitFiles(ctx context.Context, sha string) ([]string, bool, error) {
	var files []string
	lastFull := false
	for page := 1; page <= maxCommitFilesPages; page++ {
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		path := fmt.Sprintf("repos/%s/%s/commits/%s?%s", c.owner, c.repo, url.PathEscape(sha), q.Encode())
		var resp commitResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, false, translateErr("listing files of commit "+sha, err)
		}
		for _, f := range resp.Files {
			files = append(files, cleanLine(f.Filename))
		}
		lastFull = len(resp.Files) >= 100
		if !lastFull {
			break
		}
	}
	return files, lastFull || len(files) >= githubFilesCap, nil
}

// CommitsTouching implements forge.Forge: GET .../commits?sha={ref}&path={path}&since=…,
// newest first as GitHub orders it, paged to the same bound.
func (c *Client) CommitsTouching(ctx context.Context, ref, path string, since time.Time) ([]string, error) {
	var shas []string
	for page := 1; page <= maxCommitFilesPages; page++ {
		q := url.Values{
			"sha":      {ref},
			"path":     {path},
			"since":    {since.UTC().Format(time.RFC3339)},
			"per_page": {"100"},
			"page":     {fmt.Sprint(page)},
		}
		apiPath := fmt.Sprintf("repos/%s/%s/commits?%s", c.owner, c.repo, q.Encode())
		var batch []commitResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, apiPath, nil, &batch); err != nil {
			return nil, translateErr("listing commits touching "+path, err)
		}
		for _, r := range batch {
			shas = append(shas, r.SHA)
		}
		if len(batch) < 100 {
			break
		}
	}
	return shas, nil
}

type contentsResponse struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// ReadFile implements forge.Forge: GET .../contents/{path}?ref=…, base64-decoded. A 404 is
// ok=false (the file is simply not there); a directory at that path is an error, since the
// caller asked for a file.
func (c *Client) ReadFile(ctx context.Context, ref, path string) ([]byte, bool, error) {
	q := url.Values{"ref": {ref}}
	apiPath := fmt.Sprintf("repos/%s/%s/contents/%s?%s", c.owner, c.repo, escapePath(path), q.Encode())
	var resp contentsResponse
	if err := c.rest.DoWithContext(ctx, http.MethodGet, apiPath, nil, &resp); err != nil {
		var herr *ghapi.HTTPError
		if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, translateErr("reading "+path+" at "+ref, err)
	}
	if resp.Type != "file" {
		return nil, false, fmt.Errorf("github: reading %s at %s: is a %s, not a file", path, ref, redact.Strings(resp.Type))
	}
	if resp.Encoding != "base64" {
		return nil, false, fmt.Errorf("github: reading %s at %s: unexpected encoding %q", path, ref, redact.Strings(resp.Encoding))
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return nil, false, fmt.Errorf("github: reading %s at %s: %w", path, ref, err)
	}
	return b, true, nil
}

// escapePath escapes each segment of a repo path for the contents endpoint, keeping the
// slashes that separate them.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
