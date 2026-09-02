package registry

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/abradner/hoist/pkg/k8s"
)

// isolateCache points XDG_CACHE_HOME at a fresh temp dir so a test never reads or writes the
// operator's real cache, and never leaks state to another test.
func isolateCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

// withConfig sets img's ConfigFile to one with the given Created/Labels, so a test can assert
// Config reads exactly those values back rather than whatever random.Image happened to
// produce (it sets neither).
func withConfig(t *testing.T, img v1.Image, created time.Time, labels map[string]string) v1.Image {
	t.Helper()
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf.Created = v1.Time{Time: created}
	cf.Config.Labels = labels
	out, err := mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// countingTransport counts requests to the blobs endpoint specifically — go-containerregistry
// issues a "ping" GET to /v2/ before *every* request regardless of authenticator, and a
// manifest GET/HEAD for the tag/digest itself either way, so counting all GETs cannot
// distinguish a cache hit from a miss. Only a config *blob* fetch (GET .../blobs/sha256:...)
// happens on a miss and never on a hit — exactly cache.go's own claim.
type countingTransport struct {
	inner    http.RoundTripper
	mu       sync.Mutex
	blobGETs int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
		c.mu.Lock()
		c.blobGETs++
		c.mu.Unlock()
	}
	return c.inner.RoundTrip(r)
}

func (c *countingTransport) blobFetches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blobGETs
}

func TestConfigSinglePlatformReadsCreatedAndLabels(t *testing.T) {
	isolateCache(t)
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)

	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	img = withConfig(t, img, created, map[string]string{"org.example.rev": "abc123"})
	ref, err := name.ParseReference(tr.open + "/example/single:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	wantDigest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	meta, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/single:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Digest != wantDigest.String() {
		t.Errorf("Digest = %s, want %s", meta.Digest, wantDigest)
	}
	if !meta.Created.Equal(created) {
		t.Errorf("Created = %s, want %s", meta.Created, created)
	}
	if meta.Labels["org.example.rev"] != "abc123" {
		t.Errorf("Labels = %+v", meta.Labels)
	}
}

// The digest a multi-arch tag resolves to is the index digest (matching Head — see
// TestHeadMultiArchReturnsIndexDigest), but Created/Labels must come from the linux/amd64
// child's own config blob, never the index (which carries neither) and never the arm64
// sibling's (which this test gives an intentionally different Created/Labels, so reading the
// wrong child would be caught).
func TestConfigMultiArchReadsAmd64ChildConfig(t *testing.T) {
	isolateCache(t)
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)

	amd64Created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	arm64Created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	amd64Img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	amd64Img = withConfig(t, amd64Img, amd64Created, map[string]string{"arch": "amd64"})
	arm64Img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	arm64Img = withConfig(t, arm64Img, arm64Created, map[string]string{"arch": "arm64"})

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd64Img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm64Img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	ref, err := name.ParseReference(tr.open + "/example/multi:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	wantIndexDigest, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	amd64Digest, err := amd64Img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	meta, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/multi:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Digest != wantIndexDigest.String() {
		t.Errorf("Digest = %s, want the index digest %s (not %s)", meta.Digest, wantIndexDigest, amd64Digest)
	}
	if !meta.Created.Equal(amd64Created) {
		t.Errorf("Created = %s, want the amd64 child's %s (arm64's is %s)", meta.Created, amd64Created, arm64Created)
	}
	if meta.Labels["arch"] != "amd64" {
		t.Errorf("Labels = %+v, want the amd64 child's, not the arm64 sibling's", meta.Labels)
	}
}

