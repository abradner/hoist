package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
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
	kubeContext := fs.String("kube-context", "", "kubeconfig context to restart in (the selected repo's kube.context when configured)")
	dryRun := fs.Bool("dry-run", false, "print what would be restarted, and any reason a restart would not be graceful, without touching the cluster")
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
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *env == "" {
		fmt.Fprintln(stderr, "hoist restart: --repo and --env are required")
		fs.Usage()
		return exitUsage
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	targets, err := restartTargets(r, *env, splitFamilies(*family))
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}

	// Production's gate. §4.5's PR-and-approval-comment gate cannot apply to an operation that
	// commits nothing, so what stands in its place is a second, distinct acknowledgement — the
	// same shape --confirm-direct already uses, and the same reasoning: a production restart
	// must never be one keystroke or one line of shell history. Checked before the cluster is
	// touched, and a --dry-run of something that would be refused still says so.
	if code := checkProductionRestart(eff, *env, *confirmProduction, stderr); code != 0 {
		return code
	}

	kctx := *kubeContext
	if kctx == "" && eff.cfg != nil {
		kctx = eff.cfg.Kube.Context
	}
	ro, usedContext, err := newRollout(kctx)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Read every target first, so the operator sees the whole set — and every reason a restart
	// of it would not be graceful — before any of it rolls.
	before := make([]rollout.DeploymentStatus, 0, len(targets))
	var absent []string
	for _, t := range targets {
		st, rerr := ro.Deployment(ctx, *env, t)
		if rerr != nil {
			// A Deployment the repo declares but the cluster does not have is a real mismatch
			// worth seeing — a family not deployed yet, a rename that landed in git only — but
			// it is not a reason to refuse to restart the ones that ARE there. Named in the
			// listing so the set is never quietly smaller than it looks (principle 5). Any
			// other read failure is a genuine failure and stops here.
			if errors.Is(rerr, rollout.ErrNotFound) {
				absent = append(absent, t)
				continue
			}
			fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(rerr.Error()))
			return exitFailure
		}
		st.Name = t // the name asked for, never whatever the read echoed back
		before = append(before, st)
	}
	printRestartTargets(stdout, *env, usedContext, before, absent)
	if len(before) == 0 {
		fmt.Fprintf(stderr, "hoist restart: none of %s's Deployments exist in the cluster\n", *env)
		return exitFailure
	}

	if *dryRun {
		fmt.Fprintf(stdout, "\ndry run: nothing was restarted\n")
		return 0
	}

	at := time.Now().UTC()
	var restarted []string
	for _, st := range before {
		if rerr := restartOne(ctx, ro, *env, st.Name, at); rerr != nil {
			fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(rerr.Error()))
			// Say what did roll before the failure: some pods are already restarting, and an
			// operator needs to know which.
			if len(restarted) > 0 {
				fmt.Fprintf(stderr, "hoist restart: already restarted: %s\n", strings.Join(restarted, ", "))
			}
			return exitFailure
		}
		restarted = append(restarted, st.Name)
	}
	fmt.Fprintf(stdout, "\nrestarted %d Deployment(s) at %s\n", len(restarted), at.Format(time.RFC3339))

	return watchRestart(ctx, ro, *env, restarted, cfg.Poll, stdout, stderr)
}

// restartOne patches one Deployment and resolves the one ambiguous outcome the patch has: the
// API server can commit the change and the connection can still fail before the response
// arrives. Reporting that as a failure is wrong in a way that matters — the pods are already
// rolling, and an operator who re-runs would roll them a second time.
//
// So a failed patch is followed by a read: if the annotation already carries this invocation's
// own stamp, the write landed and the error was only in hearing about it. Safe because the stamp
// is fixed for the whole invocation and unique to it (rollout.RestartStampLayout), so finding it
// there cannot mean someone else's restart.
//
// ErrNotFound is terminal either way: a Deployment that is not there was not restarted, and no
// amount of re-reading changes that.
func restartOne(ctx context.Context, ro rollout.Rollout, env, name string, at time.Time) error {
	err := ro.Restart(ctx, env, name, at)
	if err == nil || errors.Is(err, rollout.ErrNotFound) {
		return err
	}
	st, rerr := ro.Deployment(ctx, env, name)
	if rerr == nil && st.RestartedAt == at.UTC().Format(rollout.RestartStampLayout) {
		return nil
	}
	return err
}

