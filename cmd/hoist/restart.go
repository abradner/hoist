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

	appplan "github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// runRestart is `hoist restart`: roll an env's Deployments without changing a single image.
//
// It goes through git rather than patching the live cluster, and that is the whole design.
// `kubectl rollout restart` works by writing kubectl.kubernetes.io/restartedAt onto the live
// Deployment's pod template. Every Application in the target repo runs with selfHeal: true, so
// Argo would see an annotation present live and absent in git, call it drift, and revert it —
// and because the reverted field is on spec.template, the revert is itself a second rollout.
// A live patch therefore buys two restarts, a drift alarm and a race. Writing the annotation
// into the manifest instead makes the restart a change git describes: reviewable, resumable,
// attributable, and converged by the same Argo the promotion path already waits on (§4.1).
//
// Everything after "what to write" is shared with promote and deploy: the same preflight, the
// same claim-then-rescan, the same steps, the same drive loop, artifacts from the same
// templates. Production is gated identically — a PR, and whatever approval mode the repo
// configures for that env — through the same checkDirectPreflight and DirectCommitGateStep,
// so this path adds no second opinion about what production means (§4.5).
func runRestart(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	env := fs.String("env", "", "target env: the Argo destination namespace whose Deployments to restart (required)")
	family := fs.String("family", "", "comma-separated family names to restart; default every family in --env")
	base := fs.String("base", "main", "the GitOps repo's default branch: what the restart branch is created from and the PR targets")
	direct := fs.Bool("direct", false, "commit straight to --base with no PR — non-production envs only. internal/engine.DirectCommitGateStep refuses this outright for any env listed in the selected repo's envs.production, regardless of this flag. Requires --confirm-direct=<env> too")
	confirmDirect := fs.String("confirm-direct", "", "the operator's explicit second acknowledgement required alongside --direct: must repeat --env's exact value (refused otherwise)")
	kubeContext := fs.String("kube-context", "", "kubeconfig context for the Argo/rollout steps (the selected repo's kube.context when configured)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway (has no effect on ci.none: block)")
	dryRun := fs.Bool("dry-run", false, "print the diff this restart would make and exit without touching git, the forge or the cluster")
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

	// Same gate, same position as promote and deploy: before anything is discovered, planned
	// or written. A --dry-run still runs it for --direct (a dry run of something that would be
	// refused should say so) but not for a plain dry run, which opens no PR and derives no id.
	if *direct || !*dryRun {
		if code := checkDirectPreflight("hoist restart", eff, *direct, *confirmDirect, *env, stderr); code != 0 {
			return code
		}
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	// One instant for the whole plan, taken here rather than inside BuildRestartPlan: it is the
	// entire content of the change, so it is also this promotion's identity (engine.DeriveID),
	// and a caller that cannot see it cannot report what it did.
	at := time.Now().UTC()
	plan, err := gitops.BuildRestartPlan(r, *env, splitFamilies(*family), at)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}

	if *dryRun {
		printRestartPlan(stdout, plan)
		diff, derr := restartDiff(eff.repo, plan.Restarts)
		if derr != nil {
			fmt.Fprintf(stderr, "hoist restart: %v\n", derr)
			return exitFailure
		}
		fmt.Fprintf(stdout, "\n%s", diff)
		return 0
	}

	// The clone must agree with origin/<base> for the files this plan touches, exactly as the
	// promotion path requires: the plan's line numbers were read off this checkout's own disk,
	// and a stale checkout means an anchor that no longer points where it did.
	if err := checkCloneCurrentForRestart(context.Background(), eff.repo, *base, plan.Restarts); err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	// Both modes, unlike promote and deploy, where this check is direct-only. Their omission
	// is visible: a promotion that misses an occurrence leaves a file the reviewer can see is
	// absent from a diff full of image changes. A restart's omission is invisible — the file
	// that should have been touched simply has no diff line, in a PR whose every hunk looks
	// identical — so a PR is not the second look here that it is there, and `restart --env X`
	// could silently restart less than the env it just claimed (Copilot, PR #79).
	if err := checkNoMissingWorkloadAtFreshBase(context.Background(), eff.repo, *base, eff.appsRoot, *env, splitFamilies(*family), plan); err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}

	f, err := newForge(eff.cfg.GitHub)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	kctx := *kubeContext
	if kctx == "" && eff.cfg != nil {
		kctx = eff.cfg.Kube.Context
	}
	argoApps, err := engine.ArgoAppNamesForRestart(r, plan.TargetEnv, plan.Restarts)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	a, _, err := newArgo(kctx)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	ro, _, err := newRollout(kctx)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	s, release, err := buildPromotionForConfirm(ctx, eff, plan, *base, *overrideCINone, newGit, f, argoApps)
	if s != nil {
		s.Direct = *direct
	}
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	released := false
	defer func() {
		if !released {
			released = true
			release()
		}
	}()

	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		fmt.Fprintf(stderr, "hoist restart: %v\n", err)
		return exitFailure
	}
	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist restart: waiting for signing approval...")
		}
	}
	var steps []engine.Step
	if *direct {
		steps = engine.AllDirectSteps(newGit, a, ro, eff.cfg.Envs.Production, true, onWaiting)
	} else {
		steps = engine.AllSteps(newGit, f, a, ro, onWaiting)
	}
	save := func(st *engine.PromotionState) error {
		if err := engine.SaveState(statePath, st); err != nil {
			return err
		}
		if !released {
			released = true
			release()
		}
		return nil
	}
	driveErr := driveToCompletion(ctx, steps, s, save, cfg.Poll, stderr)
	return reportDriveResult(stdout, stderr, "hoist restart", "", plan.TargetEnv, s, driveErr)
}