func TestConfigCachesByDigestSkipsRefetch(t *testing.T) {
	isolateCache(t)
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	img = withConfig(t, img, time.Now().UTC().Truncate(time.Second), map[string]string{"k": "v"})
	ref, err := name.ParseReference(tr.open + "/example/cached:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}

	ct := &countingTransport{inner: tr.transport}
	c, err := New(AuthConfig{
		Order:     []AuthSource{AuthKeychain},
		Keychain:  k8s.StaticKeychain{Username: "good", Password: "pw-good"},
		Transport: ct,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/cached:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if ct.blobFetches() == 0 {
		t.Fatalf("first call (cache miss): expected at least one config-blob fetch, got none")
	}

	blobsBefore := ct.blobFetches()
	second, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/cached:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest != first.Digest || !second.Created.Equal(first.Created) || second.Labels["k"] != first.Labels["k"] {
		t.Errorf("second call = %+v, want identical to first %+v", second, first)
	}
	if got := ct.blobFetches(); got != blobsBefore {
		t.Errorf("second call fetched %d more config blob(s); a digest-keyed cache hit should need none, only Head's own manifest check", got-blobsBefore)
	}
}

func TestConfigCorruptCacheEntryIsRefetchedNotAnError(t *testing.T) {
	isolateCache(t)
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second)
	img = withConfig(t, img, created, nil)
	ref, err := name.ParseReference(tr.open + "/example/corrupt:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	if _, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/corrupt:v1")); err != nil {
		t.Fatal(err)
	}

	path, err := cacheFile(digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	meta, err := c.Config(context.Background(), mustRef(t, "ghcr.io/example/corrupt:v1"))
	if err != nil {
		t.Fatalf("a corrupt cache entry must be treated as a miss and refetched, not returned as an error: %v", err)
	}
	if !meta.Created.Equal(created) {
		t.Errorf("Created = %s, want %s (refetched correctly despite the corrupt cache file)", meta.Created, created)
	}
	// The corrupt entry must actually have been overwritten by the refetch, not left corrupt
	// on disk for the next reader to trip over too.
	if _, ok := loadCache(digest.String()); !ok {
		t.Errorf("expected the refetch to repair the cache entry on disk")
	}
}

func TestLoadCacheMissingIsAMiss(t *testing.T) {
	isolateCache(t)
	if _, ok := loadCache("sha256:" + fixedHex()); ok {
		t.Fatal("expected a miss for a digest nothing has cached")
	}
}

func TestLoadCacheRejectsDigestMismatchInFile(t *testing.T) {
	isolateCache(t)
	digest := "sha256:" + fixedHex()
	if err := saveCache(ImageMeta{Digest: digest, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	path, err := cacheFile(digest)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite with a well-formed cache file recorded under a *different* digest than the
	// filename implies — simulating a corrupted rename or a hand-edited file.
	other := "sha256:" + strings0("1", 64)
	data := []byte(`{"digest":"` + other + `","created":"2020-01-01T00:00:00Z"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadCache(digest); ok {
		t.Fatal("a cache file recorded under a different digest than its own filename must be a miss")
	}
}

func TestSaveCacheThenLoadCacheRoundTrips(t *testing.T) {
	isolateCache(t)
	digest := "sha256:" + fixedHex()
	want := ImageMeta{Digest: digest, Created: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Labels: map[string]string{"a": "b"}}
	if err := saveCache(want); err != nil {
		t.Fatal(err)
	}
	got, ok := loadCache(digest)
	if !ok {
		t.Fatal("expected a hit after saveCache")
	}
	if got.Digest != want.Digest || !got.Created.Equal(want.Created) || got.Labels["a"] != "b" {
		t.Errorf("loadCache = %+v, want %+v", got, want)
	}
}

func TestCacheFileRejectsMalformedDigest(t *testing.T) {
	isolateCache(t)
	for _, bad := range []string{"", "not-a-digest", "sha256:tooshort", "sha1:" + fixedHex()} {
		if _, err := cacheFile(bad); err == nil {
			t.Errorf("cacheFile(%q) should have been rejected", bad)
		}
	}
}

// TestCacheFileUsesXDGCacheHome pins the directory this package's own doc comment promises:
// $XDG_CACHE_HOME/hoist/registry, mirroring internal/engine.CacheDir's rule.
func TestCacheFileUsesXDGCacheHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	digest := "sha256:" + fixedHex()
	path, err := cacheFile(digest)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "hoist", "registry", "sha256-"+fixedHex()+".json")
	if path != want {
		t.Errorf("cacheFile = %s, want %s", path, want)
	}
}

func fixedHex() string { return strings0("a", 64) }

func strings0(ch string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ch[0]
	}
	return string(b)
}
