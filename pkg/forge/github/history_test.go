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
			"GET /repos/example/gitops":            static(200, `{"full_name":"example/gitops"}`),
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
	// GitHub pages oldest first; the bound keeps the NEWEST 300 (commits 50..349), because a
	// migration in the newest fifty is the one that must not fall off the end.
	if cmp.Commits[0].SHA != shas[50] || cmp.Commits[299].SHA != shas[349] {
		t.Fatalf("kept %s..%s; want the newest 300 (%s..%s)", cmp.Commits[0].SHA, cmp.Commits[299].SHA, shas[50], shas[349])
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
		"GET /repos/example/gitops":                   static(200, `{"full_name":"example/gitops"}`),
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

// A 404 is what GitHub answers for a missing ref AND for a private repository the token cannot
// read. Only the first is ok=false; the second is a scope error, per the Forge contract —
// pkg/migrate would otherwise degrade every revision to "unknown" without saying why.
func TestResolveRefTreatsAnInvisibleRepoAsAScopeError(t *testing.T) {
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/v9": static(404, `{"message":"Not Found"}`),
		"GET /repos/example/gitops":            static(404, `{"message":"Not Found"}`),
	})
	_, ok, err := c.ResolveRef(context.Background(), "v9")
	if ok || err == nil || !strings.Contains(err.Error(), "repo scope") || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("ok=%v err=%v; want a scope error naming the repository", ok, err)
	}
	// Compare's 404 goes the same way, and the probe runs once per Client.
	c2 := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": static(404, `{"message":"Not Found"}`),
		"GET /repos/example/gitops/commits/v9":        static(404, `{"message":"Not Found"}`),
		"GET /repos/example/gitops":                   static(404, `{"message":"Not Found"}`),
	})
	_, err = c2.Compare(context.Background(), "aaa", "bbb")
	if errors.Is(err, forge.ErrUnknownRef) || err == nil || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("err = %v; want a scope error, not ErrUnknownRef", err)
	}
	if _, _, err2 := c2.ResolveRef(context.Background(), "v9"); err2 == nil {
		t.Fatal("second 404 on the same client must reuse the probe's answer")
	}
}

// Commit text is whatever its author typed: a crafted escape sequence in a subject must never
// reach the operator's terminal. Two layers hold that: go-gh's REST client rewrites C0 control
// bytes in response bodies to caret notation (ESC becomes the two characters "^["), and clean
// strips whatever still arrives as a real control byte — the layer a transport without that
// courtesy (a GitLab adaptor, a future go-gh) would rely on.
func TestCommitTextIsStrippedOfTerminalControls(t *testing.T) {
	const commit = `{"sha":"c1","commit":{"message":"fix: \u001b]0;evil\u0007 \u001b[31mred\u001b[0m\u0000 done\n\nbody\ttab \u001b[2J","author":{"name":"Dev\u001b[1m","date":"2026-03-01T00:00:00Z"},"committer":{"date":"2026-03-01T00:00:00Z"}}}`
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/compare/aaa...bbb": static(200, `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"commits":[`+commit+`],"files":[{"filename":"a\u001b[2Jb"}]}`),
	})
	cmp, err := c.Compare(context.Background(), "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{cmp.Commits[0].Subject, cmp.Commits[0].Body, cmp.Commits[0].Author, cmp.Files[0]} {
		for _, r := range s {
			if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
				t.Fatalf("control byte %#x survived in %q", r, s)
			}
		}
	}
	if !strings.Contains(cmp.Commits[0].Body, "\t") {
		t.Fatalf("a tab is legitimate commit-body text and must survive: %q", cmp.Commits[0].Body)
	}
	// Positive control on clean itself, with real control bytes rather than go-gh's carets.
	if got := clean("fix: \x1b]0;evil\x07 \x1b[31mred\x1b[0m\x00 done\n\tbody"); got != "fix:  red done\n\tbody" {
		t.Fatalf("clean = %q", got)
	}
}