// splitFamilies turns the comma-separated --family value into the slice BuildRestartPlan takes,
// dropping empties so `--family ""` and an unset flag mean the same thing (every family).
func splitFamilies(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// printRestartPlan is the dry run: what would be written, where, and what it supersedes.
// Deliberately not printPlan — that renders an image diff, and a restart changes no image, so
// every column it prints would be empty (AGENTS.md principle 1).
func printRestartPlan(w io.Writer, plan gitops.Plan) {
	fmt.Fprintf(w, "restart %s: %d Deployment(s)\n\n", plan.TargetEnv, len(plan.Restarts))
	for _, re := range plan.Restarts {
		was := re.Old
		if was == "" {
			was = "never restarted by hoist"
		}
		fmt.Fprintf(w, "  %s %s  (%s)\n", re.Kind, re.Name, re.File)
		fmt.Fprintf(w, "    %s: %s -> %s\n", gitops.RestartAnnotation, was, re.New)
	}
	if len(plan.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings (%d):\n", len(plan.Warnings))
		for _, wn := range plan.Warnings {
			fmt.Fprintf(w, "  [%s] %s\n", wn.Code, wn.Message)
		}
	}
	fmt.Fprintf(w, "\nNo image changes. Argo rolls the pods because the pod template changed.\n")
}

// checkNoMissingWorkloadAtFreshBase is checkNoMissingOccurrenceAtFreshBase for a restart:
// direct mode's only defence against writing a subset of what it claims to write.
//
// checkCloneCurrentForRestart compares the files this plan already names, which is enough for
// the PR path because a PR is reviewed against origin's own tree before it merges. A direct
// push has no second look, so a Deployment origin/<base> has gained — a new file in a family,
// or a whole new family — is invisible to a plan built from the local checkout: every file the
// plan knows about matches, the check passes, and `hoist restart --env X` restarts less than
// the whole env it just told the operator it restarted (Copilot, PR #79).
func checkNoMissingWorkloadAtFreshBase(ctx context.Context, cloneDir, base, appsRoot, env string, families []string, plan gitops.Plan) error {
	snap, cleanup, err := discoverAtFreshBase(ctx, newGit, cloneDir, base)
	if err != nil {
		return err
	}
	defer cleanup()

	freshRepo, err := gitops.Discover(snap, appsRoot)
	if err != nil {
		return fmt.Errorf("discovering origin/%s's own current tree: %w", base, err)
	}
	// The same instant, so the comparison is about which workloads exist, never about the
	// timestamps: two plans built a moment apart would otherwise differ in every value.
	freshPlan, err := gitops.BuildRestartPlan(freshRepo, env, families, restartStampOf(plan))
	if err != nil {
		return fmt.Errorf("planning against origin/%s's own current tree: %w", base, err)
	}

	known := make(map[string]bool, len(plan.Restarts))
	for _, r := range plan.Restarts {
		known[fmt.Sprintf("%s#%d", r.File, r.Doc)] = true
	}
	var missing []string
	for _, r := range freshPlan.Restarts {
		if !known[fmt.Sprintf("%s#%d", r.File, r.Doc)] {
			missing = append(missing, fmt.Sprintf("%s %s (%s)", r.Kind, r.Name, r.File))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"origin/%s has Deployment(s) %s's own local checkout doesn't know about at all: %s — fetch/merge to update your clone and re-run; direct mode never writes a file it can't already see locally",
		base, cloneDir, strings.Join(missing, ", "),
	)
}

// restartStampOf recovers the instant a built restart plan writes, so a second plan built for
// comparison can be stamped identically.
func restartStampOf(plan gitops.Plan) time.Time {
	if len(plan.Restarts) == 0 {
		return time.Now().UTC()
	}
	t, err := time.Parse(time.RFC3339, plan.Restarts[0].New)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}

// restartDiff renders what printRestartPlan's summary describes, byte for byte — the same
// function the TUI's confirm screen shows, so the two surfaces cannot drift.
func restartDiff(root string, restarts []gitops.RestartEdit) (string, error) {
	return appplan.RenderRestartDiff(root, restarts)
}

// checkCloneCurrentForRestart is checkCloneCurrentForBase for a restart plan: the same
// question — does this checkout still agree with origin/<base> for the files about to be
// written — asked of the files a restart names rather than the files an image edit names.
func checkCloneCurrentForRestart(ctx context.Context, cloneDir, base string, restarts []gitops.RestartEdit) error {
	seen := map[string]bool{}
	var files []gitops.Edit
	for _, re := range restarts {
		if seen[re.File] {
			continue
		}
		seen[re.File] = true
		// checkCloneCurrentForBase reads only Edit.File; the rest of the value is unused by it,
		// so this borrows the existing comparison rather than growing a second copy of it.
		files = append(files, gitops.Edit{Occurrence: gitops.Occurrence{File: re.File}})
	}
	return checkCloneCurrentForBase(ctx, newGit, cloneDir, base, files)
}

// familiesOfRestart recovers which families a built restart plan covers, so a plan built for
// comparison against origin's tree can be given the same selection. Derived from the plan's own
// files rather than carried alongside it: the plan is what crossed the boundary, and a second
// copy of the selection could disagree with the files it is supposed to describe.
func familiesOfRestart(r *gitops.Repo, plan gitops.Plan) []string {
	env, ok := r.Envs[plan.TargetEnv]
	if !ok {
		return nil
	}
	byDir := make(map[string]string, len(env.Families))
	for name, f := range env.Families {
		byDir[f.Dir] = name
	}
	seen := map[string]bool{}
	var out []string
	for _, re := range plan.Restarts {
		name, ok := byDir[path.Dir(re.File)]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
