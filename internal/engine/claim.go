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
		state, raw := examineExisting(path)
		switch state {
		case existingVanished:
			// Released (or reclaimed by someone else) between our failed create and now — the
			// slot may be free again; loop back and try the create once more.
			continue
		case existingStale:
			// The owning process almost certainly died before ever writing real state (kill -9
			// between claiming and the first save) — remove and retry. removeIfUnchanged (not a
			// bare os.Remove) closes the reclaim-from-stale race this comment used to describe
			// as already closed but wasn't: two concurrent callers can both classify the same
			// claim as stale, and a bare os.Remove(path) here removes whatever is CURRENTLY at
			// path by name — which, if the other caller's own remove-then-tryClaim cycle already
			// completed, is that caller's brand-new LIVE claim, not the stale one this call
			// examined. removeIfUnchanged only removes path if its content still byte-for-byte
			// matches what examineExisting just read; if it doesn't (someone else already
			// changed it), this call removes nothing and simply loops back to re-examine fresh
			// state on the next iteration — never a second, uninformed delete.
			removeIfUnchanged(path, raw)
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

// examineExisting inspects path when a create attempt against it failed with os.ErrExist,
// returning both the classification and the exact raw bytes read (nil unless state is
// existingStale, since that is the only case a caller needs them for — see
// removeIfUnchanged). A second, later call against the same path is not guaranteed to see the
// same content: that gap is exactly what removeIfUnchanged closes for the stale-reclaim path.
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
	if time.Since(rec.ClaimedAt) > claimStaleAfter {
		return existingStale, data
	}
	return existingLive, nil
}

// removeIfUnchanged removes path only if its content still byte-for-byte matches expected —
// the compare half of a compare-and-remove for ClaimInFlight's stale-reclaim path. Without
// this, two concurrent callers that both examined the same stale claim and both decided to
// remove it could interleave as: caller A removes the stale file and installs its own fresh
// live claim at path; caller B, still acting on its own earlier (now outdated) examination,
// then calls a bare os.Remove(path) — which deletes A's brand-new live claim by name alone,
// with no idea it is no longer the file B looked at. Reading path again immediately before
// removing and comparing byte-for-byte against what was read when the caller decided "stale"
// ensures a caller only ever removes the exact claim it examined: if the content changed (or
// the file is already gone), this is a no-op and the caller must loop back to re-examine fresh
// state rather than assume its remove succeeded. Reports whether it actually removed anything
// — ClaimInFlight's own loop doesn't need the answer (it re-examines fresh either way), but
// the race test that proves this closes the reclaim-from-stale race does.
func removeIfUnchanged(path string, expected []byte) bool {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, expected) {
		return false
	}
	return os.Remove(path) == nil
}
