package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
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

// A scaled-to-zero repo with a bare tag goes to the registry, and the section reports
// which credential source authenticated — by name.
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
	opts, err := resolutionOptions(cfg, &rc, rc.Promotable, f)
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "from-config" || len(opts.order) != 2 || opts.order[0] != resolve.SourceManifest || opts.order[1] != resolve.SourceRegistry {
		t.Errorf("repo defaults not applied: %+v", opts)
	}
	if len(opts.auth) != 2 || opts.auth[0] != registry.AuthCluster || opts.auth[1] != registry.AuthOp || opts.clusterSecret != "app-staging/ghcr-pull" || opts.opRef != "op://vault/item/field" {
		t.Errorf("registry defaults not applied: %+v", opts)
	}
	// Flags win, one by one.
	opts, err = resolutionOptions(cfg, &rc, rc.Promotable, resolveFlags{kubeContext: "flag", digestSources: "none", registryAuth: "env", clusterSecret: "x/y", opRef: "op://a/b/c"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "flag" || opts.order != nil || len(opts.auth) != 1 || opts.auth[0] != registry.AuthEnv || opts.clusterSecret != "x/y" || opts.opRef != "op://a/b/c" {
		t.Errorf("flags did not win: %+v", opts)
	}
	// A registries[] entry that covers no promotable prefix supplies nothing.
	opts, err = resolutionOptions(cfg, &rc, []string{"quay.io/other/"}, resolveFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.clusterSecret != "" || opts.opRef != "" || len(opts.auth) != 4 {
		t.Errorf("unrelated registry entry was applied: %+v", opts)
	}
	// No config at all: the documented defaults.
	opts, err = resolutionOptions(&config.Config{}, nil, []string{"ghcr.io/"}, resolveFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.kubeContext != "" || len(opts.order) != 3 || len(opts.auth) != 4 || opts.clusterSecret != "" || opts.opRef != "" {
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
