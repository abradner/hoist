package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
)

// The adaptor constructors are variables so tests substitute fakes: no test points a
// client at a cluster or a registry.
var (
	newCluster  = k8s.NewCluster
	newRegistry = func(cfg registry.AuthConfig) (registry.Registry, error) { return registry.New(cfg) }
)

// resolveFlags are the plan flags that shape digest resolution, as given ("" = not given).
type resolveFlags struct {
	kubeContext, digestSources, registryAuth, clusterSecret, opRef string
}

// resolveOptions is what the flags and the config agree resolution does. auth,
// clusterSecret and opRef are only ever the *explicit* --registry-auth/--cluster-secret/
// --op-ref override, applied identically to every repo when given — the operator naming
// the chain outright. When they are not given (the zero value), each image repo's own
// registries[] entry decides instead: see registryEntryFor and entryAuthConfig in
// runResolution. Before F4 this struct carried one merged chain, derived from a single
// registries[] entry chosen for the whole run and then applied to every promotable
// repo — which is exactly the bug: a repo a different entry covered, or none did, got
// that one entry's credentials, op ref included.
type resolveOptions struct {
	kubeContext   string
	order         []resolve.Source
	auth          []registry.AuthSource // "" override; nil = per-repo config decides
	clusterSecret string                // "" = per-repo config decides
	opRef         string                // "" = per-repo config decides
	registries    []config.RegistryConfig
}

// resolutionOptions applies the precedence: a flag given wins outright, for every repo;
// else the selected repo's config (kube.context, digest_sources); registry credentials
// (auth, cluster, op) are resolved later, per image repo, in runResolution — never here
// as one merged chain (F4). --digest-sources none turns resolution off, which is exactly
// M1's plan.
func resolutionOptions(cfg *config.Config, rc *config.RepoConfig, f resolveFlags) (resolveOptions, error) {
	var opts resolveOptions
	if cfg != nil {
		opts.registries = cfg.Registries
	}

	opts.kubeContext = f.kubeContext
	if opts.kubeContext == "" && rc != nil {
		opts.kubeContext = rc.Kube.Context
	}

	sources := splitList(f.digestSources)
	switch {
	case f.digestSources == "" && rc != nil:
		sources = rc.DigestSources
	case f.digestSources == "":
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
		opts.order = order
	}

	if f.registryAuth != "" {
		auth, err := registry.ParseAuthOrder(splitList(f.registryAuth))
		if err != nil {
			return opts, fmt.Errorf("--registry-auth: %w", err)
		}
		if len(auth) == 0 {
			return opts, fmt.Errorf("--registry-auth: empty; list at least one of env, keychain, cluster, op")
		}
		opts.auth = auth
	}

	opts.clusterSecret = f.clusterSecret
	if opts.clusterSecret != "" {
		ns, name, ok := strings.Cut(opts.clusterSecret, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return opts, fmt.Errorf("--cluster-secret: want namespace/name, got %q", opts.clusterSecret)
		}
	}
	opts.opRef = f.opRef
	if opts.opRef != "" && !strings.HasPrefix(opts.opRef, "op://") {
		return opts, fmt.Errorf("--op-ref: want an op://vault/item/field reference")
	}
	return opts, nil
}

// registryEntryFor selects, for one image repo, the registries[] entry that covers it —
// the longest Prefix that is a prefix of repo. nil means no entry covers repo; that repo
// still resolves through the registry source, but with the default auth chain and no
// cluster or op link (F4): a config entry scoped to one prefix must never lend its
// credentials to a repo it does not cover.
func registryEntryFor(registries []config.RegistryConfig, repo string) *config.RegistryConfig {
	var best *config.RegistryConfig
	for i := range registries {
		r := &registries[i]
		if r.Prefix != "" && strings.HasPrefix(repo, r.Prefix) {
			if best == nil || len(r.Prefix) > len(best.Prefix) {
				best = r
			}
		}
	}
	return best
}

