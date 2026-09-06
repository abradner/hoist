package main

import (
	"context"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
)

// The matrix's drift question goes to the pods whatever digest_sources the config orders for
// planning: with "none" the planning resolver builds nothing and asks nothing, while the drift
// resolver still builds exactly one cluster client and no registry adaptor.
func TestDriftResolverAsksPodsOnlyWhateverTheConfigSays(t *testing.T) {
	r, err := gitops.Discover(absFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	rc := config.RepoConfig{Path: absFixture(t), Promotable: []string{"ghcr.io/example/"}, DigestSources: []string{"none"}}
	cfg := &config.Config{Repos: []config.RepoConfig{rc}}

	contexts, authCfgs := installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if _, err := buildResolveFunc(cfg, &rc, rc.Promotable)(context.Background(), r, "app-staging"); err != nil {
		t.Fatal(err)
	}
	if len(*contexts) != 0 || len(*authCfgs) != 0 {
		t.Fatalf("planning with none: clusters %v registries %d; want nothing built", *contexts, len(*authCfgs))
	}

	contexts, authCfgs = installFakes(t, &k8s.Fake{}, &registry.Fake{})
	if _, err := buildDriftResolveFunc(cfg, &rc, rc.Promotable)(context.Background(), r, "app-staging"); err != nil {
		t.Fatal(err)
	}
	if len(*contexts) != 1 || len(*authCfgs) != 0 {
		t.Fatalf("drift with none: clusters %v registries %d; want one cluster and no registry", *contexts, len(*authCfgs))
	}
}
