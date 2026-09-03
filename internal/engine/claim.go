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
// the end of the whole promotion.
//
// # History: automatic stale-claim reclaim, removed
//
// Earlier drafts of this file tried to make a stuck claim self-heal: if a claim's owner is
// killed (`kill -9`) before ever writing state, its claim file would otherwise block that target
// env forever, so each draft added a way to notice a claim looked old and reclaim it
// automatically. Three drafts, three different races, each one closing the previous draft's gap
// by adding a new mechanism that turned out to have the exact same class of gap one layer up:
// a bare "create, and on EEXIST check age and maybe delete-then-retry" was TOCTOU between the
// age check and the delete; a tempfile+link create plus a compare-then-remove reclaim closed the
// create race but left two concurrent reclaimers able to both decide the same stale claim was
// theirs to remove; and serializing reclaim+release under a lock directory closed *that* gap but
// the lock directory's own acquisition ("is this lock dir stale, if so remove and recreate") was
// itself a non-atomic read-judge-remove sequence with no lock protecting it — the identical bug,
// one level up.
//
// The lesson each round was actually teaching: automatic reclaim of a possibly-abandoned claim,
// under concurrency, keeps reintroducing this bug class at whatever layer is asked to make it
// safe, because "notice it looks stale" and "remove it" can never be made atomic together without
// something serializing every caller who might race that same decision — and each fix to that
// serialization primitive was itself another caller racing another decision. So this file no
// longer tries: **automatic reclaim is removed.** A claim conflict — whether the existing claim
// looks fresh or looks old — is always reported to the operator as a clear, actionable error
// naming the target env and the claim's age; a genuinely abandoned claim (rare: it only happens
// if a process is killed in the few-seconds window between claiming and its first durable state
// save) now requires a human to notice and delete the claim file, rather than code trying to
// safely auto-heal it under concurrent contention. tryClaim's atomic create was always correct
// through every draft above and is unchanged; only the reclaim-on-conflict behavior is gone.
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

// claimStaleAfter is not used to trigger any automatic action (see this file's package doc
// comment) — it only tunes the conflict error's phrasing: a claim younger than this is described
// as possibly still starting up, one older is described as old enough to be worth checking on.
// Purely cosmetic; changing it never changes what ClaimInFlight does.
const claimStaleAfter = 5 * time.Minute

// claimRecord is a claim file's own content: enough to name what claimed it (for the conflict
// message) and to show its age. Never consulted for anything but that — like a promotion's own
// state file, it is an index, not evidence (AGENTS.md §4.1's shape applied here too).
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
// short.
//
// On conflict — a claim already exists at this path, whether it looks live or looks stale by
// age, or can't be read/parsed at all — err is always a plain, actionable error naming the
// target env and the existing claim's age (or that its age couldn't be determined). This file no
// longer distinguishes those cases by auto-deleting and retrying (see the package doc comment for
// why); every one of them is the same answer: a human needs to look, and either wait or remove
// the claim file manually.
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

	if cerr := tryClaim(path, data); cerr != nil {
		if !errors.Is(cerr, os.ErrExist) {
			return nil, fmt.Errorf("claiming %s/%s: %w", repoFullName, targetEnv, cerr)
		}
		return nil, claimConflictError(repoFullName, targetEnv, path)
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		releaseOwnClaim(path, data)
	}, nil
}

// claimConflictError reports the claim already at path in one clear, actionable message: it
// names the target env and repo, and — best-effort, since the file may be unreadable or from a
// version that can't parse — the claim's age. It never treats "can't tell" as license to reclaim
// (see this file's package doc comment); every path through here ends in the same kind of answer,
// only the age description differs.
func claimConflictError(repoFullName, targetEnv, path string) error {
	age := "an unknown age"
	hint := "or one that hasn't finished starting up"
	if data, rerr := os.ReadFile(path); rerr == nil {
		var rec claimRecord
		if jerr := json.Unmarshal(data, &rec); jerr == nil {
			d := time.Since(rec.ClaimedAt)
			age = d.Round(time.Second).String()
			if d > claimStaleAfter {
				hint = "old enough that it may be abandoned — hoist no longer guesses that automatically"
			}
		}
	}
	return fmt.Errorf(
		"another `hoist promote` claimed %s for %s %s ago (%s) — wait for it, or if you're sure it's dead (the process was killed), remove the claim file at %s manually and retry",
		targetEnv, repoFullName, age, hint, path,
	)
}

// tryClaim is the atomic exclusive-create primitive: exactly one concurrent caller for a given
// path ever succeeds, every other gets an error satisfying errors.Is(err, os.ErrExist). It
// writes data to a temp file in the same directory first, closes it (so the content is fully
// flushed), and only then links it into place — never a bare os.OpenFile(O_CREATE|O_EXCL)
// followed by a separate Write. That distinction is load-bearing: with OpenFile-then-Write, path
// exists (as an empty or partially-written file) for the whole window between the create and the
// write finishing, so a concurrent loser reading it in that window could see empty/truncated
// content; os.Link only ever makes path visible once the temp file's full content is already
// durably written and closed, and link(2) itself fails atomically if path already exists, so
// there is never a moment where path exists with less than its full, valid content.
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

// releaseOwnClaim is what ClaimInFlight's returned release closure actually calls. There is no
// longer any other automated actor that can remove or replace this exact claim file — no
// automatic reclaim (see the package doc comment) — so the only things that can ever touch path
// are this call and an operator manually deleting it out-of-band. No lock is needed: a lock's
// entire job would be serializing this against a concurrent reclaimer, and there isn't one
// anymore.
//
// It still only removes path if its content still byte-for-byte matches data, and treats path
// already being gone as a no-op rather than an error — cheap insurance against an operator's
// manual intervention (deleting the claim file themselves, or replacing it) landing at an
// inconvenient moment, not a race-closing mechanism (there is no race left to close).
func releaseOwnClaim(path string, data []byte) {
	current, err := os.ReadFile(path)
	if err != nil {
		return // already gone (operator cleared it, or it was never left behind) — nothing to do.
	}
	if !bytes.Equal(current, data) {
		return // no longer what this caller wrote — an operator's own edit; leave it alone.
	}
	_ = os.Remove(path)
}