// entryAuthConfig applies the flag-override-else-entry-else-default precedence, for one
// registries[] entry (nil for a repo no entry covers). An explicit flag wins outright, the
// same value for every repo; otherwise the entry decides, and only that entry's own
// values — never another entry's, and never anything beyond the default auth order for a
// repo no entry covers.
func entryAuthConfig(e *config.RegistryConfig, opts resolveOptions) (auth []registry.AuthSource, clusterSecret, opRef string) {
	auth = opts.auth
	if len(auth) == 0 {
		if e != nil {
			// e.Auth is config-validated (internal/config.Validate): ParseAuthOrder
			// cannot fail on it.
			auth, _ = registry.ParseAuthOrder(e.Auth)
		} else {
			auth = registry.DefaultAuthOrder
		}
	}
	clusterSecret = opts.clusterSecret
	if clusterSecret == "" && e != nil && e.Cluster.Namespace != "" {
		clusterSecret = e.Cluster.Namespace + "/" + e.Cluster.Secret
	}
	opRef = opts.opRef
	if opRef == "" && e != nil {
		opRef = e.Op
	}
	return auth, clusterSecret, opRef
}

// resolutionReport is the resolution section of the plan output.
type resolutionReport struct {
	order       []resolve.Source
	auth        []registry.AuthSource // the configured chain, for the "all failed" label
	kubeContext string                // "" when no cluster was opened
	registry    registry.Registry
	res         map[string]resolve.Resolution
}

