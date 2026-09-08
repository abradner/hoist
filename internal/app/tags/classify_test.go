package tags

import (
	"testing"

	"github.com/abradner/hoist/pkg/forge"
)

// The classification rule (#91), one case per shape the live registry actually listed plus
// the ones the issue named.
func TestClassify(t *testing.T) {
	cases := []struct {
		tag  string
		want Class
	}{
		{"v1.2.3", ClassRelease},
		{"v202609060428", ClassRelease},
		{"1.2.3", ClassRelease},
		{"release-2026.09", ClassRelease},
		{"v1.2.3-rc1", ClassRelease},
		{"sha-055c877f", ClassDigest},
		{"sha256-0123456789abcdef", ClassDigest},
		{"latest", ClassMoving},
		{"main", ClassMoving},
		{"master", ClassMoving},
		{"fix-docker-workflow-secrets-context", ClassMoving},
		{"sha-abc", ClassMoving},    // too short to be a hash
		{"v", ClassMoving},          // a prefix with no number
		{"release", ClassMoving},    // no version under the prefix
		{"1.2.3.4.5", ClassRelease}, // any depth of dots
	}
	for _, c := range cases {
		if got := Classify(c.tag); got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

// DeriveRows groups before it orders: releases first (by git date when mapped), then digest
// tags, then moving tags, each group keeping the ordering it had — and nothing is dropped.
func TestDeriveRowsGroupsByClassThenOrders(t *testing.T) {
	regTags := []string{"fix-docker-workflow-secrets-context", "latest", "main", "sha-055c877f", "v1", "v2", "sha-1234567"}
	gitTags := []forge.GitTag{{Name: "v1", Date: fixedNow.AddDate(0, 0, -10)}, {Name: "v2", Date: fixedNow.AddDate(0, 0, -1)}}
	got := tagsOf(DeriveRows(regTags, gitTags, true))
	want := []string{"v2", "v1", "sha-055c877f", "sha-1234567", "fix-docker-workflow-secrets-context", "latest", "main"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v (nothing filtered out)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
	// Unmapped: the same groups, and Reorder keeps them while sorting loaded rows within.
	rows := DeriveRows(regTags, nil, false)
	for i := range rows {
		rows[i].MetaLoaded = true
		rows[i].Meta.Created = fixedNow.AddDate(0, 0, -len(rows)+i) // later rows newer
	}
	got = tagsOf(Reorder(rows, false))
	want = []string{"v2", "v1", "sha-1234567", "sha-055c877f", "main", "latest", "fix-docker-workflow-secrets-context"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Reorder must sort within groups only: got %v, want %v", got, want)
		}
	}
}
