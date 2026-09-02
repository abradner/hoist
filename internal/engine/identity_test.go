package engine

import (
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

const digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// TestDeriveIDMatchesFixedVector reuses pkg/image's own frozen fixed vector
// (TestPromotionIDFixedVector) rather than inventing a new hash — DeriveID must produce
// byte-for-byte the id image.PromotionID already gives for these refs and env.
func TestDeriveIDMatchesFixedVector(t *testing.T) {
	plan := gitops.Plan{
		TargetEnv: "app-production",
		Edits: []gitops.Edit{
			{New: image.Ref{Repo: "ghcr.io/example/web", Tag: "v202601010101", Digest: digestA}},
			{New: image.Ref{Repo: "ghcr.io/example/counta", Tag: "v202601010101", Digest: digestB}},
		},
	}
	const want = "dh4arammqe" // frozen in pkg/image/image_test.go's TestPromotionIDFixedVector
	if got := DeriveID("example/gitops", plan); got != want {
		t.Fatalf("DeriveID = %q, want %q", got, want)
	}
}

func TestIdentityScheme(t *testing.T) {
	const id = "dh4arammqe"
	if got, want := BranchName("app-production", id), "hoist/app-production/dh4arammqe"; got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
	if got, want := Marker(id), "<!-- hoist:id=dh4arammqe -->"; got != want {
		t.Errorf("Marker = %q, want %q", got, want)
	}
	if got, want := CommitTrailer(id), "hoist-id: dh4arammqe"; got != want {
		t.Errorf("CommitTrailer = %q, want %q", got, want)
	}
}

// TestSameInputsSameID proves the "two calls with the same repo, target env and resolved refs
// must produce the same id, byte for byte" requirement directly against DeriveID (not just
// against image.PromotionID, which pkg/image already covers): building two separate Plan
// values with the same edits gives the same id.
func TestSameInputsSameID(t *testing.T) {
	newPlan := func() gitops.Plan {
		return gitops.Plan{
			TargetEnv: "app-staging",
			Edits: []gitops.Edit{
				{New: image.Ref{Repo: "ghcr.io/example/web", Tag: "v1", Digest: digestA}},
			},
		}
	}
	a := DeriveID("example/gitops", newPlan())
	b := DeriveID("example/gitops", newPlan())
	if a != b {
		t.Fatalf("DeriveID not deterministic: %q vs %q", a, b)
	}
}
