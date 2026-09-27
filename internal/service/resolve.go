package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
)

// ResolveOptions is what the flags and the config agree digest resolution does — the moved
// form of cmd/hoist's own resolveOptions (F4). Auth, ClusterSecret and OpRef are only ever the
// *explicit* --registry-auth/--cluster-secret/--op-ref override, applied identically to every
// repo when given; when they are not given (the zero value), each image repo's own
// registries[] entry decides instead (registryEntryFor/entryAuthConfig, below).
type ResolveOptions struct {
	KubeContext   string
	Order         []resolve.Source
	Auth          []registry.AuthSource // "" override; nil = per-repo config decides
	ClusterSecret string                // "" = per-repo config decides
	OpRef         string                // "" = per-repo config decides
	Registries    []config.RegistryConfig
}

// NewResolveOptions applies the precedence: a flag given wins outright, for every repo; else
// the selected repo's config (kube.context, digest_sources); registry credentials (auth,
// cluster, op) are resolved later, per image repo, in resolveImages — never here as one merged
// chain (F4). digestSources "none" turns resolution off (M1's plan). Moved from cmd/hoist's own
// resolutionOptions unchanged in behavior; kubeContext replaces resolveFlags.kubeContext since
// this package does not know that cmd-only type.
func NewResolveOptions(cfg *config.Config, rc *config.RepoConfig, digestSources, registryAuth, clusterSecret, opRef, kubeContext string) (ResolveOptions, error) {
	var opts ResolveOptions
	if cfg != nil {
		opts.Registries = cfg.Registries
	}

	opts.KubeContext = kubeContext
	if rc != nil {
		opts.KubeContext = KubeContextFor(*rc, kubeContext)
	}

	sources := splitList(digestSources)
	switch {
	case digestSources == "" && rc != nil:
		sources = rc.DigestSources
	case digestSources == "":
		sources = []string{"pods", "manifest", "registry"}
	}
	if len(sources) != 1 || sources[0] != "none" {
		order, err := resolve.ParseOrder(sources)
		if err != nil {
			return opts, fmt.Errorf("--digest-sources: %w", err)
		}
		if len(order) == 0 {
			return opts, fmt.Errorf("--digest-sources: empty; use none to plan without resolution")
		}
		opts.Order = order
	}

	if registryAuth != "" {
		auth, err := registry.ParseAuthOrder(splitList(registryAuth))
		if err != nil {
			return opts, fmt.Errorf("--registry-auth: %w", err)
		}
		if len(auth) == 0 {
			return opts, fmt.Errorf("--registry-auth: empty; list at least one of env, keychain, cluster, op")
		}
		opts.Auth = auth
	}

	opts.ClusterSecret = clusterSecret
	if opts.ClusterSecret != "" {
		ns, name, ok := strings.Cut(opts.ClusterSecret, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return opts, fmt.Errorf("--cluster-secret: want namespace/name, got %q", opts.ClusterSecret)
		}
	}
	opts.OpRef = opRef
	if opts.OpRef != "" && !strings.HasPrefix(opts.OpRef, "op://") {
		return opts, fmt.Errorf("--op-ref: want an op://vault/item/field reference")
	}
	return opts, nil
}

