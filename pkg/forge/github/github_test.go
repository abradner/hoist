package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// paginate mimics GitHub's own pagination arithmetic against a fixed-size virtual dataset:
// offset (page-1)*perPage, perPage items, clamped to items' bounds. Real tests for the
// exact-at-bound sentinel request MUST route through this rather than keying a canned response
// off the page number alone — a mock that ignores per_page and only special-cases "page ==
// bound+1" cannot tell a correct per_page=100 sentinel from the broken per_page=1 one a prior
// round shipped (both would get the same canned "empty" response), which is exactly how that
// regression passed its own test once already (AGENTS.md §8 "watch the setup").
func paginate(items []string, page, perPage int) []string {
	start := (page - 1) * perPage
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

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

// TestChecksTreatsSkippedAsItsOwnBucketNotSuccess is the P1 regression at the wire-parsing
// layer: a "skipped" conclusion must land in Skipped, never folded into Success (the bug this
// task fixes — see internal/engine's TestCIGreenSkippedRequiredCheckNeverSatisfies for the
// gating-level regression).
func TestChecksTreatsSkippedAsItsOwnBucketNotSuccess(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/deadbeef/check-runs": static(200, `{"check_runs": [
			{"name": "unit-tests", "status": "completed", "conclusion": "success"},
			{"name": "integration-tests", "status": "completed", "conclusion": "skipped"}
		]}`),
	})
	sum, err := c.Checks(context.Background(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 2 || sum.Success != 1 || sum.Skipped != 1 || sum.Failure != 0 {
		t.Fatalf("CheckSummary = %+v, want Total=2 Success=1 Skipped=1 Failure=0", sum)
	}
	if len(sum.SkippedNames) != 1 || sum.SkippedNames[0] != "integration-tests" {
		t.Fatalf("SkippedNames = %v, want [integration-tests]", sum.SkippedNames)
	}
}

// TestChecksPaginatesBeyondFirstPage is the P1 regression for "Checks doesn't paginate": a
// commit with more than 100 check runs must have every page fetched, so a failure sitting only
// on page 2 is still detected rather than silently invisible behind GitHub's per_page cap.
func TestChecksPaginatesBeyondFirstPage(t *testing.T) {
	page1 := make([]string, 100)
	for i := range page1 {
		page1[i] = `{"name": "job-` + fmt.Sprint(i) + `", "status": "completed", "conclusion": "success"}`
	}
	body1 := `{"check_runs": [` + strings.Join(page1, ",") + `]}`
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/deadbeef/check-runs": func(r *http.Request) (int, string) {
			switch r.URL.Query().Get("page") {
			case "1":
				return 200, body1
			case "2":
				return 200, `{"check_runs": [{"name": "page-2-failure", "status": "completed", "conclusion": "failure"}]}`
			default:
				return 200, `{"check_runs": []}`
			}
		},
	})
	sum, err := c.Checks(context.Background(), "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Total != 101 {
		t.Fatalf("Total = %d, want 101 (100 on page 1 + 1 on page 2) — page 2 was never fetched", sum.Total)
	}
	if sum.Failure != 1 || len(sum.FailedNames) != 1 || sum.FailedNames[0] != "page-2-failure" {
		t.Fatalf("CheckSummary = %+v, want the page-2 failure detected", sum)
	}
}

// TestChecksMoreThanBoundFailsClosed is the P1-adjacent hardening regression: a commit with
// MORE check-runs than maxCheckRunPages*100 (every page up to the bound comes back full) must
// fail with a clear error, never silently return a truncated CheckSummary — round 1's own
// pagination fix moved the "only page 1 is fetched" bug to a further-out boundary
// (maxCheckRunPages) rather than removing the failure mode; a pending or failed run sitting
// just past that boundary must never be hidden behind a false "green".
func TestChecksMoreThanBoundFailsClosed(t *testing.T) {
	fullPage := func(name string) string {
		runs := make([]string, 100)
		for i := range runs {
			runs[i] = fmt.Sprintf(`{"name": "%s-%d", "status": "completed", "conclusion": "success"}`, name, i)
		}
		return `{"check_runs": [` + strings.Join(runs, ",") + `]}`
	}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/deadbeef/check-runs": func(r *http.Request) (int, string) {
			// Every page, including the last one this loop is allowed to fetch, comes back
			// completely full — there is no way to tell from here whether a page 11 exists,
			// which is exactly the point: it must refuse to answer rather than guess "no".
			return 200, fullPage("page-" + r.URL.Query().Get("page"))
		},
	})
	_, err := c.Checks(context.Background(), "deadbeef")
	if err == nil {
		t.Fatal("expected an error when every page up to the bound comes back full, got nil")
	}
	if !strings.Contains(err.Error(), "deadbeef") || !strings.Contains(err.Error(), "10") {
		t.Fatalf("error should name the sha and the page bound, got: %v", err)
	}
}

// TestChecksExactlyAtBoundIsNotAnError is the false-positive regression for
// TestChecksMoreThanBoundFailsClosed's own fix: a commit with EXACTLY maxCheckRunPages*100
// check-runs also has its last allowed page come back completely full — the same signal a
// truncated result gives — but there is no page 11, so this must NOT error. Checks resolves the
// ambiguity with one extra sentinel request for page maxCheckRunPages+1, at the SAME per_page=100
// every other page uses; this test backs the mock with a real, fixed-size virtual dataset and
// computes each response via paginate's genuine GitHub-style (page-1)*per_page arithmetic — a
// mock that instead special-cased "page == bound+1 => empty" regardless of per_page would not
// have caught the round-4 regression this test is also guarding (a per_page=1 sentinel asks for
// a different, earlier item and almost always finds something).
func TestChecksExactlyAtBoundIsNotAnError(t *testing.T) {
	total := maxCheckRunPages * 100
	items := make([]string, total)
	for i := range items {
		items[i] = fmt.Sprintf(`{"name": "run-%d", "status": "completed", "conclusion": "success"}`, i)
	}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/deadbeef/check-runs": func(r *http.Request) (int, string) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			slice := paginate(items, page, perPage)
			return 200, `{"check_runs": [` + strings.Join(slice, ",") + `]}`
		},
	})
	sum, err := c.Checks(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("a commit with exactly %d check-runs must not be treated as truncated: %v", total, err)
	}
	if sum.Total != total {
		t.Fatalf("Total = %d, want exactly %d", sum.Total, total)
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

// TestCommentsPaginatesBeyondFirstPage is the P1 regression for "Comments doesn't paginate": a
// PR with more than 100 comments must have every page fetched, so an approve/reject sitting only
// on page 2 is still found rather than silently invisible behind GitHub's per_page cap.
func TestCommentsPaginatesBeyondFirstPage(t *testing.T) {
	page1 := make([]string, 100)
	for i := range page1 {
		page1[i] = fmt.Sprintf(`{"id": %d, "body": "noise", "user": {"login": "alice", "type": "User"}}`, i)
	}
	body1 := `[` + strings.Join(page1, ",") + `]`
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/issues/7/comments": func(r *http.Request) (int, string) {
			switch r.URL.Query().Get("page") {
			case "1":
				return 200, body1
			case "2":
				return 200, `[{"id": 999, "body": "hoist approve abc", "user": {"login": "bob", "type": "User"}}]`
			default:
				return 200, "[]"
			}
		},
	})
	comments, err := c.Comments(context.Background(), 7, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 101 {
		t.Fatalf("Comments returned %d, want 101 (100 on page 1 + 1 on page 2) — page 2 was never fetched", len(comments))
	}
	if comments[100].Author != "bob" || comments[100].Body != "hoist approve abc" {
		t.Fatalf("last comment = %+v, want bob's page-2 approval", comments[100])
	}
}

// TestCommentsMoreThanBoundFailsClosed is Comments' half of the P1-adjacent hardening
// regression (see TestChecksMoreThanBoundFailsClosed): a PR with MORE comments than
// maxCommentPages*100 must fail with a clear error rather than silently return a truncated
// list — a later approve or reject sitting just past the bound must never be invisible to
// ApprovedStep's newest-match scan.
func TestCommentsMoreThanBoundFailsClosed(t *testing.T) {
	fullPage := func(page string) string {
		items := make([]string, 100)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id": %s%02d, "body": "noise", "user": {"login": "alice", "type": "User"}}`, page, i)
		}
		return `[` + strings.Join(items, ",") + `]`
	}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/issues/7/comments": func(r *http.Request) (int, string) {
			return 200, fullPage(r.URL.Query().Get("page"))
		},
	})
	_, err := c.Comments(context.Background(), 7, time.Time{})
	if err == nil {
		t.Fatal("expected an error when every page up to the bound comes back full, got nil")
	}
	if !strings.Contains(err.Error(), "#7") || !strings.Contains(err.Error(), "10") {
		t.Fatalf("error should name the PR and the page bound, got: %v", err)
	}
}

