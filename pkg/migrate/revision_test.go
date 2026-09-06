package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestResolveFullLabelWinsWithoutAForgeCall(t *testing.T) {
	f := &forge.Fake{Refs: map[string]string{"v3": shaB}}
	r, err := Resolver{Forge: f}.Resolve(context.Background(), ResolveIn{
		Ref:    image.Ref{Repo: "ghcr.io/example/app", Tag: "v3"},
		Labels: map[string]string{RevisionLabel: shaA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.SHA != shaA || r.Source != SourceLabel {
		t.Fatalf("r = %+v; want the label's sha", r)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("a full-length label needs no forge call; got %v", f.Calls)
	}
}

func TestResolveShortLabelIsExpanded(t *testing.T) {
	f := &forge.Fake{Refs: map[string]string{"aaaaaaa": shaA}}
	r, err := Resolver{Forge: f}.Resolve(context.Background(), ResolveIn{
		Ref:    image.Ref{Repo: "x", Tag: "v3"},
		Labels: map[string]string{RevisionLabel: "aaaaaaa"},
	})
	if err != nil || r.SHA != shaA || r.Source != SourceLabel || r.Detail != "aaaaaaa" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestResolveFallsThroughLabelToTagToShaTag(t *testing.T) {
	cases := []struct {
		name   string
		tag    string
		labels map[string]string
		refs   map[string]string
		want   Source
		sha    string
	}{
		{"git tag when no label", "v3", nil, map[string]string{"v3": shaB}, SourceGitTag, shaB},
		{"git tag when label is junk", "v3", map[string]string{RevisionLabel: "not-a-sha"}, map[string]string{"v3": shaB}, SourceGitTag, shaB},
		{"sha- tag when no git tag", "sha-cccccccc", nil, map[string]string{"cccccccc": shaC}, SourceShaTag, shaC},
		{"git tag named sha- beats the prefix", "sha-cccccccc", nil, map[string]string{"sha-cccccccc": shaB, "cccccccc": shaC}, SourceGitTag, shaB},
		{"unknown", "v9", nil, nil, SourceUnknown, ""},
		{"digest only, no labels", "", nil, nil, SourceUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &forge.Fake{Refs: tc.refs}
			r, err := Resolver{Forge: f}.Resolve(context.Background(), ResolveIn{Ref: image.Ref{Repo: "x", Tag: tc.tag, Digest: "sha256:0000"}, Labels: tc.labels})
			if err != nil {
				t.Fatal(err)
			}
			if r.Source != tc.want || r.SHA != tc.sha {
				t.Fatalf("r = %+v; want %s %s", r, tc.want, tc.sha)
			}
		})
	}
}

// ok=false means "try the next source"; an error means stop. A scope gap read as "unknown"
// would silently degrade every revision (AGENTS.md §6.1 item 1's gotcha at this boundary).
func TestResolveForgeErrorStopsTheWalk(t *testing.T) {
	f := &forge.Fake{ResolveErr: errors.New("HTTP 403 the gh token may be missing the repo scope")}
	r, err := Resolver{Forge: f}.Resolve(context.Background(), ResolveIn{Ref: image.Ref{Repo: "x", Tag: "v3"}})
	if err == nil || !strings.Contains(err.Error(), "repo scope") {
		t.Fatalf("err = %v; want the forge's error", err)
	}
	if r.Resolved() {
		t.Fatalf("r = %+v; must not report a revision on error", r)
	}
}
