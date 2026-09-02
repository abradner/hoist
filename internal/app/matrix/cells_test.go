package matrix

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func occ(repo, tag, digest string) gitops.Occurrence {
	return gitops.Occurrence{Ref: image.Ref{Repo: repo, Tag: tag, Digest: digest}}
}

// fixture is a three-env repo whose families each exercise one cell rule.
func fixture() *gitops.Repo {
	r := &gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{}}
	add := func(env, fam string, occs ...gitops.Occurrence) {
		e := r.Envs[env]
		if e == nil {
			e = &gitops.Env{Name: env, Families: map[string]*gitops.Family{}}
			r.Envs[env] = e
		}
		e.Families[fam] = &gitops.Family{Name: fam, Occurrences: occs}
	}
	// pinned: same pinned ref in every env.
	add("a", "pinned", occ("ghcr.io/x/app", "v1", digestA))
	add("b", "pinned", occ("ghcr.io/x/app", "v1", digestA))
	add("c", "pinned", occ("ghcr.io/x/app", "v1", digestA))
	// drift: unpinned in a, a newer pinned tag in b, b's tag unpinned in c.
	add("a", "drift", occ("ghcr.io/x/app", "v1", ""))
	add("b", "drift", occ("ghcr.io/x/app", "v2", digestB))
	add("c", "drift", occ("ghcr.io/x/app", "v2", ""))
	// absent: missing from b, so c has no previous column to differ from.
	add("a", "absent", occ("ghcr.io/x/app", "v1", digestA))
	add("c", "absent", occ("ghcr.io/x/app", "v9", digestB))
	// thirdparty: no first-party image anywhere; the drift marker still applies.
	add("a", "thirdparty", occ("docker.io/lib/redis", "7", ""))
	add("b", "thirdparty", occ("docker.io/lib/redis", "8", ""))
	add("c", "thirdparty", occ("docker.io/lib/redis", "8", ""))
	// multi: two first-party image repos in one family.
	add("a", "multi", occ("ghcr.io/x/web", "v1", digestA), occ("ghcr.io/x/worker", "v1", digestA))
	add("b", "multi", occ("ghcr.io/x/web", "v1", digestA), occ("ghcr.io/x/worker", "v1", ""))
	// mixedtags: one image repo under two tags, one of them unpinned.
	add("a", "mixedtags", occ("ghcr.io/x/app", "v2", digestB), occ("ghcr.io/x/app", "v1", ""))
	// sidecar: first-party plus a third-party sidecar; only the first-party image counts.
	add("a", "sidecar", occ("ghcr.io/x/app", "v1", digestA), occ("docker.io/lib/redis", "7", ""))
	add("b", "sidecar", occ("ghcr.io/x/app", "v1", digestA), occ("docker.io/lib/redis", "8", ""))
	// empty: an Application whose manifests carry no containers.
	add("b", "empty")
	return r
}

func TestComputeCells(t *testing.T) {
	got := Compute(fixture(), []string{"ghcr.io/"})
	if diff := cmp.Diff([]string{"a", "b", "c"}, got.Envs); diff != "" {
		t.Fatalf("envs (-want +got):\n%s", diff)
	}
	want := map[string][]string{
		"pinned":     {"@  v1", "@  v1", "@  v1"},
		"drift":      {"   v1", "@≠ v2", " ≠ v2"},
		"absent":     {"@  v1", "", "@  v9"},
		"thirdparty": {"!  7", "!≠ 8", "!  8"},
		"multi":      {"@  2 images", " ≠ 2 images", ""},
		"mixedtags":  {"   v1,v2", "", ""},
		"sidecar":    {"@  v1", "@  v1", ""},
		"empty":      {"", "   no images", ""},
	}
	var families []string
	for _, row := range got.Rows {
		families = append(families, row.Family)
		cells := make([]string, len(row.Cells))
		for i, c := range row.Cells {
			cells[i] = c.String()
		}
		if diff := cmp.Diff(want[row.Family], cells); diff != "" {
			t.Errorf("%s (-want +got):\n%s", row.Family, diff)
		}
	}
	if diff := cmp.Diff([]string{"absent", "drift", "empty", "mixedtags", "multi", "pinned", "sidecar", "thirdparty"}, families); diff != "" {
		t.Errorf("families (-want +got):\n%s", diff)
	}
}

