package github

import (
	"context"
	"encoding/base64"
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

func commitJSON(sha, message string) string {
	return fmt.Sprintf(`{"sha":%q,"commit":{"message":%q,"author":{"name":"Dev","date":"2026-03-01T00:00:00Z"},"committer":{"date":"2026-03-01T00:00:00Z"}}}`, sha, message)
}

func TestResolveRefFoldsNotFoundAndAmbiguousIntoNotOK(t *testing.T) {
	for _, status := range []int{404, 422} {
		c := newTestClient(t, map[string]func(*http.Request) (int, string){
			"GET /repos/example/gitops/commits/v9": static(status, `{"message":"nope"}`),
		})
		sha, ok, err := c.ResolveRef(context.Background(), "v9")
		if err != nil || ok || sha != "" {
			t.Fatalf("status %d: sha=%q ok=%v err=%v; want not ok, no error", status, sha, ok, err)
		}
	}
}

// A scope gap must be an error, never ok=false: pkg/migrate reads ok=false as "try the next
// source" and would otherwise degrade every revision to unknown without saying why.
func TestResolveRefSurfacesScopeGap(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/v9": static(403, `{"message":"Resource not accessible"}`),
	})
	_, _, err := c.ResolveRef(context.Background(), "v9")
	if err == nil || !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("err = %v; want a scope-gap error", err)
	}
}

func TestResolveRefReturnsFullSHA(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/v3": static(200, commitJSON("3333333333333333333333333333333333333333", "x")),
	})
	sha, ok, err := c.ResolveRef(context.Background(), "v3")
	if err != nil || !ok || sha != "3333333333333333333333333333333333333333" {
		t.Fatalf("sha=%q ok=%v err=%v", sha, ok, err)
	}
}

func TestComparePaginatesAndReportsTruncation(t *testing.T) {
	// 350 commits in the range, the adaptor pages to 300: Truncated, Total from the forge.
	shas := make([]string, 350)
	for i := range shas {
		shas[i] = fmt.Sprintf("%040d", i)
	}
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": func(r *http.Request) (int, string) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			var items []string
			for _, s := range paginate(shas, page, per) {
				items = append(items, commitJSON(s, "subject\n\nbody line"))
			}
			files := `[]`
			if page == 1 {
				files = `[{"filename":"db/migrate/1.rb"},{"filename":"app/x.rb"}]`
			}
			return 200, fmt.Sprintf(`{"status":"ahead","ahead_by":350,"behind_by":0,"total_commits":350,"commits":[%s],"files":%s}`, strings.Join(items, ","), files)
		},
	})
	cmp, err := c.Compare(context.Background(), "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Commits) != 300 || cmp.Total != 350 || !cmp.Truncated {
		t.Fatalf("len=%d total=%d truncated=%v; want 300/350/true", len(cmp.Commits), cmp.Total, cmp.Truncated)
	}
	if cmp.Status != "ahead" || cmp.AheadBy != 350 {
		t.Fatalf("status=%q ahead=%d", cmp.Status, cmp.AheadBy)
	}
	if got := cmp.Commits[0]; got.Subject != "subject" || got.Body != "body line" || got.Author != "Dev" {
		t.Fatalf("commit[0] = %+v", got)
	}
	if len(cmp.Files) != 2 || cmp.FilesTruncated {
		t.Fatalf("files=%v truncated=%v", cmp.Files, cmp.FilesTruncated)
	}
}

func TestCompareShortRangeIsNotTruncated(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": static(200, `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"commits":[`+commitJSON("c1", "one")+`],"files":[]}`),
	})
	cmp, err := c.Compare(context.Background(), "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Truncated || len(cmp.Commits) != 1 {
		t.Fatalf("cmp = %+v", cmp)
	}
}

func TestCompareUnknownRefIsTyped(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": static(404, `{"message":"Not Found"}`),
	})
	_, err := c.Compare(context.Background(), "aaa", "bbb")
	if !errors.Is(err, forge.ErrUnknownRef) {
		t.Fatalf("err = %v; want ErrUnknownRef", err)
	}
	if !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("err %q should still name the scope possibility: a private repo 404s the same way", err)
	}
}

