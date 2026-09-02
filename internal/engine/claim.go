package engine

// ClaimInFlight closes a TOCTOU race findInFlight (cmd/hoist/drive.go) cannot close on its own:
// findInFlight's one-in-flight-per-target-env check (AGENTS.md invariant 5) is read-only —
// engine.ListStates scans state files already on disk — so two `hoist promote` invocations
// racing the same repo+targetEnv can both observe "no conflicting state file yet" before either
// has written its own, and both proceed to open separate branches/PRs for the same env. A claim
// file closes exactly that window: ClaimInFlight atomically creates one for (repoFullName,
// targetEnv) the instant a caller has decided to proceed, so a second concurrent caller's own
// attempt fails and it gets a clear conflict error instead of silently racing through.
//
// The claim's job ends quickly, on purpose: once the promotion's own real state file has been
// durably written (its first successful SaveState call), engine.ListStates/findInFlight can see
// it and enforce invariant 5 for the rest of the promotion's life — which can be hours, waiting
// on CI or a human. The claim file only needs to survive the few seconds between "decided to
// proceed" and that first save, so release (returned by ClaimInFlight) removes it there, never at
// the end of the whole promotion. A claim a caller never released (the owning process died —
// kill -9 — before ever writing state) is recovered by age: claimStaleAfter names the bound past
// which a claim is treated as abandoned and reclaimed rather than blocking forever.
import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// claimStaleAfter bounds how long an unreleased claim file is trusted: comfortably longer than
// the slowest realistic gap between ClaimInFlight succeeding and Drive's first successful save
// (worktree creation, one signed commit, one push, one PR-create call — seconds, not minutes,
// under normal operation), so a live claim is never mistaken for abandoned, while a process
// killed before ever saving state does not leave a permanent phantom lock on its target env.
const claimStaleAfter = 5 * time.Minute

// claimStaleAfterOverride lets a test scale the staleness bound down to milliseconds so it can
// exercise the reclaim path without actually sleeping out the real 5-minute claimStaleAfter. Zero
// (its value everywhere outside a test) means "use claimStaleAfter"; a test sets it directly and
// must restore it to zero (e.g. via t.Cleanup) before returning, so it never leaks into an
// unrelated test — the same discipline testStaleClaimHook below already documents.
var claimStaleAfterOverride time.Duration

// claimStaleBound is the staleness bound examineExisting actually applies: claimStaleAfterOverride
// when a test has set one, claimStaleAfter otherwise.
func claimStaleBound() time.Duration {
	if claimStaleAfterOverride > 0 {
		return claimStaleAfterOverride
	}
	return claimStaleAfter
}

// claimAttempts bounds ClaimInFlight's retry loop against a claim file that keeps changing out
// from under it (a stale claim reclaimed, or a live one released, exactly while being examined):
// each retry only ever happens because the *previous* attempt observed real forward progress
// (the conflicting entry vanished or was proven stale and removed), never a blind spin, so a
// small bound is enough to guarantee termination without ever looping forever.
const claimAttempts = 4

// lockAttempts and lockRetryDelay bound acquireClaimLock's own retry loop against the lock
// directory itself (never the claim file): a caller that cannot os.Mkdir the lock dir because
// another caller currently holds it backs off lockRetryDelay and tries again, up to
// lockAttempts times. This is ordinary mutex-acquisition spinning, not the same "only retry on
// observed forward progress" reasoning claimAttempts documents — but the critical section the
// lock guards is at most a couple of file syscalls (a read and, sometimes, a remove — whether
// deciding staleness or deciding release), realistically microseconds, so
// lockAttempts*lockRetryDelay (a few seconds) is comfortably above any queue depth this process
// will ever see even with many goroutines racing the same claim.
const lockAttempts = 500
const lockRetryDelay = 5 * time.Millisecond

// lockStaleAfter bounds how long a lock directory is trusted to be genuinely held: holding
// acquireClaimLock's critical section is only ever a couple of file operations (never a
// network call, never anything that waits on another process), so it should complete in
// milliseconds — nothing like claimStaleAfter's 5-minute bound for the claim file itself, which
// exists to survive real waiting (CI, human approval). A lock dir older than this was almost
// certainly left behind by a process that died (kill -9) between os.Mkdir and its deferred
// os.Remove, and is reclaimed the same way a stale claim file is: judged abandoned by age, then
// removed so a crash inside the critical section cannot wedge every future caller for this
// target env forever.
const lockStaleAfter = 2 * time.Second

