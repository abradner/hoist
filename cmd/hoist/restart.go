package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// runRestart is `hoist restart`: roll an env's Deployments without changing the image references
// they declare.
//
// It talks to the cluster directly, and deliberately writes nothing to git. `kubectl rollout
// restart` stamps the pod template's restart annotation on the live Deployment, and Argo does
// not treat that as drift even with selfHeal on: its diff is a three-way merge, so a field Argo
// never set and that is absent from the manifest is owned by someone else and left alone — the
// same reason `kubectl apply` does not delete fields it never wrote.
//
// (An earlier design wrote the annotation into the manifest and drove the whole promotion
// pipeline, on the assumption that Argo would revert a live patch. That assumption was wrong —
// annotations added this way have survived weeks on self-healing Applications that report
// Synced — and it cost a commit, a PR, a CI run and an approval for what is one API call.)
//
// So there is no plan, no worktree, no branch, no PR, no state file, and nothing to resume:
// re-running restarts again, which is the operation. What remains is the part that matters —
// naming what will roll, gating production behind a second acknowledgement, and watching the
// rollout that follows.
func runRestart(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command). Read only to learn which Deployments each family declares — a restart writes nothing to it")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	env := fs.String("env", "", "target env: the namespace whose Deployments to restart (required)")
	family := fs.String("family", "", "comma-separated family names to restart; default every family in --env")
	confirmProduction := fs.String("confirm-production", "", "required when --env is listed in the selected repo's envs.production: repeat --env's exact value to acknowledge restarting production (refused otherwise)")
	kubeContext := fs.String("kube-context", sel.kubeContext, "kubeconfig context to restart in (the selected repo's kube.context when configured; may also be given before the command)")
	dryRun := fs.Bool("dry-run", false, "print what would be restarted, and any reason a restart would not be graceful, without touching the cluster")
	if err := parseFlagsOnly(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.kubeContext = *repo, *appsRoot, *kubeContext
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	set, err := selectRepo(cfg, sel)
	if err != nil {
		return fail(stderr, "hoist restart", exitFailure, err)
	}
	if set.RepoDir == "" || *env == "" {
		fmt.Fprintln(stderr, "hoist restart: --repo and --env are required")
		fs.Usage()
		return exitUsage
	}

	r, err := gitops.Discover(set.RepoDir, set.AppsRoot)
	if err != nil {
		return fail(stderr, "hoist restart", exitFailure, err)
	}
	// An explicitly supplied selector that normalises to nothing is refused rather than treated
	// as "all". `--family "$FAMILY"` with the variable unset would otherwise restart every
	// family in the env — the widest possible blast radius from what reads like a narrowing
	// An omitted flag still means all; only a given-but-empty one is an
	// error, which is a distinction fs.Visit can make and the value alone cannot.
	families := splitFamilies(*family)
	if sel.given["family"] && len(families) == 0 {
		return failf(stderr, "hoist restart", exitUsage, "--family was given but names no family; omit it to restart every family in --env")
	}
	targets, err := restart.Targets(r, *env, families)
	if err != nil {
		return fail(stderr, "hoist restart", exitFailure, err)
	}

	// Production's gate, checked before the cluster is touched; a --dry-run of something that
	// would be refused still says so.
	svc := service.New(set, serviceDeps())
	if err := set.CheckRestartConfirmed(*env, *confirmProduction); err != nil {
		code, _ := renderStartError(io.Discard, stderr, "hoist restart", err)
		return code
	}

	// set.KubeContext is already the flag, else the selected repo's kube.context (selectRepo).
	_, usedContext, err := svc.RolloutNamed(set.KubeContext)
	if err != nil {
		return fail(stderr, "hoist restart", exitFailure, err)
	}

	ctx, stop := commandContext(0)
	defer stop()

	// Read every target first, so the operator sees the whole set — and every reason a restart
	// of it would not be graceful — before any of it rolls.
	pl, err := svc.RestartRead(ctx, *env, targets)
	if err != nil {
		return fail(stderr, "hoist restart", exitFailure, err)
	}
	printRestartTargets(stdout, *env, usedContext, pl)
	if len(pl.Targets) == 0 {
		return failf(stderr, "hoist restart", exitFailure, "none of %s's Deployments exist in the cluster", *env)
	}

	if *dryRun {
		fmt.Fprintf(stdout, "\ndry run: nothing was restarted\n")
		return 0
	}

	at := time.Now().UTC()
	restarted, rerr := svc.RestartDo(ctx, pl, at)
	if rerr != nil {
		fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(rerr.Error()))
		// Say what did roll before the failure: some pods are already restarting, and an
		// operator needs to know which.
		if len(restarted) > 0 {
			fmt.Fprintf(stderr, "hoist restart: already restarted: %s\n", strings.Join(restarted, ", "))
		}
		return exitFailure
	}
	fmt.Fprintf(stdout, "\nrestarted %d Deployment(s) at %s\n", len(restarted), at.Format(time.RFC3339))

	return watchRestart(ctx, svc, *env, restarted, at, stdout, stderr)
}

