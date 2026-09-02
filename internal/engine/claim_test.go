package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// TestClaimInFlightManyRacersAgainstOneStaleClaimExactlyOneWins is the genuine, adversarial
// regression for finding #1 (round 3): round 2's own "fix" for this exact scenario
// (removeIfUnchanged, a two-syscall compare-then-remove) still let two concurrent callers who
// had each independently read the *same* stale claim's bytes both conclude they were entitled to
// remove-and-reclaim it, because the read-to-decide-stale and the remove were still two separate
// operations with a window between them for another racer's whole remove-and-reclaim cycle to
// land unseen. This test does not manufacture that interleaving by hand (the earlier,
// now-replaced sequential test — and finding #1's own round-2 "fix" — both did exactly that and
// both still hid the bug): it launches many real goroutines, all racing ClaimInFlight for the
// exact same repo/env against one pre-seeded, genuinely stale claim, released to run
// simultaneously off one barrier, under -race. Exactly one may ever install a fresh live claim;
// every loser must get a real error, never a silent second success.
func TestClaimInFlightManyRacersAgainstOneStaleClaimExactlyOneWins(t *testing.T) {
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

	const n = 40
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
			start.Wait() // line every goroutine up so they all race the exact same stale claim
			rel, err := ClaimInFlight("example/gitops", "app-production", "racer-"+string(rune('a'+i%26))+"-"+string(rune('0'+i/26)))
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
		t.Fatalf("expected exactly one winner reclaiming the one stale claim among %d racers, got %d", n, wins)
	}
	for i := 0; i < n; i++ {
		if successes[i] {
			continue
		}
		if errs[i] == nil {
			t.Fatalf("racer %d neither succeeded nor got an error", i)
		}
	}

	// No stray lock directory should survive a clean run: every acquirer released it.
	if _, statErr := os.Stat(staleClaimLockPath(path)); !os.IsNotExist(statErr) {
		t.Fatalf("expected the stale-claim lock directory to be gone after all racers finished, stat err: %v", statErr)
	}
}

// TestClaimInFlightStaleReclaimForcedInterleavingNeverDoubleWins is the deterministic companion
// to TestClaimInFlightManyRacersAgainstOneStaleClaimExactlyOneWins: rather than hoping real
// goroutine scheduling lands in the nanosecond-scale gap finding #1 (round 3) describes, it uses
// testStaleClaimHook to force the exact adversarial interleaving by hand, through the real,
// public ClaimInFlight end to end (never the package-private helpers driven directly, which is
// exactly what let round 2's own regression test for this same finding pass while the bug
// remained — a test that forces one racer's full cycle to complete strictly *before* the other
// even starts proves nothing about interleaving, only about sequencing).
//
// Sequence forced here: racer A's tryClaim fails against the pre-seeded stale claim, it judges
// the claim stale, and removeIfUnchanged confirms the content still matches — then, via the
// hook, A pauses with that compare already done but its own os.Remove not yet called. While A is
// paused, racer B runs its own full claim attempt against the very same still-present stale
// file: B also judges it stale, removes it, and installs a brand-new live claim via tryClaim —
// entirely unseen by A, which is still sitting between its own compare and its own remove. A is
// then released and its os.Remove finally executes. If the lock did not serialize A and B (the
// bug this test regresses), A's stale os.Remove(path) silently deletes B's brand-new live claim
// by name, and A itself goes on to win a second, illegitimate claim — two winners. With the lock
// in place, B can never even reach its own examine while A holds it: B spins on the lock
// directory instead, and only proceeds once A has released it, by which point A's own claim (or
// A's failure) is already the final, single word on who won.
func TestClaimInFlightStaleReclaimForcedInterleavingNeverDoubleWins(t *testing.T) {
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

	aPausedBeforeRemove := make(chan struct{})
	letAResume := make(chan struct{})
	var pauseClaimed atomic.Bool
	testStaleClaimHook = func() {
		// Only racer A (whichever caller first flips this false->true) ever pauses here; every
		// later call into this same hook — racer B's, and (against the fix) any of A's own later
		// retries — must sail straight through with no wait at all. sync.Once is deliberately
		// NOT used for this: Once.Do serializes every caller on the same internal lock until the
		// first call's function returns, so a second goroutine's Do call would itself block for
		// the exact same duration as the first — indistinguishable, from B's own goroutine's
		// point of view, from B being genuinely paused too, which defeats the entire point of
		// this hook (this was caught by hand: an earlier version of this test used sync.Once
		// here and deadlocked outright, because the test's own bDone channel waited for B to
		// finish while B was itself unknowingly blocked on Once's internal mutex, not on
		// anything this test controls).
		if pauseClaimed.CompareAndSwap(false, true) {
			close(aPausedBeforeRemove)
			<-letAResume
		}
	}
	t.Cleanup(func() { testStaleClaimHook = nil })

	var wg sync.WaitGroup
	var relA, relB func()
	var errA, errB error

	wg.Add(1)
	go func() {
		defer wg.Done()
		relA, errA = ClaimInFlight("example/gitops", "app-production", "racer-a")
	}()

	<-aPausedBeforeRemove // A has confirmed the claim still looks stale and is paused right before removing it

	wg.Add(1)
	go func() {
		defer wg.Done()
		relB, errB = ClaimInFlight("example/gitops", "app-production", "racer-b")
	}()
	// Give B a real window to act while A is paused. Against the mutant (no lock), B's hook call
	// is a genuine no-op (see above) and its whole judge-and-reclaim cycle completes in
	// microseconds, so this is enormously generous; against the fix, B cannot even finish its own
	// examine while A holds the lock, so B just spins on the lock directory for this entire
	// window and resolves once A releases below — either way, this is not a race the test can
	// lose by waiting too long, only one it could fail to provoke by not waiting long enough.
	time.Sleep(50 * time.Millisecond)
	close(letAResume)
	wg.Wait()

	wins := 0
	var releases []func()
	if errA == nil {
		wins++
		releases = append(releases, relA)
	}
	if errB == nil {
		wins++
		releases = append(releases, relB)
	}
	for _, r := range releases {
		r()
	}
	if wins != 1 {
		t.Fatalf("expected exactly one of the forced-interleaving racers to win a live claim, got %d (errA=%v errB=%v)", wins, errA, errB)
	}
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
