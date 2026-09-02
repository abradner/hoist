package forge

import (
	"context"
	"testing"
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
