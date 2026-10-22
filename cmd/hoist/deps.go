package main

import (
	"time"

	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/forge/github"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

// adaptors are the real clients every command builds its Service from — git, the forge, Argo,
// the rollout reader, the cluster, the registry — and the one place a test substitutes a fake: no
// test points a client at a real cluster, registry, Argo CD instance or GitHub repo, or lets
// hoist touch a checkout other than a fixture it created itself (AGENTS.md hard constraints). A
// test replaces a field (`adaptors.Forge = …`) and restores it with t.Cleanup. It is one package
// variable where there used to be six (newGit, newForge, newCluster, newRegistry, newArgo,
// newRollout), each reassigned across the test files.
var adaptors = service.Deps{
	Git:      func() git.Git { return git.Exec{} },
	Forge:    func(ownerRepo string) (forge.Forge, error) { return github.New(ownerRepo) },
	Argo:     argo.NewFromKubeconfig,
	Rollout:  rollout.NewFromKubeconfig,
	Cluster:  k8s.NewCluster,
	Registry: func(cfg registry.AuthConfig) (registry.Registry, error) { return registry.New(cfg) },
}

// serviceDeps is what every command constructs its Service with. Each function reads its field
// of adaptors when it is CALLED, not when serviceDeps runs, so a test that swaps one mid-test
// reaches a Service built before or after the swap alike.
func serviceDeps() service.Deps {
	return service.Deps{
		Git:      func() git.Git { return adaptors.Git() },
		Forge:    func(ownerRepo string) (forge.Forge, error) { return adaptors.Forge(ownerRepo) },
		Argo:     func(kubeContext string) (argo.Argo, string, error) { return adaptors.Argo(kubeContext) },
		Rollout:  func(kubeContext string) (rollout.Rollout, string, error) { return adaptors.Rollout(kubeContext) },
		Cluster:  func(kubeContext string) (k8s.Cluster, string, error) { return adaptors.Cluster(kubeContext) },
		Registry: func(cfg registry.AuthConfig) (registry.Registry, error) { return adaptors.Registry(cfg) },
		Store:    service.FileStore{},
		Now:      time.Now,
	}
}
