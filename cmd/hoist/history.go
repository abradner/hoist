package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

// buildHistoryFuncs adapts pkg/migrate over the configured registries, the per-app forges and
// the gitops repo's own forge into the plain function values internal/app's screens take
// (AGENTS.md §4.8, the same shape as buildTagsFunc). It opens nothing itself: registry and
// forge clients are built on first use and memoised per app repo for the session, so the tag
// picker and both confirm screens share one client and one migrate.Cache.
//
// Degradation follows buildTagsFunc: an app repo whose forge cannot be built (no gh access
// yet) is reported as unmapped through the Delta error, never as a failure to open a screen.
// gitopsForge/forgeErr are the gitops repo's own forge as runTUI built it — LiveAge blames the
// manifest there; with forgeErr set it returns that error and the screen says the age is
// unavailable. base is the root --base (#105): the blame fallback for a HEAD that was never
// pushed. svc is the session's own Service (its Settings carry the root --kube-context/
// --registry-auth/--cluster-secret/--op-ref, #132): historyAdaptor.registry below calls
// svc.RegistryFor exactly as buildTagsFunc does, so both adaptors pick the same registries[]
// entry the identical way (F4) rather than each re-deriving it.
//
// repoRoot and blameRef are no longer captured once at boot (Train 2 design PR 7): every
// LiveAge call reads svc.Repo(), the service's own current view, through historyAdaptor.blame
// below, which memoises the resolved HEAD per view directory so an F5 refresh that lands mid-
// session is picked up by the very next LiveAge call rather than only after a restart, while a
// repeated ask against the same view costs no extra git call.
func buildHistoryFuncs(rc *config.RepoConfig, gitopsForge forge.Forge, forgeErr error, base string, svc *service.Service) history.Funcs {
	if rc == nil {
		return history.Funcs{}
	}
	blamer := migrate.Blamer{Forge: gitopsForge}
	h := &historyAdaptor{rc: rc, svc: svc, forges: map[string]forgeOrErr{}, regs: map[string]registryOrErr{}}
	// LiveAge blames the gitops repo, which needs no app mapping at all: a repo with an empty
	// repos[].apps still gets "declares v1 · since 4 weeks ago" — only the delta needs apps.
	liveAge := func(ctx context.Context, occ gitops.Occurrence) (migrate.LineAge, error) {
		if forgeErr != nil {
			return migrate.LineAge{}, fmt.Errorf("live age needs the gitops repo's forge: %w", forgeErr)
		}
		repoRoot, blameRef, err := h.blame(ctx)
		if err != nil {
			return migrate.LineAge{}, err
		}
		if blameRef == "" {
			return migrate.LineAge{}, fmt.Errorf("live age: the checkout at %s has no resolvable HEAD", repoRoot)
		}
		ages, err := blamer.LiveAge(ctx, migrate.LiveAgeIn{Ref: blameRef, FallbackRef: base, Path: occ.File, Lines: []int{occ.Line}})
		if err != nil {
			return migrate.LineAge{}, err
		}
		age, ok := ages[occ.Line]
		if !ok {
			return migrate.LineAge{}, fmt.Errorf("live age: %s has no line %d at %s", occ.File, occ.Line, blameRef)
		}
		return age, nil
	}
	if len(rc.Apps) == 0 {
		return history.Funcs{Mapped: func(string) bool { return false }, LiveAge: liveAge}
	}
	return history.Funcs{
		Mapped: func(imageRepo string) bool { _, ok := rc.Apps[imageRepo]; return ok },
		Revision: func(ctx context.Context, ref image.Ref) (migrate.Revision, error) {
			return h.revision(ctx, ref)
		},
		Delta: func(ctx context.Context, from, to image.Ref) (migrate.Delta, error) {
			return h.delta(ctx, from, to)
		},
		LiveAge: liveAge,
	}
}

type forgeOrErr struct {
	f   forge.Forge
	err error
}

type registryOrErr struct {
	r   registry.Registry
	err error
}

