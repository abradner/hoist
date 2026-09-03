package forge

import (
	"context"
	"testing"
	"time"
)

func TestFakeCreateThenFindByBranch(t *testing.T) {
	f := &Fake{}
	pr, err := f.CreatePR(context.Background(), PRSpec{Title: "t", Body: "<!-- hoist:id=abc -->\nbody", Head: "hoist/env/abc", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 1 {
		t.Fatalf("Number = %d, want 1", pr.Number)
	}
	got, ok, err := f.FindPR(context.Background(), "hoist/env/abc", "<!-- hoist:id=abc -->")
	if err != nil || !ok {
		t.Fatalf("FindPR: ok=%v err=%v", ok, err)
	}
	if got.Number != pr.Number {
		t.Fatalf("found #%d, want #%d", got.Number, pr.Number)
	}
}

func TestFakeFindByMarkerWhenBranchRenamed(t *testing.T) {
	f := &Fake{}
	pr, err := f.CreatePR(context.Background(), PRSpec{Title: "t", Body: "<!-- hoist:id=abc -->\nbody", Head: "hoist/env/abc", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the branch having been renamed: no PR carries the original head anymore, but
	// the marker is still in the body.
	got, ok, err := f.FindPR(context.Background(), "some-other-branch-name", "<!-- hoist:id=abc -->")
	if err != nil || !ok {
		t.Fatalf("FindPR by marker: ok=%v err=%v", ok, err)
	}
	if got.Number != pr.Number {
		t.Fatalf("found #%d, want #%d", got.Number, pr.Number)
	}
}

func TestFakeFindPRNotFound(t *testing.T) {
	f := &Fake{}
	_, ok, err := f.FindPR(context.Background(), "nope", "<!-- hoist:id=nope -->")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestFakeRefusesSecondOpenPRForSameHead(t *testing.T) {
	f := &Fake{}
	spec := PRSpec{Title: "t", Body: "<!-- hoist:id=abc -->", Head: "hoist/env/abc", Base: "main"}
	if _, err := f.CreatePR(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreatePR(context.Background(), spec); err == nil {
		t.Fatal("expected the second CreatePR for the same open head branch to be refused")
	}
	if len(f.PRs()) != 1 {
		t.Fatalf("PRs() = %d, want exactly 1", len(f.PRs()))
	}
}

// TestFakeCommentsSortsRegardlessOfAddOrder is round-9's regression: Comments' own doc comment
// promises "oldest first", matching the real adaptor, but the implementation used to just
// return whatever order AddComment calls happened to populate — a test adding comments
// out-of-chronological-order (or a future concurrent AddComment) would silently get them back
// unsorted, which could mask a real "last valid comment wins" bug at the ApprovedStep layer
// (a fake that doesn't actually enforce ordering proves nothing about that logic).
func TestFakeCommentsSortsRegardlessOfAddOrder(t *testing.T) {
	f := &Fake{}
	pr, err := f.CreatePR(context.Background(), PRSpec{Head: "hoist/env/abc", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	newest := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	oldest := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	middle := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// Added deliberately out of order: newest first, then oldest, then middle.
	f.AddComment(pr.Number, Comment{ID: 3, Body: "newest", CreatedAt: newest})
	f.AddComment(pr.Number, Comment{ID: 1, Body: "oldest", CreatedAt: oldest})
	f.AddComment(pr.Number, Comment{ID: 2, Body: "middle", CreatedAt: middle})

	got, err := f.Comments(context.Background(), pr.Number, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("Comments = %+v, want 3", got)
	}
	if got[0].Body != "oldest" || got[1].Body != "middle" || got[2].Body != "newest" {
		t.Fatalf("Comments not sorted oldest-first despite being added out of order: %+v", got)
	}
}

func TestFakeHeadSHAFromTestSetup(t *testing.T) {
	f := &Fake{HeadSHAs: map[string]string{"hoist/env/abc": "deadbeef"}}
	pr, err := f.CreatePR(context.Background(), PRSpec{Head: "hoist/env/abc", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != "deadbeef" {
		t.Fatalf("HeadSHA = %q, want deadbeef", pr.HeadSHA)
	}
}