// claimRecord is a claim file's own content: enough to name what claimed it (for the conflict
// message) and to decide staleness (ClaimedAt). Never consulted for anything but that — like a
// promotion's own state file, it is an index, not evidence (AGENTS.md §4.1's shape applied here
// too).
type claimRecord struct {
	ID           string
	RepoFullName string
	TargetEnv    string
	ClaimedAt    time.Time
}

// claimFileName derives a claim file's name from repoFullName and targetEnv: a hash, not the raw
// strings, since repoFullName ("owner/name") contains a path separator and targetEnv is an
// operator-controlled namespace name — neither is safe to embed directly into a filename. The
// leading "." and ".claim" suffix keep it outside ListStates' own listing, which only considers
// entries ending in ".json".
func claimFileName(repoFullName, targetEnv string) string {
	sum := sha256.Sum256([]byte(repoFullName + "\x00" + targetEnv))
	return fmt.Sprintf(".%x.claim", sum[:16])
}

// ClaimPath is the claim file path for repoFullName/targetEnv, under the same
// $XDG_STATE_HOME/hoist/promotions/ directory StatePath keeps state files in.
func ClaimPath(repoFullName, targetEnv string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "promotions", claimFileName(repoFullName, targetEnv)), nil
}

// ClaimInFlight atomically claims repoFullName/targetEnv for promotion id. On success, release
// removes the claim; the caller must call it once the promotion's own state file has been
// durably written for the first time (or immediately, on any error path that never reaches that
// point) — see this file's package doc comment for why the claim's lifetime is deliberately
// short. On conflict (another live claim exists, or an existing one can't be read at all — fail
// closed rather than risk two winners), err is a plain, actionable error naming the target env.
//
// release does not remove path unconditionally (finding #4, round 4): a caller can be paused —
// GC, OS scheduling, a slow step between claiming and its first state save — for longer than
// claimStaleAfter. While paused, another caller can legitimately observe this caller's claim as
// stale, reclaim it under acquireClaimLock, and install its own fresh live claim at the same
// path. If this caller then resumed and had release remove path by name, it would delete that
// second caller's live claim, not its own already-gone one — falsely freeing the env while the
// second caller is still actively promoting, and opening the door to a third, concurrent winner.
// release instead goes through releaseOwnClaim, which acquires the same acquireClaimLock every
// other claim-file mutation (create-retry's stale examine, and now release) is serialized
// against, and only removes path if it still holds exactly the bytes this call originally wrote —
// never whatever happens to be there by the time release actually runs.
func ClaimInFlight(repoFullName, targetEnv, id string) (release func(), err error) {
	path, err := ClaimPath(repoFullName, targetEnv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("preparing to claim %s/%s: %w", repoFullName, targetEnv, err)
	}
	data, err := json.Marshal(claimRecord{ID: id, RepoFullName: repoFullName, TargetEnv: targetEnv, ClaimedAt: time.Now()})
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < claimAttempts; attempt++ {
		cerr := tryClaim(path, data)
		if cerr == nil {
			released := false
			return func() {
				if released {
					return
				}
				released = true
				releaseOwnClaim(path, data)
			}, nil
		}
		if !errors.Is(cerr, os.ErrExist) {
			return nil, fmt.Errorf("claiming %s/%s: %w", repoFullName, targetEnv, cerr)
		}
		state, lerr := examineAndMaybeReclaim(path)
		if lerr != nil {
			return nil, fmt.Errorf("claiming %s/%s: %w", repoFullName, targetEnv, lerr)
		}
		switch state {
		case existingVanished, existingStale:
			// existingVanished: released (or reclaimed by someone else) between our failed
			// create and now. existingStale: examineAndMaybeReclaim judged it stale and already
			// removed it, under the lock that serializes that decision against every other
			// concurrent caller (see acquireClaimLock's doc comment) — a bare
			// examine-then-remove here would reopen the exact double-reclaim race this file
			// exists to close, no matter how carefully the two steps compared bytes, because the
			// gap between them is still a gap. Either way the slot may be free now; loop back
			// and try the create once more.
			continue
		case existingLive:
			return nil, fmt.Errorf(
				"another `hoist promote` just claimed %s for %s (or one that hasn't finished starting up) — wait for it, or run `hoist promote` again once it either fails or writes its state file",
				targetEnv, repoFullName,
			)
		default: // existingUnknown: readable-but-unparsable, or unreadable for some other reason
			// (permissions). Never treat "can't tell" as "safe to reclaim" — that ambiguity is
			// exactly what let a loser see a still-being-written claim as parseable-garbage and
			// steal it out from under its winner; fail closed and let the operator look.
			return nil, fmt.Errorf(
				"claiming %s/%s: an existing claim file at %s could not be read; remove it manually only if you're sure no promotion is using it, then retry",
				repoFullName, targetEnv, path,
			)
		}
	}
	return nil, fmt.Errorf("claiming %s/%s: gave up after %d attempts against a claim file that kept changing underneath us", repoFullName, targetEnv, claimAttempts)
}

