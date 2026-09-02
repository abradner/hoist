package registry

import (
	"context"
	"fmt"
	"sort"

	"github.com/abradner/hoist/pkg/image"
)

// Fake is an in-memory Registry for tests in other packages. Digests maps "repo:tag" to
// the digest Head returns; TagLists maps repo to Tags' answer; Configs maps ref.String()
// (either "repo:tag" or "repo@digest", whichever a test's caller asks Config with) to what
// Config returns for it. Err, when set, is returned by every call. Auth is what
// AuthSourceUsed reports once a call has succeeded. Calls records every call as "Head <ref>",
// "Tags <repo>" or "Config <ref>".
type Fake struct {
	Digests  map[string]string
	TagLists map[string][]string
	Configs  map[string]ImageMeta
	Err      error
	Auth     string
	Calls    []string

	used bool
}

// Head implements Registry.
func (f *Fake) Head(_ context.Context, ref image.Ref) (string, error) {
	f.Calls = append(f.Calls, "Head "+ref.String())
	if f.Err != nil {
		return "", f.Err
	}
	d, ok := f.Digests[ref.String()]
	if !ok {
		return "", fmt.Errorf("registry: %s: status 404 Not Found: MANIFEST_UNKNOWN", ref)
	}
	f.used = true
	return d, nil
}

// Tags implements Registry.
func (f *Fake) Tags(_ context.Context, repo string) ([]string, error) {
	f.Calls = append(f.Calls, "Tags "+repo)
	if f.Err != nil {
		return nil, f.Err
	}
	f.used = true
	out := append([]string(nil), f.TagLists[repo]...)
	sort.Strings(out)
	return out, nil
}

// Config implements Registry.
func (f *Fake) Config(_ context.Context, ref image.Ref) (ImageMeta, error) {
	f.Calls = append(f.Calls, "Config "+ref.String())
	if f.Err != nil {
		return ImageMeta{}, f.Err
	}
	m, ok := f.Configs[ref.String()]
	if !ok {
		return ImageMeta{}, fmt.Errorf("registry: %s: status 404 Not Found: MANIFEST_UNKNOWN", ref)
	}
	f.used = true
	return m, nil
}

// AuthSourceUsed implements AuthReporter.
func (f *Fake) AuthSourceUsed() string {
	if !f.used {
		return ""
	}
	return f.Auth
}

// Consulted implements AuthReporter: whether Head or Tags was called at all, win or lose.
func (f *Fake) Consulted() bool {
	return len(f.Calls) > 0
}
