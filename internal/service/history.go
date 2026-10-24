package service

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

// history is the memory behind the commit-history use case: one migrate.Cache for the session, so
// the tag picker and both confirm screens share every resolved revision and delta, the migrations
// policy read per app-repo revision, the registry clients, and the last HEAD a blame resolved.
//
// What it does not hold is a failed client. A forge or registry client is remembered once it was
// built and never when it was not (ForgeFor's rule), so a `gh` login that was missing when the
// client was built and is fixed mid-session recovers history and the live age at once instead of
// staying broken until a restart (CS-3). Two limits: a client that was built with a token that
// later turned bad is remembered (go-gh fixes the token into the client), and a revision the
// registry could not label is answered from git tags alone and cached by migrate.Cache.
type history struct {
	mu       sync.Mutex
	regs     map[string]registry.Registry // image repo -> built client
	policies map[string]policyResult      // app repo + " " + sha -> migrations prefix
	cache    migrate.Cache

	// blameFor/blameSHA memoise the last HEAD LiveAge resolved with git, keyed by the exact
	// *gitops.Repo the view held at the time — never by the root path alone. Every
	// LoadRepo/RefreshRepo allocates a new *gitops.Repo (gitops.Discover's return), so this key
	// changes on every refresh even when the path does not, which is what lets a moved HEAD be
	// picked up on the next call while a burst of calls against one unchanged view costs one git
	// call.
	blameFor *gitops.Repo
	blameSHA string
}

type policyResult struct{ prefix, source string }

// HistoryMapped answers, without a network call, whether repos[].apps maps imageRepo to an app
// repo — so a screen can say "no app repo in repos[].apps" before spending a spinner on it.
func (s *Service) HistoryMapped(imageRepo string) bool {
	if s.settings.Repo == nil {
		return false
	}
	_, ok := s.settings.Repo.Apps[imageRepo]
	return ok
}

// appRepoFor is the app repo imageRepo is mapped to, or an ErrUnresolved naming repos[].apps.
func (s *Service) appRepoFor(imageRepo string) (string, error) {
	if s.settings.Repo != nil {
		if appRepo, ok := s.settings.Repo.Apps[imageRepo]; ok {
			return appRepo, nil
		}
	}
	return "", fmt.Errorf("%w: %s has no app repo in repos[].apps", migrate.ErrUnresolved, imageRepo)
}

// appForge is the forge client for an app repo, or the reason there is none.
func (s *Service) appForge(appRepo string) (forge.Forge, error) {
	f, err := s.ForgeFor(appRepo)
	if err != nil {
		return nil, fmt.Errorf("app repo %s: %w", appRepo, err)
	}
	return f, nil
}

// labels fetches the image's config-blob labels for the revision label, or nil when the registry
// cannot answer — the next source is tried, and Revision.Source records which one did. A registry
// failure is deliberately not an error here: the tag picker already reported it on the row, and a
// delta over git tags alone is still an answer.
func (s *Service) labels(ctx context.Context, ref image.Ref) map[string]string {
	s.hist.mu.Lock()
	reg, ok := s.hist.regs[ref.Repo]
	s.hist.mu.Unlock()
	if !ok {
		var err error
		if reg, err = s.RegistryFor(ref.Repo); err != nil {
			return nil
		}
		s.hist.mu.Lock()
		if s.hist.regs == nil {
			s.hist.regs = map[string]registry.Registry{}
		}
		s.hist.regs[ref.Repo] = reg
		s.hist.mu.Unlock()
	}
	meta, err := reg.Config(ctx, ref)
	if err != nil {
		return nil
	}
	return meta.Labels
}

// HistoryRevision resolves which app-repo commit built ref.
func (s *Service) HistoryRevision(ctx context.Context, ref image.Ref) (migrate.Revision, error) {
	appRepo, err := s.appRepoFor(ref.Repo)
	if err != nil {
		return migrate.Revision{Ref: ref, Source: migrate.SourceUnknown}, err
	}
	f, err := s.appForge(appRepo)
	if err != nil {
		return migrate.Revision{Ref: ref, Source: migrate.SourceUnknown}, fmt.Errorf("%w: %w", migrate.ErrUnresolved, err)
	}
	return s.hist.cache.Revision(ctx, migrate.RevisionKey{AppRepo: appRepo, Ref: ref.String()}, func(ctx context.Context) (migrate.Revision, error) {
		return migrate.Resolver{Forge: f}.Resolve(ctx, migrate.ResolveIn{Ref: ref, Labels: s.labels(ctx, ref)})
	})
}

// migrationsPath memoises migrate.MigrationsPath per app-repo revision: it is the one call a delta
// makes before the cache lookup (the prefix is part of the key), so without this a cached delta
// would still cost one ReadFile per ask.
func (s *Service) migrationsPath(ctx context.Context, f forge.Forge, appRepo, sha, configured string) (string, string, error) {
	key := appRepo + " " + sha
	s.hist.mu.Lock()
	p, ok := s.hist.policies[key]
	s.hist.mu.Unlock()
	if ok {
		return p.prefix, p.source, nil
	}
	prefix, source, err := migrate.MigrationsPath(ctx, f, sha, configured)
	if err != nil {
		return "", "", err
	}
	s.hist.mu.Lock()
	if s.hist.policies == nil {
		s.hist.policies = map[string]policyResult{}
	}
	s.hist.policies[key] = policyResult{prefix: prefix, source: source}
	s.hist.mu.Unlock()
	return prefix, source, nil
}