// tryClaim is the atomic exclusive-create primitive: exactly one concurrent caller for a given
// path ever succeeds, every other gets an error satisfying errors.Is(err, os.ErrExist). It
// deliberately never acquires acquireClaimLock itself: create-vs-create is already safe without
// one (os.Link's atomicity — POSIX guarantees exactly one linker for a given new name wins, no
// read-then-decide gap on this side at all), and create-vs-{stale-reclaim, release} is safe
// because those two are the only paths that ever remove a claim file, and both fully serialize
// their own read-judge-remove decision under acquireClaimLock before either one calls os.Remove.
// By the time ClaimInFlight's retry loop calls tryClaim again after an EEXIST, whatever decision
// examineAndMaybeReclaim made about the previous occupant (remove it, or leave it alone) has
// already completed and the lock has been released — so this retry's os.Link either finds path
// genuinely free (the previous occupant is gone, race-free) or genuinely still occupied (nothing
// this call is entitled to touch), never a state some other caller is mid-decision about. It writes
// data to a temp file in the same directory first, closes it (so the content is fully flushed),
// and only then links it into place — never a bare os.OpenFile(O_CREATE|O_EXCL) followed by a
// separate Write. That distinction is load-bearing: with OpenFile-then-Write, path exists (as an
// empty or partially-written file) for the whole window between the create and the write
// finishing, so a concurrent loser calling examineExisting on it can read empty/truncated
// content, fail to json.Unmarshal it, and (if that were treated as "can't tell, so stale") delete
// and reclaim it while the winner's own Write/Close is still in flight — the exact race this
// function is written to make impossible: os.Link only ever makes path visible once the temp
// file's full content is already durably written and closed, and link(2) itself fails atomically
// if path already exists, so there is never a moment where path exists with less than its full,
// valid content.
func tryClaim(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".claim-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Link (below, on success) gives the same inode a second name; the temp name itself is
	// always removed here, whether or not that link ever happens.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmpName, path)
}

// existingClaimState is examineExisting's answer about a claim file found at path when a create
// attempt against it failed with os.ErrExist.
type existingClaimState int

const (
	// existingLive: read and parsed fine, and it's within claimStaleAfter — a real conflict.
	existingLive existingClaimState = iota
	// existingStale: read and parsed fine, but older than claimStaleAfter — reclaimable.
	existingStale
	// existingVanished: gone by the time it was read (released, or reclaimed by another
	// concurrent caller) — worth trying the create again, not a conflict at all.
	existingVanished
	// existingUnknown: exists but couldn't be read or parsed for any other reason. Never treated
	// as reclaimable (see ClaimInFlight's doc comment on tryClaim for why that would reopen the
	// exact race this file exists to close).
	existingUnknown
)

// examineExisting inspects path when a create attempt against it failed with os.ErrExist,
// returning both the classification and the exact raw bytes read (nil unless state is
// existingStale, since that is the only case a caller needs them for — see removeIfUnchanged).
// A second, later call against the same path is not guaranteed to see the same content —
// callers that intend to act on an existingStale result (i.e. remove the file) must do so only
// while holding the lock acquireClaimLock provides (see examineAndMaybeReclaim); calling
// this alone, unlocked, and then removing based on what it returned is exactly the TOCTOU this
// package used to have and no longer does.
func examineExisting(path string) (state existingClaimState, raw []byte) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return existingVanished, nil
		}
		return existingUnknown, nil
	}
	var rec claimRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return existingUnknown, nil
	}
	if time.Since(rec.ClaimedAt) > claimStaleBound() {
		return existingStale, data
	}
	return existingLive, nil
}

