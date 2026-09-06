package migrate

import (
	"context"
	"errors"
	"testing"
)

func TestCacheDeltaFillsOnceAndNeverCachesErrors(t *testing.T) {
	var c Cache
	key := DeltaKey{AppRepo: "me/app", FromSHA: shaA, ToSHA: shaB, Prefix: "db/migrate/"}
	calls := 0
	failing := func(context.Context) (Delta, error) { calls++; return Delta{}, errors.New("rate limited") }
	if _, err := c.Delta(context.Background(), key, failing); err == nil {
		t.Fatal("expected the fill's error")
	}
	if _, err := c.Delta(context.Background(), key, failing); err == nil {
		t.Fatal("an error must be retried, not served from the cache")
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	calls = 0
	ok := func(context.Context) (Delta, error) { calls++; return Delta{Total: 3}, nil }
	for i := 0; i < 3; i++ {
		d, err := c.Delta(context.Background(), key, ok)
		if err != nil || d.Total != 3 {
			t.Fatalf("d=%+v err=%v", d, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d; want one fill for three reads", calls)
	}
	// A different prefix is a different delta.
	other := key
	other.Prefix = "migrations/"
	if _, err := c.Delta(context.Background(), other, ok); err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestCacheRevisionCachesUnknownAsAnAnswer(t *testing.T) {
	var c Cache
	calls := 0
	fill := func(context.Context) (Revision, error) { calls++; return Revision{Source: SourceUnknown}, nil }
	for i := 0; i < 2; i++ {
		if _, err := c.Revision(context.Background(), RevisionKey{AppRepo: "me/app", Ref: "x:v9"}, fill); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d; unknown is an answer and is cached", calls)
	}
}
