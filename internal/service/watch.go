package service

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// WatchSnapshot is one read of an Argo Application and the workloads its family declares:
// plain values, no cluster types, so the watch screen and `hoist watch` render the same thing
// from the same read. Workloads lists every Deployment, then every Job and CronJob.
type WatchSnapshot struct {
	// App is the Application's metadata.name; Namespace its destination namespace (the env).
	App, Namespace string
	// SyncStatus, HealthStatus, Revision and OperationPhase are Argo's own status words, ""
	// when Argo has not filled one in; ReconciledAt is zero when it never has.
	SyncStatus, HealthStatus, Revision, OperationPhase string
	ReconciledAt                                       time.Time
	Workloads                                          []WatchWorkload
}

// WatchWorkload is one Deployment, Job or CronJob's current state.
type WatchWorkload struct {
	Kind, Name string
	// Replicas is a Deployment's spec.replicas; 0 for a Job/CronJob, which has none.
	Replicas int32
	// Complete and DeadlineExceeded are kubectl's own rollout verdict, Deployments only.
	Complete, DeadlineExceeded bool
	// Detail is the kubectl-style line: what is still pending, or that it rolled out.
	Detail string
	// Images are "container name=ref" pairs, initContainers marked.
	Images []string
}

// Watch is a read-only handle on one Argo Application and the workloads its family declares,
// opened by OpenWatchApp or OpenWatchFamily. It holds the two cluster clients it reads through,
// and nothing here can write: Read calls Argo.Get, Rollout.Deployment and Rollout.JobLike only —
// never Argo.Refresh — which is the whole of watch's read-only guarantee (a compile-time fact of
// the calls below, and a test asserts no Refresh ever reaches a fake).
type Watch struct {
	// App and Namespace name what is watched; KubeContext is the kubeconfig context the clients
	// resolved to, by name.
	App, Namespace, KubeContext string
	// Interval is the cadence both faces poll at: the tighter of poll.argo and poll.rollout,
	// since one tick refreshes an Argo status and a workload's, so neither reading is staler than
	// its own configured cadence already promises. There is no dedicated watch knob.
	Interval time.Duration

	argo        argo.Argo
	rollout     rollout.Rollout
	app         argo.Application
	deployments []string
	jobLikes    []gitops.Workload
}

// OpenWatchApp opens the Application named app (its metadata.name) in r, the way
// `hoist watch --app` does. The repo is the caller's: the CLI reads its working tree, the TUI
// the service's current view.
func (s *Service) OpenWatchApp(r *gitops.Repo, app string) (*Watch, error) {
	var target *gitops.ArgoApp
	for i := range r.Apps {
		if r.Apps[i].Name == app {
			target = &r.Apps[i]
			break
		}
	}
	if target == nil {
		var names []string
		for _, a := range r.Apps {
			names = append(names, a.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no Application %q found under %s (known: %s)", app, s.settings.AppsRoot, strings.Join(names, ", "))
	}
	return s.openWatch(target.Name, target.Namespace, r.Envs[target.Namespace].Families[path.Base(target.SourcePath)])
}

// OpenWatchFamily opens one family of one env in the service's current repo view (Repo), so a
// family an F5 refresh just added is watchable at once. It resolves the family's Application
// (gitops.Family.App, the wrapper OpenWatchApp finds by name) and reads from the same place.
func (s *Service) OpenWatchFamily(env, family string) (*Watch, error) {
	r := s.Repo().Repo
	if r == nil {
		return nil, fmt.Errorf("watch: no repo loaded yet")
	}
	e, ok := r.Envs[env]
	if !ok {
		return nil, fmt.Errorf("no env %q in the repo", env)
	}
	fam, ok := e.Families[family]
	if !ok || fam == nil {
		return nil, fmt.Errorf("no family %q in %s", family, env)
	}
	if fam.App == "" {
		return nil, fmt.Errorf("%s in %s has no Argo Application", family, env)
	}
	return s.openWatch(fam.App, env, fam)
}

func (s *Service) openWatch(app, namespace string, fam *gitops.Family) (*Watch, error) {
	// The clients come from the service per open, which memoises only a success: a cluster that
	// could not be reached last time is retried now, never wedged for the session, and the error
	// is the real one from whichever client failed.
	a, used, err := s.ArgoNamed(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	ro, err := s.Rollout(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	w := &Watch{
		App: app, Namespace: namespace, KubeContext: used,
		Interval: s.watchInterval(),
		argo:     a, rollout: ro,
		app:         argo.Application{Namespace: s.settings.ArgoNamespace(), Name: app},
		deployments: fam.Named("Deployment"),
	}
	for _, wl := range fam.Workloads() {
		if wl.Kind == "Job" || wl.Kind == "CronJob" {
			w.jobLikes = append(w.jobLikes, wl)
		}
	}
	return w, nil
}

func (s *Service) watchInterval() time.Duration {
	interval := s.settings.Poll.Argo
	if r := s.settings.Poll.Rollout; r < interval {
		interval = r
	}
	return interval
}

// ArgoNamespace is where the Application custom resources live for the selected repo
// (RepoConfig.Kube.ArgoNamespace), or the default when running from flags alone.
func (s Settings) ArgoNamespace() string {
	if s.Repo == nil {
		return config.DefaultArgoNamespace
	}
	return s.Repo.Kube.ArgoNamespace
}

// Read re-observes the Application and every workload it declares; nothing is cached between
// calls (AGENTS.md §4.1).
func (w *Watch) Read(ctx context.Context) (WatchSnapshot, error) {
	st, err := w.argo.Get(ctx, w.app)
	if err != nil {
		return WatchSnapshot{}, fmt.Errorf("reading Argo Application %s: %w", w.app, err)
	}
	snap := WatchSnapshot{
		App: w.app.Name, Namespace: w.Namespace,
		SyncStatus: st.SyncStatus, HealthStatus: st.HealthStatus, Revision: st.SyncRevision,
		OperationPhase: st.OperationPhase, ReconciledAt: st.ReconciledAt,
	}
	for _, name := range w.deployments {
		ds, err := w.rollout.Deployment(ctx, w.Namespace, name)
		if err != nil {
			return WatchSnapshot{}, fmt.Errorf("reading Deployment %s/%s: %w", w.Namespace, name, err)
		}
		wl := WatchWorkload{Kind: "Deployment", Name: name, Replicas: ds.Replicas, Complete: ds.Complete, DeadlineExceeded: ds.DeadlineExceeded, Detail: ds.Detail}
		for _, img := range ds.Images {
			kind := "container"
			if img.Init {
				kind = "initContainer"
			}
			wl.Images = append(wl.Images, fmt.Sprintf("%s %s=%s", kind, img.Name, img.Image))
		}
		snap.Workloads = append(snap.Workloads, wl)
	}
	for _, jl := range w.jobLikes {
		js, err := w.rollout.JobLike(ctx, w.Namespace, jl.Name, jl.Kind)
		if err != nil {
			return WatchSnapshot{}, fmt.Errorf("reading %s %s/%s: %w", jl.Kind, w.Namespace, jl.Name, err)
		}
		snap.Workloads = append(snap.Workloads, WatchWorkload{Kind: jl.Kind, Name: jl.Name, Detail: js.Detail})
	}
	return snap, nil
}
