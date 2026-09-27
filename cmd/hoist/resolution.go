package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
	"github.com/abradner/hoist/pkg/rollout"
)

// The adaptor constructors are variables so tests substitute fakes: no test points a client at
// a cluster, a registry, Argo CD or a Deployment (AGENTS.md hard constraints — never a real
// cluster or Argo CD instance). serviceDeps() (cmd/hoist/deps.go) wraps each in a closure that
// reads it at call time, so a test reassigning one mid-test still reaches every Service built
// before or after the swap.
var (
	newCluster  = k8s.NewCluster
	newRegistry = func(cfg registry.AuthConfig) (registry.Registry, error) { return registry.New(cfg) }
	newArgo     = argo.NewFromKubeconfig
	newRollout  = rollout.NewFromKubeconfig
)

// resolveFlags are the plan flags that shape digest resolution, as given ("" = not given).
type resolveFlags struct {
	kubeContext, digestSources, registryAuth, clusterSecret, opRef string
}

// emptyResolveFlag is the one refusal every face gives an explicit empty --digest-sources
// or --registry-auth: "" means "the config decides" only when the flag was not given at
// all. The root flag set (#132) and `hoist plan` both call it, so the message is the same
// by construction; the caller prefixes its own name.
func emptyResolveFlag(given map[string]bool, rf resolveFlags) (msg string, bad bool) {
	for _, f := range []struct{ name, val, hint string }{
		{"digest-sources", rf.digestSources, "use none to plan without resolution"},
		{"registry-auth", rf.registryAuth, "list at least one of env, keychain, cluster, op"},
	} {
		if given[f.name] && strings.TrimSpace(f.val) == "" {
			return fmt.Sprintf("--%s: empty; %s", f.name, f.hint), true
		}
	}
	return "", false
}

// printResolution renders the resolution section: how each repo was resolved, and which cluster
// context and registry credential source were involved — by name only (AGENTS.md §4.4, R-002).
// Moved out of internal/service's own Resolution type (a renderer, not a use-case function —
// AGENTS.md §4.3's own "pkg is activity-shaped" reasoning applies to service just as much: it
// builds a Resolution, it does not print one) — the CLI's own print, rewritten to read
// service.Resolution's plain fields rather than a registry.Registry it used to hold directly.
func printResolution(w io.Writer, rep *service.Resolution) {
	names := make([]string, 0, len(rep.Order))
	for _, s := range rep.Order {
		names = append(names, string(s))
	}
	parts := []string{"sources " + strings.Join(names, ",")}
	if rep.KubeContext != "" {
		parts = append(parts, "kube context "+rep.KubeContext)
	} else {
		parts = append(parts, "cluster not consulted")
	}
	switch {
	case rep.AuthUsed != "":
		parts = append(parts, "registry auth: "+rep.AuthUsed)
	case rep.Consulted:
		// The registry was asked — every link in the chain, including the anonymous
		// fallback, failed. Distinct from "not consulted" (adaptor never built, or built
		// but every repo resolved before reaching the registry in the order): a warning
		// elsewhere already says the registry was asked, so this label must agree.
		authNames := make([]string, 0, len(rep.AuthTried))
		for _, a := range rep.AuthTried {
			authNames = append(authNames, string(a))
		}
		parts = append(parts, "registry: consulted; all auth sources failed ("+strings.Join(authNames, ", ")+")")
	default:
		parts = append(parts, "registry not consulted")
	}
	fmt.Fprintf(w, "Resolution (%s):\n", strings.Join(parts, "; "))
	for _, repo := range resolve.Repos(rep.Res) {
		r := rep.Res[repo]
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
