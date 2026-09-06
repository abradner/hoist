package migrate

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
)

// Source names which evidence resolved an image reference to a git revision, in the order
// Resolver tries them. The screens print it: a label is the build's own claim about itself,
// a tag is whatever the tag points at today.
type Source string

const (
	// SourceLabel means the image's org.opencontainers.image.revision label.
	SourceLabel Source = "image label"
	// SourceGitTag means the app repo has a git tag named exactly like the image tag.
	SourceGitTag Source = "git tag"
	// SourceShaTag means the image tag is "sha-<prefix>" and the prefix names a commit.
	SourceShaTag Source = "sha tag"
	// SourceUnknown means nothing answered. Revision.SHA is empty.
	SourceUnknown Source = "unknown"
)

// RevisionLabel is the OCI annotation a build stamps with the commit it was built from
// (docker/metadata-action and buildx both set it by default).
const RevisionLabel = "org.opencontainers.image.revision"

// Revision is one image reference resolved to a commit in its app repo. Detail is the tag or
// prefix that answered, for the screen to show beside Source; empty for a label.
type Revision struct {
	Ref    image.Ref
	SHA    string
	Source Source
	Detail string
}

// Resolved reports whether a commit was found.
func (r Revision) Resolved() bool { return r.SHA != "" }

// ResolveIn is Resolver.Resolve's input. Labels is the image's config-blob labels
// (registry.ImageMeta.Labels); nil means the caller did not fetch them, and the label source
// is skipped rather than treated as absent.
type ResolveIn struct {
	Ref    image.Ref
	Labels map[string]string
}

// Resolver resolves image references against one app repo's forge.
type Resolver struct {
	Forge forge.Forge
}

var (
	fullSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	shaTag    = regexp.MustCompile(`^sha-([0-9a-f]{7,40})$`)
	shortSHA  = regexp.MustCompile(`^[0-9a-f]{7,39}$`)
	looksLike = func(s string) bool { return fullSHA.MatchString(s) || shortSHA.MatchString(s) }
)

// Resolve tries each Source in order and returns the first that answers. A forge error stops
// the walk and is returned: a token scope gap must never read as "unknown" (the forge's own
// ok=false is the only thing that means "not there"). A full-length label sha is trusted as
// is — verifying it against the repo costs a call the common case does not need, and Compare
// will report ErrUnknownRef if it is not actually in the repo.
func (r Resolver) Resolve(ctx context.Context, in ResolveIn) (Revision, error) {
	out := Revision{Ref: in.Ref, Source: SourceUnknown}
	if v := strings.TrimSpace(in.Labels[RevisionLabel]); v != "" {
		if fullSHA.MatchString(v) {
			out.SHA, out.Source = v, SourceLabel
			return out, nil
		}
		if looksLike(v) {
			sha, ok, err := r.Forge.ResolveRef(ctx, v)
			if err != nil {
				return out, fmt.Errorf("migrate: resolving %s from its %s label: %w", in.Ref, RevisionLabel, err)
			}
			if ok {
				out.SHA, out.Source, out.Detail = sha, SourceLabel, v
				return out, nil
			}
		}
	}
	if in.Ref.Tag == "" {
		return out, nil
	}
	sha, ok, err := r.Forge.ResolveRef(ctx, in.Ref.Tag)
	if err != nil {
		return out, fmt.Errorf("migrate: resolving tag %s: %w", in.Ref.Tag, err)
	}
	if ok {
		out.SHA, out.Source, out.Detail = sha, SourceGitTag, in.Ref.Tag
		return out, nil
	}
	if m := shaTag.FindStringSubmatch(in.Ref.Tag); m != nil {
		sha, ok, err := r.Forge.ResolveRef(ctx, m[1])
		if err != nil {
			return out, fmt.Errorf("migrate: resolving %s as a commit prefix: %w", in.Ref.Tag, err)
		}
		if ok {
			out.SHA, out.Source, out.Detail = sha, SourceShaTag, m[1]
			return out, nil
		}
	}
	return out, nil
}