// historyAdaptor holds the memoised clients and the cache behind buildHistoryFuncs.
type historyAdaptor struct {
	rc  *config.RepoConfig
	svc *service.Service

	mu       sync.Mutex
	forges   map[string]forgeOrErr    // app repo -> forge
	regs     map[string]registryOrErr // image repo -> registry
	policies map[string]policyResult  // app repo + " " + sha -> migrations prefix
	cache    migrate.Cache

	// blameFor/blameSHA memoise the last HEAD h.blame resolved via newGit, keyed by the exact
	// *gitops.Repo the view held at the time — never by the root path string alone. Every
	// LoadRepo/RefreshRepo call allocates a brand-new *gitops.Repo (gitops.Discover's own
	// return), so this key changes on every refresh even when the root path is unchanged,
	// which is what lets a moved HEAD be picked up on the very next LiveAge call rather than
	// staying pinned to whatever HEAD happened to be current the first time this ran.
	blameFor *gitops.Repo
	blameSHA string
}

// blame answers the checkout root and the ref LiveAge should blame, read from svc.Repo()'s
// CURRENT view every call (Train 2 design PR 7) rather than a value captured once at TUI boot —
// a repo view an F5 refresh (or a landed promotion, PR 4) just replaced is picked up
// immediately. For a RepoFromOrigin view, RepoView.SHA is already the exact HEAD that view's
// directory was checked out to (repo.go's own doc comment), so no git call is needed at all; for
// a RepoFromClone view (no configured repo, or origin unreachable) HEAD is read via
// newGit.RevParse and cached per view (blameFor's own doc comment), so a burst of LiveAge calls
// against one unchanged view costs one git call, not one per occurrence, while a view a later
// LoadRepo replaces is never read from the stale cache. ("", "", nil) — never an error — when
// the checkout has no resolvable HEAD at all (a brand new repo with no commits): the caller
// reports that as its own named gap rather than a blame failure.
func (h *historyAdaptor) blame(ctx context.Context) (root, ref string, err error) {
	view := h.svc.Repo()
	if view.Repo == nil {
		return "", "", fmt.Errorf("live age: no repo loaded")
	}
	root = view.Repo.Root
	if view.SHA != "" {
		return root, view.SHA, nil
	}
	h.mu.Lock()
	if h.blameFor == view.Repo {
		ref = h.blameSHA
		h.mu.Unlock()
		return root, ref, nil
	}
	h.mu.Unlock()
	sha, ok, gerr := newGit.RevParse(ctx, root, "HEAD")
	if gerr != nil {
		return root, "", gerr
	}
	if ok {
		ref = sha
	}
	h.mu.Lock()
	h.blameFor, h.blameSHA = view.Repo, ref
	h.mu.Unlock()
	return root, ref, nil
}

type policyResult struct {
	prefix, source string
}

// migrationsPath memoises migrate.MigrationsPath per app-repo revision: it is the one call
// Delta makes before the cache lookup (the prefix is part of the key), so without this a
// cached delta would still cost one ReadFile per ask.
func (h *historyAdaptor) migrationsPath(ctx context.Context, f forge.Forge, appRepo, sha, configured string) (string, string, error) {
	key := appRepo + " " + sha
	h.mu.Lock()
	p, ok := h.policies[key]
	h.mu.Unlock()
	if ok {
		return p.prefix, p.source, nil
	}
	prefix, source, err := migrate.MigrationsPath(ctx, f, sha, configured)
	if err != nil {
		return "", "", err
	}
	h.mu.Lock()
	if h.policies == nil {
		h.policies = map[string]policyResult{}
	}
	h.policies[key] = policyResult{prefix: prefix, source: source}
	h.mu.Unlock()
	return prefix, source, nil
}

func (h *historyAdaptor) forge(appRepo string) (forge.Forge, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if fe, ok := h.forges[appRepo]; ok {
		return fe.f, fe.err
	}
	f, err := newForge(appRepo)
	if err != nil {
		err = fmt.Errorf("app repo %s: %w", appRepo, err)
	}
	h.forges[appRepo] = forgeOrErr{f: f, err: err}
	return f, err
}

// registry builds the registry client for imageRepo the way buildTagsFunc does: the one
// registries[] entry covering it (F4: never another entry's credentials), the cluster link
// only when the entry opts in. A registry that cannot be built is not fatal here — labels
// are one of four revision sources — so the error is folded into "no labels".
func (h *historyAdaptor) registry(imageRepo string) (registry.Registry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if re, ok := h.regs[imageRepo]; ok {
		return re.r, re.err
	}
	reg, err := h.svc.RegistryFor(imageRepo)
	h.regs[imageRepo] = registryOrErr{r: reg, err: err}
	return reg, err
}

