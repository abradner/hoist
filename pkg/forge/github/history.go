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
		Subject: redact.Strings(strings.TrimSpace(subject)),
		Body:    redact.Strings(body),
		Author:  redact.Strings(r.Commit.Author.Name),
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
	var out forge.Comparison
	for page := 1; page <= maxComparePages; page++ {
		q := url.Values{"per_page": {"100"}, "page": {fmt.Sprint(page)}}
		path := fmt.Sprintf("repos/%s/%s/compare/%s...%s?%s", c.owner, c.repo, url.PathEscape(base), url.PathEscape(head), q.Encode())
		var resp compareResponse
		if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &resp); err != nil {
			var herr *ghapi.HTTPError
			if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound {
				return forge.Comparison{}, fmt.Errorf("%w: %w", forge.ErrUnknownRef, translateErr(fmt.Sprintf("comparing %s...%s", base, head), err))
			}
			return forge.Comparison{}, translateErr(fmt.Sprintf("comparing %s...%s", base, head), err)
		}
		if page == 1 {
			out.Status, out.AheadBy, out.BehindBy, out.Total = resp.Status, resp.AheadBy, resp.BehindBy, resp.TotalCommits
			for _, f := range resp.Files {
				out.Files = append(out.Files, redact.Strings(f.Filename))
			}
			out.FilesTruncated = len(resp.Files) >= githubFilesCap
		}
		for _, r := range resp.Commits {
			out.Commits = append(out.Commits, toCommit(r))
		}
		if len(resp.Commits) < 100 {
			break
		}
	}
	out.Truncated = len(out.Commits) < out.Total
	return out, nil
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
			files = append(files, redact.Strings(f.Filename))
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
