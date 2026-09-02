package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	digestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	web     = "ghcr.io/example/web"
	counta  = "ghcr.io/example/counta"
	ns      = "app-staging"
)

func occ(t *testing.T, file, container, ref string) gitops.Occurrence {
	t.Helper()
	r, err := image.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	return gitops.Occurrence{File: file, Line: 1, Kind: "Deployment", Name: "x", Container: container, Raw: ref, Ref: r}
}

func running(pod, container, repo, digest string) k8s.RunningImage {
	return k8s.RunningImage{Pod: pod, Container: container, Ref: image.Ref{Repo: repo, Digest: digest}}
}

func fakeCluster(imgs ...k8s.RunningImage) *k8s.Fake {
	return &k8s.Fake{Images: map[string][]k8s.RunningImage{ns: imgs}}
}

// resolveWeb resolves in and returns web's resolution.
func resolveWeb(t *testing.T, in Input, cluster k8s.Cluster, reg registry.Registry) Resolution {
	t.Helper()
	res, err := Resolve(context.Background(), in, cluster, reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := res[web]
	if !ok {
		t.Fatalf("no resolution for %s in %v", web, Repos(res))
	}
	return r
}

func codes(ws []gitops.Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func TestPodsThatAgreeWinOverABareTag(t *testing.T) {
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{
		occ(t, "web.yaml", "web", web+":sha-1a2b"),
		occ(t, "web.yaml", "worker", web+":sha-1a2b"),
	}}
	cluster := fakeCluster(running("web-1", "web", web, digestA), running("web-1", "worker", web, digestA), running("web-2", "web", web, digestA))
	reg := &registry.Fake{}
	r := resolveWeb(t, in, cluster, reg)
	if want := (image.Ref{Repo: web, Tag: "sha-1a2b", Digest: digestA}); r.Ref != want {
		t.Errorf("Ref = %s, want %s", r.Ref, want)
	}
	if r.Source != SourcePods || r.Detail != "3 running containers agree; manifest is not pinned" {
		t.Errorf("Source %q, Detail %q", r.Source, r.Detail)
	}
	if len(r.Warnings) != 0 || len(r.Alternatives) != 0 {
		t.Errorf("unexpected warnings %v / alternatives %v", codes(r.Warnings), r.Alternatives)
	}
	if len(reg.Calls) != 0 {
		t.Errorf("registry consulted although the pods answered: %v", reg.Calls)
	}
	if calls := strings.Join(cluster.Calls, ","); calls != "RunningImages "+ns {
		t.Errorf("cluster calls %q, want exactly one list of %s", calls, ns)
	}
}

// The mid-rollout adversary: one pod on the new digest, one on the old. The warning
// names every pod, container and digest, and the choice follows the stated rule.
func TestRunningDisagreementWarnsAndChoosesDeterministically(t *testing.T) {
	cases := map[string]struct {
		manifest string
		pods     []k8s.RunningImage
		want     string
		reason   string
		alts     []string
	}{
		"manifest pin among the running digests wins": {
			manifest: web + ":v2@" + digestB,
			pods:     []k8s.RunningImage{running("web-1", "web", web, digestA), running("web-2", "web", web, digestB), running("web-3", "web", web, digestA)},
			want:     digestB, reason: "it matches the manifest pin", alts: []string{digestA},
		},
		"else the most frequent": {
			manifest: web + ":v2",
			pods:     []k8s.RunningImage{running("web-1", "web", web, digestC), running("web-2", "web", web, digestB), running("web-3", "web", web, digestC)},
			want:     digestC, reason: "the most frequent, 2 of 3", alts: []string{digestB},
		},
		"else the lexically smallest": {
			manifest: web + ":v2@" + digestD,
			pods:     []k8s.RunningImage{running("web-2", "web", web, digestC), running("web-1", "web", web, digestB)},
			want:     digestB, reason: "the lexically smallest; every digest runs once", alts: []string{digestC},
		},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", tc.manifest)}}
			r := resolveWeb(t, in, fakeCluster(tc.pods...), &registry.Fake{})
			if r.Ref.Digest != tc.want || r.Source != SourcePods {
				t.Errorf("chose %s from %s, want %s from pods", r.Ref.Digest, r.Source, tc.want)
			}
			var disagree *gitops.Warning
			for i := range r.Warnings {
				if r.Warnings[i].Code == WarnRunningDisagrees {
					disagree = &r.Warnings[i]
				}
			}
			if disagree == nil {
				t.Fatalf("no %s warning; got %v", WarnRunningDisagrees, codes(r.Warnings))
			}
			for _, p := range tc.pods {
				line := "pod " + p.Pod + " container " + p.Container + " " + p.Ref.Digest
				if !strings.Contains(disagree.Message, line) {
					t.Errorf("warning does not name %q:\n%s", line, disagree.Message)
				}
			}
			if !strings.Contains(disagree.Message, "using "+tc.want+" ("+tc.reason+")") {
				t.Errorf("warning does not state the choice and reason:\n%s", disagree.Message)
			}
			var gotAlts []string
			for _, a := range r.Alternatives {
				gotAlts = append(gotAlts, a.Digest)
				if a.Repo != web || a.Tag != "v2" {
					t.Errorf("alternative %s lost the repo or tag", a)
				}
			}
			for _, want := range tc.alts {
				found := false
				for _, g := range gotAlts {
					found = found || g == want
				}
				if !found {
					t.Errorf("alternatives %v lack %s", gotAlts, want)
				}
			}
		})
	}
}

