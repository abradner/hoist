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
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
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
	// thirdparty: no first-party image anywhere; the cross-column Differs still applies.
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
	tb := Compute(fixture(), []string{"ghcr.io/"}, nil)
	if diff := cmp.Diff([]string{"a", "b", "c"}, tb.Envs); diff != "" {
		t.Fatalf("Envs (-want +got):\n%s", diff)
	}
	got := map[string][]string{}
	for _, r := range tb.Rows {
		cells := make([]string, len(r.Cells))
		for i, c := range r.Cells {
			cells[i] = c.String()
		}
		got[r.Family] = cells
	}
	want := map[string][]string{
		"absent":     {"v1  pinned", "", "v9  pinned"},
		"drift":      {"v1  unpinned", "v2  pinned", "v2  unpinned"},
		"empty":      {"", "no images", ""},
		"mixedtags":  {"2 versions  split", "", ""},
		"multi":      {"2 images  pinned", "2 images  unpinned", ""},
		"pinned":     {"v1  pinned", "v1  pinned", "v1  pinned"},
		"sidecar":    {"v1  pinned", "v1  pinned", ""},
		"thirdparty": {"7  external", "8  external", "8  external"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("cells (-want +got):\n%s", diff)
	}
}

// Every state word, and the flags behind them, on the cells that define them.
func TestComputeStates(t *testing.T) {
	tb := Compute(fixture(), []string{"ghcr.io/"}, nil)
	cell := func(fam string, col int) Cell {
		for _, r := range tb.Rows {
			if r.Family == fam {
				return r.Cells[col]
			}
		}
		t.Fatalf("no family %q", fam)
		return Cell{}
	}
	cases := []struct {
		fam  string
		col  int
		want Cell
	}{
		{"pinned", 1, Cell{Present: true, Text: "v1", State: StatePinned, Pinned: true}},
		{"drift", 1, Cell{Present: true, Text: "v2", State: StatePinned, Pinned: true, Differs: true}},
		{"drift", 2, Cell{Present: true, Text: "v2", State: StateUnpinned, Differs: true}},
		{"absent", 1, Cell{}},
		{"absent", 2, Cell{Present: true, Text: "v9", State: StatePinned, Pinned: true}},
		{"thirdparty", 0, Cell{Present: true, Text: "7", State: StateExternal, ThirdParty: true}},
		{"thirdparty", 1, Cell{Present: true, Text: "8", State: StateExternal, ThirdParty: true, Differs: true}},
		{"multi", 1, Cell{Present: true, Text: "2 images", State: StateUnpinned, Differs: true}},
		{"mixedtags", 0, Cell{Present: true, Text: "2 versions", State: StateSplit}},
		{"empty", 1, Cell{Present: true, Text: "no images"}},
	}
	for _, tc := range cases {
		got := cell(tc.fam, tc.col)
		got.key = ""
		if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(Cell{})); diff != "" {
			t.Errorf("%s[%d] (-want +got):\n%s", tc.fam, tc.col, diff)
		}
	}
}

