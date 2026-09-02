package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// The fixture's staging pins for web and marketing.
	webPinned = "sha256:1f7e5c3a9b2d4e6f8a0c1b3d5e7f9a2b4c6d8e0f1a3b5c7d9e1f2a4b6c8d0e2f"
	mktPinned = "sha256:77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee"
)

// installFakes swaps the adaptor constructors for the test's fakes and records what they
// were asked for.
func installFakes(t *testing.T, cluster *k8s.Fake, reg *registry.Fake) (contexts *[]string, authCfgs *[]registry.AuthConfig) {
	t.Helper()
	origC, origR := newCluster, newRegistry
	t.Cleanup(func() { newCluster, newRegistry = origC, origR })
	contexts, authCfgs = &[]string{}, &[]registry.AuthConfig{}
	newCluster = func(ctx string) (k8s.Cluster, string, error) {
		*contexts = append(*contexts, ctx)
		if cluster == nil {
			return nil, "", errors.New("k8s: kube context \"" + ctx + "\" is not in the kubeconfig")
		}
		name := ctx
		if name == "" {
			name = "current-context"
		}
		return cluster, name, nil
	}
	newRegistry = func(cfg registry.AuthConfig) (registry.Registry, error) {
		*authCfgs = append(*authCfgs, cfg)
		return reg, nil
	}
	return contexts, authCfgs
}

