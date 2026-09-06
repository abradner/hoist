// Package history holds the function types through which screens ask about commit history
// and migration deltas — types only, no Bubble Tea, no adaptor construction. Three screens
// share them (the tag picker, the deploy confirm, the plan confirm), which is why they live
// here rather than being declared three times; cmd/hoist builds the values
// (buildHistoryFuncs), exactly as it does for plan.ResolveFunc and tags.BuildFunc
// (AGENTS.md §4.8: screens take function values, never adaptors).
package history

import (
	"context"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
)

// RevisionFunc resolves one image reference to a git revision in its app repo.
type RevisionFunc func(ctx context.Context, ref image.Ref) (migrate.Revision, error)

// DeltaFunc is what a screen calls, once: everything between resolving both ends, fetching
// their labels, choosing the migrations prefix and caching happens inside it. An
// unresolvable end is a migrate.ErrUnresolved the screen renders as a named gap; an
// unmapped image repo is the same error with a reason naming repos[].apps.
type DeltaFunc func(ctx context.Context, from, to image.Ref) (migrate.Delta, error)

// LiveAgeFunc dates one manifest occurrence in the gitops repo: when the line last changed.
type LiveAgeFunc func(ctx context.Context, occ gitops.Occurrence) (migrate.LineAge, error)

// Funcs is the bundle a screen is handed. Mapped answers, without a network call, whether
// an image repo has an app repo at all — so a screen can say "no app repo in repos[].apps"
// before spending a spinner on it. A zero Funcs (every field nil) is "no history available";
// screens check for nil and degrade.
type Funcs struct {
	Mapped   func(imageRepo string) bool
	Revision RevisionFunc
	Delta    DeltaFunc
	LiveAge  LiveAgeFunc
}
