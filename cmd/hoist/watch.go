package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// runWatch is `hoist watch --app <name>`: a read-only snapshot (or, without --once, a poll
// loop) of one Argo Application's sync/health/revision and the rollout progress of every
// Deployment/Job/CronJob its family declares. It is READ-ONLY by construction (AGENTS.md
// invariant 5 of the M5 brief): this function, and everything it calls, holds only an
// argo.Argo and a rollout.Rollout value and never once calls Argo.Refresh — the interface
// itself has no other write method, so this is a compile-time guarantee, not just a
// convention; watchAppSnapshot's own tests additionally assert zero "Refresh" calls ever
// appear in a Fake's Calls after a run, as the brief's own "test or code-inspection-provable
// guarantee" asks for both.
func runWatch(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	app := fs.String("app", "", "the Argo Application's own metadata.name to watch (required)")
	kubeContext := fs.String("kube-context", sel.kubeContext, "kubeconfig context (default: the selected repo's kube.context, else the kubeconfig's current context; may also be given before the command)")
	once := fs.Bool("once", false, "print one snapshot and exit, instead of polling until interrupted")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.kubeContext = *repo, *appsRoot, *kubeContext
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		fmt.Fprintf(stderr, "hoist watch: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" && *app == "" {
		fmt.Fprintln(stderr, "hoist watch: --repo and --app are required")
		fs.Usage()
		return exitUsage
	}
	if eff.repo == "" {
		fmt.Fprintln(stderr, "hoist watch: --repo is required (no configured repo to default to)")
		fs.Usage()
		return exitUsage
	}
	if *app == "" {
		fmt.Fprintln(stderr, "hoist watch: --app is required")
		fs.Usage()
		return exitUsage
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist watch: %v\n", err)
		return exitFailure
	}
	var target *gitops.ArgoApp
	for i := range r.Apps {
		if r.Apps[i].Name == *app {
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
		fmt.Fprintf(stderr, "hoist watch: no Application %q found under %s (known: %s)\n", *app, eff.appsRoot, strings.Join(names, ", "))
		return exitFailure
	}
	fam := r.Envs[target.Namespace].Families[path.Base(target.SourcePath)]

	argoNamespace := argoNamespaceOf(eff.cfg)
	ctxName := *kubeContext
	if eff.cfg != nil && ctxName == "" {
		ctxName = eff.cfg.Kube.Context
	}

	a, usedCtx, err := newArgo(ctxName)
	if err != nil {
		fmt.Fprintf(stderr, "hoist watch: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	ro, _, err := newRollout(ctxName)
	if err != nil {
		fmt.Fprintf(stderr, "hoist watch: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}

	deployments, jobLikes := familyWorkloads(fam)
	fmt.Fprintf(stdout, "hoist watch: %s (namespace %s, kube context %s)\n", target.Name, target.Namespace, usedCtx)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	renderOnce := func() int {
		snap, err := watchAppSnapshot(ctx, a, ro, argo.Application{Namespace: argoNamespace, Name: target.Name}, target.Namespace, deployments, jobLikes)
		if err != nil {
			fmt.Fprintf(stderr, "hoist watch: %s\n", redact.Strings(err.Error()))
			return exitFailure
		}
		fmt.Fprint(stdout, snap)
		return 0
	}
	if code := renderOnce(); code != 0 {
		return code
	}
	if *once {
		return 0
	}

	ticker := time.NewTicker(watchInterval(cfg.Poll))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			renderOnce()
		}
	}
}

// watchInterval is the poll cadence both faces of watch share: the smaller of poll.argo and
// poll.rollout, since one loop refreshes both an Argo status and a Deployment/Job status on
// every tick — never a constant either face invents on its own (invariant 5). There is no
// dedicated "watch" poll knob in internal/config; reusing the tighter of the two existing
// ones means neither reading ever goes stale by more than its own configured cadence already
// promises elsewhere. The TUI's watch screen (buildWatchFunc, wiring.go) polls at this same
// value, so the two faces cannot drift apart on cadence.
func watchInterval(poll config.PollConfig) time.Duration {
	interval := time.Duration(poll.Argo)
	if r := time.Duration(poll.Rollout); r < interval {
		interval = r
	}
	return interval
}

// argoNamespaceOf is where the Application custom resources live for a configured repo
// (RepoConfig.Kube.ArgoNamespace), or the default when running from flags alone.
func argoNamespaceOf(rc *config.RepoConfig) string {
	if rc == nil {
		return config.DefaultArgoNamespace
	}
	return rc.Kube.ArgoNamespace
}

// familyWorkloads lists the distinct Deployment names, and the distinct (kind, name) Job/
// CronJob pairs, fam's own occurrences declare — the same Kind-based grouping RolledOutStep
// uses (groupEditsByWorkload in internal/engine), applied to every occurrence discovery found
// rather than only the ones a specific promotion's plan touched, since `hoist watch` has no
// plan to consult.
func familyWorkloads(fam *gitops.Family) (deployments []string, jobLikes []jobLikeName) {
	if fam == nil {
		return nil, nil
	}
	seenDeployment := map[string]bool{}
	seenJobLike := map[jobLikeName]bool{}
	for _, o := range fam.Occurrences {
		switch o.Kind {
		case "Deployment":
			if !seenDeployment[o.Name] {
				seenDeployment[o.Name] = true
				deployments = append(deployments, o.Name)
			}
		case "Job", "CronJob":
			ref := jobLikeName{Name: o.Name, Kind: o.Kind}
			if !seenJobLike[ref] {
				seenJobLike[ref] = true
				jobLikes = append(jobLikes, ref)
			}
		}
	}
	sort.Strings(deployments)
	sort.Slice(jobLikes, func(i, j int) bool {
		if jobLikes[i].Kind != jobLikes[j].Kind {
			return jobLikes[i].Kind < jobLikes[j].Kind
		}
		return jobLikes[i].Name < jobLikes[j].Name
	})
	return deployments, jobLikes
}

type jobLikeName struct{ Name, Kind string }

// watchAppSnapshot renders one read-only snapshot for the CLI: readWatchSnapshot's values,
// one line for the Application and one per workload.
func watchAppSnapshot(ctx context.Context, a argo.Argo, ro rollout.Rollout, app argo.Application, namespace string, deployments []string, jobLikes []jobLikeName) (string, error) {
	snap, err := readWatchSnapshot(ctx, a, ro, app, namespace, deployments, jobLikes)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  sync=%s health=%s revision=%s operation=%s reconciled=%s\n",
		orNone(snap.SyncStatus), orNone(snap.HealthStatus), orNone(snap.Revision), orNone(snap.OperationPhase), formatTime(snap.ReconciledAt))
	for _, w := range snap.Workloads {
		fmt.Fprintf(&b, "  %s %s: %s", w.Kind, w.Name, w.Detail)
		if len(w.Images) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(w.Images, ", "))
		}
		fmt.Fprintln(&b)
	}
	return b.String(), nil
}

// readWatchSnapshot is the one read both faces of watch make: a.Get's current Argo status,
// then ro.Deployment/ro.JobLike for every workload the family declares, flattened into the
// watch screen's plain Snapshot (internal/app/watch carries no cluster types of its own). It
// calls Argo.Get and Rollout.Deployment/JobLike only — never Argo.Refresh — which is the whole
// of watch's read-only guarantee (see runWatch's own doc comment); TestWatchNeverCallsRefresh
// and TestBuildWatchFuncNeverCallsRefresh each pin it from their own face.
func readWatchSnapshot(ctx context.Context, a argo.Argo, ro rollout.Rollout, app argo.Application, namespace string, deployments []string, jobLikes []jobLikeName) (watch.Snapshot, error) {
	st, err := a.Get(ctx, app)
	if err != nil {
		return watch.Snapshot{}, fmt.Errorf("reading Argo Application %s: %w", app, err)
	}
	snap := watch.Snapshot{
		App: app.Name, Namespace: namespace,
		SyncStatus: st.SyncStatus, HealthStatus: st.HealthStatus, Revision: st.SyncRevision,
		OperationPhase: st.OperationPhase, ReconciledAt: st.ReconciledAt,
	}
	for _, name := range deployments {
		ds, err := ro.Deployment(ctx, namespace, name)
		if err != nil {
			return watch.Snapshot{}, fmt.Errorf("reading Deployment %s/%s: %w", namespace, name, err)
		}
		w := watch.Workload{Kind: "Deployment", Name: name, Replicas: ds.Replicas, Complete: ds.Complete, DeadlineExceeded: ds.DeadlineExceeded, Detail: ds.Detail}
		for _, img := range ds.Images {
			kind := "container"
			if img.Init {
				kind = "initContainer"
			}
			w.Images = append(w.Images, fmt.Sprintf("%s %s=%s", kind, img.Name, img.Image))
		}
		snap.Workloads = append(snap.Workloads, w)
	}
	for _, jl := range jobLikes {
		js, err := ro.JobLike(ctx, namespace, jl.Name, jl.Kind)
		if err != nil {
			return watch.Snapshot{}, fmt.Errorf("reading %s %s/%s: %w", jl.Kind, namespace, jl.Name, err)
		}
		snap.Workloads = append(snap.Workloads, watch.Workload{Kind: jl.Kind, Name: jl.Name, Detail: js.Detail})
	}
	return snap, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "(never)"
	}
	return t.Format(time.RFC3339)
}