// removeIfUnchanged removes path only if its content still byte-for-byte matches expected. This
// used to be this package's *only* defense against the double-reclaim race (a bare
// compare-then-remove, with the compare and the remove as two separate syscalls) — and it still
// left a real gap: two concurrent callers could each read the same stale bytes, and whichever
// one's remove-and-reclaim finished first would leave the second one comparing its own
// (now-stale) snapshot against content the first caller had since replaced, off by exactly the
// window between "read to compare" and "remove". Closing that gap needed a real mutual-exclusion
// primitive, not a tighter comparison — see acquireClaimLock and examineAndMaybeReclaim, which
// now serialize the whole read-judge-remove decision so at most one caller is ever inside it at a
// time. removeIfUnchanged is kept as defense in depth *underneath* that lock (a caller holding the
// lock still only removes the exact bytes it just read, in case something outside ClaimInFlight's
// own protocol touched the file), not as the race-closing mechanism itself. It is the shared
// primitive both examineAndMaybeReclaim's stale-reclaim and releaseOwnClaim's release now build
// on — the one place any caller's claim-file mutation actually removes something, always
// compare-gated, always called with the lock held. Reports whether it actually removed anything,
// for the tests that exercise it directly.
func removeIfUnchanged(path string, expected []byte) bool {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, expected) {
		return false
	}
	if testStaleClaimHook != nil {
		testStaleClaimHook()
	}
	return os.Remove(path) == nil
}

// testStaleClaimHook, when set by a test, is called the instant removeIfUnchanged has confirmed
// path's content still matches expected but before its own os.Remove executes — the one
// deterministic way to force the exact interleaving finding #1 (round 3) describes: another
// caller's entire read-compare-remove-reclaim cycle landing, unseen, in the gap between this
// compare succeeding and this remove actually running. Real goroutine scheduling essentially
// never lands in a gap this narrow by chance, which is exactly why round 2's own regression test
// for this same finding could pass while the bug remained — it never forced the interleaving,
// only hoped for it. Always nil outside a test; a test that sets this must restore it to nil
// (e.g. via t.Cleanup) before returning, so it never leaks into an unrelated test.
var testStaleClaimHook func()

// claimLockPath derives the lock directory path for the claim file at path: a fixed,
// deterministic name (path + ".lock") every concurrent caller derives the same way, so
// os.Mkdir's atomicity on that one path is the actual serialization primitive — POSIX guarantees
// exactly one caller's mkdir on a given path succeeds when several race it, with every other
// caller getting EEXIST.
func claimLockPath(path string) string {
	return path + ".lock"
}

