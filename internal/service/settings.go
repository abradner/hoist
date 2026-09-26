package service

import (
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
)

// Settings is what one invocation resolved from flags and the config file, once, at the
// boundary — cmd/hoist's own settingsFor builds it from the effective flag/config precedence
// selectRepo already computes; the TUI shares the exact same struct rather than rebuilding an
// equivalent one, which is what let the two faces' kube-context fallback drift into five
// separate copies (AGENTS.md's Divergences).
type Settings struct {
	RepoDir, AppsRoot string
	Base              string
	Promotable        []string

	// KubeContext is the already-reconciled context for the CURRENTLY selected repo: the
	// flag when given, else Repo's own kube.context, else "" (the kubeconfig's current
	// context). Every cluster-touching call for THIS run reads this field directly — never
	// re-derives it from Repo.Kube.Context itself, which is what collapses cmd/hoist's five
	// duplicate fallbacks (resolution.go, deploy.go, restart.go, history.go, main.go) into
	// the one place selectRepo already computes it (settingsFor, cmd/hoist).
	KubeContext string
	// KubeOverride is the flag alone, never a config fallback — what `hoist promotions`/
	// `hoist resume --env` and the TUI's in-flight pane use to observe a promotion against a
	// DIFFERENT repo's own state file, where Repo (below) does not name that other repo's
	// config entry. KubeContextFor is how a caller applies it against some other RepoConfig.
	KubeOverride string

	// Repo is the selected repo's own config entry, nil when running on flags alone (no
	// repos[] configured, or --repo named an unconfigured path). Config is the whole loaded
	// file, needed for cfg.Registries and for repoConfigFor-style repo lookups by GitHub name
	// when observing some other promotion's state.
	Repo   *config.RepoConfig
	Config *config.Config

	// Resolve is the digest-resolution chain for THIS run: order, registry auth, cluster
	// secret, op ref (NewResolveOptions, resolve.go). Added in PR B, alongside the rest of
	// the resolution machinery it was deferred with in PR A's own doc comment.
	Resolve ResolveOptions

	Poll     engine.PollIntervals
	Deadline time.Duration
	Retain   time.Duration
}

// ProductionEnvs is Repo.Envs.Production, unfiltered — flags-only runs (Repo nil) have no
// production list at all, which AGENTS.md §4.5 treats as "nothing is known to be
// production", never as "everything is", since a flags-only invocation has no config to have
// declared one.
func (s Settings) ProductionEnvs() []string {
	if s.Repo == nil {
		return nil
	}
	return s.Repo.Envs.Production
}

// KubeContextFor is the one precedence a caller applies when it holds some OTHER RepoConfig
// than the currently selected one (an in-flight promotion for a different repos[] entry): the
// operator's own --kube-context override wins outright, exactly as it does for the selected
// repo in settingsFor; otherwise that repo's own configured context.
func (s Settings) KubeContextFor(rc config.RepoConfig) string {
	return KubeContextFor(rc, s.KubeOverride)
}

// KubeContextFor is the free-function form of the same precedence, for a caller that has not
// built a Settings at all (cmd/hoist's own test-only buildResolveFunc entry point) but still
// must not read rc.Kube.Context directly — every kube-context fallback in this codebase goes
// through this one function so the precedence can never drift between callers again.
func KubeContextFor(rc config.RepoConfig, override string) string {
	if override != "" {
		return override
	}
	return rc.Kube.Context
}
