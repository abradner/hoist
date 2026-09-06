package migrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/forge"
)

func TestLiveAgeBlamesRequestedLines(t *testing.T) {
	when := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	f := &forge.Fake{Blames: map[string]map[int]forge.LineOrigin{
		"head f.yaml": {21: {SHA: "1111", Date: when}},
	}}
	got, err := Blamer{Forge: f}.LiveAge(context.Background(), LiveAgeIn{Ref: "head", FallbackRef: "main", Path: "f.yaml", Lines: []int{21, 99}})
	if err != nil {
		t.Fatal(err)
	}
	if got[21].SHA != "1111" || !got[21].Since.Equal(when) || got[21].Approximate {
		t.Fatalf("got = %+v", got)
	}
	if _, ok := got[99]; ok {
		t.Fatal("line 99 must be absent")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %v; the fallback must not be tried when the ref resolves", f.Calls)
	}
}

func TestLiveAgeFallsBackToBaseAndMarksApproximate(t *testing.T) {
	f := &forge.Fake{Blames: map[string]map[int]forge.LineOrigin{
		"main f.yaml": {21: {SHA: "2222", Date: time.Now()}},
	}}
	// BlameErr is global on the fake, so drive the unknown-ref path through a wrapper that
	// fails only for the unpushed ref.
	got, err := Blamer{Forge: unknownFor{Forge: f, ref: "unpushed"}}.LiveAge(context.Background(), LiveAgeIn{Ref: "unpushed", FallbackRef: "main", Path: "f.yaml", Lines: []int{21}})
	if err != nil {
		t.Fatal(err)
	}
	if got[21].SHA != "2222" || !got[21].Approximate {
		t.Fatalf("got = %+v; want the fallback's answer marked approximate", got)
	}
}

func TestLiveAgeOtherErrorsPropagate(t *testing.T) {
	f := &forge.Fake{BlameErr: errors.New("HTTP 403")}
	_, err := Blamer{Forge: f}.LiveAge(context.Background(), LiveAgeIn{Ref: "head", FallbackRef: "main", Path: "f.yaml", Lines: []int{1}})
	if err == nil {
		t.Fatal("expected the forge's error")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %v; a non-unknown-ref error must not fall back", f.Calls)
	}
}

type unknownFor struct {
	forge.Forge
	ref string
}

func (u unknownFor) BlameLines(ctx context.Context, ref, path string, lines []int) (map[int]forge.LineOrigin, error) {
	if ref == u.ref {
		return nil, forge.ErrUnknownRef
	}
	return u.Forge.BlameLines(ctx, ref, path, lines)
}
