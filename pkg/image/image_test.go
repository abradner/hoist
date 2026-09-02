package image

import (
	"strings"
	"testing"
)

const (
	digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseFourShapesRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
		out  string // canonical String(); defaults to in
	}{
		{in: "ghcr.io/example/web:v202601010101@" + digestA, want: Ref{"ghcr.io/example/web", "v202601010101", digestA}},
		{in: "ghcr.io/example/web:v202601010101", want: Ref{"ghcr.io/example/web", "v202601010101", ""}},
		{in: "ghcr.io/example/web@" + digestA, want: Ref{"ghcr.io/example/web", "", digestA}},
		{in: "ghcr.io/example/web", want: Ref{"ghcr.io/example/web", "", ""}},
		{in: "docker-pullable://ghcr.io/example/web@" + digestA, want: Ref{"ghcr.io/example/web", "", digestA}, out: "ghcr.io/example/web@" + digestA},
		{in: "docker.io/temporalio/server:1.31.2", want: Ref{"docker.io/temporalio/server", "1.31.2", ""}},
		{in: "localhost:5000/app", want: Ref{"localhost:5000/app", "", ""}},
		{in: "localhost:5000/app:sha-0123456789abcdef0123456789abcdef01234567", want: Ref{"localhost:5000/app", "sha-0123456789abcdef0123456789abcdef01234567", ""}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
		out := c.out
		if out == "" {
			out = c.in
		}
		if s := got.String(); s != out {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, s, out)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"ghcr.io/example/web:v1@sha256:" + strings.Repeat("A", 64), // uppercase hex is not a digest
		"ghcr.io/example/web:v1@sha256:" + strings.Repeat("a", 63), // short
		"ghcr.io/example/web:v1@sha256:" + strings.Repeat("a", 65), // long
		"ghcr.io/example/web:v1@sha512:" + strings.Repeat("a", 64), // wrong algorithm
		"ghcr.io/example/web:v1@" + strings.Repeat("a", 64),        // no algorithm
		"ghcr.io/example/web@",
		"ghcr.io/example/web:",
		"ghcr.io/example/web:v1 # comment",
		"ghcr.io/example/web:-bad",
		"ghcr.io//web:v1",
		"/ghcr.io/web:v1",
	}
	for _, in := range bad {
		if r, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", in, r)
		}
	}
}

func TestPinned(t *testing.T) {
	if (Ref{Repo: "r", Tag: "t"}).Pinned() {
		t.Error("bare tag reported as pinned")
	}
	if !(Ref{Repo: "r", Digest: digestA}).Pinned() {
		t.Error("digest-bearing ref reported as unpinned")
	}
}

func TestValidate(t *testing.T) {
	good := []Ref{
		{Repo: "ghcr.io/example/web", Tag: "v9", Digest: digestA},
		{Repo: "ghcr.io/example/web", Digest: digestA},
		{Repo: "ghcr.io/example/web", Tag: "v9"},
		{Repo: "localhost:5000/app"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", r, err)
		}
	}
	bad := map[string]Ref{
		"empty repo":         {Tag: "v9", Digest: digestA},
		"repo with space":    {Repo: "ghcr.io/example/we b", Digest: digestA},
		"uppercase digest":   {Repo: "ghcr.io/example/web", Tag: "v9", Digest: "sha256:" + strings.Repeat("A", 64)},
		"short digest":       {Repo: "ghcr.io/example/web", Tag: "v9", Digest: "sha256:DEADBEEF"},
		"wrong algorithm":    {Repo: "ghcr.io/example/web", Tag: "v9", Digest: "sha512:" + strings.Repeat("a", 64)},
		"digest without alg": {Repo: "ghcr.io/example/web", Tag: "v9", Digest: strings.Repeat("a", 64)},
		"bad tag":            {Repo: "ghcr.io/example/web", Tag: "-v9", Digest: digestA},
	}
	for name, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) = nil, want error", name, r)
		}
	}
	// Positive control: everything Parse accepts, Validate accepts.
	for _, in := range []string{"ghcr.io/example/web:v1@" + digestA, "ghcr.io/example/web@" + digestB, "ghcr.io/example/web"} {
		r, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("Parse(%q) accepted but Validate rejects: %v", in, err)
		}
	}
}

// The expected value was computed outside this package (python hashlib + base64.b32encode
// over the documented preimage) so the scheme cannot drift with the implementation.
func TestPromotionIDFixedVector(t *testing.T) {
	refs := []Ref{
		{Repo: "ghcr.io/example/web", Tag: "v202601010101", Digest: digestA},
		{Repo: "ghcr.io/example/counta", Tag: "v202601010101", Digest: digestB},
	}
	const want = "dh4arammqe"
	if got := PromotionID("example/gitops", "app-production", refs); got != want {
		t.Fatalf("PromotionID = %q, want %q", got, want)
	}
}

// Frozen from the implementation (go run over PromotionID, then cross-checked with python
// hashlib + base64.b32encode over the documented preimage) so that the dedup step of the
// scheme cannot drift silently: the input carries ghcr.io/example/web@digestA twice under
// two tags. Without dedup the value would be "adbbyam5c6".
func TestPromotionIDFixedVectorWithDuplicate(t *testing.T) {
	const digestC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	refs := []Ref{
		{Repo: "ghcr.io/example/web", Tag: "v202601010101", Digest: digestA},
		{Repo: "ghcr.io/example/counta", Tag: "v202601010101", Digest: digestB},
		{Repo: "ghcr.io/example/web", Tag: "v202601010102", Digest: digestA},
		{Repo: "ghcr.io/example/api", Tag: "v1", Digest: digestC},
	}
	const want = "jucx6bse2f"
	if got := PromotionID("example/gitops", "app-staging", refs); got != want {
		t.Fatalf("PromotionID = %q, want %q", got, want)
	}
}

func TestPromotionIDProperties(t *testing.T) {
	a := Ref{Repo: "ghcr.io/example/web", Digest: digestA}
	b := Ref{Repo: "ghcr.io/example/counta", Digest: digestB}
	base := PromotionID("example/gitops", "app-production", []Ref{a, b})
	if got := PromotionID("example/gitops", "app-production", []Ref{b, a}); got != base {
		t.Errorf("order-dependent: %q vs %q", got, base)
	}
	if got := PromotionID("example/gitops", "app-production", []Ref{a, b, a}); got != base {
		t.Errorf("duplicate repo@digest changed the id: %q vs %q", got, base)
	}
	if got := PromotionID("example/gitops", "app-staging", []Ref{a, b}); got == base {
		t.Error("target env not part of the id")
	}
	if got := PromotionID("other/gitops", "app-production", []Ref{a, b}); got == base {
		t.Error("repo full name not part of the id")
	}
	tagged := Ref{Repo: a.Repo, Tag: "v9", Digest: a.Digest}
	if got := PromotionID("example/gitops", "app-production", []Ref{tagged, b}); got != base {
		t.Error("tag leaked into the id; only repo@digest may contribute")
	}
	if len(base) != 10 || strings.ToLower(base) != base {
		t.Errorf("id %q is not 10 lowercase characters", base)
	}
}
