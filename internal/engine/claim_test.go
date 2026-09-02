package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestClaimInFlightRaceExactlyOneWins is the P2 regression for the TOCTOU race findInFlight
// cannot close on its own: two concurrent callers racing ClaimInFlight for the same
// (repoFullName, targetEnv) must never both succeed — exactly one must win, and the other must
// get a clear conflict error, never silently proceed alongside the winner. Run under -race (per
// AGENTS.md §6, every `go test` invocation in this repo's gate is), which is what actually
// exercises the concurrent-write path this test is for, not just its sequential logic.
func TestClaimInFlightRaceExactlyOneWins(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	const n = 8
	var wg sync.WaitGroup
	successes := make([]bool, n)
	errs := make([]error, n)
	releases := make([]func(), n)

	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait() // line every goroutine up so they race the same instant
			rel, err := ClaimInFlight("example/gitops", "app-production", "id-"+string(rune('a'+i)))
			if err == nil {
				successes[i] = true
				releases[i] = rel
			} else {
				errs[i] = err
			}
		}(i)
	}
	start.Done()
	wg.Wait()

	wins := 0
	for i := 0; i < n; i++ {
		if successes[i] {
			wins++
			if releases[i] != nil {
				releases[i]()
			}
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one winner among %d concurrent claimers, got %d", n, wins)
	}
	for i := 0; i < n; i++ {
		if successes[i] {
			continue
		}
		if errs[i] == nil {
			t.Fatalf("goroutine %d neither succeeded nor got an error", i)
		}
	}
}

// TestClaimInFlightReleaseAllowsAReclaim proves the intended lifecycle: once release() is
// called (standing in for "the promotion's first state file write landed"), the same
// repo/targetEnv can be claimed again — the claim's job is done, not held for the whole
// promotion.
func TestClaimInFlightReleaseAllowsAReclaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	release, err := ClaimInFlight("example/gitops", "app-production", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimInFlight("example/gitops", "app-production", "second"); err == nil {
		t.Fatal("expected a live claim to refuse a second claimant")
	}
	release()
	if release2, err := ClaimInFlight("example/gitops", "app-production", "second"); err != nil {
		t.Fatalf("expected released claim to allow a new one, got %v", err)
	} else {
		release2()
	}
}

// TestClaimInFlightDifferentEnvsDoNotConflict is the negative case: a claim on one target env
// must never block a claim on a different one, even for the same repo.
func TestClaimInFlightDifferentEnvsDoNotConflict(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	release1, err := ClaimInFlight("example/gitops", "app-staging", "id-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release1()
	release2, err := ClaimInFlight("example/gitops", "app-production", "id-2")
	if err != nil {
		t.Fatalf("a different target env must not conflict: %v", err)
	}
	release2()
}

// TestClaimInFlightStaleClaimIsReclaimed is the named adversary: a process that claimed an env
// and then died (kill -9, before ever writing real state) must not leave a permanent phantom
// lock — a claim file older than claimStaleAfter is reclaimable by a later caller.
func TestClaimInFlightStaleClaimIsReclaimed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Manufacture a claim file that is already older than claimStaleAfter, simulating a process
	// that claimed the env and then died before ever calling release() or writing state.
	stale := `{"ID":"dead-process","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Add(-2*claimStaleAfter).Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := ClaimInFlight("example/gitops", "app-production", "new-attempt")
	if err != nil {
		t.Fatalf("a stale claim should be reclaimable, got %v", err)
	}
	release()
}

// TestClaimInFlightFreshClaimIsNotReclaimed is TestClaimInFlightStaleClaimIsReclaimed's
// counterpart: a claim file well within claimStaleAfter must still refuse a second claimant —
// staleness recovery must not turn into "always allow a second claim".
func TestClaimInFlightFreshClaimIsNotReclaimed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := `{"ID":"live-process","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ClaimInFlight("example/gitops", "app-production", "new-attempt"); err == nil {
		t.Fatal("a fresh, live claim must not be reclaimed")
	}
}
