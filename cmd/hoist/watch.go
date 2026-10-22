package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
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
	if err := parseFlagsOnly(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.kubeContext = *repo, *appsRoot, *kubeContext
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		return fail(stderr, "hoist watch", exitFailure, err)
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
		return fail(stderr, "hoist watch", exitFailure, err)
	}
	w, err := service.New(settingsFor(cfg, eff), serviceDeps()).OpenWatchApp(r, *app)
	if err != nil {
		return fail(stderr, "hoist watch", exitFailure, err)
	}
	fmt.Fprintf(stdout, "hoist watch: %s (namespace %s, kube context %s)\n", w.App, w.Namespace, w.KubeContext)

	ctx, stop := commandContext(0)
	defer stop()

	renderOnce := func() int {
		snap, err := w.Read(ctx)
		if err != nil {
			return fail(stderr, "hoist watch", exitFailure, err)
		}
		fmt.Fprint(stdout, formatWatchSnapshot(snap))
		return 0
	}
	if code := renderOnce(); code != 0 {
		return code
	}
	if *once {
		return 0
	}

	ticker := time.NewTicker(w.Interval)
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

// formatWatchSnapshot renders one read for the CLI: one line for the Application and one per
// workload.
func formatWatchSnapshot(snap service.WatchSnapshot) string {
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
	return b.String()
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
