package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// fakeTransport answers every request from a table keyed by "METHOD path", so these tests
// never touch a real GitHub host — go-gh's own auth/keyring resolution is bypassed entirely
// by supplying an explicit AuthToken and Transport (see newTestClient), per the hard
// constraint that no test in this repo points at a real GitHub repo.
type fakeTransport struct {
	t        *testing.T
	handlers map[string]func(*http.Request) (int, string)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Path
	h, ok := f.handlers[key]
	if !ok {
		f.t.Fatalf("unexpected request %s (query %q)", key, req.URL.RawQuery)
	}
	status, body := h(req)
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
		Request:    req,
	}, nil
}

func newTestClient(t *testing.T, handlers map[string]func(*http.Request) (int, string)) *Client {
	t.Helper()
	ft := &fakeTransport{t: t, handlers: handlers}
	rest, err := ghapi.NewRESTClient(ghapi.ClientOptions{
		Host:      "github.com",
		AuthToken: "test-token-not-real",
		Transport: ft,
	})
	if err != nil {
		t.Fatal(err)
	}
	return newWithClient(rest, "example", "gitops")
}

func static(status int, body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return status, body }
}

func TestCreatePR(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"POST /repos/example/gitops/pulls": static(201, `{
			"number": 42,
			"html_url": "https://github.com/example/gitops/pull/42",
			"head": {"ref": "hoist/app-production/abc", "sha": "deadbeef"},
			"base": {"ref": "main"}
		}`),
	})
	pr, err := c.CreatePR(context.Background(), forge.PRSpec{Title: "t", Body: "b", Head: "hoist/app-production/abc", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/example/gitops/pull/42" || pr.HeadSHA != "deadbeef" {
		t.Fatalf("PR = %+v", pr)
	}
}

func TestFindPRByHeadBranch(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/pulls": func(r *http.Request) (int, string) {
			if r.URL.Query().Get("head") != "example:hoist/app-production/abc" {
				return 200, "[]"
			}
			return 200, `[{"number": 7, "body": "<!-- hoist:id=abc -->\nx"}]`
		},
	})
	pr, ok, err := c.FindPR(context.Background(), "hoist/app-production/abc", "<!-- hoist:id=abc -->")
	if err != nil || !ok {
		t.Fatalf("FindPR: ok=%v err=%v", ok, err)
	}
	if pr.Number != 7 {
		t.Fatalf("Number = %d, want 7", pr.Number)
	}
}

func TestFindPRFallsBackToBodyMarkerSearch(t *testing.T) {
	calls := map[string]int{}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/pulls": func(r *http.Request) (int, string) {
			q := r.URL.Query()
			calls[q.Get("state")+q.Get("page")]++
			switch {
			case q.Get("head") != "":
				return 200, "[]" // nothing matches by head: the branch was renamed
			case q.Get("state") == "open" && q.Get("page") == "1":
				return 200, `[{"number": 1, "body": "unrelated"}]`
			case q.Get("state") == "closed" && q.Get("page") == "1":
				return 200, `[{"number": 9, "body": "<!-- hoist:id=abc -->\nrenamed branch, found by marker"}]`
			}
			return 200, "[]"
		},
	})
	pr, ok, err := c.FindPR(context.Background(), "some-renamed-branch", "<!-- hoist:id=abc -->")
	if err != nil || !ok {
		t.Fatalf("FindPR: ok=%v err=%v", ok, err)
	}
	if pr.Number != 9 {
		t.Fatalf("Number = %d, want 9 (found via closed-PR body search)", pr.Number)
	}
	if calls["open1"] == 0 || calls["closed1"] == 0 {
		t.Fatalf("expected both an open and a closed search page, got %v", calls)
	}
}

func TestFindPRNotFound(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/pulls": func(*http.Request) (int, string) { return 200, "[]" },
	})
	_, ok, err := c.FindPR(context.Background(), "nope", "<!-- hoist:id=nope -->")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestTranslateErrNamesScopeGapOn403(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"POST /repos/example/gitops/pulls": static(403, `{"message":"Resource not accessible by integration"}`),
	})
	_, err := c.CreatePR(context.Background(), forge.PRSpec{Head: "h", Base: "main"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("error %q does not name the likely scope gap", err.Error())
	}
}

// Self-review finding (M3 pre-open): herr.Message is free-form text from GitHub's own
// response body — untrusted upstream output in the same sense pkg/registry's error codes
// are (AGENTS.md §4.10) — so translateErr must scrub it through redact.Strings like every
// other adaptor's output, not echo it verbatim.
func TestTranslateErrRedactsRegisteredSecrets(t *testing.T) {
	const secret = "SECRET-TOKEN-XYZ"
	redact.Register(secret)
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"POST /repos/example/gitops/pulls": static(500, `{"message":"backend rejected `+secret+`"}`),
	})
	_, err := c.CreatePR(context.Background(), forge.PRSpec{Head: "h", Base: "main"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q leaks the registered secret", err.Error())
	}
}

func TestChecksSummarizesRollup(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/deadbeef/check-runs": static(200, `{"check_runs": [
			{"status": "completed", "conclusion": "success"},
			{"status": "completed", "conclusion": "failure"},
			{"status": "in_progress", "conclusion": ""}
		]}`),
	})
	sum, err := c.Checks(context.Background(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 3 || sum.Success != 1 || sum.Failure != 1 || sum.Pending != 1 {
		t.Fatalf("CheckSummary = %+v", sum)
	}
}

// TestCommentsExcludesBots checks the exact set the filter is supposed to draw: a "User"
// comment and an "Organization" comment (a real GitHub account type for an org-owned account,
// not a bot — M4's real approval-comment author check depends on this list not silently
// dropping it) must both survive, while a "Bot" comment must still be excluded.
func TestCommentsExcludesBots(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/issues/7/comments": static(200, `[
			{"id": 1, "body": "hoist approve abc", "user": {"login": "alex", "type": "User"}},
			{"id": 2, "body": "beep boop", "user": {"login": "some-bot", "type": "Bot"}},
			{"id": 3, "body": "hoist approve abc", "user": {"login": "some-org", "type": "Organization"}}
		]`),
	})
	comments, err := c.Comments(context.Background(), 7, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 {
		t.Fatalf("Comments = %+v, want 2 (User and Organization survive, Bot excluded)", comments)
	}
	if comments[0].Author != "alex" || comments[1].Author != "some-org" {
		t.Fatalf("Comments = %+v, want alex then some-org", comments)
	}
}

func TestNewRejectsMalformedRepo(t *testing.T) {
	for _, bad := range []string{"", "noSlash", "owner/", "/name", "owner/a/b"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) should have been rejected", bad)
		}
	}
}