func TestComputeFlags(t *testing.T) {
	got := Compute(fixture(), []string{"ghcr.io/"})
	byName := map[string]Row{}
	for _, row := range got.Rows {
		byName[row.Family] = row
	}
	cases := []struct {
		family string
		env    int
		want   Cell
	}{
		{"pinned", 1, Cell{Present: true, Text: "v1", Pinned: true}},
		{"drift", 1, Cell{Present: true, Text: "v2", Pinned: true, Differs: true}},
		{"drift", 2, Cell{Present: true, Text: "v2", Differs: true}},
		{"absent", 1, Cell{}},
		{"absent", 2, Cell{Present: true, Text: "v9", Pinned: true}},
		{"thirdparty", 0, Cell{Present: true, Text: "7", ThirdParty: true}},
		{"thirdparty", 1, Cell{Present: true, Text: "8", ThirdParty: true, Differs: true}},
		{"multi", 1, Cell{Present: true, Text: "2 images", Differs: true}},
		{"mixedtags", 0, Cell{Present: true, Text: "v1,v2"}},
		{"empty", 1, Cell{Present: true, Text: "no images"}},
	}
	for _, tc := range cases {
		c := byName[tc.family].Cells[tc.env]
		c.key = ""
		if diff := cmp.Diff(tc.want, c, cmp.AllowUnexported(Cell{})); diff != "" {
			t.Errorf("%s[%d] (-want +got):\n%s", tc.family, tc.env, diff)
		}
	}
}

// TestMarkerContent pins the marker for every flag combination: pin state first (@ pinned,
// ! third-party-only, which wins over Pinned), drift second (≠), blank otherwise, and blank
// entirely for an absent cell whatever its other fields say.
func TestMarkerContent(t *testing.T) {
	cases := []struct {
		name string
		cell Cell
		want string
	}{
		{"absent", Cell{}, "  "},
		{"absent ignores flags", Cell{Pinned: true, Differs: true, ThirdParty: true}, "  "},
		{"present unpinned", Cell{Present: true}, "  "},
		{"pinned", Cell{Present: true, Pinned: true}, "@ "},
		{"drifted", Cell{Present: true, Differs: true}, " ≠"},
		{"pinned and drifted", Cell{Present: true, Pinned: true, Differs: true}, "@≠"},
		{"third-party", Cell{Present: true, ThirdParty: true}, "! "},
		{"third-party and drifted", Cell{Present: true, ThirdParty: true, Differs: true}, "!≠"},
		{"third-party wins over pinned", Cell{Present: true, ThirdParty: true, Pinned: true}, "! "},
	}
	for _, tc := range cases {
		if got := tc.cell.Marker(); got != tc.want {
			t.Errorf("%s: Marker(%+v) = %q, want %q", tc.name, tc.cell, got, tc.want)
		}
	}
}

func TestTagOrDigest(t *testing.T) {
	if got := tagOrDigest(image.Ref{Repo: "ghcr.io/x/app", Digest: digestA}); got != "sha256:aaaaaaaaaaaa" {
		t.Errorf("tag-less ref shows %q", got)
	}
	if got := tagOrDigest(image.Ref{Repo: "ghcr.io/x/app", Tag: "v1", Digest: digestA}); got != "v1" {
		t.Errorf("tagged ref shows %q", got)
	}
}

func TestDisplayRootNeverShowsAFullPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                       ".",
		"repo":                   "repo",
		"testdata/repo":          "testdata/repo",
		"../../testdata/repo":    "repo",
		"..":                     "..",
		"/home/someone/src/repo": "repo",
	} {
		if got := displayRoot(in); got != want {
			t.Errorf("displayRoot(%q) = %q, want %q", in, got, want)
		}
	}
}
