package registry

import (
	"context"
	"fmt"
	"strings"
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

// preferredPlatform is the child Config reads Created/Labels from when an index carries more
// than one: linux/amd64, chosen only so a dual-arch index gives the same answer every time.
//
// It is a PREFERENCE, not a requirement, and that distinction is the whole point. An earlier
// version required it, on a stated assumption that "this tool's target fleets run amd64
// servers". That assumption expired: the first real fleet is arm64, and every image built for
// it since is a single-child arm64 index — so Config failed on every one of them with "no child
// with platform linux/amd64", which the tag picker rendered as a bare "load failed" and which
// made those tags unselectable, since hoist will not write a reference it has no digest for.
//
// Created and Labels come from the build, not the architecture, so any linux child answers the
// question equally well. Preferring one merely keeps a dual-arch index deterministic.
var preferredPlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// linuxChild picks the child manifest to read the config blob from: the preferred platform when
// the index has it, otherwise the first linux child in index order. Attestation manifests
// (platform unknown/unknown, which every buildx index carries) are never candidates.
//
// Returns ok=false with the platforms that ARE present, so the error can say what was found
// rather than only what was missing — the difference between a two-minute diagnosis and an
// afternoon of one.
func linuxChild(idx v1.ImageIndex) (h v1.Hash, available []string, ok bool) {
	m, err := idx.IndexManifest()
	if err != nil {
		return v1.Hash{}, nil, false
	}
	var first *v1.Hash
	for i := range m.Manifests {
		p := m.Manifests[i].Platform
		if p == nil || p.OS != "linux" || p.Architecture == "" || p.Architecture == "unknown" {
			continue
		}
		available = append(available, p.OS+"/"+p.Architecture)
		if p.OS == preferredPlatform.OS && p.Architecture == preferredPlatform.Architecture {
			return m.Manifests[i].Digest, available, true
		}
		if first == nil {
			d := m.Manifests[i].Digest
			first = &d
		}
	}
	if first == nil {
		return v1.Hash{}, available, false
	}
	return *first, available, true
}

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
		// Fetched without a platform option so this code, not go-containerregistry, decides
		// which child to read — the option can only require a platform, and requiring one is
		// exactly what broke on a single-arch index.
		desc, err := remote.Get(nref, remote.WithContext(ctx), remote.WithAuth(a), remote.WithTransport(c.cfg.Transport))
		if err != nil {
			return err
		}
		img, err := configImage(desc)
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

// configImage resolves a descriptor to the image whose config blob carries Created and Labels:
// the descriptor itself when it is already a single image manifest, or a linux child when it is
// an index. desc.Digest — the index's own digest, the one Head returned and the one a pod's
// imageID reports — is what ImageMeta.Digest keeps; only the config blob is read from the child.
func configImage(desc *remote.Descriptor) (v1.Image, error) {
	if !desc.MediaType.IsIndex() {
		return desc.Image()
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, err
	}
	h, available, ok := linuxChild(idx)
	if !ok {
		if len(available) == 0 {
			return nil, fmt.Errorf("index %s has no linux child manifest to read image metadata from", desc.Digest)
		}
		return nil, fmt.Errorf("index %s has no linux child manifest to read image metadata from; it carries %s",
			desc.Digest, strings.Join(available, ", "))
	}
	return idx.Image(h)
}
