package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/abradner/hoist/pkg/image"
)

// ImageMeta is what Config reads from one image's config blob: the digest a pull by tag
// would pin (the same value Head returns for the same ref — see this field's own note
// below), when the image was built, and its OCI/Docker config labels. Nothing here is
// guessed or derived from a tag; every field is read fresh from the registry unless a cache
// entry answers it first (cache.go).
type ImageMeta struct {
	// Digest is the top-level digest Head would also return for this ref: the image index
	// digest for a multi-arch tag, never one platform manifest's (see the package doc's note
	// on Head for why that is the digest a pod's imageID reports). This is deliberately not
	// the digest of whatever platform-specific manifest Created/Labels were actually read
	// from — see configPlatform's doc comment — so that a cache keyed by ImageMeta.Digest
	// (cache.go) is keyed by exactly the same value every other digest-keyed thing in this
	// codebase uses (Head, a pod's imageID, image.Ref.Digest).
	Digest  string
	Created time.Time
	Labels  map[string]string
}

// configPlatform is the platform Config resolves a multi-arch index to before reading a
// config blob: linux/amd64. AGENTS.md's M6 brief allows either a fixed linux/amd64 or "the
// caller's host arch"; a fixed platform is chosen because hoist's own process architecture
// (an operator's laptop, frequently arm64 today) has no necessary relationship to the
// cluster's (this tool's target fleets run amd64 servers) — resolving to whatever arch
// happens to run hoist would silently read the wrong platform's Created/Labels on an arm64
// laptop pointed at an amd64 fleet, and do it invisibly (both platforms usually exist in the
// same index). A repo whose images are genuinely arm64-only, or mixed-fleet, is a real future
// need but not one anything in this milestone exercises; this is the one place that
// assumption would need to become a parameter.
var configPlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// Config implements the Registry interface's AGENTS.md M6 addition: per-digest image
// metadata read from the image's config blob, never the index. For a multi-arch image, the
// index itself carries no Created/Labels — those live only on the platform-specific child
// manifest's config blob — so Config resolves ref to configPlatform's child first: Get's
// remote.WithPlatform option plus Descriptor.Image() (called through desc.Image() below) do
// that resolution for a single round of "fetch, is it an index, walk to the matching child"
// this package does not have to reimplement; called on a reference that is already a
// single-platform image manifest, Image() just returns it unchanged, so this same code path
// handles both shapes.
//
// Config first asks Head for ref's own digest (a cheap manifest HEAD, reusing this Client's
// same credential chain and cached winner) and checks the on-disk cache for that digest
// before ever fetching a config blob — see cache.go's doc comment for why a digest-keyed
// cache is always safe to trust with no invalidation policy. Only a cache miss reaches the
// network for the config blob itself.
func (c *Client) Config(ctx context.Context, ref image.Ref) (ImageMeta, error) {
	if err := ref.Validate(); err != nil {
		return ImageMeta{}, err
	}
	digest, err := c.Head(ctx, ref)
	if err != nil {
		return ImageMeta{}, err
	}
	if meta, ok := loadCache(digest); ok {
		return meta, nil
	}

	// Re-parse as an exact digest reference — never the possibly-tagged ref this call
	// started with — so the config-blob fetch below cannot land on a different manifest than
	// the one Head just confirmed: a tag is mutable between these two requests even within
	// one call to Config, a digest is not (AGENTS.md principle 3).
	byDigest := image.Ref{Repo: ref.Repo, Digest: digest}
	nref, err := name.ParseReference(byDigest.String())
	if err != nil {
		return ImageMeta{}, fmt.Errorf("registry: %s: %w", ref, err)
	}

	var meta ImageMeta
	err = c.do(ctx, nref.Context().Registry, func(a authn.Authenticator) error {
		desc, err := remote.Get(nref, remote.WithContext(ctx), remote.WithAuth(a), remote.WithTransport(c.cfg.Transport), remote.WithPlatform(configPlatform))
		if err != nil {
			return err
		}
		img, err := desc.Image()
		if err != nil {
			return err
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			return err
		}
		meta = ImageMeta{Digest: desc.Digest.String(), Created: cfg.Created.Time, Labels: cfg.Config.Labels}
		return nil
	})
	if err != nil {
		return ImageMeta{}, fmt.Errorf("registry: %s: %w", ref, err)
	}
	if meta.Digest != digest {
		// Belt and suspenders: a digest reference is supposed to be exactly what it names: a
		// registry that answered with different content than the digest it was asked for is a
		// registry lying, and a cache write for that would poison every future lookup of the
		// digest Head actually confirmed.
		return ImageMeta{}, fmt.Errorf("registry: %s: Head resolved %s but the config fetch returned content for %s; refusing to cache a mismatch", ref, digest, meta.Digest)
	}
	// Best-effort: a cache write failure just means this digest is fetched again next time,
	// never a reason to fail a lookup that already succeeded (cache.go's own doc comment).
	_ = saveCache(meta)
	return meta, nil
}