// splitFamilies turns the comma-separated --family value into a slice, dropping empties so
// `--family ""` and an unset flag mean the same thing (every family).
func splitFamilies(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// printRestartTargets names what will roll, what it supersedes, and every reason it will not be
// graceful — before anything is written.
func printRestartTargets(w io.Writer, env, kubeContext string, pl restart.Plan) {
	fmt.Fprintf(w, "restart %s (context %s): %d Deployment(s)\n\n", env, kubeContext, len(pl.Targets))
	for _, st := range pl.Targets {
		was := st.RestartedAt
		if was == "" {
			was = "never restarted this way"
		}
		fmt.Fprintf(w, "  %s  (%d replica(s), %s, last restart: %s)\n", st.Name, st.Replicas, st.Strategy, was)
		for _, c := range st.GracefulRestartConcerns() {
			fmt.Fprintf(w, "    warning: %s\n", c)
		}
	}
	for _, name := range pl.Absent {
		fmt.Fprintf(w, "  %s\n    warning: declared in the repo but not in the cluster — not restarted\n", name)
	}
}

// watchRestart follows the rollout the restart just started, at the configured cadence and
// within the configured deadline. This is the same question RolledOutStep asks of a promotion,
// asked directly: there is no promotion here to hang a step on.
func watchRestart(ctx context.Context, svc *service.Service, env string, names []string, at time.Time, stdout, stderr io.Writer) int {
	interval := svc.RestartInterval()
	if deadline := svc.Settings().Deadline; deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	pending := append([]string(nil), names...)
	for {
		progress, err := svc.RestartObserve(ctx, env, pending, at)
		if err != nil {
			return fail(stderr, "hoist restart", exitFailure, err)
		}
		var still []string
		for _, pr := range progress {
			switch {
			case pr.Superseded:
				// Something else restarted it while this one was in flight, so the rollout now
				// running is not this command's to report as its own success.
				return failf(stderr, "hoist restart", exitFailure, "%s was restarted by something else while this one was in flight; the pods are rolling, but not for this command", pr.Name)
			case pr.Blocked != "":
				return failf(stderr, "hoist restart", exitFailure, "%s: %s", pr.Name, pr.Blocked)
			case pr.Done:
				fmt.Fprintf(stdout, "  %s: %s\n", pr.Name, pr.Detail)
			default:
				still = append(still, pr.Name)
			}
		}
		if len(still) == 0 {
			fmt.Fprintf(stdout, "all %d Deployment(s) rolled\n", len(names))
			return 0
		}
		pending = still
		select {
		case <-ctx.Done():
			return failf(stderr, "hoist restart", exitFailure, "still rolling: %s (%v)", strings.Join(pending, ", "), ctx.Err())
		case <-time.After(interval):
		}
	}
}