// Commit messages are upstream text nothing here wrote (AGENTS.md invariant 6).
func TestCompareRedactsCommitText(t *testing.T) {
	const secret = "HISTORY-SECRET-QQQ"
	redact.Register(secret)
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": static(200, `{"status":"ahead","total_commits":1,"commits":[`+commitJSON("c1", "token "+secret+"\n\nbody "+secret)+`],"files":[{"filename":"`+secret+`.rb"}]}`),
	})
	cmp, err := c.Compare(context.Background(), "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{cmp.Commits[0].Subject, cmp.Commits[0].Body, cmp.Files[0]} {
		if strings.Contains(s, secret) {
			t.Fatalf("%q leaks the registered secret", s)
		}
	}
}

func TestCommitFilesListsPaths(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/c1": static(200, `{"sha":"c1","commit":{"message":"m","author":{"name":"d","date":"2026-01-01T00:00:00Z"}},"files":[{"filename":"db/migrate/1.rb"},{"filename":"app/a.rb"}]}`),
	})
	files, truncated, err := c.CommitFiles(context.Background(), "c1")
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if len(files) != 2 || files[0] != "db/migrate/1.rb" {
		t.Fatalf("files = %v", files)
	}
}

func TestCommitsTouchingSendsRefPathAndSince(t *testing.T) {
	since := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits": func(r *http.Request) (int, string) {
			q := r.URL.Query()
			if q.Get("sha") != "bbb" || q.Get("path") != "db/migrate" || q.Get("since") != "2026-02-01T12:00:00Z" {
				t.Fatalf("query = %v", q)
			}
			return 200, `[` + commitJSON("c2", "two") + `,` + commitJSON("c1", "one") + `]`
		},
	})
	shas, err := c.CommitsTouching(context.Background(), "bbb", "db/migrate", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(shas) != 2 || shas[0] != "c2" {
		t.Fatalf("shas = %v", shas)
	}
}

func TestReadFileDecodesBase64AndFoldsNotFound(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("migrations: db/migrate/\n"))
	// GitHub wraps base64 content at 60 columns with newlines; the decoder must strip them.
	wrapped := content[:10] + `\n` + content[10:] // the JSON escape, as GitHub sends it
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/contents/.hoist.yaml": func(r *http.Request) (int, string) {
			if r.URL.Query().Get("ref") != "abc" {
				t.Fatalf("ref = %q", r.URL.Query().Get("ref"))
			}
			return 200, `{"type":"file","encoding":"base64","content":"` + wrapped + `"}`
		},
		"GET /repos/example/gitops/contents/missing.yaml": static(404, `{"message":"Not Found"}`),
	})
	b, ok, err := c.ReadFile(context.Background(), "abc", ".hoist.yaml")
	if err != nil || !ok || string(b) != "migrations: db/migrate/\n" {
		t.Fatalf("b=%q ok=%v err=%v", b, ok, err)
	}
	_, ok, err = c.ReadFile(context.Background(), "abc", "missing.yaml")
	if err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
}

func TestReadFileRefusesADirectory(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/contents/db": static(200, `[{"type":"dir"}]`),
	})
	// A directory listing decodes into a struct with zero fields set; Type != "file".
	_, _, err := c.ReadFile(context.Background(), "abc", "db")
	if err == nil {
		t.Fatal("expected an error for a directory")
	}
}

// headerTransport is fakeTransport with response headers, for the rate-limit case.
type headerTransport struct {
	status  int
	body    string
	headers map[string]string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	for k, v := range h.headers {
		header.Set(k, v)
	}
	return &http.Response{StatusCode: h.status, Body: io.NopCloser(strings.NewReader(h.body)), Header: header, Request: req}, nil
}

// A 403 with the rate-limit exhausted is the likeliest 403 the history calls produce, and
// sending the operator to `gh auth refresh` for it would be wrong.
func TestTranslateErrNamesRateLimitExhaustion(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	rest, err := ghapi.NewRESTClient(ghapi.ClientOptions{
		Host: "github.com", AuthToken: "test-token-not-real",
		Transport: &headerTransport{status: 403, body: `{"message":"API rate limit exceeded"}`, headers: map[string]string{
			"X-Ratelimit-Remaining": "0",
			"X-Ratelimit-Reset":     strconv.FormatInt(reset.Unix(), 10),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := newWithClient(rest, "example", "gitops")
	_, err = c.Compare(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rate limit") || strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("err = %q; want the rate-limit message, not the scope hint", err)
	}
	if !strings.Contains(err.Error(), reset.Local().Format("15:04")) {
		t.Fatalf("err = %q; want the reset time", err)
	}
}