// Drift is decided by what the cluster runs against what the manifest declares: a
// different digest on a pinned manifest, a different tag on an unpinned one. A repo the
// cluster did not report is never drifted, and drift wins over the pin word.
func TestComputeDrift(t *testing.T) {
	running := Running{
		"b": {
			"ghcr.io/x/app": {{Repo: "ghcr.io/x/app", Tag: "v2", Digest: digestC}}, // pinned family says digestA
			"ghcr.io/x/web": {{Repo: "ghcr.io/x/web", Tag: "v1", Digest: digestA}}, // multi: web agrees, worker unreported
		},
		"c": {
			"ghcr.io/x/app": {{Repo: "ghcr.io/x/app", Tag: "v3", Digest: digestC}}, // drift family is unpinned v2: tag differs
		},
	}
	tb := Compute(fixture(), []string{"ghcr.io/"}, running)
	got := map[string]Cell{}
	for _, r := range tb.Rows {
		for i, c := range r.Cells {
			got[r.Family+"/"+tb.Envs[i]] = c
		}
	}
	if c := got["pinned/b"]; c.State != StateDrifted || c.Running != "v2" || c.Compared != "by digest" {
		t.Errorf("pinned/b = %+v; want drifted, running v2, compared by digest", c)
	}
	if c := got["pinned/a"]; c.State != StatePinned {
		t.Errorf("pinned/a = %+v; env a was not asked, so nothing there is drifted", c)
	}
	if c := got["multi/b"]; c.State != StateUnpinned {
		t.Errorf("multi/b = %+v; web agrees and worker was not reported", c)
	}
	if c := got["drift/c"]; c.State != StateDrifted || c.Running != "v3" || c.Compared != "by tag" {
		t.Errorf("drift/c = %+v; want drifted by tag", c)
	}
	if c := got["drift/b"]; c.State != StateDrifted {
		t.Errorf("drift/b = %+v; manifest digestB vs running digestC", c)
	}
	if c := got["thirdparty/b"]; c.State != StateExternal {
		t.Errorf("thirdparty/b = %+v; external cells are never compared", c)
	}
	// Positive control the other way: a cluster that agrees leaves the word alone.
	agree := Running{"b": {"ghcr.io/x/app": {{Repo: "ghcr.io/x/app", Tag: "v1", Digest: digestA}}}}
	tb = Compute(fixture(), []string{"ghcr.io/"}, agree)
	for _, r := range tb.Rows {
		if r.Family == "pinned" && r.Cells[1].State != StatePinned {
			t.Errorf("pinned/b with an agreeing cluster = %+v", r.Cells[1])
		}
	}
}

// Every running build reaches the cell (#122): a partial rollout is drifted when one of
// its builds is undeclared and the sentence counts both; a bare manifest is compared by
// tag with the tag the pod pulled, and a pod that reported no tag cannot be compared with
// it at all; a pinned manifest is compared by digest.
func TestDriftKeepsEveryRunningBuildAndNamesTheComparison(t *testing.T) {
	const app = "ghcr.io/x/app"
	ref := func(tag, digest string) image.Ref { return image.Ref{Repo: app, Tag: tag, Digest: digest} }
	cases := []struct {
		name         string
		occs         []gitops.Occurrence
		running      []image.Ref
		wantState    State
		wantRunning  string
		wantCompared string
	}{
		{"partial rollout: one build declared, one not", []gitops.Occurrence{occ(app, "v1", digestA)},
			[]image.Ref{ref("v1", digestA), ref("v2", digestB)},
			StateDrifted, "2 builds running (v1, v2)", "by digest"},
		{"two builds, both declared (split manifest mid-rollout)", []gitops.Occurrence{occ(app, "v1", digestA), occ(app, "v2", digestB)},
			[]image.Ref{ref("v1", digestA), ref("v2", digestB)},
			StateSplit, "", ""},
		{"bare manifest, pod pulled the same tag", []gitops.Occurrence{occ(app, "v1", "")},
			[]image.Ref{ref("v1", digestA)},
			StateUnpinned, "", ""},
		{"bare manifest, pod pulled another tag", []gitops.Occurrence{occ(app, "v1", "")},
			[]image.Ref{ref("v2", digestB)},
			StateDrifted, "v2", "by tag"},
		{"bare manifest, pod reported no tag: not comparable", []gitops.Occurrence{occ(app, "v1", "")},
			[]image.Ref{ref("", digestB)},
			StateUnpinned, "", ""},
		{"pinned manifest, pod digest differs", []gitops.Occurrence{occ(app, "v1", digestA)},
			[]image.Ref{ref("v1", digestB)},
			StateDrifted, "v1", "by digest"},
	}
	for _, tc := range cases {
		got := cellFor(&gitops.Family{Name: "f", Occurrences: tc.occs}, []string{"ghcr.io/"}, map[string][]image.Ref{app: tc.running})
		if got.State != tc.wantState || got.Running != tc.wantRunning || got.Compared != tc.wantCompared {
			t.Errorf("%s: state %q running %q compared %q; want %q %q %q", tc.name, got.State, got.Running, got.Compared, tc.wantState, tc.wantRunning, tc.wantCompared)
		}
	}
}

