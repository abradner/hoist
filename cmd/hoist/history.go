package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/config"
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
// unavailable. blameRef is the checkout's HEAD sha (line numbers were read from that tree);
// the default branch is the fallback when HEAD was never pushed; "" when unknown.
func buildHistoryFuncs(cfg *config.Config, rc *config.RepoConfig, r *gitops.Repo, gitopsForge forge.Forge, forgeErr error, blameRef string) history.Funcs {
	// The TUI has no --base flag (promote/deploy default theirs to "main" too), so the
	// blame fallback is the same literal until one exists.
	const base = "main"
	if rc == nil {
		return history.Funcs{}
	}
	var repoRoot string
	if r != nil {
		repoRoot = r.Root
	}
	blamer := migrate.Blamer{Forge: gitopsForge}
	// LiveAge blames the gitops repo, which needs no app mapping at all: a repo with an empty
	// repos[].apps still gets "declares v1 · since 4 weeks ago" — only the delta needs apps.
	liveAge := func(ctx context.Context, occ gitops.Occurrence) (migrate.LineAge, error) {
		if forgeErr != nil {
			return migrate.LineAge{}, fmt.Errorf("live age needs the gitops repo's forge: %w", forgeErr)
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
	var registries []config.RegistryConfig
	if cfg != nil {
		registries = cfg.Registries
	}
	h := &historyAdaptor{rc: rc, registries: registries, forges: map[string]forgeOrErr{}, regs: map[string]registryOrErr{}}
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
	rc         *config.RepoConfig
	registries []config.RegistryConfig

	mu       sync.Mutex
	forges   map[string]forgeOrErr    // app repo -> forge
	regs     map[string]registryOrErr // image repo -> registry
	policies map[string]policyResult  // app repo + " " + sha -> migrations prefix
	cache    migrate.Cache
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
	entry := registryEntryFor(h.registries, imageRepo)
	auth, clusterSecret, opRef := entryAuthConfig(entry, resolveOptions{})
	regCfg := registry.AuthConfig{Order: auth, OpRef: opRef}
	if clusterSecret != "" && has(auth, registry.AuthCluster) {
		if cluster, _, err := newCluster(h.rc.Kube.Context); err == nil {
			regCfg.ClusterSecret, regCfg.Cluster = clusterSecret, cluster
		}
	}
	reg, err := newRegistry(regCfg)
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
