package service

import (
	"sync"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/rollout"
)

// Service is the lazy, memoized client cache built over Deps. A CLI command builds one per
// invocation (its clients never outlive the command); the TUI builds exactly one for the
// whole session, which is why memoization has to be success-only (a client that failed to
// build stays worth retrying on the next call — a `hoist watch`-style boot failure should
// never wedge every later attempt for the rest of the session) and why Git/Cluster are never
// memoized at all: Git carries no per-target state to cache (git.Exec{} is stateless, and a
// test swaps newGit mid-run — abandon_test), and Cluster is opened fresh per call today (the
// drift column already does this; caching it would be a behavior change).
type Service struct {
	settings Settings
	deps     Deps

	mu       sync.Mutex
	forges   map[string]forge.Forge
	argos    map[string]argo.Argo
	rollouts map[string]rollout.Rollout

	// cleanupKept remembers, for this process, a landed promotion whose cleanup was refused or
	// failed AFTER origin was asked about it, keyed by id with the local branch tip it was
	// judged at (cleanup.go). Without it a listing that polls would pay that origin traffic
	// again on every poll for as long as the reason stands.
	cleanupMu   sync.Mutex
	cleanupKept map[string]keptCleanup

	view RepoView
	// refreshMu serializes LoadRepo(RepoFromOrigin) against itself, WITHIN this one process:
	// it removes and recreates one fixed cached worktree (repoViewDir, keyed only by the clone
	// path), so two concurrent refreshes racing against that same directory can corrupt it
	// (index.lock contention, a worktree registration torn between the two). The TUI's own
	// matrix already avoids issuing two concurrent asks (askRepoRefresh's
	// refreshingRepo/refreshAgain coalescing), but that is UI-level politeness, not the actual
	// guarantee: a completion-triggered refresh and an F5 the operator
	// presses in the same instant both reach LoadRepo directly, and this mutex is what makes
	// two such loads inside one running `hoist` unable to run at once — see AGENTS.md §8's
	// deletion test: the matrix's own guard could be deleted without this directory becoming
	// corruptible from those two callers, which is what makes it politeness rather than a
	// second copy of the same enforcement. A `sync.Mutex` cannot reach across processes: a
	// separate `hoist` CLI invocation racing a running TUI session against the
	// same clone is a DIFFERENT `Service` in a different process, with its own `refreshMu`, and
	// is not covered by this at all — that race is still open (docs/repo-map.md's own risk
	// register is where a fix for it would be tracked, not this comment).
	refreshMu sync.Mutex
}

// New builds a Service from settings and deps. Nothing here calls out to git, the forge or
// the cluster — every client is built lazily, on first use, through the matching provider
// method below.
func New(settings Settings, deps Deps) *Service {
	return &Service{
		settings: settings,
		deps:     deps,
		forges:   map[string]forge.Forge{},
		argos:    map[string]argo.Argo{},
		rollouts: map[string]rollout.Rollout{},
	}
}

// Settings returns the Settings s was built with.
func (s *Service) Settings() Settings { return s.settings }

// Git returns a git client. Never memoized (see Service's own doc comment) — s.deps.Git is
// called every time.
func (s *Service) Git() git.Git { return s.deps.Git() }

// ForgeFor returns a forge client for ownerRepo, memoized on success: once a forge for
// ownerRepo has been built, every later call for the same ownerRepo returns the same value
// without calling Deps.Forge again. A failed build is never cached, so the next call retries
// it (a lapsed `gh` login fixed mid-session should not need a restart).
func (s *Service) ForgeFor(ownerRepo string) (forge.Forge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deps.NoCache {
		if f, ok := s.forges[ownerRepo]; ok {
			return f, nil
		}
	}
	f, err := s.deps.Forge(ownerRepo)
	if err != nil {
		return nil, err
	}
	if !s.deps.NoCache {
		s.forges[ownerRepo] = f
	}
	return f, nil
}

