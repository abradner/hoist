package engine

import (
	"os"
	"path/filepath"
	"strings"
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

// TestClaimInFlightStaleReclaimRaceNeverDoubleWins is the P2 regression for finding #3: two
// concurrent callers that both examine the exact same genuinely-stale claim must not both end
// up believing they hold the sole live claim. The bug this proves fixed: examineExisting's
// classification and a later remove are two separate syscalls with a window between them, and a
// bare os.Remove(path) in that window removes whatever is CURRENTLY at path by name — which, if
// a second racer's own remove-then-reclaim cycle has already completed, is that racer's
// brand-new LIVE claim, not the stale one this call examined. Run under -race (per AGENTS.md
// §6, every `go test` invocation in this repo's gate is): both "racers" are real goroutines,
// with a channel barrier forcing the exact interleaving the naive unconditional-remove code
// could not tell apart from "still the same stale claim I looked at" — racer A completes a full
// remove-and-reclaim cycle strictly before racer B's own removeIfUnchanged call runs.
func TestClaimInFlightStaleReclaimRaceNeverDoubleWins(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"ID":"dead-process","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Add(-2*claimStaleAfter).Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both racers independently examine the exact same stale claim before either reclaims it —
	// exactly what two concurrent ClaimInFlight callers hitting the same failed tryClaim would
	// each do.
	stateA, rawA := examineExisting(path)
	stateB, rawB := examineExisting(path)
	if stateA != existingStale || stateB != existingStale {
		t.Fatalf("setup: expected both racers to observe existingStale, got A=%v B=%v", stateA, stateB)
	}

	aDone := make(chan struct{})
	var aRemoved, aClaimed, bRemoved bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(aDone)
		aRemoved = removeIfUnchanged(path, rawA)
		if aRemoved {
			fresh := `{"ID":"A-fresh-live-claim","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
				time.Now().Format(time.RFC3339Nano) + `"}`
			aClaimed = tryClaim(path, []byte(fresh)) == nil
		}
	}()
	go func() {
		defer wg.Done()
		<-aDone // force B's compare-and-remove to run only once A's full cycle has completed
		bRemoved = removeIfUnchanged(path, rawB)
	}()
	wg.Wait()

	if !aRemoved || !aClaimed {
		t.Fatalf("racer A should have won the remove and installed its own fresh live claim: aRemoved=%v aClaimed=%v", aRemoved, aClaimed)
	}
	if bRemoved {
		t.Fatal("racer B must not remove a claim that changed since it examined it — this is the exact double-winner race finding #3 describes")
	}

	// The surviving claim on disk must be A's fresh one, untouched by B.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("A's claim should still be on disk: %v", err)
	}
	if !strings.Contains(string(data), "A-fresh-live-claim") {
		t.Fatalf("expected A's fresh claim content, got %s", data)
	}

	// Integration-level confirmation: the full public ClaimInFlight loop, run against this same
	// now-live claim, correctly reports a real conflict rather than a second win.
	if _, err := ClaimInFlight("example/gitops", "app-production", "a-third-caller"); err == nil {
		t.Fatal("a third caller must see A's surviving live claim as a genuine conflict")
	}
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