// Split is per image repo, on builds (#118): one tag pinned to two digests is split; a
// multi-repo cell whose sidecar alone runs under two tags is split; and the mixed case —
// one tag pinned and bare — stays unpinned, since that is one build.
func TestSplitIsJudgedPerRepoOnBuilds(t *testing.T) {
	cases := []struct {
		name string
		occs []gitops.Occurrence
		want Cell
	}{
		{"one tag, two digests", []gitops.Occurrence{occ("ghcr.io/x/app", "v1", digestA), occ("ghcr.io/x/app", "v1", digestB)},
			Cell{Present: true, Text: "2 versions", State: StateSplit, Pinned: true}},
		{"intra-repo split inside a multi-repo cell", []gitops.Occurrence{occ("ghcr.io/x/app", "v1", ""), occ("ghcr.io/x/sidecar", "v1", ""), occ("ghcr.io/x/sidecar", "v2", "")},
			Cell{Present: true, Text: "2 images", State: StateSplit}},
		{"same tag pinned and bare is one build", []gitops.Occurrence{occ("ghcr.io/x/app", "v1", digestA), occ("ghcr.io/x/app", "v1", "")},
			Cell{Present: true, Text: "v1", State: StateUnpinned}},
		{"two repos each on one build", []gitops.Occurrence{occ("ghcr.io/x/app", "v1", digestA), occ("ghcr.io/x/sidecar", "v2", digestA)},
			Cell{Present: true, Text: "2 images", State: StatePinned, Pinned: true}},
	}
	for _, tc := range cases {
		got := cellFor(&gitops.Family{Name: "f", Occurrences: tc.occs}, []string{"ghcr.io/"}, nil)
		got.key = ""
		if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(Cell{})); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

func TestCellStringBlankWhenAbsent(t *testing.T) {
	if got := (Cell{Pinned: true, State: StatePinned, Text: "v1"}).String(); got != "" {
		t.Fatalf("absent cell renders %q", got)
	}
}

func TestTagOrDigest(t *testing.T) {
	if got := tagOrDigest(image.Ref{Repo: "r", Digest: digestA}); got != "sha256:aaaaaaaaaaaa" {
		t.Errorf("digest-only ref = %q", got)
	}
	if got := tagOrDigest(image.Ref{Repo: "r", Tag: "v1", Digest: digestA}); got != "v1" {
		t.Errorf("tagged ref = %q", got)
	}
}

func TestFirstPartyRepos(t *testing.T) {
	r := fixture()
	if got := FirstPartyRepos(r.Envs["a"].Families["multi"], []string{"ghcr.io/"}); len(got) != 2 || got[0] != "ghcr.io/x/web" {
		t.Errorf("multi = %v", got)
	}
	if got := FirstPartyRepos(r.Envs["a"].Families["thirdparty"], []string{"ghcr.io/"}); len(got) != 0 {
		t.Errorf("thirdparty = %v", got)
	}
	if got := FirstPartyRepos(nil, []string{"ghcr.io/"}); got != nil {
		t.Errorf("nil family = %v", got)
	}
}

func TestDisplayRootNeverShowsAFullPath(t *testing.T) {
	for in, want := range map[string]string{"": ".", "repo": "repo", "/home/me/src/gitops": "gitops", "../gitops": "gitops", "..": ".."} {
		if got := displayRoot(in); got != want {
			t.Errorf("displayRoot(%q) = %q, want %q", in, got, want)
		}
	}
}
