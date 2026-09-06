package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abradner/hoist/pkg/forge"
)

// LiveAgeIn names lines of one file at one ref of the gitops repo. FallbackRef is tried when
// Ref does not resolve on the forge — the operator's checkout may be ahead of anything
// pushed — and an answer from it is marked Approximate, since the line numbers were read from
// Ref's tree, not FallbackRef's.
type LiveAgeIn struct {
	Ref         string
	FallbackRef string
	Path        string
	Lines       []int
}

// LineAge is when one line last changed: the commit and its committer date.
type LineAge struct {
	SHA         string
	Since       time.Time
	Approximate bool
}

// Blamer dates manifest lines against the gitops repo's forge.
type Blamer struct {
	Forge forge.Forge
}

// LiveAge blames the requested lines. Lines past the end of the file are absent from the
// result. An ErrUnknownRef on Ref with a FallbackRef set retries once against the fallback.
func (b Blamer) LiveAge(ctx context.Context, in LiveAgeIn) (map[int]LineAge, error) {
	origins, err := b.Forge.BlameLines(ctx, in.Ref, in.Path, in.Lines)
	approximate := false
	if err != nil && errors.Is(err, forge.ErrUnknownRef) && in.FallbackRef != "" && in.FallbackRef != in.Ref {
		origins, err = b.Forge.BlameLines(ctx, in.FallbackRef, in.Path, in.Lines)
		approximate = true
	}
	if err != nil {
		return nil, fmt.Errorf("migrate: dating %s: %w", in.Path, err)
	}
	out := make(map[int]LineAge, len(origins))
	for n, o := range origins {
		out[n] = LineAge{SHA: o.SHA, Since: o.Date, Approximate: approximate}
	}
	return out, nil
}