// A pinned manifest that disagrees with the pods: the order decides which wins, the
// other is an alternative, and the disagreement is a warning either way.
func TestManifestPinVersusRunningFollowsTheOrder(t *testing.T) {
	manifest := occ(t, "web.yaml", "web", web+":v2@"+digestB)
	cluster := fakeCluster(running("web-1", "web", web, digestA))

	r := resolveWeb(t, Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{manifest}}, cluster, &registry.Fake{})
	if r.Ref.Digest != digestA || r.Source != SourcePods {
		t.Errorf("default order: chose %s from %s, want the running digest", r.Ref.Digest, r.Source)
	}
	if diff := cmp.Diff([]image.Ref{{Repo: web, Tag: "v2", Digest: digestB}}, r.Alternatives); diff != "" {
		t.Errorf("alternatives (-want +got):\n%s", diff)
	}
	if got := codes(r.Warnings); len(got) != 1 || got[0] != WarnRunningVsManifest {
		t.Fatalf("warnings = %v, want [%s]", got, WarnRunningVsManifest)
	}
	if !strings.Contains(r.Warnings[0].Message, "using the running digest") || !strings.Contains(r.Warnings[0].Message, digestB) || !strings.Contains(r.Warnings[0].Message, digestA) {
		t.Errorf("warning: %s", r.Warnings[0].Message)
	}

	r = resolveWeb(t, Input{Namespace: ns, Order: []Source{SourceManifest, SourcePods, SourceRegistry}, Occurrences: []gitops.Occurrence{manifest}}, cluster, &registry.Fake{})
	if r.Ref.Digest != digestB || r.Source != SourceManifest {
		t.Errorf("manifest-first order: chose %s from %s, want the manifest pin", r.Ref.Digest, r.Source)
	}
	if diff := cmp.Diff([]image.Ref{{Repo: web, Tag: "v2", Digest: digestA}}, r.Alternatives); diff != "" {
		t.Errorf("alternatives (-want +got):\n%s", diff)
	}
	if got := codes(r.Warnings); len(got) != 1 || got[0] != WarnRunningVsManifest || !strings.Contains(r.Warnings[0].Message, "using the manifest pin") {
		t.Errorf("warnings = %v: %v", got, r.Warnings)
	}
}

func TestScaledToZeroUsesTheManifestPin(t *testing.T) {
	reg := &registry.Fake{}
	r := resolveWeb(t, Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+":v2@"+digestB)}}, fakeCluster(), reg)
	if r.Ref.Digest != digestB || r.Source != SourceManifest || r.Detail != "pinned in the manifest; no running pods" {
		t.Errorf("got %s from %s (%s)", r.Ref, r.Source, r.Detail)
	}
	if len(r.Warnings) != 0 || len(reg.Calls) != 0 {
		t.Errorf("warnings %v, registry calls %v", codes(r.Warnings), reg.Calls)
	}
}

func TestScaledToZeroBareTagAsksTheRegistry(t *testing.T) {
	reg := &registry.Fake{Digests: map[string]string{web + ":sha-1a2b": digestC}, Auth: "keychain"}
	r := resolveWeb(t, Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+":sha-1a2b")}}, fakeCluster(), reg)
	if want := (image.Ref{Repo: web, Tag: "sha-1a2b", Digest: digestC}); r.Ref != want || r.Source != SourceRegistry {
		t.Errorf("got %s from %s, want %s from the registry", r.Ref, r.Source, want)
	}
	if r.Detail != "registry HEAD of tag sha-1a2b" {
		t.Errorf("Detail = %q", r.Detail)
	}
	if calls := strings.Join(reg.Calls, ","); calls != "Head "+web+":sha-1a2b" {
		t.Errorf("registry calls %q", calls)
	}
}