func has[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// multiRegistry is the registry.Registry cmd/hoist hands to resolve.Resolve: it implements
// registry.PerRepo so each image repo is asked through the registries[] entry that
// actually covers it (F4), never a client built for a different entry or for none. byRepo
// is built once, eagerly, before Resolve runs — registry.New performs no I/O of its own
// (§9 gotcha 5), so building an entry's Client the run turns out not to need costs
// nothing; only a request through it would, and Resolve only ever requests the repos it
// was asked to resolve.
type multiRegistry struct {
	byRepo  map[string]registry.Registry
	primary registry.Registry // the first Client built; used when the interface itself is called directly
}

// ForRepo implements registry.PerRepo.
func (m *multiRegistry) ForRepo(repo string) registry.Registry { return m.byRepo[repo] }

// Head and Tags satisfy registry.Registry for a caller that does not know about PerRepo.
// resolve.Resolve always calls ForRepo first when it sees PerRepo, so these are a fallback
// that should not be reached in practice.
func (m *multiRegistry) Head(ctx context.Context, ref image.Ref) (string, error) {
	if m.primary == nil {
		return "", fmt.Errorf("registry: no registry configured for %s", ref.Repo)
	}
	return m.primary.Head(ctx, ref)
}

func (m *multiRegistry) Tags(ctx context.Context, repo string) ([]string, error) {
	if m.primary == nil {
		return nil, fmt.Errorf("registry: no registry configured for %s", repo)
	}
	return m.primary.Tags(ctx, repo)
}

// AuthSourceUsed implements registry.AuthReporter by combining every distinct Client this
// run built. The common case — every promotable repo covered by the same entry, or by
// none — built exactly one Client, so this reads exactly as it did before F4; several
// distinct entries report "entry: source" per entry instead of collapsing into each
// other's answer.
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

// Consulted implements registry.AuthReporter: true when any distinct Client was asked.
func (m *multiRegistry) Consulted() bool {
	for _, c := range m.distinctClients() {
		if ar, ok := c.(registry.AuthReporter); ok && ar.Consulted() {
			return true
		}
	}
	return false
}

// distinctClients lists the Clients this run built, each once, in a stable order (the
// order their repos sort in) so AuthSourceUsed is deterministic.
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

// runResolution builds only the adaptors the options call for — the cluster when pods
// are a source or some matched registries[] entry's cluster credential is configured, the
// registry when it is a source — then resolves the source env's promotable occurrences.
// The registries[] entry for the registry source is chosen per image repo (F4): see
// registryEntryFor and entryAuthConfig.
func runResolution(ctx context.Context, r *gitops.Repo, from string, prefixes []string, opts resolveOptions, overrides map[string]image.Ref) (*resolutionReport, error) {
	rep := &resolutionReport{order: opts.order, auth: opts.auth}
	wantPods := has(opts.order, resolve.SourcePods)
	wantRegistry := has(opts.order, resolve.SourceRegistry)

	var occ []gitops.Occurrence
	if env, ok := r.Envs[from]; ok {
		for _, f := range env.Families {
			for _, o := range f.Occurrences {
				if isPromotable(o.Ref.Repo, prefixes) {
					occ = append(occ, o)
				}
			}
		}
	}

	// The registries[] entry per repo, by longest matching prefix — nil for a repo no
	// entry covers. Several repos may map to the same entry (or to nil).
	entryFor := map[string]*config.RegistryConfig{}
	if wantRegistry {
		for _, o := range occ {
			if _, ok := entryFor[o.Ref.Repo]; !ok {
				entryFor[o.Ref.Repo] = registryEntryFor(opts.registries, o.Ref.Repo)
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
		var err error
		cluster, rep.kubeContext, err = newCluster(opts.kubeContext)
		if err != nil {
			return nil, err
		}
	}

	if wantRegistry && len(entryFor) > 0 {
		mr := &multiRegistry{byRepo: map[string]registry.Registry{}}
		built := map[*config.RegistryConfig]registry.Registry{}
		var primaryAuth []registry.AuthSource
		for repo, e := range entryFor {
			c, ok := built[e]
			if !ok {
				auth, clusterSecret, opRef := entryAuthConfig(e, opts)
				cfg := registry.AuthConfig{Order: auth, OpRef: opRef}
				if clusterSecret != "" && has(auth, registry.AuthCluster) {
					cfg.ClusterSecret, cfg.Cluster = clusterSecret, cluster
				}
				var err error
				c, err = newRegistry(cfg)
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
		rep.registry = mr
		if len(rep.auth) == 0 {
			rep.auth = primaryAuth
		}
	}

	res, err := resolve.Resolve(ctx, resolve.Input{Namespace: from, Occurrences: occ, Order: opts.order, Overrides: overrides}, cluster, rep.registry)
	if err != nil {
		return nil, err
	}
	rep.res = res
	return rep, nil
}

// digests merges the resolutions into BuildPlan's digests argument. Caller overrides are
// passed through verbatim — including one BuildPlan will refuse — so the refusal stays
// BuildPlan's, exactly as in M1.
func (rep *resolutionReport) digests(overrides map[string]image.Ref) map[string]image.Ref {
	out := resolve.Digests(rep.res)
	for repo, ref := range overrides {
		out[repo] = ref
	}
	return out
}

// print renders the section: how each repo was resolved, and which cluster context and
// registry credential source were involved — by name only (AGENTS.md §4.4, R-002).
func (rep *resolutionReport) print(w io.Writer) {
	names := make([]string, 0, len(rep.order))
	for _, s := range rep.order {
		names = append(names, string(s))
	}
	parts := []string{"sources " + strings.Join(names, ",")}
	if rep.kubeContext != "" {
		parts = append(parts, "kube context "+rep.kubeContext)
	} else {
		parts = append(parts, "cluster not consulted")
	}
	var used string
	var consulted bool
	if ar, ok := rep.registry.(registry.AuthReporter); ok && rep.registry != nil {
		used, consulted = ar.AuthSourceUsed(), ar.Consulted()
	}
	switch {
	case used != "":
		parts = append(parts, "registry auth: "+used)
	case consulted:
		// The registry was asked — every link in the chain, including the anonymous
		// fallback, failed. Distinct from "not consulted" (adaptor never built, or built
		// but every repo resolved before reaching the registry in the order): a warning
		// elsewhere already says the registry was asked, so this label must agree.
		authNames := make([]string, 0, len(rep.auth))
		for _, a := range rep.auth {
			authNames = append(authNames, string(a))
		}
		parts = append(parts, "registry: consulted; all auth sources failed ("+strings.Join(authNames, ", ")+")")
	default:
		parts = append(parts, "registry not consulted")
	}
	fmt.Fprintf(w, "Resolution (%s):\n", strings.Join(parts, "; "))
	for _, repo := range resolve.Repos(rep.res) {
		r := rep.res[repo]
		if !r.Resolved() {
			fmt.Fprintf(w, "  %s  unresolved; see warnings\n", repo)
			continue
		}
		fmt.Fprintf(w, "  %s  [%s] %s\n", r.Ref, r.Source, redact.Strings(r.Detail))
		for _, a := range r.Alternatives {
			fmt.Fprintf(w, "    alternative: %s\n", a)
		}
	}
	fmt.Fprintln(w)
}