// TestCommentsExactlyAtBoundIsNotAnError is Comments' half of the false-positive regression (see
// TestChecksExactlyAtBoundIsNotAnError): a PR with EXACTLY maxCommentPages*100 comments must not
// be treated as truncated just because its last allowed page came back full — Comments' own
// sentinel request for the page past the bound, at the SAME per_page=100 every other page uses,
// resolves it. As in the Checks test above, the mock is backed by a real, fixed-size virtual
// dataset and computed via paginate's genuine (page-1)*per_page arithmetic, so a regression back
// to a smaller per_page on just the sentinel request (round 4's actual bug: it asks for a
// different, earlier item and almost always finds something) makes this fail rather than pass by
// accident.
func TestCommentsExactlyAtBoundIsNotAnError(t *testing.T) {
	total := maxCommentPages * 100
	items := make([]string, total)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id": %d, "body": "noise", "user": {"login": "alice", "type": "User"}}`, i)
	}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/issues/7/comments": func(r *http.Request) (int, string) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			slice := paginate(items, page, perPage)
			return 200, `[` + strings.Join(slice, ",") + `]`
		},
	})
	comments, err := c.Comments(context.Background(), 7, time.Time{})
	if err != nil {
		t.Fatalf("a PR with exactly %d comments must not be treated as truncated: %v", total, err)
	}
	if len(comments) != total {
		t.Fatalf("Comments returned %d, want exactly %d", len(comments), total)
	}
}