// HistoryDelta is the commits and migrations between two builds of one image repo: everything
// between resolving both ends, fetching their labels, choosing the migrations prefix and caching
// happens inside it. An unresolvable end is a migrate.ErrUnresolved a screen renders as a named
// gap; an unmapped image repo is the same error with a reason naming repos[].apps.
func (s *Service) HistoryDelta(ctx context.Context, from, to image.Ref) (migrate.Delta, error) {
	if from.Repo != to.Repo {
		return migrate.Delta{}, fmt.Errorf("history: %s and %s are different image repos", from.Repo, to.Repo)
	}
	appRepo, err := s.appRepoFor(to.Repo)
	if err != nil {
		return migrate.Delta{}, err
	}
	f, err := s.appForge(appRepo)
	if err != nil {
		return migrate.Delta{}, fmt.Errorf("%w: %w", migrate.ErrUnresolved, err)
	}
	fromRev, err := s.HistoryRevision(ctx, from)
	if err != nil {
		return migrate.Delta{From: fromRev}, err
	}
	toRev, err := s.HistoryRevision(ctx, to)
	if err != nil {
		return migrate.Delta{From: fromRev, To: toRev}, err
	}
	if !fromRev.Resolved() || !toRev.Resolved() {
		// Comparer.Delta reports the same ErrUnresolved; short-circuit here so the policy
		// file is not read for a delta that cannot be computed anyway.
		return migrate.Comparer{Forge: f}.Delta(ctx, migrate.DeltaIn{From: fromRev, To: toRev})
	}
	prefix, source, err := s.migrationsPath(ctx, f, appRepo, toRev.SHA, s.settings.Repo.Migrations[to.Repo])
	if err != nil {
		return migrate.Delta{From: fromRev, To: toRev}, err
	}
	key := migrate.DeltaKey{AppRepo: appRepo, FromSHA: fromRev.SHA, ToSHA: toRev.SHA, Prefix: prefix}
	return s.hist.cache.Delta(ctx, key, func(ctx context.Context) (migrate.Delta, error) {
		d, err := migrate.Comparer{Forge: f}.Delta(ctx, migrate.DeltaIn{From: fromRev, To: toRev, Migrations: prefix})
		if err != nil && errors.Is(err, forge.ErrUnknownRef) {
			// The revision was resolved (a label the build stamped) but the app repo does not
			// hold it — a build from a fork, or a merge ref GitHub has since dropped. Say
			// that, not "unknown". Compare cannot say which end is missing (both are one 404),
			// so name both.
			return d, fmt.Errorf("%w: %s (from the %s) or %s (from the %s) is not in %s: %w", migrate.ErrUnresolved, shortSHA(fromRev.SHA), fromRev.Source, shortSHA(toRev.SHA), toRev.Source, appRepo, err)
		}
		d.PrefixSource = source
		return d, err
	})
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// blame answers the checkout root and the ref LiveAge should blame, read from the service's
// CURRENT repo view on every call rather than a value captured once at boot — a view an F5
// refresh (or a landed promotion) just replaced is picked up immediately. For a RepoFromOrigin
// view, RepoView.SHA is already the exact HEAD that view's directory was checked out to, so no
// git call is needed; for a RepoFromClone view (no configured repo, or origin unreachable) HEAD is
// read with git and cached per view. ("", "", nil) — never an error — when the checkout has no
// resolvable HEAD at all (a brand new repo with no commits): the caller reports that as its own
// named gap rather than a blame failure.
func (s *Service) blame(ctx context.Context) (root, ref string, err error) {
	view := s.Repo()
	if view.Repo == nil {
		return "", "", fmt.Errorf("live age: no repo loaded")
	}
	root = view.Repo.Root
	if view.SHA != "" {
		return root, view.SHA, nil
	}
	s.hist.mu.Lock()
	if s.hist.blameFor == view.Repo {
		ref = s.hist.blameSHA
		s.hist.mu.Unlock()
		return root, ref, nil
	}
	s.hist.mu.Unlock()
	sha, ok, gerr := s.Git().RevParse(ctx, root, "HEAD")
	if gerr != nil {
		return root, "", gerr
	}
	if ok {
		ref = sha
	}
	s.hist.mu.Lock()
	s.hist.blameFor, s.hist.blameSHA = view.Repo, ref
	s.hist.mu.Unlock()
	return root, ref, nil
}

// LiveAge dates one manifest occurrence in the gitops repo: when the line last changed. It blames
// the gitops repo's own forge, which needs no app mapping at all — a repo with an empty
// repos[].apps still gets "declares v1 · since 4 weeks ago".
func (s *Service) LiveAge(ctx context.Context, occ gitops.Occurrence) (migrate.LineAge, error) {
	github := ""
	if s.settings.Repo != nil {
		github = s.settings.Repo.GitHub
	}
	f, err := s.ForgeFor(github)
	if err != nil {
		return migrate.LineAge{}, fmt.Errorf("live age needs the gitops repo's forge: %w", err)
	}
	repoRoot, blameRef, err := s.blame(ctx)
	if err != nil {
		return migrate.LineAge{}, err
	}
	if blameRef == "" {
		return migrate.LineAge{}, fmt.Errorf("live age: the checkout at %s has no resolvable HEAD", repoRoot)
	}
	ages, err := migrate.Blamer{Forge: f}.LiveAge(ctx, migrate.LiveAgeIn{Ref: blameRef, FallbackRef: s.settings.Base, Path: occ.File, Lines: []int{occ.Line}})
	if err != nil {
		return migrate.LineAge{}, err
	}
	age, ok := ages[occ.Line]
	if !ok {
		return migrate.LineAge{}, fmt.Errorf("live age: %s has no line %d at %s", occ.File, occ.Line, blameRef)
	}
	return age, nil
}