// Argo returns an Argo client for kubeContext, memoized on success exactly as ForgeFor is.
func (s *Service) Argo(kubeContext string) (argo.Argo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deps.NoCache {
		if a, ok := s.argos[kubeContext]; ok {
			return a, nil
		}
	}
	a, _, err := s.deps.Argo(kubeContext)
	if err != nil {
		return nil, err
	}
	if !s.deps.NoCache {
		s.argos[kubeContext] = a
	}
	return a, nil
}

// Rollout returns a rollout client for kubeContext, memoized on success exactly as ForgeFor
// and Argo are.
func (s *Service) Rollout(kubeContext string) (rollout.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deps.NoCache {
		if ro, ok := s.rollouts[kubeContext]; ok {
			return ro, nil
		}
	}
	ro, _, err := s.deps.Rollout(kubeContext)
	if err != nil {
		return nil, err
	}
	if !s.deps.NoCache {
		s.rollouts[kubeContext] = ro
	}
	return ro, nil
}

// Cluster returns a cluster client for kubeContext, and the kubeconfig context name it
// actually used (an empty kubeContext resolves to the kubeconfig's current context, which
// callers report by name, never an address — AGENTS.md §4.4/§4.10). Never memoized: the
// matrix's drift column already opens one per call, and every env it asks about in one pass
// shares the same kubeContext, so memoizing here would only save a connection this method
// does not itself hold open anyway.
func (s *Service) Cluster(kubeContext string) (k8s.Cluster, string, error) {
	return s.deps.Cluster(kubeContext)
}

// ArgoRolloutFor builds the Argo and rollout clients for rc's own kube context via deps — what
// `hoist promotions`/`hoist resume` need to observe or drive a promotion that may belong to a
// DIFFERENT repos[] entry than the one a long-lived Service was built for, so the kube context
// has to be read from rc, not from a Settings field. kubeOverride is the operator's own
// --kube-context, applied through KubeContextFor exactly as it is for the currently selected
// repo — the one rule every face that lists or resumes promotions applies, so a state file
// from another repo is observed and driven against the cluster the operator named, not that
// repo's default.
//
// A free function over Deps, not a Service method, because cmd/hoist's own callers
// (cmd/hoist's runPromotions/runResume) build one of these per
// promotion in a loop over possibly-different repos[] entries — exactly what buildArgoRolloutIn
// (cmd/hoist/resume.go) did before this move, unmemoized, since each call may target a
// different cluster context. A Service that already holds the right Settings can still get the
// same answer via its own ArgoRolloutFor method below.
func ArgoRolloutFor(deps Deps, rc config.RepoConfig, kubeOverride string) (argo.Argo, rollout.Rollout, error) {
	kctx := KubeContextFor(rc, kubeOverride)
	a, _, err := deps.Argo(kctx)
	if err != nil {
		return nil, nil, err
	}
	ro, _, err := deps.Rollout(kctx)
	if err != nil {
		return nil, nil, err
	}
	return a, ro, nil
}

// ArgoRolloutFor is the Service-method form of the free function above, using s's own Deps.
func (s *Service) ArgoRolloutFor(rc config.RepoConfig, kubeOverride string) (argo.Argo, rollout.Rollout, error) {
	return ArgoRolloutFor(s.deps, rc, kubeOverride)
}

// RepoConfigFor finds cfg's repos[] entry whose GitHub name matches repoFullName — how both
// `hoist promotions` and `hoist resume` locate the credential/CloneDir context a stored
// PromotionState doesn't carry a config reference for (state files are repo-agnostic beyond
// RepoFullName itself, on purpose — AGENTS.md §4.1: the state file is an index, not a second
// copy of config).
//
// Moved from cmd/hoist/resume.go's repoConfigFor unchanged.
func RepoConfigFor(cfg *config.Config, repoFullName string) (config.RepoConfig, bool) {
	if cfg == nil {
		return config.RepoConfig{}, false
	}
	for _, r := range cfg.Repos {
		if r.GitHub == repoFullName {
			return r, true
		}
	}
	return config.RepoConfig{}, false
}