// restartTargets is the distinct Deployment names the named families declare in env, or every
// family's when none are named.
//
// Read from the GitOps repo rather than by listing the namespace, because "family" is a concept
// the repo defines and the cluster does not: a namespace holds several families' workloads
// (asn-production alone holds half a dozen), so listing it would restart far more than asked.
// gitops.Discover already records each occurrence's Kind and Name, so this needs no new parsing.
func restartTargets(r *gitops.Repo, env string, families []string) ([]string, error) {
	e, ok := r.Envs[env]
	if !ok {
		return nil, fmt.Errorf("unknown env %q", env)
	}
	only := map[string]bool{}
	for _, f := range families {
		if _, ok := e.Families[f]; !ok {
			return nil, fmt.Errorf("%s has no family %q", env, f)
		}
		only[f] = true
	}
	seen := map[string]bool{}
	var out []string
	for name, fam := range e.Families {
		if len(only) > 0 && !only[name] {
			continue
		}
		for _, occ := range fam.Occurrences {
			// Deployments only. A Job or CronJob has no rollout to trigger, and re-running one
			// is a different operation with different consequences.
			if occ.Kind != "Deployment" || seen[occ.Name] {
				continue
			}
			seen[occ.Name] = true
			out = append(out, occ.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no Deployment to restart%s", env, onlyLabel(families))
	}
	sort.Strings(out)
	return out, nil
}

func onlyLabel(families []string) string {
	if len(families) == 0 {
		return ""
	}
	return " in " + strings.Join(families, ", ")
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

// checkProductionRestart is the whole of production's gate: a second, distinct acknowledgement
// repeating the env's exact name. An env the config does not list as production needs none.
func checkProductionRestart(eff effective, env, confirm string, stderr io.Writer) int {
	if eff.cfg == nil {
		// Fail closed. A flags-only run has no envs.production list, so every env would look
		// non-production by omission and `hoist --repo /path restart --env <production>` would
		// walk straight through the gate this function exists to be. checkDirectPreflight
		// refuses the same shape for the same reason (AGENTS.md §8: never provide a fallback
		// default for required configuration) — and here the answer cannot even be "refuse
		// outright", because a restart against an unconfigured repo is a legitimate thing to
		// want; it is "we cannot tell, so acknowledge it".
		if confirm != env {
			fmt.Fprintf(stderr, "hoist restart: no configured repo, so hoist cannot tell whether %s is production; pass --confirm-production=%s to restart it anyway\n", env, env)
			return exitUsage
		}
		return 0
	}
	if !eff.cfg.Envs.IsProduction(env) {
		return 0
	}
	if confirm == "" {
		fmt.Fprintf(stderr, "hoist restart: %s is a production env; restarting it requires --confirm-production=%s as well\n", env, env)
		return exitUsage
	}
	if confirm != env {
		fmt.Fprintf(stderr, "hoist restart: --confirm-production=%q does not match --env %q; repeat the exact env to acknowledge\n", confirm, env)
		return exitUsage
	}
	return 0
}

// printRestartTargets names what will roll, what it supersedes, and every reason it will not be
// graceful — before anything is written.
func printRestartTargets(w io.Writer, env, kubeContext string, sts []rollout.DeploymentStatus, absent []string) {
	fmt.Fprintf(w, "restart %s (context %s): %d Deployment(s)\n\n", env, kubeContext, len(sts))
	for _, st := range sts {
		was := st.RestartedAt
		if was == "" {
			was = "never restarted this way"
		}
		fmt.Fprintf(w, "  %s  (%d replica(s), %s, last restart: %s)\n", st.Name, st.Replicas, st.Strategy, was)
		for _, c := range st.GracefulRestartConcerns() {
			fmt.Fprintf(w, "    warning: %s\n", c)
		}
	}
	for _, name := range absent {
		fmt.Fprintf(w, "  %s\n    warning: declared in the repo but not in the cluster — not restarted\n", name)
	}
}

// watchRestart follows the rollout the restart just started, at the configured cadence and
// within the configured deadline. This is the same question RolledOutStep asks of a promotion,
// asked directly: there is no promotion here to hang a step on.
func watchRestart(ctx context.Context, ro rollout.Rollout, env string, names []string, poll config.PollConfig, stdout, stderr io.Writer) int {
	interval := time.Duration(poll.Rollout)
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if deadline := time.Duration(poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	pending := append([]string(nil), names...)
	for {
		var still []string
		for _, name := range pending {
			st, err := ro.Deployment(ctx, env, name)
			if err != nil {
				fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(err.Error()))
				return exitFailure
			}
			switch {
			case st.DeadlineExceeded:
				fmt.Fprintf(stderr, "hoist restart: %s: %s\n", name, st.Detail)
				return exitFailure
			case st.Complete:
				fmt.Fprintf(stdout, "  %s: %s\n", name, st.Detail)
			default:
				still = append(still, name)
			}
		}
		if len(still) == 0 {
			fmt.Fprintf(stdout, "all %d Deployment(s) rolled\n", len(names))
			return 0
		}
		pending = still
		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "hoist restart: still rolling: %s (%v)\n", strings.Join(pending, ", "), ctx.Err())
			return exitFailure
		case <-time.After(interval):
		}
	}
}