func TestRegistryFailureLeavesOnlyThatRepoUnresolved(t *testing.T) {
	reg := &registry.Fake{Err: errors.New("registry: no credential source worked for ghcr.io: keychain: status 403 Forbidden: DENIED")}
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{
		occ(t, "web.yaml", "web", web+":sha-1a2b"),
		occ(t, "counta.yaml", "counta", counta+":v1@"+digestB),
	}}
	res, err := Resolve(context.Background(), in, fakeCluster(), reg)
	if err != nil {
		t.Fatal(err)
	}
	w := res[web]
	if w.Resolved() || w.Source != "" {
		t.Errorf("web should be unresolved, got %s from %s", w.Ref, w.Source)
	}
	if got := codes(w.Warnings); len(got) != 1 || got[0] != WarnUnresolved {
		t.Fatalf("web warnings = %v", got)
	}
	for _, want := range []string{"no running pods", "manifest is not pinned", "status 403 Forbidden: DENIED"} {
		if !strings.Contains(w.Warnings[0].Message, want) {
			t.Errorf("unresolved warning lacks %q: %s", want, w.Warnings[0].Message)
		}
	}
	if c := res[counta]; !c.Resolved() || c.Ref.Digest != digestB {
		t.Errorf("counta should still resolve from its pin, got %+v", c)
	}
	digests := Digests(res)
	if _, ok := digests[web]; ok {
		t.Error("an unresolved repo reached the digests map")
	}
	if digests[counta].Digest != digestB {
		t.Errorf("digests = %v", digests)
	}
	if got := codes(Warnings(res)); len(got) != 1 {
		t.Errorf("Warnings() = %v", got)
	}
}

func TestOverrideBeatsEveryOtherSource(t *testing.T) {
	ov := image.Ref{Repo: web, Tag: "v9", Digest: digestD}
	reg := &registry.Fake{Digests: map[string]string{web + ":v2": digestC}}
	in := Input{Namespace: ns, Order: DefaultOrder, Overrides: map[string]image.Ref{web: ov},
		Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+":v2@"+digestB)}}
	r := resolveWeb(t, in, fakeCluster(running("web-1", "web", web, digestA)), reg)
	if r.Ref != ov || r.Source != SourceOverride {
		t.Errorf("got %s from %s, want the override", r.Ref, r.Source)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("an override should not warn: %v", codes(r.Warnings))
	}
	// Alternatives keep the manifest's tag: that is the tag those digests were seen with.
	if diff := cmp.Diff([]image.Ref{{Repo: web, Tag: "v2", Digest: digestA}, {Repo: web, Tag: "v2", Digest: digestB}}, r.Alternatives); diff != "" {
		t.Errorf("alternatives (-want +got):\n%s", diff)
	}
	if len(reg.Calls) != 0 {
		t.Errorf("registry consulted under an override: %v", reg.Calls)
	}
	if Digests(map[string]Resolution{web: r})[web] != ov {
		t.Error("Digests() should carry the override")
	}
}

// Pods cannot supply a tag and hoist never invents one: a tagless manifest stays
// unresolved so BuildPlan's own refusal applies.
func TestTaglessManifestStaysUnresolved(t *testing.T) {
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+"@"+digestB)}}
	r := resolveWeb(t, in, fakeCluster(running("web-1", "web", web, digestA)), &registry.Fake{})
	if r.Resolved() {
		t.Fatalf("resolved %s without a tag to write", r.Ref)
	}
	if got := codes(r.Warnings); len(got) != 2 || got[0] != WarnRunningVsManifest || got[1] != WarnUnresolved {
		t.Fatalf("warnings = %v", got)
	}
	if !strings.Contains(r.Warnings[1].Message, "never fabricates a tag") {
		t.Errorf("unresolved warning: %s", r.Warnings[1].Message)
	}
	// A sibling occurrence with a tag supplies it.
	in.Occurrences = append(in.Occurrences, occ(t, "web.yaml", "worker", web+":v2@"+digestB))
	r = resolveWeb(t, in, fakeCluster(running("web-1", "web", web, digestA)), &registry.Fake{})
	if want := (image.Ref{Repo: web, Tag: "v2", Digest: digestA}); r.Ref != want {
		t.Errorf("Ref = %s, want %s", r.Ref, want)
	}
}

// imageID names the repo the runtime pulled: a docker.io alias must match its manifest,
// a mirror must not (it is a different repo, so the manifest and registry decide).
func TestRunningReposCompareCanonically(t *testing.T) {
	const nginx = "docker.io/library/nginx"
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{
		occ(t, "nginx.yaml", "nginx", nginx+":1.27"),
		occ(t, "web.yaml", "web", web+":v2@"+digestB),
	}}
	cluster := fakeCluster(
		running("nginx-1", "nginx", "index.docker.io/library/nginx", digestC),
		running("web-1", "web", "mirror.example/ghcr/example/web", digestA),
	)
	res, err := Resolve(context.Background(), in, cluster, &registry.Fake{})
	if err != nil {
		t.Fatal(err)
	}
	if r := res[nginx]; r.Source != SourcePods || r.Ref.Digest != digestC || r.Ref.Repo != nginx {
		t.Errorf("nginx: got %s from %s, want the alias matched", r.Ref, r.Source)
	}
	if r := res[web]; r.Source != SourceManifest || r.Ref.Digest != digestB {
		t.Errorf("web: got %s from %s, want the manifest (the mirror is another repo)", r.Ref, r.Source)
	}
}

