package service

import (
	"context"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/registry"
)

// TagSource is what the tag picker reads for one image repo: its registry tags, the mapped app
// repo's git tags when repos[].apps maps it, and one tag's registry metadata. OpenTagSource builds
// it; each method then asks the registry or forge itself, so the picker can render the registry's
// fast answer without waiting on the forge's slow N+1 crawl (forge.Forge.Tags).
type TagSource struct {
	imageRepo string
	reg       registry.Registry
	regErr    error
	app       forge.Forge

	// Mapped is the static answer the picker starts from: repos[].apps names an app repo AND a
	// forge client for it could be built. False when it is not mapped, when the forge could not
	// be built (warn, don't block — principle 5: the picker still opens, in registry order) and
	// when the registry client itself could not.
	Mapped bool
}

// OpenTagSource builds the tag source for imageRepo. The registry client is the one
// registries[] entry covering it (RegistryFor, F4: never another entry's credentials). A registry
// that cannot be built is not fatal here: every call that needs it reports the real error, so
// the picker opens and says why. The forge client comes from ForgeFor, which remembers only a
// success, so a `gh` login fixed mid-session is picked up the next time the picker opens.
func (s *Service) OpenTagSource(imageRepo string) *TagSource {
	t := &TagSource{imageRepo: imageRepo}
	t.reg, t.regErr = s.RegistryFor(imageRepo)
	if t.regErr != nil {
		return t
	}
	if rc := s.settings.Repo; rc != nil {
		if appRepo, ok := rc.Apps[imageRepo]; ok {
			if f, err := s.ForgeFor(appRepo); err == nil {
				t.app, t.Mapped = f, true
			}
		}
	}
	return t
}

// RegistryTags lists the image repo's tags from its registry.
func (t *TagSource) RegistryTags(ctx context.Context) ([]string, error) {
	if t.regErr != nil {
		return nil, t.regErr
	}
	return t.reg.Tags(ctx, t.imageRepo)
}

// Err is why the registry client could not be built, nil when it was. A caller that has no use
// for the other methods without one (the picker shows the error on the registry list and has no
// metadata to fetch) checks it.
func (t *TagSource) Err() error { return t.regErr }

// HasGitTags reports whether GitTags has anything to ask: the repo is mapped to an app repo with
// a working forge client.
func (t *TagSource) HasGitTags() bool { return t.app != nil }

// GitTags lists the mapped app repo's git tags (commit dates, for invariant 3's ordering). mapped
// is this call's own observed answer: true with the tags, false with the forge's error when the
// lookup fails at run time, so the picker degrades to registry order and a caller that wants to
// can say why. Before this reported the error it was dropped, and a forge failure was
// indistinguishable from an unmapped repo.
func (t *TagSource) GitTags(ctx context.Context) (tags []forge.GitTag, mapped bool, err error) {
	if t.app == nil {
		return nil, false, nil
	}
	tags, err = t.app.Tags(ctx)
	if err != nil {
		return nil, false, err
	}
	return tags, true, nil
}

// Meta fetches one tag's registry metadata, lazily, for the row the picker currently needs it for.
func (t *TagSource) Meta(ctx context.Context, tag string) (registry.ImageMeta, error) {
	if t.regErr != nil {
		return registry.ImageMeta{}, t.regErr
	}
	ref, err := image.Parse(t.imageRepo + ":" + tag)
	if err != nil {
		return registry.ImageMeta{}, err
	}
	return t.reg.Config(ctx, ref)
}
