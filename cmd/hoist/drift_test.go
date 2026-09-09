package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
)

// The matrix's drift question goes to the pods whatever digest_sources the config orders for
// planning: with "none" the planning resolver builds nothing and asks nothing, while the drift
// function still opens exactly one cluster client, in the context it was given, and no registry
// adaptor — and asks for exactly the env's namespace.
func TestDriftFuncAsksPodsOnlyWhateverTheConfigSays(t *testing.T) {
	r, err := gitops.Discover(absFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	rc := config.RepoConfig{Path: absFixture(t), Promotable: []string{"ghcr.io/example/"}, DigestSources: []string{"none"}}
	cfg := &config.Config{Repos: []config.RepoConfig{rc}}

	contexts, authCfgs := installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if _, err := buildResolveFunc(cfg, &rc, rc.Promotable)(context.Background(), r, "app-staging", nil); err != nil {
		t.Fatal(err)
	}
	if len(*contexts) != 0 || len(*authCfgs) != 0 {
		t.Fatalf("planning with none: clusters %v registries %d; want nothing built", *contexts, len(*authCfgs))
	}

	fake := &k8s.Fake{}
	contexts, authCfgs = installFakes(t, fake, &registry.Fake{})
	if _, err := buildDriftFunc("my-cluster")(context.Background(), "app-staging"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"my-cluster"}, *contexts); diff != "" || len(*authCfgs) != 0 {
		t.Fatalf("drift: clusters %v registries %d; want one cluster in my-cluster and no registry", *contexts, len(*authCfgs))
	}
	if diff := cmp.Diff([]string{"RunningImages app-staging"}, fake.Calls); diff != "" {
		t.Fatalf("cluster calls (-want +got):\n%s", diff)
	}
}

// A partial rollout reaches the matrix as two builds, not the one a plan would pick (#122):
// every distinct running reference is kept, the pod's own reported tag rides beside its
// digest (and only when it names the same repo), and identical observations from several
// pods collapse to one entry.
func TestDriftFuncKeepsEveryRunningBuild(t *testing.T) {
	const web = "ghcr.io/example/web"
	fake := &k8s.Fake{Images: map[string][]k8s.RunningImage{"app-staging": {
		{Pod: "web-1", Container: "web", Ref: image.Ref{Repo: web, Digest: digestA}, Image: image.Ref{Repo: web, Tag: "v1"}},
		{Pod: "web-2", Container: "web", Ref: image.Ref{Repo: web, Digest: digestA}, Image: image.Ref{Repo: web, Tag: "v1"}},
		{Pod: "web-3", Container: "web", Ref: image.Ref{Repo: web, Digest: digestB}, Image: image.Ref{Repo: web, Tag: "v2"}},
		// A mirror: the pod pulled under another name, so its tag says nothing about web.
		{Pod: "api-1", Container: "api", Ref: image.Ref{Repo: "ghcr.io/example/api", Digest: digestB}, Image: image.Ref{Repo: "mirror.invalid/api", Tag: "v9"}},
	}}}
	installFakes(t, fake, &registry.Fake{})
	got, err := buildDriftFunc("")(context.Background(), "app-staging")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]image.Ref{
		web:                   {{Repo: web, Tag: "v1", Digest: digestA}, {Repo: web, Tag: "v2", Digest: digestB}},
		"ghcr.io/example/api": {{Repo: "ghcr.io/example/api", Digest: digestB}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("running refs (-want +got):\n%s", diff)
	}
}