// labels fetches the image's config-blob labels for the revision label, or nil when the
// registry cannot answer — the next source is tried, and Revision.Source records which one
// did. A registry failure is deliberately not an error here: the tag picker already reported
// it on the row, and a delta over git tags alone is still an answer.
func (h *historyAdaptor) labels(ctx context.Context, ref image.Ref) map[string]string {
	reg, err := h.registry(ref.Repo)
	if err != nil {
		return nil
	}
	meta, err := reg.Config(ctx, ref)
	if err != nil {
		return nil
	}
	return meta.Labels
}

func (h *historyAdaptor) revision(ctx context.Context, ref image.Ref) (migrate.Revision, error) {
	appRepo, ok := h.rc.Apps[ref.Repo]
	if !ok {
		return migrate.Revision{Ref: ref, Source: migrate.SourceUnknown}, fmt.Errorf("%w: %s has no app repo in repos[].apps", migrate.ErrUnresolved, ref.Repo)
	}
	f, err := h.forge(appRepo)
	if err != nil {
		return migrate.Revision{Ref: ref, Source: migrate.SourceUnknown}, fmt.Errorf("%w: %w", migrate.ErrUnresolved, err)
	}
	return h.cache.Revision(ctx, migrate.RevisionKey{AppRepo: appRepo, Ref: ref.String()}, func(ctx context.Context) (migrate.Revision, error) {
		return migrate.Resolver{Forge: f}.Resolve(ctx, migrate.ResolveIn{Ref: ref, Labels: h.labels(ctx, ref)})
	})
}

func (h *historyAdaptor) delta(ctx context.Context, from, to image.Ref) (migrate.Delta, error) {
	if from.Repo != to.Repo {
		return migrate.Delta{}, fmt.Errorf("history: %s and %s are different image repos", from.Repo, to.Repo)
	}
	appRepo, ok := h.rc.Apps[to.Repo]
	if !ok {
		return migrate.Delta{}, fmt.Errorf("%w: %s has no app repo in repos[].apps", migrate.ErrUnresolved, to.Repo)
	}
	f, err := h.forge(appRepo)
	if err != nil {
		return migrate.Delta{}, fmt.Errorf("%w: %w", migrate.ErrUnresolved, err)
	}
	fromRev, err := h.revision(ctx, from)
	if err != nil {
		return migrate.Delta{From: fromRev}, err
	}
	toRev, err := h.revision(ctx, to)
	if err != nil {
		return migrate.Delta{From: fromRev, To: toRev}, err
	}
	if !fromRev.Resolved() || !toRev.Resolved() {
		// Comparer.Delta reports the same ErrUnresolved; short-circuit here so the policy
		// file is not read for a delta that cannot be computed anyway.
		return migrate.Comparer{Forge: f}.Delta(ctx, migrate.DeltaIn{From: fromRev, To: toRev})
	}
	prefix, source, err := h.migrationsPath(ctx, f, appRepo, toRev.SHA, h.rc.Migrations[to.Repo])
	if err != nil {
		return migrate.Delta{From: fromRev, To: toRev}, err
	}
	key := migrate.DeltaKey{AppRepo: appRepo, FromSHA: fromRev.SHA, ToSHA: toRev.SHA, Prefix: prefix}
	d, err := h.cache.Delta(ctx, key, func(ctx context.Context) (migrate.Delta, error) {
		d, err := migrate.Comparer{Forge: f}.Delta(ctx, migrate.DeltaIn{From: fromRev, To: toRev, Migrations: prefix})
		if err != nil && errors.Is(err, forge.ErrUnknownRef) {
			// The revision was resolved (a label the build stamped) but the app repo does not
			// hold it — a build from a fork, or a merge ref GitHub has since dropped. Say
			// that, not "unknown".
			// Compare cannot say which end is missing (both are one 404), so name both.
			return d, fmt.Errorf("%w: %s (from the %s) or %s (from the %s) is not in %s: %w", migrate.ErrUnresolved, short(fromRev.SHA), fromRev.Source, short(toRev.SHA), toRev.Source, appRepo, err)
		}
		d.PrefixSource = source
		return d, err
	})
	return d, err
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