// The newest bound must be contiguous whatever the total: for 450 commits the last pages
// hold 3..5 (250 commits) and the fill is the tail of page 2, not of page 1 (Copilot, #124).
func TestCompareKeepsTheNewestContiguousCommits(t *testing.T) {
	for _, total := range []int{300, 301, 350, 450, 1001} {
		shas := make([]string, total)
		for i := range shas {
			shas[i] = fmt.Sprintf("%040d", i)
		}
		pagesHit := map[int]bool{}
		c := newTestClient(t, map[string]func(*http.Request) (int, string){
			"GET /repos/example/gitops/compare/aaa...bbb": func(r *http.Request) (int, string) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
				pagesHit[page] = true
				var items []string
				for _, s := range paginate(shas, page, per) {
					items = append(items, commitJSON(s, "s"))
				}
				return 200, fmt.Sprintf(`{"status":"ahead","ahead_by":%d,"behind_by":0,"total_commits":%d,"commits":[%s],"files":[]}`, total, total, strings.Join(items, ","))
			},
		})
		cmp, err := c.Compare(context.Background(), "aaa", "bbb")
		if err != nil {
			t.Fatal(err)
		}
		want := min(total, 300)
		if len(cmp.Commits) != want || cmp.Commits[0].SHA != shas[total-want] || cmp.Commits[want-1].SHA != shas[total-1] {
			t.Fatalf("total %d: got %d commits %s..%s; want the newest %d (%s..%s)", total, len(cmp.Commits), cmp.Commits[0].SHA, cmp.Commits[len(cmp.Commits)-1].SHA, want, shas[total-want], shas[total-1])
		}
		for i := 1; i < len(cmp.Commits); i++ {
			if cmp.Commits[i].SHA != shas[total-want+i] {
				t.Fatalf("total %d: gap at %d", total, i)
			}
		}
		if cmp.Truncated != (total > 300) {
			t.Fatalf("total %d: truncated=%v", total, cmp.Truncated)
		}
		if !pagesHit[1] {
			t.Fatalf("total %d: page 1 (metadata, files) was never read", total)
		}
	}
}

// A transient probe failure must not be remembered: a 5xx while the network hiccups would
// otherwise make every later 404 on this client a "not visible" error for the session.
func TestRepoVisibilityCachesOnlyDefinitiveAnswers(t *testing.T) {
	probes := 0
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/v9": static(404, `{"message":"Not Found"}`),
		"GET /repos/example/gitops": func(*http.Request) (int, string) {
			probes++
			if probes == 1 {
				return 503, `{"message":"unavailable"}`
			}
			return 200, `{"full_name":"example/gitops"}`
		},
	})
	if _, _, err := c.ResolveRef(context.Background(), "v9"); err == nil {
		t.Fatal("the first probe failed; that must surface as an error")
	}
	_, ok, err := c.ResolveRef(context.Background(), "v9")
	if err != nil || ok {
		t.Fatalf("after the probe recovered: ok=%v err=%v; want a plain not-found", ok, err)
	}
	if _, _, err := c.ResolveRef(context.Background(), "v9"); err != nil || probes != 2 {
		t.Fatalf("a visible repo is remembered: probes=%d err=%v", probes, err)
	}
}

// A file name is one line on a confirm screen; git allows a newline in a path, and a
// migration file named with one would add rows to the deploy screen's migration list.
func TestFileNamesAndSubjectsAreOneLine(t *testing.T) {
	if got := cleanLine("db/migrate/1.rb\nextra row\tcell"); got != "db/migrate/1.rbextra rowcell" {
		t.Fatalf("cleanLine = %q", got)
	}
	if got := clean("body line\n\tindented"); got != "body line\n\tindented" {
		t.Fatalf("clean must keep a body's newline and tab: %q", got)
	}
}

// A 403 that is a rate limit is the moment's answer, not the repository's visibility, and
// must not pin "not visible" on the client for the session.
func TestRateLimitedProbeIsNotCached(t *testing.T) {
	probes := 0
	c := newTestClient(t, map[string]func(*http.Request) (int, string){
		"GET /repos/example/gitops/commits/v9": static(404, `{"message":"Not Found"}`),
		"GET /repos/example/gitops": func(*http.Request) (int, string) {
			probes++
			if probes == 1 {
				return 403, `{"message":"API rate limit exceeded for user"}`
			}
			return 200, `{"full_name":"example/gitops"}`
		},
	})
	if _, _, err := c.ResolveRef(context.Background(), "v9"); err == nil {
		t.Fatal("the rate-limited probe must surface as an error")
	}
	_, ok, err := c.ResolveRef(context.Background(), "v9")
	if err != nil || ok || probes != 2 {
		t.Fatalf("after the limit lifted: ok=%v err=%v probes=%d; want a plain not-found from a second probe", ok, err, probes)
	}
}
