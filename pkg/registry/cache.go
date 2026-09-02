package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cacheDigestRE is the same "sha256 + 64 lowercase hex" shape pkg/image.Ref accepts —
// duplicated rather than imported: pkg/image has no exported "is this a well-formed digest"
// function to call, only Ref.Validate (which also demands a non-empty Repo, which the cache
// deliberately never keys on — see this file's package doc note below).
var cacheDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// cacheDir is $XDG_CACHE_HOME/hoist/registry, else ~/.cache/hoist/registry — the same XDG
// rule internal/engine.CacheDir and internal/config.DefaultPath already state for
// $XDG_CACHE_HOME/hoist. pkg/ never imports internal/ (AGENTS.md §4.3), so this is its own
// copy of that same handful of lines rather than a shared helper.
//
// A note on why this isn't github.com/adrg/xdg: the M6 brief that asked for this cache said
// adrg/xdg was "already a dependency" — it is not (go.mod has no such module, and this repo
// already has its own hand-rolled XDG resolution in three places: CacheDir/StateDir above and
// config.DefaultPath). AGENTS.md's hard constraints refuse a new dependency without a
// specific, stated reason; "the brief assumed one was already vendored" is not that reason
// when an established, working, dependency-free pattern already exists for exactly this in
// the same codebase. This function mirrors that pattern instead of introducing a second way
// to compute the same path.
func cacheDir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "hoist", "registry"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating registry cache dir: %w", err)
	}
	return filepath.Join(home, ".cache", "hoist", "registry"), nil
}

// cacheFile is the one JSON file digest's entry lives at. AGENTS.md's M6 brief invariant 2
// states the reasoning this package leans on throughout cache.go: a digest is content-
// addressed, so ImageMeta for one never changes once written — a digest-keyed cache entry
// therefore never goes stale and needs no invalidation policy, no TTL, no ETag, nothing but
// "does a readable, well-formed entry exist for this exact digest". Keying on the digest
// alone (never the repo) is deliberate too: the same image content pushed under two repo
// paths — a mirror, a rename — is still the same bytes, and there is no reason to fetch or
// store its Created/Labels twice.
//
// The digest's ':' is replaced with '-' for the filename: colons are refused in a path
// component on at least one platform this tool might one day run on, even though every
// platform it runs on today (AGENTS.md §6's darwin/linux dev loop) tolerates it happily.
func cacheFile(digest string) (string, error) {
	if !cacheDigestRE.MatchString(digest) {
		return "", fmt.Errorf("registry: cache: %q is not a well-formed digest", digest)
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strings.Replace(digest, ":", "-", 1)+".json"), nil
}

// cachedMeta is cacheFile's on-disk shape: a plain struct decoded with DisallowUnknownFields
// (this repo's "one typed struct, decode with KnownFields" discipline, AGENTS.md §4.9,
// applied here even though this is a cache file rather than the config file that rule was
// written for) so a field from a newer or older hoist version reads as "not this shape" —
// which loadCache treats exactly like a missing or corrupt file: a cache miss, never a hard
// error (invariant 2 again: there is nothing to invalidate carefully, only "usable or not").
type cachedMeta struct {
	Digest  string            `json:"digest"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// loadCache reads digest's cache entry. Anything at all wrong with it — the file does not
// exist, cannot be read, is not valid JSON, has an unrecognized field, or (an extra
// self-check beyond what the filename alone already guarantees) its own recorded Digest
// doesn't match the name it was read from — is a cache miss (ok=false), never an error a
// caller has to handle: Config's whole point in calling this is "answer instantly if
// possible, refetch otherwise", not "surface disk corruption to an operator running a tag
// picker".
func loadCache(digest string) (ImageMeta, bool) {
	path, err := cacheFile(digest)
	if err != nil {
		return ImageMeta{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImageMeta{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c cachedMeta
	if err := dec.Decode(&c); err != nil {
		return ImageMeta{}, false
	}
	if c.Digest != digest {
		return ImageMeta{}, false
	}
	return ImageMeta(c), true
}

// saveCache writes meta's entry atomically — a temp file in the same directory, then
// rename, mirroring internal/engine.SaveState's own pattern for exactly the same reason: a
// process killed mid-write must never leave a partial file at the path loadCache will later
// trust. The file states its permissions explicitly (AGENTS.md §8) rather than inheriting the
// umask: 0600, since nothing about a config blob's Created/Labels is secret, but there is no
// reason for another local user to be able to read it either. A write failure is returned to
// the caller, but Config's own call site treats it as advisory (a cache that failed to write
// is simply refetched next time) — see Config's doc comment.
func saveCache(meta ImageMeta) error {
	path, err := cacheFile(meta.Digest)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cachedMeta(meta), "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meta-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	success = true
	return nil
}