func TestIsAllowedAuthorAcceptsWriteMaintainAndAdmin(t *testing.T) {
	for _, tc := range []struct {
		permission string
		want       bool
	}{
		{"admin", true},
		{"maintain", true},
		{"write", true},
		{"triage", false},
		{"read", false},
		{"none", false},
	} {
		c := newTestClient(t, map[string]func(*http.Request) (int, string){
			"GET /repos/example/gitops/collaborators/alice/permission": static(200, `{"permission":"`+tc.permission+`"}`),
		})
		got, err := c.IsAllowedAuthor(context.Background(), "alice")
		if err != nil {
			t.Fatalf("permission=%s: %v", tc.permission, err)
		}
		if got != tc.want {
			t.Errorf("permission=%s: IsAllowedAuthor = %v, want %v", tc.permission, got, tc.want)
		}
	}
}

func TestIsAllowedAuthorSurfacesScopeGap(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/collaborators/mallory/permission": static(403, `{"message":"Must have push access"}`),
	})
	_, err := c.IsAllowedAuthor(context.Background(), "mallory")
	if err == nil {
		t.Fatal("expected the 403 to surface as an error, not fold into false")
	}
	if !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("error %q does not name the likely scope gap", err.Error())
	}
}

func TestMergePRSquashesAndReturnsFreshPR(t *testing.T) {
	var gotBody string
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"PUT /repos/example/gitops/pulls/7/merge": func(r *http.Request) (int, string) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			return 200, `{"merged": true, "sha": "merge123"}`
		},
		"GET /repos/example/gitops/pulls/7": static(200, `{"number": 7, "merged": true, "merge_commit_sha": "merge123", "head": {"ref": "hoist/env/abc", "sha": "deadbeef"}}`),
	})
	pr, err := c.MergePR(context.Background(), 7, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.MergeSHA != "merge123" {
		t.Fatalf("MergePR result = %+v", pr)
	}
	if !strings.Contains(gotBody, `"sha":"deadbeef"`) || !strings.Contains(gotBody, `"merge_method":"squash"`) {
		t.Fatalf("merge request body = %s, want the expected head sha and squash method", gotBody)
	}
}

// TestMergePRRefusesStaleHead is the named adversary, at the wire level: GitHub's own atomic
// "merge iff head is X" answers a mismatched sha with 409, which must translate to
// forge.ErrStaleHead — never a generic error a caller might mistake for something retryable.
func TestMergePRRefusesStaleHead(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"PUT /repos/example/gitops/pulls/7/merge": static(409, `{"message":"Head branch was modified. Review and try the merge again."}`),
	})
	_, err := c.MergePR(context.Background(), 7, "stale-sha")
	if err == nil || !errors.Is(err, forge.ErrStaleHead) {
		t.Fatalf("expected forge.ErrStaleHead, got %v", err)
	}
}

func TestNewRejectsMalformedRepo(t *testing.T) {
	for _, bad := range []string{"", "noSlash", "owner/", "/name", "owner/a/b"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) should have been rejected", bad)
		}
	}
}
