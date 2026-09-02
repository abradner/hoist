package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
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

// resolveOptions is what the flags and the config agree resolution does.
type resolveOptions struct {
	kubeContext   string
	order         []resolve.Source
	auth          []registry.AuthSource
	clusterSecret string
	opRef         string
}

// resolutionOptions applies the precedence: a flag given wins; else the selected repo's
// config (kube.context, digest_sources) and the registries[] entry whose prefix covers a
// promotable prefix (auth, cluster, op); else the defaults. --digest-sources none turns
// resolution off, which is exactly M1's plan.
func resolutionOptions(cfg *config.Config, rc *config.RepoConfig, prefixes []string, f resolveFlags) (resolveOptions, error) {
	var opts resolveOptions
	var reg *config.RegistryConfig
	if cfg != nil {
		reg = registryFor(cfg, prefixes)
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

	authNames := splitList(f.registryAuth)
	switch {
	case f.registryAuth == "" && reg != nil:
		authNames = reg.Auth
	case f.registryAuth == "":
		authNames = []string{"env", "keychain", "cluster", "op"}
	}
	auth, err := registry.ParseAuthOrder(authNames)
	if err != nil {
		return opts, fmt.Errorf("--registry-auth: %w", err)
	}
	if len(auth) == 0 {
		return opts, fmt.Errorf("--registry-auth: empty; list at least one of env, keychain, cluster, op")
	}
	opts.auth = auth

	opts.clusterSecret = f.clusterSecret
	if opts.clusterSecret == "" && reg != nil && reg.Cluster.Namespace != "" {
		opts.clusterSecret = reg.Cluster.Namespace + "/" + reg.Cluster.Secret
	}
	if opts.clusterSecret != "" {
		ns, name, ok := strings.Cut(opts.clusterSecret, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return opts, fmt.Errorf("--cluster-secret: want namespace/name, got %q", opts.clusterSecret)
		}
	}
	opts.opRef = f.opRef
	if opts.opRef == "" && reg != nil {
		opts.opRef = reg.Op
	}
	if opts.opRef != "" && !strings.HasPrefix(opts.opRef, "op://") {
		return opts, fmt.Errorf("--op-ref: want an op://vault/item/field reference")
	}
	return opts, nil
}

// registryFor picks the registries[] entry for the promotable prefixes: the first whose
// prefix covers one of them (or is covered by one), else nil.
func registryFor(cfg *config.Config, prefixes []string) *config.RegistryConfig {
	for i := range cfg.Registries {
		r := &cfg.Registries[i]
		for _, p := range prefixes {
			if strings.HasPrefix(p, r.Prefix) || strings.HasPrefix(r.Prefix, p) {
				return r
			}
		}
	}
	return nil
}

// resolutionReport is the resolution section of the plan output.
type resolutionReport struct {
	order       []resolve.Source
	kubeContext string // "" when no cluster was opened
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

// runResolution builds only the adaptors the options call for — the cluster when pods
// are a source or the registry's cluster credential is configured, the registry when it
// is a source — then resolves the source env's promotable occurrences.
func runResolution(ctx context.Context, r *gitops.Repo, from string, prefixes []string, opts resolveOptions, overrides map[string]image.Ref) (*resolutionReport, error) {
	rep := &resolutionReport{order: opts.order}
	wantPods := has(opts.order, resolve.SourcePods)
	wantRegistry := has(opts.order, resolve.SourceRegistry)
	clusterAuth := wantRegistry && has(opts.auth, registry.AuthCluster) && opts.clusterSecret != ""

	var cluster k8s.Cluster
	if wantPods || clusterAuth {
		var err error
		cluster, rep.kubeContext, err = newCluster(opts.kubeContext)
		if err != nil {
			return nil, err
		}
	}
	if wantRegistry {
		cfg := registry.AuthConfig{Order: opts.auth, OpRef: opts.opRef}
		if clusterAuth {
			cfg.ClusterSecret, cfg.Cluster = opts.clusterSecret, cluster
		}
		reg, err := newRegistry(cfg)
		if err != nil {
			return nil, err
		}
		rep.registry = reg
	}

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
	used := ""
	if ar, ok := rep.registry.(registry.AuthReporter); ok && rep.registry != nil {
		used = ar.AuthSourceUsed()
	}
	if used == "" {
		parts = append(parts, "registry not consulted")
	} else {
		parts = append(parts, "registry auth: "+used)
	}
	fmt.Fprintf(w, "Resolution (%s):\n", strings.Join(parts, "; "))
	for _, repo := range resolve.Repos(rep.res) {
		r := rep.res[repo]
		if !r.Resolved() {
			fmt.Fprintf(w, "  %s  unresolved; see warnings\n", repo)
			continue
		}
		fmt.Fprintf(w, "  %s  [%s] %s\n", r.Ref, r.Source, r.Detail)
		for _, a := range r.Alternatives {
			fmt.Fprintf(w, "    alternative: %s\n", a)
		}
	}
	fmt.Fprintln(w)
}
