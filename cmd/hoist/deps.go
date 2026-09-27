package main

import (
	"time"

	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

// serviceDeps wires internal/service's own constructor seams to the package-level test seams
// promote.go and resolution.go already declare (newGit, newForge, newArgo, newRollout,
// newCluster, newRegistry) — read inside each closure, AT CALL TIME, rather than captured once
// here. About 108 test assignments across this package reassign one of these mid-test; a
// closure that reads the variable when called, rather than snapshotting its value when
// serviceDeps() runs, is what lets every one of those tests keep working unchanged whether the
// swap happens before or after a Service is built.
func serviceDeps() service.Deps {
	return service.Deps{
		Git:      func() git.Git { return newGit },
		Forge:    func(ownerRepo string) (forge.Forge, error) { return newForge(ownerRepo) },
		Argo:     func(kubeContext string) (argo.Argo, string, error) { return newArgo(kubeContext) },
		Rollout:  func(kubeContext string) (rollout.Rollout, string, error) { return newRollout(kubeContext) },
		Cluster:  func(kubeContext string) (k8s.Cluster, string, error) { return newCluster(kubeContext) },
		Registry: func(cfg registry.AuthConfig) (registry.Registry, error) { return newRegistry(cfg) },
		Store:    service.FileStore{},
		Now:      time.Now,
	}
}