func run3(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// --digest-sources none is M1: byte for byte, and no adaptor is even constructed.
func TestPlanDigestSourcesNoneIsByteIdenticalToM1(t *testing.T) {
	contexts, authCfgs := installFakes(t, nil, nil)
	want, err := os.ReadFile("../../testdata/golden/plan-dry-run.txt")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := run3(t, planArgs("--dry-run", "--digest-sources", "none")...)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	if out != string(want) {
		t.Errorf("output differs from M1's golden:\n--- got\n%s\n--- want\n%s", out, want)
	}
	if len(*contexts) != 0 || len(*authCfgs) != 0 {
		t.Errorf("adaptors were constructed: clusters %v, registries %d", *contexts, len(*authCfgs))
	}
	if strings.Contains(out, "Resolution") {
		t.Error("a resolution section appeared without resolution")
	}
}

// The default sources against a fake cluster: running pods pin the bare-tag repos, the
// section names the context and the source per repo, and no adaptor sees another
// namespace.
func TestPlanResolvesFromFakePods(t *testing.T) {
	cluster := &k8s.Fake{Images: map[string][]k8s.RunningImage{"app-staging": {
		{Pod: "web-1", Container: "web", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
		{Pod: "web-1", Container: "worker", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
	}}}
	reg := &registry.Fake{Auth: "keychain"}
	contexts, _ := installFakes(t, cluster, reg)
	code, out, errOut := run3(t, planArgs("--dry-run", "--kube-context", "my-cluster")...)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "Resolution (sources pods,manifest,registry; kube context my-cluster; registry not consulted):") {
		t.Errorf("section header missing or wrong:\n%s", out)
	}
	for _, want := range []string{
		"ghcr.io/example/web:v202602150930@" + digestA + "  [pods] 2 running containers agree",
		"alternative: ghcr.io/example/web:v202602150930@" + webPinned,
		"ghcr.io/example/marketing:sha-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d@" + mktPinned + "  [manifest] pinned in the manifest; no running pods",
		"[running-vs-manifest] app-staging: ghcr.io/example/web manifests pin " + webPinned + " but its pods run " + digestA,
		"+          image: ghcr.io/example/web:v202602150930@" + digestA,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "+          image: ghcr.io/example/web:v202602150930@"+digestA) != 3 {
		t.Errorf("the running digest should land on all three production web containers:\n%s", out)
	}
	if got := strings.Join(*contexts, ","); got != "my-cluster" {
		t.Errorf("kube contexts requested: %q, want my-cluster once", got)
	}
	if got := strings.Join(cluster.Calls, ","); got != "RunningImages app-staging" {
		t.Errorf("cluster calls %q, want exactly the source namespace", got)
	}
	if len(reg.Calls) != 0 {
		t.Errorf("registry consulted although every repo resolved: %v", reg.Calls)
	}
}

func TestPlanDigestOverrideBeatsPods(t *testing.T) {
	cluster := &k8s.Fake{Images: map[string][]k8s.RunningImage{"app-staging": {
		{Pod: "web-1", Container: "web", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
	}}}
	installFakes(t, cluster, &registry.Fake{})
	code, out, errOut := run3(t, planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web:v9@"+digestB)...)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	if n := strings.Count(out, "+          image: ghcr.io/example/web:v9@"+digestB); n != 3 {
		t.Errorf("override should land on all three production web containers, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "+          image: ghcr.io/example/web:v202602150930@"+digestA) {
		t.Error("the pod digest was written despite the override")
	}
	if !strings.Contains(out, "ghcr.io/example/web:v9@"+digestB+"  [override] caller-supplied digest") {
		t.Errorf("section does not show the override:\n%s", out)
	}
	// A tagless override is still BuildPlan's refusal, resolution or not.
	code, _, errOut = run3(t, planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web@"+digestB)...)
	if code != exitFailure || !strings.Contains(errOut, "a tag is required") {
		t.Errorf("tagless override: exit %d, stderr %s", code, errOut)
	}
}

// F4 regression: registryEntryFor and entryAuthConfig, together, are what used to be a
// single registryFor call applied to every repo — the bug. registryEntryFor picks by
// longest matching prefix, never the first entry that merely overlaps; entryAuthConfig
// must never leak one entry's cluster secret or op ref onto a repo a different entry (or
// no entry) covers.
func TestRegistryEntryForPicksLongestMatchNeverFirstOverlap(t *testing.T) {
	registries := []config.RegistryConfig{
		{Prefix: "ghcr.io/", Auth: []string{"env"}, Op: "op://vault/broad/field"},
		{Prefix: "ghcr.io/example/web", Auth: []string{"cluster"}, Cluster: config.ClusterSecret{Namespace: "app-staging", Secret: "web-pull"}},
	}
	cases := []struct {
		repo, wantPrefix string
	}{
		{"ghcr.io/example/web", "ghcr.io/example/web"}, // matches both; the longer, more specific entry wins
		// "ghcr.io/example/web" carries no trailing slash, so it must not cover a
		// same-string-prefixed but distinct repo path any more than a bare host prefix
		// may cover a different host (TestRegistryEntryForRejectsHostConfusion) — the
		// boundary rule in gitops.MatchesPrefix is the same rule at every path segment,
		// not a host-only special case. "web" and "webhooks" are different image repos.
		{"ghcr.io/example/webhooks", "ghcr.io/"},  // falls through to the broad entry, not the web-scoped one
		{"ghcr.io/example/marketing", "ghcr.io/"}, // only the broad entry covers it
		{"quay.io/example/other", ""},             // no entry covers it at all
	}
	for _, tc := range cases {
		e := registryEntryFor(registries, tc.repo)
		got := ""
		if e != nil {
			got = e.Prefix
		}
		if got != tc.wantPrefix {
			t.Errorf("registryEntryFor(%q) = %q, want %q", tc.repo, got, tc.wantPrefix)
		}
	}

	// entryAuthConfig: the entry decides its own values only. The broad ghcr.io/ entry's
	// op ref must never appear for a repo the specific web entry covers, and the web
	// entry's cluster secret must never appear for a repo only the broad entry covers.
	webEntry := registryEntryFor(registries, "ghcr.io/example/web")
	auth, clusterSecret, opRef := entryAuthConfig(webEntry, resolveOptions{})
	if len(auth) != 1 || auth[0] != registry.AuthCluster || clusterSecret != "app-staging/web-pull" || opRef != "" {
		t.Errorf("web entry: auth=%v clusterSecret=%q opRef=%q, want cluster/app-staging/web-pull/\"\"", auth, clusterSecret, opRef)
	}
	broadEntry := registryEntryFor(registries, "ghcr.io/example/marketing")
	auth, clusterSecret, opRef = entryAuthConfig(broadEntry, resolveOptions{})
	if len(auth) != 1 || auth[0] != registry.AuthEnv || clusterSecret != "" || opRef != "op://vault/broad/field" {
		t.Errorf("broad entry: auth=%v clusterSecret=%q opRef=%q, want env/\"\"/op://vault/broad/field", auth, clusterSecret, opRef)
	}
	// A repo no entry covers gets the default chain, and neither entry's cluster secret
	// or op ref.
	auth, clusterSecret, opRef = entryAuthConfig(nil, resolveOptions{})
	if len(auth) != len(registry.DefaultAuthOrder) || clusterSecret != "" || opRef != "" {
		t.Errorf("unmatched repo: auth=%v clusterSecret=%q opRef=%q, want the default chain and nothing else", auth, clusterSecret, opRef)
	}
	// An explicit flag overrides every entry outright, for any repo.
	auth, clusterSecret, opRef = entryAuthConfig(webEntry, resolveOptions{auth: []registry.AuthSource{registry.AuthEnv}, clusterSecret: "x/y", opRef: "op://flag/a/b"})
	if len(auth) != 1 || auth[0] != registry.AuthEnv || clusterSecret != "x/y" || opRef != "op://flag/a/b" {
		t.Errorf("flag override: auth=%v clusterSecret=%q opRef=%q", auth, clusterSecret, opRef)
	}
}

// F4 regression, end to end: two repos, each covered by a different registries[] entry,
// must resolve through two distinct Clients, each carrying only its own entry's
// credentials — never a merged chain built from whichever entry a single global lookup
// happened to pick first (the pre-fix bug: one entry's op ref and cluster secret applied
// to every promotable repo, ghcr.io/example/marketing included even though the entry that
// carried them names only ghcr.io/example/web).
func TestPlanScopesRegistryCredentialsPerRepoEntry(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cluster/apps/web-app-staging-app.yaml", argoApp("web-staging", "cluster/apps/app-staging/web", "app-staging"))
	writeFile(t, root, "cluster/apps/mkt-app-staging-app.yaml", argoApp("mkt-staging", "cluster/apps/app-staging/marketing", "app-staging"))
	writeFile(t, root, "cluster/apps/web-app-production-app.yaml", argoApp("web-production", "cluster/apps/app-production/web", "app-production"))
	writeFile(t, root, "cluster/apps/mkt-app-production-app.yaml", argoApp("mkt-production", "cluster/apps/app-production/marketing", "app-production"))
	writeFile(t, root, "cluster/apps/app-staging/web/app.yaml", deployment("ghcr.io/example/web:sha-abc123"))
	writeFile(t, root, "cluster/apps/app-staging/marketing/app.yaml", deployment("ghcr.io/example/marketing:sha-def456"))
	writeFile(t, root, "cluster/apps/app-production/web/app.yaml", deployment("ghcr.io/example/web:sha-000000@"+digestA))
	writeFile(t, root, "cluster/apps/app-production/marketing/app.yaml", deployment("ghcr.io/example/marketing:sha-000000@"+digestB))

	reg := &registry.Fake{Digests: map[string]string{
		"ghcr.io/example/web:sha-abc123":       digestA,
		"ghcr.io/example/marketing:sha-def456": digestB,
	}}
	_, authCfgs := installFakes(t, &k8s.Fake{}, reg)

	cfgPath := writeConfig(t, `
repos:
  - path: `+root+`
    promotable: [ghcr.io/example/]
registries:
  - prefix: ghcr.io/example/web
    op: op://vault/web-only/field
  - prefix: ghcr.io/example/marketing
    cluster: { namespace: app-staging, secret: mkt-pull }
`)
	code, out, errOut := run3(t, "--config", cfgPath, "plan", "--from", "app-staging", "--to", "app-production", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s\nstdout:\n%s", code, errOut, out)
	}
	if len(*authCfgs) != 2 {
		t.Fatalf("expected one Client per matched entry (2), got %d: %+v", len(*authCfgs), *authCfgs)
	}
	var webCfg, mktCfg *registry.AuthConfig
	for i := range *authCfgs {
		c := &(*authCfgs)[i]
		switch {
		case c.OpRef != "":
			webCfg = c
		case c.ClusterSecret != "":
			mktCfg = c
		}
	}
	if webCfg == nil {
		t.Fatalf("no Client carried the web entry's op ref: %+v", *authCfgs)
	}
	if webCfg.OpRef != "op://vault/web-only/field" || webCfg.ClusterSecret != "" {
		t.Errorf("web's Client leaked or lost credentials: %+v", webCfg)
	}
	if mktCfg == nil {
		t.Fatalf("no Client carried the marketing entry's cluster secret: %+v", *authCfgs)
	}
	if mktCfg.ClusterSecret != "app-staging/mkt-pull" || mktCfg.OpRef != "" {
		t.Errorf("marketing's Client leaked or lost credentials: %+v", mktCfg)
	}
}

// A scaled-to-zero repo with a bare tag goes to the registry, and the section reports
// which credential source authenticated — by name.
// F9 regression: buildResolveFunc's own comment claims "resolutionOptions runs once here
// since it does not depend on the source env", but the closure it returned called
// resolutionOptions(cfg, rc, ...) fresh on every invocation — reading rc, a pointer,
// however it stood at call time. Prove it now runs exactly once by mutating rc's
// kube.context after building the closure: with resolutionOptions computed once at build
// time, that later mutation must have no effect on the context runResolution actually
// requests.
func TestBuildResolveFuncComputesOptionsOnce(t *testing.T) {
	contexts, _ := installFakes(t, &k8s.Fake{}, &registry.Fake{})
	rc := &config.RepoConfig{DigestSources: []string{"pods"}, Kube: config.KubeConfig{Context: "first-context"}}
	fn := buildResolveFunc(&config.Config{}, rc, []string{"ghcr.io/"})

	// Mutate rc after the closure is built. A per-call resolutionOptions would pick this
	// up on the next invocation; a once-computed opts must not.
	rc.Kube.Context = "second-context"

	repo := &gitops.Repo{Root: "x", Envs: map[string]*gitops.Env{"app-staging": {Name: "app-staging"}}}
	if _, err := fn(context.Background(), repo, "app-staging"); err != nil {
		t.Fatal(err)
	}
	if _, err := fn(context.Background(), repo, "app-staging"); err != nil {
		t.Fatal(err)
	}
	for _, c := range *contexts {
		if c != "first-context" {
			t.Errorf("kube context requested = %q, want first-context on every call (resolutionOptions must run once): %v", c, *contexts)
		}
	}
	if len(*contexts) != 2 {
		t.Fatalf("expected 2 calls (one per fn invocation), got %d: %v", len(*contexts), *contexts)
	}
}

func TestPlanFallsBackToRegistryAndReportsAuthSource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cluster/apps/web-app-staging-app.yaml", argoApp("web-staging", "cluster/apps/app-staging/web", "app-staging"))
	writeFile(t, root, "cluster/apps/web-app-production-app.yaml", argoApp("web-production", "cluster/apps/app-production/web", "app-production"))
	writeFile(t, root, "cluster/apps/app-staging/web/app.yaml", deployment("ghcr.io/example/web:sha-abc123"))
	writeFile(t, root, "cluster/apps/app-production/web/app.yaml", deployment("ghcr.io/example/web:sha-000000@"+digestB))
	reg := &registry.Fake{Digests: map[string]string{"ghcr.io/example/web:sha-abc123": digestA}, Auth: "cluster secret app-staging/ghcr-pull"}
	_, authCfgs := installFakes(t, &k8s.Fake{}, reg)

	args := []string{"plan", "--repo", root, "--from", "app-staging", "--to", "app-production", "--dry-run",
		"--registry-auth", "cluster,env", "--cluster-secret", "app-staging/ghcr-pull", "--op-ref", "op://vault/item/field"}
	code, out, errOut := run3(t, args...)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	for _, want := range []string{
		"registry auth: cluster secret app-staging/ghcr-pull",
		"ghcr.io/example/web:sha-abc123@" + digestA + "  [registry] registry HEAD of tag sha-abc123",
		"+          image: ghcr.io/example/web:sha-abc123@" + digestA,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(*authCfgs) != 1 {
		t.Fatalf("registries constructed: %d", len(*authCfgs))
	}
	cfg := (*authCfgs)[0]
	if len(cfg.Order) != 2 || cfg.Order[0] != registry.AuthCluster || cfg.Order[1] != registry.AuthEnv || cfg.ClusterSecret != "app-staging/ghcr-pull" || cfg.Cluster == nil || cfg.OpRef != "op://vault/item/field" {
		t.Errorf("registry auth config = %+v", cfg)
	}
	if strings.Contains(out, "op://") {
		t.Error("the op reference reached the plan output")
	}
}

// Without --digest-sources none, a repo the fakes cannot answer is reported and the M1
// rule decides: a bare tag with a target occurrence still refuses to plan.
func TestPlanUnresolvedRepoKeepsM1Refusal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cluster/apps/web-app-staging-app.yaml", argoApp("web-staging", "cluster/apps/app-staging/web", "app-staging"))
	writeFile(t, root, "cluster/apps/web-app-production-app.yaml", argoApp("web-production", "cluster/apps/app-production/web", "app-production"))
	writeFile(t, root, "cluster/apps/app-staging/web/app.yaml", deployment("ghcr.io/example/web:sha-abc123"))
	writeFile(t, root, "cluster/apps/app-production/web/app.yaml", deployment("ghcr.io/example/web:sha-000000@"+digestB))
	installFakes(t, &k8s.Fake{}, &registry.Fake{Err: errors.New("registry: no credential source worked for ghcr.io: keychain: status 403 Forbidden: DENIED")})
	code, out, errOut := run3(t, "plan", "--repo", root, "--from", "app-staging", "--to", "app-production", "--dry-run")
	if code != exitFailure || !strings.Contains(errOut, "a bare tag with no digest") {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
}

// When every registry auth link fails for a repo with nothing to write in the target
// (source-only, a warning rather than a refusal — issue #13), the resolution section must
// still say the registry was actually asked. AuthSourceUsed() alone can't tell "never
// asked" from "asked, every source failed" apart — both report "" — so the label needs
// Consulted() too.
func TestPlanReportsRegistryConsultedWhenEveryAuthLinkFails(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cluster/apps/web-app-staging-app.yaml", argoApp("web-staging", "cluster/apps/app-staging/web", "app-staging"))
	writeFile(t, root, "cluster/apps/marketing-app-production-app.yaml", argoApp("marketing-production", "cluster/apps/app-production/marketing", "app-production"))
	writeFile(t, root, "cluster/apps/app-staging/web/app.yaml", deployment("ghcr.io/example/web:sha-abc123"))
	writeFile(t, root, "cluster/apps/app-production/marketing/app.yaml", deployment("ghcr.io/example/marketing:sha-000000@"+digestB))
	reg := &registry.Fake{Err: errors.New("registry: no credential source worked for ghcr.io: keychain: status 403 Forbidden: DENIED")}
	installFakes(t, &k8s.Fake{}, reg)

	code, out, errOut := run3(t, "plan", "--repo", root, "--from", "app-staging", "--to", "app-production", "--dry-run",
		"--registry-auth", "env,keychain")
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "registry: consulted; all auth sources failed (env, keychain)") {
		t.Errorf("resolution section does not say the registry was consulted:\n%s", out)
	}
	if strings.Contains(out, "registry not consulted") {
		t.Errorf("resolution section falsely says the registry was not consulted:\n%s", out)
	}
	if len(reg.Calls) == 0 {
		t.Fatal("test bug: the registry fake was never called")
	}
}

// R-002's third guard end to end: a value registered anywhere in the process — not just
// the local hide list a particular error happened to carry — is scrubbed wherever it later
// surfaces: a warning built from a fake registry's error body, and a fake cluster's raw
// error reaching stderr.
func TestPlanScrubsARegisteredSecretFromRegistryAndKubeErrors(t *testing.T) {
	const secret = "SECRET-TOKEN-XYZ"
	redact.Register(secret)

	// Registry body: the fake's error text embeds the secret, as a leaked response body
	// would, and the repo is source-only so the plan completes instead of refusing.
	root := t.TempDir()
	writeFile(t, root, "cluster/apps/web-app-staging-app.yaml", argoApp("web-staging", "cluster/apps/app-staging/web", "app-staging"))
	writeFile(t, root, "cluster/apps/marketing-app-production-app.yaml", argoApp("marketing-production", "cluster/apps/app-production/marketing", "app-production"))
	writeFile(t, root, "cluster/apps/app-staging/web/app.yaml", deployment("ghcr.io/example/web:sha-abc123"))
	writeFile(t, root, "cluster/apps/app-production/marketing/app.yaml", deployment("ghcr.io/example/marketing:sha-000000@"+digestB))
	installFakes(t, &k8s.Fake{}, &registry.Fake{Err: errors.New("registry said: token " + secret + " rejected")})
	code, out, errOut := run3(t, "plan", "--repo", root, "--from", "app-staging", "--to", "app-production", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, errOut)
	}
	if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
		t.Errorf("registered secret leaked from a fake registry body:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(out, "registry said: token <redacted> rejected") {
		t.Errorf("expected the warning to show <redacted> in place of the secret:\n%s", out)
	}

	// kube error: the fake cluster's raw error embeds the secret and surfaces through
	// runResolution's failure path straight to stderr, with no local hide list at all.
	installFakes(t, &k8s.Fake{Err: errors.New("k8s: dial failed, token=" + secret)}, &registry.Fake{})
	code, out, errOut = run3(t, planArgs("--dry-run")...)
	if code != exitFailure {
		t.Fatalf("exit %d, want failure; stdout %q stderr %q", code, out, errOut)
	}
	if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
		t.Errorf("registered secret leaked from a fake kube error:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "token=<redacted>") {
		t.Errorf("expected stderr to show <redacted> in place of the secret: %q", errOut)
	}
}

func TestPlanClusterErrorIsReportedWithoutAnAddress(t *testing.T) {
	installFakes(t, &k8s.Fake{Err: errors.New("k8s: listing pods in namespace app-staging: connect: connection refused")}, &registry.Fake{})
	code, out, errOut := run3(t, planArgs("--dry-run")...)
	if code != exitFailure || !strings.Contains(errOut, "connection refused") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	if out != "" {
		t.Errorf("a plan was printed anyway:\n%s", out)
	}
	installFakes(t, nil, &registry.Fake{})
	code, _, errOut = run3(t, planArgs("--dry-run", "--kube-context", "nope")...)
	if code != exitFailure || !strings.Contains(errOut, `kube context "nope"`) {
		t.Errorf("unknown context: exit %d, stderr %q", code, errOut)
	}
}

func TestPlanResolutionFlagValidation(t *testing.T) {
	installFakes(t, &k8s.Fake{}, &registry.Fake{})
	for _, extra := range [][]string{
		{"--digest-sources", "pods,vault"},
		{"--digest-sources", "pods,pods"},
		{"--digest-sources", ""},
		{"--registry-auth", "vault"},
		{"--registry-auth", ""},
		{"--cluster-secret", "no-slash"},
		{"--op-ref", "vault/item"},
	} {
		code, out, _ := run3(t, planArgs(append([]string{"--dry-run"}, extra...)...)...)
		if code != exitUsage {
			t.Errorf("%v: exit %d, want %d", extra, code, exitUsage)
		}
		if out != "" {
			t.Errorf("%v: a plan was printed anyway", extra)
		}
	}
}

// Only the adaptors the sources need are built: manifest,registry opens no cluster unless
// the registry's cluster credential is configured; pods alone opens no registry.
func TestPlanBuildsOnlyTheAdaptorsItNeeds(t *testing.T) {
	contexts, authCfgs := installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if code, _, errOut := run3(t, planArgs("--dry-run", "--digest-sources", "manifest,registry", "--registry-auth", "env,keychain")...); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(*contexts) != 0 || len(*authCfgs) != 1 {
		t.Errorf("manifest,registry with no cluster credential: clusters %v, registries %d", *contexts, len(*authCfgs))
	}
	contexts, authCfgs = installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if code, _, errOut := run3(t, planArgs("--dry-run", "--digest-sources", "manifest,registry", "--cluster-secret", "app-staging/ghcr-pull")...); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(*contexts) != 1 || len(*authCfgs) != 1 || (*authCfgs)[0].Cluster == nil {
		t.Errorf("registry with a cluster credential: clusters %v, registries %+v", *contexts, *authCfgs)
	}
	contexts, authCfgs = installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if code, out, errOut := run3(t, planArgs("--dry-run", "--digest-sources", "pods")...); code != 0 || !strings.Contains(out, "registry not consulted") {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
	if len(*contexts) != 1 || len(*authCfgs) != 0 {
		t.Errorf("pods only: clusters %v, registries %d", *contexts, len(*authCfgs))
	}
}

// The config file supplies the defaults for every resolution flag; a flag still wins.
func TestPlanResolutionDefaultsComeFromConfig(t *testing.T) {
	cfgPath := writeConfig(t, `
repos:
  - path: `+absFixture(t)+`
    promotable: [ghcr.io/example/]
    kube: { context: from-config }
    digest_sources: [manifest, registry]
registries:
  - prefix: ghcr.io/example/
    auth: [cluster, op]
    cluster: { namespace: app-staging, secret: ghcr-pull }
    op: op://vault/item/field
`)
	f := resolveFlags{}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rc := cfg.Repos[0]
	opts, err := resolutionOptions(cfg, &rc, f)
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "from-config" || len(opts.order) != 2 || opts.order[0] != resolve.SourceManifest || opts.order[1] != resolve.SourceRegistry {
		t.Errorf("repo defaults not applied: %+v", opts)
	}
	// F4: resolutionOptions no longer merges a registries[] entry into one global chain —
	// that per-repo decision is registryEntryFor/entryAuthConfig's job, in runResolution.
	// Absent a flag, auth/clusterSecret/opRef stay at the zero value here.
	if len(opts.auth) != 0 || opts.clusterSecret != "" || opts.opRef != "" {
		t.Errorf("registry fields should be unset absent a flag: %+v", opts)
	}
	if len(opts.registries) != 1 || opts.registries[0].Prefix != "ghcr.io/example/" {
		t.Errorf("registries not carried through for per-repo lookup: %+v", opts.registries)
	}
	// Flags win, one by one, and apply to every repo (entryAuthConfig).
	opts, err = resolutionOptions(cfg, &rc, resolveFlags{kubeContext: "flag", digestSources: "none", registryAuth: "env", clusterSecret: "x/y", opRef: "op://a/b/c"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "flag" || opts.order != nil || len(opts.auth) != 1 || opts.auth[0] != registry.AuthEnv || opts.clusterSecret != "x/y" || opts.opRef != "op://a/b/c" {
		t.Errorf("flags did not win: %+v", opts)
	}
	// No config at all: the documented defaults.
	opts, err = resolutionOptions(&config.Config{}, nil, resolveFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "" || len(opts.order) != 3 || len(opts.auth) != 0 || opts.clusterSecret != "" || opts.opRef != "" || len(opts.registries) != 0 {
		t.Errorf("defaults: %+v", opts)
	}

	// End to end: the configured repo drives the plan with the configured context.
	contexts, authCfgs := installFakes(t, &k8s.Fake{}, &registry.Fake{})
	code, out, errOut := run3(t, "--config", cfgPath, "plan", "--from", "app-staging", "--to", "app-production", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "sources manifest,registry; kube context from-config") {
		t.Errorf("section header:\n%s", out)
	}
	if len(*contexts) != 1 || (*contexts)[0] != "from-config" || len(*authCfgs) != 1 || (*authCfgs)[0].OpRef != "op://vault/item/field" {
		t.Errorf("adaptors: contexts %v, registries %+v", *contexts, *authCfgs)
	}
}

// The reviewer's grep: nothing in the plan output looks like a credential, and the only
// cluster identity is the context name.
func TestPlanOutputCarriesNoSecretOrAddress(t *testing.T) {
	cluster := &k8s.Fake{Images: map[string][]k8s.RunningImage{"app-staging": {
		{Pod: "web-1", Container: "web", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
		{Pod: "web-2", Container: "web", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestB}},
	}}}
	installFakes(t, cluster, &registry.Fake{Auth: "env (GHCR_TOKEN)"})
	t.Setenv("GHCR_TOKEN", "ghp_FAKE_TOKEN_VALUE")
	code, out, errOut := run3(t, planArgs("--dry-run", "--kube-context", "my-cluster", "--op-ref", "op://vault/item/field")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "[running-disagrees]") || !strings.Contains(out, "pod web-2 container web "+digestB) {
		t.Errorf("the disagreement warning is missing:\n%s", out)
	}
	for _, leak := range []string{"ghp_FAKE", "op://", "https://", "6443"} {
		if strings.Contains(out+errOut, leak) {
			t.Errorf("output carries %q", leak)
		}
	}
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := root + "/" + rel
	if err := os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func argoApp(name, path, namespace string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + name + `
spec:
  source:
    path: ` + path + `
  destination:
    namespace: ` + namespace + `
`
}

func deployment(img string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: web
          image: ` + img + `
`
}

// A direct Head/Tags call on the per-repo registry (M6's tag picker will make them) must be
// routed to the client scoped to that repo and must error for a repo with no registry —
// never fall through to whichever client happened to be built first (F4).
func TestMultiRegistryRoutesDirectCallsByRepo(t *testing.T) {
	a := &registry.Fake{Digests: map[string]string{"ghcr.io/acme/app:v1": "sha256:" + strings.Repeat("a", 64)}}
	b := &registry.Fake{Digests: map[string]string{"quay.io/other/app:v1": "sha256:" + strings.Repeat("b", 64)}}
	m := &multiRegistry{byRepo: map[string]registry.Registry{"ghcr.io/acme/app": a, "quay.io/other/app": b}, primary: a}
	ctx := context.Background()
	if d, err := m.Head(ctx, image.Ref{Repo: "quay.io/other/app", Tag: "v1"}); err != nil || !strings.HasPrefix(d, "sha256:bbbb") {
		t.Fatalf("Head routed wrong: %q, %v", d, err)
	}
	if _, err := m.Head(ctx, image.Ref{Repo: "ghcr.io/nobody/app", Tag: "v1"}); err == nil || !strings.Contains(err.Error(), "no registry configured") {
		t.Fatalf("unmatched repo: got %v, want no-registry error", err)
	}
	if _, err := m.Tags(ctx, "ghcr.io/nobody/app"); err == nil {
		t.Fatal("Tags for an unmatched repo must error, not use the primary client")
	}
}

// Codex P1 (draft #29 pass): a bare-host prefix like "ghcr.io" must never match a
// different host that merely shares its leading bytes. Before the fix, registryEntryFor
// used a raw strings.HasPrefix, so an entry configured for "ghcr.io" (no trailing slash)
// — with an op ref or a cluster secret attached — matched "ghcr.io.attacker.example/…"
// too, and entryAuthConfig would hand that host the entry's credentials.
func TestRegistryEntryForRejectsHostConfusion(t *testing.T) {
	registries := []config.RegistryConfig{
		{Prefix: "ghcr.io", Op: "op://vault/ghcr/field"}, // no trailing slash, deliberately
	}
	if e := registryEntryFor(registries, "ghcr.io.attacker.example/org/app"); e != nil {
		t.Fatalf("registryEntryFor matched an attacker-controlled host via prefix confusion: %+v", e)
	}
	if e := registryEntryFor(registries, "ghcr.io/abradner/app"); e == nil || e.Op != "op://vault/ghcr/field" {
		t.Fatalf("registryEntryFor(real repo) = %+v, want the ghcr.io entry", e)
	}
}
