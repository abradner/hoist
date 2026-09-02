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

// claimAttempts bounds ClaimInFlight's retry loop against a claim file that keeps changing out
// from under it (a stale claim reclaimed, or a live one released, exactly while being examined):
// each retry only ever happens because the *previous* attempt observed real forward progress
// (the conflicting entry vanished or was proven stale and removed), never a blind spin, so a
// small bound is enough to guarantee termination without ever looping forever.
const claimAttempts = 4

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
				_ = os.Remove(path)
			}, nil
		}
		if !errors.Is(cerr, os.ErrExist) {
			return nil, fmt.Errorf("claiming %s/%s: %w", repoFullName, targetEnv, cerr)
		}
		switch examineExisting(path) {
		case existingVanished:
			// Released (or reclaimed by someone else) between our failed create and now — the
			// slot may be free again; loop back and try the create once more.
			continue
		case existingStale:
			// The owning process almost certainly died before ever writing real state (kill -9
			// between claiming and the first save). Remove and retry — if a third caller does
			// the same thing at the same instant, exactly one of the retries wins the create
			// below and the rest report an accurate conflict, never a double win.
			_ = os.Remove(path)
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
// path ever succeeds, every other gets an error satisfying errors.Is(err, os.ErrExist). It writes
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

func examineExisting(path string) existingClaimState {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return existingVanished
		}
		return existingUnknown
	}
	var rec claimRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return existingUnknown
	}
	if time.Since(rec.ClaimedAt) > claimStaleAfter {
		return existingStale
	}
	return existingLive
}