// splitList splits a comma-separated flag value, trimming blanks and dropping empty entries —
// the service package's own copy of cmd/hoist's identical helper (both packages need it; it is
// three lines, and depending on cmd/hoist from service would invert AGENTS.md §4.8's boundary).
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// has reports whether xs contains x — a tiny generic helper shared by entryAuthConfig and
// resolveImages below.
func has[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// registryEntryFor selects, for one image repo, the registries[] entry that covers it — the
// longest Prefix that is a prefix of repo (gitops.MatchesPrefix, never a raw strings.HasPrefix:
// a bare-host prefix like "ghcr.io" must not match "ghcr.io.attacker.example/..."). nil means no
// entry covers repo; that repo still resolves through the registry source, but with the default
// auth chain and no cluster or op link — a config entry scoped to one prefix must never lend its
// credentials to a repo it does not cover.
func registryEntryFor(registries []config.RegistryConfig, repo string) *config.RegistryConfig {
	var best *config.RegistryConfig
	for i := range registries {
		r := &registries[i]
		if gitops.MatchesPrefix(repo, r.Prefix) {
			if best == nil || len(r.Prefix) > len(best.Prefix) {
				best = r
			}
		}
	}
	return best
}

// entryAuthConfig applies the flag-override-else-entry-else-default precedence, for one
// registries[] entry (nil for a repo no entry covers). An explicit flag wins outright, the same
// value for every repo; otherwise the entry decides, and only that entry's own values — never
// another entry's, and never anything beyond the default auth order for a repo no entry covers.
func entryAuthConfig(e *config.RegistryConfig, opts ResolveOptions) (auth []registry.AuthSource, clusterSecret, opRef string) {
	auth = opts.Auth
	if len(auth) == 0 {
		if e != nil {
			// e.Auth is config-validated (internal/config.Validate): ParseAuthOrder cannot
			// fail on it.
			auth, _ = registry.ParseAuthOrder(e.Auth)
		} else {
			auth = registry.DefaultAuthOrder
		}
	}
	clusterSecret = opts.ClusterSecret
	if clusterSecret == "" && e != nil && e.Cluster.Namespace != "" {
		clusterSecret = e.Cluster.Namespace + "/" + e.Cluster.Secret
	}
	opRef = opts.OpRef
	if opRef == "" && e != nil {
		opRef = e.Op
	}
	return auth, clusterSecret, opRef
}

// Resolution is one Plan call's resolution section: which sources and credential chain were
// asked, what actually authenticated, and every resolved repo — the same facts `hoist plan
// --dry-run` prints and the TUI plan screen renders (AGENTS.md §4.4/§4.10: by name only, never a
// value). The moved and renamed form of cmd/hoist's own resolutionReport; nil means "digest
// sources: none" throughout PlannedChange.
type Resolution struct {
	Order       []resolve.Source
	AuthTried   []registry.AuthSource // the configured chain, for the "all failed" label
	KubeContext string                // "" when no cluster was opened
	AuthUsed    string                // "" when no credential source authenticated
	Consulted   bool                  // true when the registry was asked at all (win or lose)
	Res         map[string]resolve.Resolution
}

// digests merges the resolutions into BuildPlan's digests argument. Caller overrides are passed
// through verbatim — including one BuildPlan will refuse — so the refusal stays BuildPlan's.
func (rep *Resolution) digests(overrides map[string]image.Ref) map[string]image.Ref {
	out := resolve.Digests(rep.Res)
	for repo, ref := range overrides {
		out[repo] = ref
	}
	return out
}

// reasons is digests' companion for gitops.BuildPlanWith: where each override came from, so the
// plan's source-disagrees warning names the pods rather than calling a pod-resolved ref
// caller-supplied. A nil report has no reasons, and BuildPlanWith's default covers the --digest
// flag.
func (rep *Resolution) reasons() map[string]string {
	if rep == nil {
		return nil
	}
	return resolve.Reasons(rep.Res)
}

// multiRegistry is the registry.Registry Plan hands to resolve.Resolve: it implements
// registry.PerRepo so each image repo is asked through the registries[] entry that actually
// covers it (F4), never a client built for a different entry or for none. byRepo is built once,
// eagerly, before Resolve runs — registry.New performs no I/O of its own, so building an entry's
// Client the run turns out not to need costs nothing; only a request through it would, and
// Resolve only ever requests the repos it was asked to resolve.
type multiRegistry struct {
	byRepo  map[string]registry.Registry
	primary registry.Registry // the first Client built; only its auth order is reported when every source fails
}

func (m *multiRegistry) ForRepo(repo string) registry.Registry { return m.byRepo[repo] }

func (m *multiRegistry) Head(ctx context.Context, ref image.Ref) (string, error) {
	r := m.ForRepo(ref.Repo)
	if r == nil {
		return "", fmt.Errorf("registry: no registry configured for %s", ref.Repo)
	}
	return r.Head(ctx, ref)
}

func (m *multiRegistry) Tags(ctx context.Context, repo string) ([]string, error) {
	r := m.ForRepo(repo)
	if r == nil {
		return nil, fmt.Errorf("registry: no registry configured for %s", repo)
	}
	return r.Tags(ctx, repo)
}

func (m *multiRegistry) Config(ctx context.Context, ref image.Ref) (registry.ImageMeta, error) {
	r := m.ForRepo(ref.Repo)
	if r == nil {
		return registry.ImageMeta{}, fmt.Errorf("registry: no registry configured for %s", ref.Repo)
	}
	return r.Config(ctx, ref)
}

func (m *multiRegistry) AuthSourceUsed() string {
	clients := m.distinctClients()
	if len(clients) == 1 {
		if ar, ok := clients[0].(registry.AuthReporter); ok {
			return ar.AuthSourceUsed()
		}
		return ""
	}
	var parts []string
	for i, c := range clients {
		if ar, ok := c.(registry.AuthReporter); ok {
			if used := ar.AuthSourceUsed(); used != "" {
				parts = append(parts, fmt.Sprintf("entry %d: %s", i+1, used))
			}
		}
	}
	return strings.Join(parts, "; ")
}

func (m *multiRegistry) Consulted() bool {
	for _, c := range m.distinctClients() {
		if ar, ok := c.(registry.AuthReporter); ok && ar.Consulted() {
			return true
		}
	}
	return false
}

func (m *multiRegistry) distinctClients() []registry.Registry {
	seen := map[registry.Registry]bool{}
	var out []registry.Registry
	repos := make([]string, 0, len(m.byRepo))
	for repo := range m.byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		c := m.byRepo[repo]
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// resolveImages builds only the adaptors s.settings.Resolve calls for — the cluster when pods
// are a source or some matched registries[] entry's cluster credential is configured, the
// registry when it is a source — then resolves the source env's promotable occurrences. The
// registries[] entry for the registry source is chosen per image repo (F4): see
// registryEntryFor and entryAuthConfig. Moved from cmd/hoist's own runResolution; s.deps.Cluster/
// Registry replace the package-level newCluster/newRegistry test seams.
func (s *Service) resolveImages(ctx context.Context, r *gitops.Repo, from string, overrides map[string]image.Ref) (*Resolution, error) {
	opts := s.settings.Resolve
	prefixes := s.settings.Promotable
	rep := &Resolution{Order: opts.Order, AuthTried: opts.Auth}
	wantPods := has(opts.Order, resolve.SourcePods)
	wantRegistry := has(opts.Order, resolve.SourceRegistry)

	var occ []gitops.Occurrence
	if env, ok := r.Envs[from]; ok {
		for _, f := range env.Families {
			for _, o := range f.Occurrences {
				if gitops.IsPromotable(o.Ref.Repo, prefixes) {
					occ = append(occ, o)
				}
			}
		}
	}

	// The registries[] entry per repo, by longest matching prefix — nil for a repo no entry
	// covers. Several repos may map to the same entry (or to nil).
	entryFor := map[string]*config.RegistryConfig{}
	if wantRegistry {
		for _, o := range occ {
			if _, ok := entryFor[o.Ref.Repo]; !ok {
				entryFor[o.Ref.Repo] = registryEntryFor(opts.Registries, o.Ref.Repo)
			}
		}
	}

	clusterAuth := false
	for _, e := range entryFor {
		auth, clusterSecret, _ := entryAuthConfig(e, opts)
		if has(auth, registry.AuthCluster) && clusterSecret != "" {
			clusterAuth = true
			break
		}
	}

	var cluster k8s.Cluster
	if wantPods || clusterAuth {
		c, kctx, err := s.deps.Cluster(opts.KubeContext)
		if err != nil {
			return nil, err
		}
		cluster, rep.KubeContext = c, kctx
	}

	var reg registry.Registry
	if wantRegistry && len(entryFor) > 0 {
		mr := &multiRegistry{byRepo: map[string]registry.Registry{}}
		built := map[*config.RegistryConfig]registry.Registry{}
		var primaryAuth []registry.AuthSource
		// Sorted, so which entry becomes mr.primary — and therefore which auth order the
		// "all auth sources failed" diagnostic names when several entries all fail — is the
		// same on every run.
		repos := make([]string, 0, len(entryFor))
		for repo := range entryFor {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			e := entryFor[repo]
			c, ok := built[e]
			if !ok {
				auth, clusterSecret, opRef := entryAuthConfig(e, opts)
				cfg := registry.AuthConfig{Order: auth, OpRef: opRef}
				if clusterSecret != "" && has(auth, registry.AuthCluster) {
					cfg.ClusterSecret, cfg.Cluster = clusterSecret, cluster
				}
				var err error
				c, err = s.deps.Registry(cfg)
				if err != nil {
					return nil, err
				}
				built[e] = c
				if mr.primary == nil {
					mr.primary, primaryAuth = c, auth
				}
			}
			mr.byRepo[repo] = c
		}
		reg = mr
		if len(rep.AuthTried) == 0 {
			rep.AuthTried = primaryAuth
		}
	}

	res, err := resolve.Resolve(ctx, resolve.Input{Namespace: from, Occurrences: occ, Order: opts.Order, Overrides: overrides}, cluster, reg)
	if err != nil {
		return nil, err
	}
	rep.Res = res
	if reg != nil {
		if ar, ok := reg.(registry.AuthReporter); ok {
			rep.AuthUsed, rep.Consulted = ar.AuthSourceUsed(), ar.Consulted()
		}
	}
	return rep, nil
}

// RegistryFor builds a registry.Registry client for one image repo, scoped to whichever
// registries[] entry covers it (or the default chain, for one none does) — the credential
// lookup M6's tag picker and commit history need for a direct call that skips Plan's own
// resolution entirely (AGENTS.md §4.10: F4's per-repo scoping applies here too, never a merged
// chain). Moved from cmd/hoist's own buildTagsFunc, which built this inline per call.
func (s *Service) RegistryFor(imageRepo string) (registry.Registry, error) {
	var registries []config.RegistryConfig
	if s.settings.Config != nil {
		registries = s.settings.Config.Registries
	}
	entry := registryEntryFor(registries, imageRepo)
	auth, clusterSecret, opRef := entryAuthConfig(entry, s.settings.Resolve)
	cfg := registry.AuthConfig{Order: auth, OpRef: opRef}
	if clusterSecret != "" && has(auth, registry.AuthCluster) {
		if cluster, _, err := s.deps.Cluster(s.settings.KubeContext); err == nil {
			cfg.ClusterSecret, cfg.Cluster = clusterSecret, cluster
		}
		// An unreachable cluster here just means the cluster credential source will itself
		// fail and the chain falls through to the next one (pkg/registry's own documented
		// behavior) — never a reason to fail building the client.
	}
	return s.deps.Registry(cfg)
}
