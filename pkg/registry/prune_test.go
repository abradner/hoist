package registry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// pruneFixture writes one cache entry per name with the given age, plus the bystanders
// PruneCache must never touch, and returns the cache directory.
func pruneFixture(t *testing.T, now time.Time, ages map[string]time.Duration) string {
	t.Helper()
	isolateCache(t)
	dir, err := cacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, age := range ages {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestPruneCacheRemovesOnlyOldEntriesOfItsOwnShape(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	old := "sha256-" + strings.Repeat("a", 64) + ".json"
	fresh := "sha256-" + strings.Repeat("b", 64) + ".json"
	dir := pruneFixture(t, now, map[string]time.Duration{
		old:                 91 * day,
		fresh:               89 * day,
		".meta-123.tmp":     2 * time.Hour,    // a killed write's leftover
		".meta-456.tmp":     10 * time.Minute, // possibly another process, mid-write
		"notes.txt":         400 * day,        // not this package's file
		"sha256-short.json": 400 * day,        // not a digest
		"sha256-" + strings.Repeat("c", 64) + ".json.bak": 400 * day,
	})
	// Bystanders of the right NAME but the wrong kind: a directory and a symlink.
	asDir := "sha256-" + strings.Repeat("d", 64) + ".json"
	if err := os.Mkdir(filepath.Join(dir, asDir), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	veryOld := now.Add(-400 * day)
	if err := os.Chtimes(outside, veryOld, veryOld); err != nil {
		t.Fatal(err)
	}
	asLink := "sha256-" + strings.Repeat("e", 64) + ".json"
	if err := os.Symlink(outside, filepath.Join(dir, asLink)); err != nil {
		t.Fatal(err)
	}
	before := names(t, dir)

	// Dry run: reports, removes nothing.
	would, err := PruneCache(90*day, now, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, ".meta-123.tmp"), filepath.Join(dir, old)}
	if strings.Join(would, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dry run = %q, want %q", would, want)
	}
	if after := names(t, dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a dry run changed the cache directory:\nbefore %q\nafter  %q", before, after)
	}

	// Seen from far enough in the future that every file here is old, the directory and the
	// symlink are still not candidates: only regular files are.
	later, err := PruneCache(90*day, now.Add(5*365*day), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range later {
		if base := filepath.Base(p); base == asDir || base == asLink {
			t.Errorf("%s is not a regular file and must never be a prune candidate", base)
		}
	}

	removed, err := PruneCache(90*day, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("removed = %q, want exactly what the dry run listed %q", removed, want)
	}
	after := names(t, dir)
	for _, gone := range []string{old, ".meta-123.tmp"} {
		for _, n := range after {
			if n == gone {
				t.Errorf("%s should have been removed", gone)
			}
		}
	}
	if len(after) != len(before)-2 {
		t.Fatalf("exactly two files should be gone:\nbefore %q\nafter  %q", before, after)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a symlink's target outside the cache must be untouched: %v", err)
	}
}

func TestPruneCacheOfAMissingDirectoryIsNothingToDo(t *testing.T) {
	isolateCache(t)
	removed, err := PruneCache(time.Hour, time.Now(), false)
	if err != nil || len(removed) != 0 {
		t.Fatalf("PruneCache = %q, %v", removed, err)
	}
	if _, err := PruneCache(0, time.Now(), false); err == nil {
		t.Fatal("a zero age would remove every entry; it must be refused")
	}
}

// TestCacheHitRefreshesTheEntrysAge is what makes PruneCache's age "since last used": an entry
// read back is as young as one just written.
func TestCacheHitRefreshesTheEntrysAge(t *testing.T) {
	isolateCache(t)
	digest := "sha256:" + fixedHex()
	if err := saveCache(ImageMeta{Digest: digest, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path, err := cacheFile(digest)
	if err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-200 * 24 * time.Hour)
	if err := os.Chtimes(path, longAgo, longAgo); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCache(digest); !ok {
		t.Fatal("expected a cache hit")
	}
	removed, err := PruneCache(90*24*time.Hour, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("an entry just read must not be pruned, removed %q", removed)
	}
}
