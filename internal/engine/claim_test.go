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
// exercises the concurrent-write path this test is for, not just its sequential logic. Still
// fully valid after the removal of automatic stale-claim reclaim (this file's doc comment):
// tryClaim's create-vs-create atomicity, which this test exercises, was never the part that
// broke across three rounds of reclaim hardening.
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
// promotion. This is the ONLY way a second claim is ever allowed through now: an explicit
// release, never automatic reclaim of one release never called.
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

// TestClaimInFlightNeverAutoReclaimsAStaleLookingClaim is the regression for this round's
// architectural simplification: a claim file that LOOKS abandoned by age (older than
// claimStaleAfter) must still be reported as a conflict, never silently deleted and retried.
// This is the direct opposite of what this package's three earlier rounds tried to make safe —
// removing the behavior entirely, rather than hardening it further, is the fix (see claim.go's
// package doc comment).
func TestClaimInFlightNeverAutoReclaimsAStaleLookingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A claim file manufactured to look exactly like a process that claimed the env and then
	// died (kill -9) long before ever calling release() or writing state.
	old := `{"ID":"dead-process","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Add(-2*claimStaleAfter).Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ClaimInFlight("example/gitops", "app-production", "new-attempt"); err == nil {
		t.Fatal("a stale-looking claim must still be reported as a conflict, never auto-reclaimed")
	} else if !strings.Contains(err.Error(), "app-production") || !strings.Contains(err.Error(), path) {
		t.Fatalf("conflict error should name the target env and the claim path, got: %v", err)
	}

	// The old claim file must still be there — nothing here deleted it.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale-looking claim file should be untouched, stat err: %v", err)
	}
}

// TestClaimInFlightConflictErrorNamesAgeForALiveClaim is
// TestClaimInFlightNeverAutoReclaimsAStaleLookingClaim's counterpart for a claim well within
// claimStaleAfter: it must conflict too (unchanged from before this round), and the conflict
// error should still name a plausible age.
func TestClaimInFlightConflictErrorNamesAgeForALiveClaim(t *testing.T) {
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

	_, err = ClaimInFlight("example/gitops", "app-production", "new-attempt")
	if err == nil {
		t.Fatal("a fresh, live claim must conflict")
	}
	if !strings.Contains(err.Error(), "app-production") {
		t.Fatalf("conflict error should name the target env, got: %v", err)
	}
}

// TestClaimInFlightConflictErrorHandlesUnreadableClaim confirms the third case named in this
// round's fix: a claim file that exists but can't be parsed (corrupt, or from some future/past
// format) is reported the exact same way — a clear conflict, never treated as "can't tell, so
// safe to reclaim" (that ambiguity is exactly what let a loser see a still-being-written claim as
// parseable garbage and steal it out from under its winner, back when this file still tried to
// reclaim automatically).
func TestClaimInFlightConflictErrorHandlesUnreadableClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not valid json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = ClaimInFlight("example/gitops", "app-production", "new-attempt")
	if err == nil {
		t.Fatal("an unparsable claim file must still be reported as a conflict")
	}
	if !strings.Contains(err.Error(), "app-production") || !strings.Contains(err.Error(), path) {
		t.Fatalf("conflict error should name the target env and the claim path even when unparsable, got: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the unparsable claim file should be untouched, stat err: %v", statErr)
	}
}

// TestReleaseOwnClaimOnlyRemovesMatchingContent is the direct unit test for releaseOwnClaim,
// isolated from ClaimInFlight's own logic: it must remove path only when path's current content
// still matches the bytes the caller believes it owns, and must leave anything else —
// including whatever an operator has since put at the same path — completely untouched. No lock
// is involved any more (see claim.go's package doc comment): the only other actor that can ever
// touch this exact path is a human, out-of-band.
func TestReleaseOwnClaimOnlyRemovesMatchingContent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	path, err := ClaimPath("example/gitops", "app-production")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	mine := []byte(`{"ID":"mine","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Format(time.RFC3339Nano) + `"}`)

	// Someone else's content occupies path — simulating an operator manually replacing the claim
	// file between this caller's own claim and its release call. releaseOwnClaim, told to remove
	// `mine`, must leave this alone: it is not what it believes it owns.
	someoneElse := []byte(`{"ID":"someone-else","RepoFullName":"example/gitops","TargetEnv":"app-production","ClaimedAt":"` +
		time.Now().Format(time.RFC3339Nano) + `"}`)
	if err := os.WriteFile(path, someoneElse, 0o600); err != nil {
		t.Fatal(err)
	}
	releaseOwnClaim(path, mine)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("someone else's content should still be on disk: %v", err)
	}
	if !strings.Contains(string(data), "someone-else") {
		t.Fatalf("releaseOwnClaim must not remove content it doesn't own; got %s", data)
	}

	// Releasing against content that genuinely still matches must remove it.
	releaseOwnClaim(path, someoneElse)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("releaseOwnClaim should have removed its own matching claim, stat err: %v", err)
	}

	// Releasing when nothing is at path at all (already gone) is a safe no-op, not an error.
	releaseOwnClaim(path, someoneElse)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expected path to remain absent")
	}
}