// acquireClaimLock is the ONE mutual-exclusion boundary every claim-file mutation at path that
// reads-then-decides goes through: examineAndMaybeReclaim's "is this claim stale, and if so
// reclaim it" decision, and releaseOwnClaim's "is this still my own claim, and if so remove it"
// decision. (Plain creation, tryClaim, deliberately does not — see tryClaim's own doc comment for
// why os.Link's atomicity already makes create-vs-create safe without it, and why
// create-vs-{reclaim, release} needs nothing more than this lock already guaranteeing those two
// finish their own remove-or-not decision before releasing it.)
//
// This file went through three independently-reasoned-about mutation paths before landing here
// (round 2 hardened create via tempfile+link; round 3 hardened stale-reclaim via this same lock,
// then named acquireStaleClaimLock; round 4 found release's own unconditional os.Remove was the
// last ungated path) — generalizing this lock to cover all of them, rather than adding a fourth
// ad-hoc compare-then-remove variant to release, is the point: there is exactly one place in this
// package that ever decides "remove what's at path", and it is always reached through here.
//
// A caller that fails to os.Mkdir the lock dir (another caller currently holds it) backs off and
// retries, bounded by lockAttempts/lockRetryDelay — it never proceeds to examine, reclaim, or
// release a claim without holding this lock first. A lock dir found older than lockStaleAfter is
// judged abandoned (the only way to hold this lock for that long is a process that died — kill
// -9 — mid-critical-section, since the section itself is only ever a couple of file operations)
// and is removed so a crash here cannot wedge every future caller for this target env forever;
// after removing it this call simply loops back and retries the os.Mkdir like any other contended
// attempt, rather than assuming it now owns the lock.
//
// This is what actually closes the races compare-and-remove alone could not: by the time any
// second caller enters its own examine (whether deciding staleness, or deciding whether to
// release), the first caller's full read-judge-remove-or-not sequence has already completed and
// released — so the second caller's examine reflects whatever is really at path *now* (a fresh
// live claim it must not touch, or genuinely still-absent state), never a snapshot taken before
// the first caller's own write landed.
func acquireClaimLock(path string) (release func(), err error) {
	lockPath := claimLockPath(path)
	for attempt := 0; attempt < lockAttempts; attempt++ {
		if mkErr := os.Mkdir(lockPath, 0o700); mkErr == nil {
			released := false
			return func() {
				if released {
					return
				}
				released = true
				_ = os.Remove(lockPath)
			}, nil
		} else if !errors.Is(mkErr, os.ErrExist) {
			return nil, fmt.Errorf("acquiring stale-claim lock: %w", mkErr)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil {
			if time.Since(info.ModTime()) > lockStaleAfter {
				// Best-effort: if this fails (someone else already removed it, or is about to),
				// just fall through to the retry below and let the next os.Mkdir decide.
				_ = os.Remove(lockPath)
				continue
			}
		}
		time.Sleep(lockRetryDelay)
	}
	return nil, fmt.Errorf("gave up waiting %s for the stale-claim lock at %s", time.Duration(lockAttempts)*lockRetryDelay, lockPath)
}

// examineAndMaybeReclaim is ClaimInFlight's entire "is this claim stale, and if so reclaim it"
// decision, executed under acquireClaimLock so at most one concurrent caller for path is ever
// inside it (release's own decision, releaseOwnClaim, is serialized against this same lock, so
// the two can never interleave either). It returns the state a fresh examineExisting found while
// holding the lock (which may differ from whatever an earlier, unlocked probe saw — that earlier
// probe is never trusted to decide anything, only this one is) and, when that state is
// existingStale, has already removed the claim file before returning — the caller has nothing
// left to do for that case but retry its own create.
func examineAndMaybeReclaim(path string) (existingClaimState, error) {
	release, err := acquireClaimLock(path)
	if err != nil {
		return existingUnknown, err
	}
	defer release()
	state, raw := examineExisting(path)
	if state == existingStale {
		removeIfUnchanged(path, raw)
	}
	return state, nil
}

// releaseOwnClaim is what ClaimInFlight's returned release closure actually calls (finding #4,
// round 4): never a bare os.Remove(path) by name, which cannot tell "the claim I'm removing is
// still mine" from "someone else already reclaimed this path as stale and installed their own
// live claim here" — the exact gap that let a paused caller's belated release delete a different,
// still-active caller's claim out from under it.
//
// It acquires acquireClaimLock — the same lock examineAndMaybeReclaim's stale-reclaim decision
// uses — before touching path at all, so this call can never interleave with a concurrent
// reclaim's own read-judge-remove sequence: either this release completes first (the normal,
// fast-path case — nothing has gone stale yet, path still holds exactly data, and it comes down
// cleanly), or a reclaim completes first and this release, once it gets the lock, reads back
// content that no longer matches data and does nothing. Either ordering is correct; what
// removeIfUnchanged's compare inside the lock rules out is a third outcome — this release seeing
// stale bytes, or no bytes at all, and removing whatever a reclaimer installed in between anyway.
//
// If the lock itself cannot be acquired (lockAttempts exhausted under extreme, sustained
// contention — vanishingly rare; each holder's own critical section is only ever a couple of file
// operations), this is a best-effort no-op rather than a fallback to the unconditional remove
// this function exists to replace: leaving this claim behind in that case is recovered the same
// way any abandoned claim is, by staleness, once claimStaleAfter passes for whichever caller's
// data is actually still there.
func releaseOwnClaim(path string, data []byte) {
	release, err := acquireClaimLock(path)
	if err != nil {
		return
	}
	defer release()
	removeIfUnchanged(path, data)
}