// Two repos sharing a tag must not share a digest: the key is the repo.
func TestDigestsAreKeyedByRepoNotTag(t *testing.T) {
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{
		occ(t, "web.yaml", "web", web+":sha-1a2b"),
		occ(t, "counta.yaml", "counta", counta+":sha-1a2b"),
	}}
	cluster := fakeCluster(running("web-1", "web", web, digestA), running("counta-1", "counta", counta, digestB))
	res, err := Resolve(context.Background(), in, cluster, &registry.Fake{})
	if err != nil {
		t.Fatal(err)
	}
	if res[web].Ref.Digest != digestA || res[counta].Ref.Digest != digestB {
		t.Errorf("web %s, counta %s", res[web].Ref, res[counta].Ref)
	}
}

func TestClusterFailureFailsResolve(t *testing.T) {
	cluster := &k8s.Fake{Err: errors.New("k8s: listing pods in namespace app-staging: Forbidden")}
	_, err := Resolve(context.Background(), Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+":v2")}}, cluster, &registry.Fake{})
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("err = %v", err)
	}
}

func TestOrderControlsWhichAdaptorsAreTouched(t *testing.T) {
	occs := []gitops.Occurrence{occ(t, "web.yaml", "web", web+":v2@"+digestB)}
	cluster := fakeCluster(running("web-1", "web", web, digestA))
	reg := &registry.Fake{}

	res, err := Resolve(context.Background(), Input{Namespace: ns, Occurrences: occs}, cluster, reg)
	if err != nil || len(res) != 0 || len(cluster.Calls) != 0 || len(reg.Calls) != 0 {
		t.Errorf("empty order: res %v, err %v, cluster %v, registry %v", res, err, cluster.Calls, reg.Calls)
	}
	r := resolveWeb(t, Input{Namespace: ns, Order: []Source{SourceManifest}, Occurrences: occs}, nil, nil)
	if r.Source != SourceManifest || len(cluster.Calls) != 0 {
		t.Errorf("manifest only: %s from %s; cluster calls %v", r.Ref, r.Source, cluster.Calls)
	}
	for _, in := range []Input{
		{Namespace: ns, Order: []Source{SourcePods}, Occurrences: occs},
		{Namespace: ns, Order: []Source{SourceRegistry}, Occurrences: occs},
		{Namespace: ns, Order: []Source{"vault"}, Occurrences: occs},
		{Namespace: ns, Order: []Source{SourcePods, SourcePods}, Occurrences: occs},
		{Namespace: "", Order: DefaultOrder, Occurrences: occs},
	} {
		if _, err := Resolve(context.Background(), in, nil, nil); err == nil {
			t.Errorf("Resolve(%+v) with no adaptors succeeded", in.Order)
		}
	}
}

func TestWarningsAreDeterministic(t *testing.T) {
	in := Input{Namespace: ns, Order: DefaultOrder, Occurrences: []gitops.Occurrence{occ(t, "web.yaml", "web", web+":v2")}}
	pods := []k8s.RunningImage{running("web-3", "web", web, digestC), running("web-1", "web", web, digestA), running("web-2", "web", web, digestB)}
	var first string
	for i := range 20 {
		r := resolveWeb(t, in, fakeCluster(pods...), &registry.Fake{})
		msg := r.Warnings[0].Message + "|" + r.Ref.String()
		for _, a := range r.Alternatives {
			msg += "|" + a.String()
		}
		if i == 0 {
			first = msg
			continue
		}
		if msg != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, msg)
		}
	}
	if !strings.Contains(first, "using "+digestA+" (the lexically smallest") {
		t.Errorf("choice: %s", first)
	}
}

func TestParseOrder(t *testing.T) {
	got, err := ParseOrder([]string{"registry", " pods"})
	if err != nil || len(got) != 2 || got[0] != SourceRegistry || got[1] != SourcePods {
		t.Errorf("ParseOrder = %v, %v", got, err)
	}
	for _, bad := range [][]string{{"none"}, {"override"}, {"pods", "pods"}} {
		if _, err := ParseOrder(bad); err == nil {
			t.Errorf("ParseOrder(%v) accepted", bad)
		}
	}
}
