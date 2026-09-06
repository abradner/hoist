package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

func newGraphQLTestClient(t *testing.T, handler func(vars map[string]any) (int, string)) *Client {
	t.Helper()
	ft := &fakeTransport{t: t, handlers: map[string]func(*http.Request) (int, string){
		"POST /graphql": func(r *http.Request) (int, string) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("graphql body: %v", err)
			}
			if !strings.Contains(req.Query, "blame(path:$path)") {
				t.Fatalf("unexpected query %q", req.Query)
			}
			return handler(req.Variables)
		},
	}}
	opts := ghapi.ClientOptions{Host: "github.com", AuthToken: "test-token-not-real", Transport: ft}
	rest, err := ghapi.NewRESTClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	gql, err := ghapi.NewGraphQLClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return newWithClients(rest, gql, "example", "gitops")
}

func TestBlameLinesMapsRangesToLines(t *testing.T) {
	c := newGraphQLTestClient(t, func(vars map[string]any) (int, string) {
		if vars["owner"] != "example" || vars["name"] != "gitops" || vars["expr"] != "abc" || vars["path"] != "cluster/apps/p/app/deployment.yaml" {
			t.Fatalf("vars = %v", vars)
		}
		return 200, `{"data":{"repository":{"object":{"blame":{"ranges":[
			{"startingLine":1,"endingLine":20,"commit":{"oid":"1111","committedDate":"2026-01-01T00:00:00Z"}},
			{"startingLine":21,"endingLine":25,"commit":{"oid":"2222","committedDate":"2026-02-01T00:00:00Z"}}
		]}}}}}`
	})
	got, err := c.BlameLines(context.Background(), "abc", "cluster/apps/p/app/deployment.yaml", []int{5, 23, 99})
	if err != nil {
		t.Fatal(err)
	}
	if got[5].SHA != "1111" || got[23].SHA != "2222" {
		t.Fatalf("got = %v", got)
	}
	if _, ok := got[99]; ok {
		t.Fatal("line 99 is past the file's end and must be absent, not zero")
	}
	if got[23].Date.Month() != 2 {
		t.Fatalf("date = %v", got[23].Date)
	}
}

// A ref the repo does not have comes back as a null object, not a GraphQL error: it must
// be ErrUnknownRef so pkg/migrate can fall back to the base branch.
func TestBlameLinesUnknownRefIsTyped(t *testing.T) {
	c := newGraphQLTestClient(t, func(map[string]any) (int, string) {
		return 200, `{"data":{"repository":{"object":null}}}`
	})
	_, err := c.BlameLines(context.Background(), "nope", "f.yaml", []int{1})
	if !errors.Is(err, forge.ErrUnknownRef) {
		t.Fatalf("err = %v; want ErrUnknownRef", err)
	}
}

func TestBlameLinesNamesScopeGapFromGraphQLError(t *testing.T) {
	const secret = "GQL-SECRET-ZZZ"
	redact.Register(secret)
	c := newGraphQLTestClient(t, func(map[string]any) (int, string) {
		return 200, `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible ` + secret + `","path":["repository"]}]}`
	})
	_, err := c.BlameLines(context.Background(), "abc", "f.yaml", []int{1})
	if err == nil || !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("err = %v; want the scope hint", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("err %q leaks the registered secret", err)
	}
}

func TestBlameLinesWithoutGraphQLClientSaysSo(t *testing.T) {
	c := newTestClient(t, nil)
	_, err := c.BlameLines(context.Background(), "abc", "f.yaml", []int{1})
	if err == nil || !strings.Contains(err.Error(), "GraphQL") {
		t.Fatalf("err = %v", err)
	}
}
