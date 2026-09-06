package github

import (
	"context"
	"errors"
	"fmt"
	"time"

	ghapi "github.com/cli/go-gh/v2/pkg/api"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// blameQuery is the one GraphQL query this adaptor makes: git blame for one file at one ref,
// as ranges of lines each attributed to a commit. REST has no blame endpoint. One query
// answers every line a caller asks about in that file, which is why BlameLines takes a list.
const blameQuery = `query($owner:String!,$name:String!,$expr:String!,$path:String!){
  repository(owner:$owner,name:$name){
    object(expression:$expr){
      ... on Commit {
        blame(path:$path){
          ranges{ startingLine endingLine commit{ oid committedDate } }
        }
      }
    }
  }
}`

type blameResponse struct {
	Repository struct {
		Object *struct {
			Blame struct {
				Ranges []struct {
					StartingLine int `json:"startingLine"`
					EndingLine   int `json:"endingLine"`
					Commit       struct {
						OID           string    `json:"oid"`
						CommittedDate time.Time `json:"committedDate"`
					} `json:"commit"`
				} `json:"ranges"`
			} `json:"blame"`
		} `json:"object"`
	} `json:"repository"`
}

// BlameLines implements forge.Forge via GraphQL. A nil object means ref does not resolve in
// this repo; that is reported as forge.ErrUnknownRef so pkg/migrate can fall back to the base
// branch and mark the age approximate, rather than as a scope failure. Requires a GraphQL
// client: a Client built through newWithClient alone (the M3 test seam) reports that it has
// none rather than dereferencing nil.
func (c *Client) BlameLines(ctx context.Context, ref, path string, lines []int) (map[int]forge.LineOrigin, error) {
	if c.gql == nil {
		return nil, errors.New("github: blame needs a GraphQL client and this Client was built without one")
	}
	vars := map[string]interface{}{"owner": c.owner, "name": c.repo, "expr": ref, "path": path}
	var resp blameResponse
	if err := c.gql.DoWithContext(ctx, blameQuery, vars, &resp); err != nil {
		return nil, translateGraphQLErr("blaming "+path+" at "+ref, err)
	}
	if resp.Repository.Object == nil {
		return nil, fmt.Errorf("github: blaming %s at %s: %w", path, ref, forge.ErrUnknownRef)
	}
	out := map[int]forge.LineOrigin{}
	for _, n := range lines {
		for _, r := range resp.Repository.Object.Blame.Ranges {
			if n >= r.StartingLine && n <= r.EndingLine {
				out[n] = forge.LineOrigin{SHA: r.Commit.OID, Date: r.Commit.CommittedDate}
				break
			}
		}
	}
	return out, nil
}

// translateGraphQLErr is translateErr's GraphQL twin. GraphQL reports a scope gap as a
// FORBIDDEN or NOT_FOUND item in a 200 response (a *ghapi.GraphQLError), not as an HTTP
// status, so the same "the gh token may be missing the repo scope" hint has to be keyed on the
// item type. A transport-level failure still arrives as *ghapi.HTTPError and goes through
// translateErr unchanged.
func translateGraphQLErr(op string, err error) error {
	var gerr *ghapi.GraphQLError
	if errors.As(err, &gerr) {
		for _, item := range gerr.Errors {
			if item.Type == "FORBIDDEN" || item.Type == "NOT_FOUND" || item.Type == "INSUFFICIENT_SCOPES" {
				return fmt.Errorf("github: %s: %s %s (the gh token may be missing the repo scope this needs — check `gh auth status`, and `gh auth refresh -s repo` if so)", op, item.Type, redact.Strings(item.Message))
			}
		}
		return fmt.Errorf("github: %s: %s", op, redact.Strings(gerr.Error()))
	}
	return translateErr(op, err)
}
