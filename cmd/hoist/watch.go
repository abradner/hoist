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
	kubeContext := fs.String("kube-context", "", "kubeconfig context (default: the selected repo's kube.context, else the kubeconfig's current context)")
	once := fs.Bool("once", false, "print one snapshot and exit, instead of polling until interrupted")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot = *repo, *appsRoot
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		fmt.Fprintf(stderr, "hoist watch: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *app == "" {
		fmt.Fprintln(stderr, "hoist watch: --repo and --app are required")
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

	argoNamespace := config.DefaultArgoNamespace
	ctxName := *kubeContext
	if eff.cfg != nil {
		argoNamespace = eff.cfg.Kube.ArgoNamespace
		if ctxName == "" {
			ctxName = eff.cfg.Kube.Context
		}
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

	// Poll interval: the smaller of poll.argo and poll.rollout, since this one loop refreshes
	// both an Argo status and a Deployment/Job status on every tick — never a constant this
	// command invents on its own (invariant 5). There is no dedicated "watch" poll knob in
	// internal/config; reusing the tighter of the two existing ones means neither reading ever
	// goes stale by more than its own configured cadence already promises elsewhere.
	interval := time.Duration(cfg.Poll.Argo)
	if r := time.Duration(cfg.Poll.Rollout); r < interval {
		interval = r
	}
	ticker := time.NewTicker(interval)
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

// watchAppSnapshot renders one read-only snapshot: a.Get's current Argo status, then
// ro.Deployment/ro.JobLike for every workload the family declares. It calls Argo.Get and
// Rollout.Deployment/JobLike only — never Argo.Refresh — which is the whole of `hoist watch`'s
// read-only guarantee (see runWatch's own doc comment).
func watchAppSnapshot(ctx context.Context, a argo.Argo, ro rollout.Rollout, app argo.Application, namespace string, deployments []string, jobLikes []jobLikeName) (string, error) {
	var b strings.Builder
	st, err := a.Get(ctx, app)
	if err != nil {
		return "", fmt.Errorf("reading Argo Application %s: %w", app, err)
	}
	fmt.Fprintf(&b, "  sync=%s health=%s revision=%s operation=%s reconciled=%s\n",
		orNone(st.SyncStatus), orNone(st.HealthStatus), orNone(st.SyncRevision), orNone(st.OperationPhase), formatTime(st.ReconciledAt))

	for _, name := range deployments {
		ds, err := ro.Deployment(ctx, namespace, name)
		if err != nil {
			return "", fmt.Errorf("reading Deployment %s/%s: %w", namespace, name, err)
		}
		var images []string
		for _, img := range ds.Images {
			kind := "container"
			if img.Init {
				kind = "initContainer"
			}
			images = append(images, fmt.Sprintf("%s %s=%s", kind, img.Name, img.Image))
		}
		fmt.Fprintf(&b, "  Deployment %s: %s", name, ds.Detail)
		if len(images) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(images, ", "))
		}
		fmt.Fprintln(&b)
	}
	for _, jl := range jobLikes {
		js, err := ro.JobLike(ctx, namespace, jl.Name, jl.Kind)
		if err != nil {
			return "", fmt.Errorf("reading %s %s/%s: %w", jl.Kind, namespace, jl.Name, err)
		}
		fmt.Fprintf(&b, "  %s %s: %s\n", jl.Kind, jl.Name, js.Detail)
	}
	return b.String(), nil
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
