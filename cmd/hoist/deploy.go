package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// runDeploy is `hoist deploy`: write one named image reference into one env, then drive the
// same pipeline `hoist promote` drives. This is the "image bump" half of hoist's problem
// statement — the operator has a new build and wants it live — where promote is the
// "staging -> production" half.
//
// It is a sibling of runPromote rather than a flag on it because the two differ in where the
// reference comes from. A promotion reads a source env and resolves what it runs (pods, then
// the manifest, then the registry); a deploy is handed the reference outright, so none of that
// resolution machinery applies and neither do --from, --digest or the digest-source flags.
// Everything after "which ref" is shared: the same freshness check, the same claim-then-rescan,
// the same steps, the same drive loop, and artifacts rendered from the same templates (which
// know to describe a deploy rather than a promotion — see gitops.Plan.Variant).
//
// --image must be fully pinned (repo:tag@sha256:...). hoist never writes a bare tag
// (invariant 1), and unlike a promotion there is no source env to resolve a digest from, so
// there is nothing to fall back to: an unpinned --image is refused outright by
// gitops.BuildDeployPlan rather than resolved.
func runDeploy(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	env := fs.String("env", "", "target env: the Argo destination namespace to write into (required)")
	img := fs.String("image", "", "the image to deploy, fully pinned: repo:tag@sha256:<64 hex> (required). Every occurrence of that repo in --env is rewritten to it")
	promotable := fs.String("promotable", sel.promotable, "comma-separated image repo prefixes hoist may write (see hoist plan -h)")
	base := fs.String("base", "main", "the GitOps repo's default branch: what the deploy branch is created from and the PR targets")
	direct := fs.Bool("direct", false, "commit straight to --base with no PR — non-production envs only. internal/engine.DirectCommitGateStep refuses this outright for any env listed in the selected repo's envs.production, regardless of this flag: this flag is not itself the gate, only how the CLI reaches it. Requires --confirm-direct=<env> too")
	confirmDirect := fs.String("confirm-direct", "", "the operator's explicit second acknowledgement required alongside --direct: must repeat --env's exact value (refused otherwise)")
	kubeContext := fs.String("kube-context", "", "kubeconfig context for the Argo/rollout steps (the selected repo's kube.context when configured)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway (has no effect on ci.none: block)")
	dryRun := fs.Bool("dry-run", false, "print the diff this deploy would make and exit without touching git, the forge or the cluster")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.promotable = *repo, *appsRoot, *promotable
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *env == "" || *img == "" {
		fmt.Fprintln(stderr, "hoist deploy: --repo, --env and --image are required")
		fs.Usage()
		return exitUsage
	}
	ref, err := image.Parse(*img)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: --image %s: %v\n", *img, err)
		return exitUsage
	}

	// The same single gate `hoist promote` uses, in the same position: before anything is
	// discovered, planned or written, so a production target is refused outright rather than
	// after a fast path could report success (see checkDirectPreflight's own doc comment).
	// A --dry-run still runs it: a dry run of something that would be refused should say so.
	if code := checkDirectPreflight("hoist deploy", eff, *direct, *confirmDirect, *env, stderr); code != 0 {
		return code
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	plan, err := gitops.BuildDeployPlan(r, *env, ref, eff.promotable)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}

	if *dryRun {
		if err := printPlan(stdout, r, &plan, eff.promotable, nil); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
			return exitFailure
		}
		return 0
	}

	if err := checkCloneCurrentForBase(context.Background(), newGit, eff.repo, *base, plan.Edits); err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	if !anyRealEdit(plan.Edits) {
		fmt.Fprintf(stdout, "hoist deploy: %s already runs %s; nothing to deploy.\n", *env, ref)
		return 0
	}

	f, err := newForge(eff.cfg.GitHub)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	kctx := *kubeContext
	if kctx == "" && eff.cfg != nil {
		kctx = eff.cfg.Kube.Context
	}
	// Only the PR path reaches an Argo step; direct mode stops at the push (issue #66), so it
	// must not require a reachable cluster for work it never does — the same split runPromote
	// makes.
	var (
		argoApps []string
		a        argo.Argo
		ro       rollout.Rollout
	)
	if !*direct {
		if argoApps, err = engine.ArgoAppNames(r, plan.TargetEnv, plan.Edits); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
			return exitFailure
		}
		if a, _, err = newArgo(kctx); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %s\n", redact.Strings(err.Error()))
			return exitFailure
		}
		if ro, _, err = newRollout(kctx); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %s\n", redact.Strings(err.Error()))
			return exitFailure
		}
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
		fmt.Fprintf(stderr, "hoist deploy: %s\n", redact.Strings(err.Error()))
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
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist deploy: waiting for signing approval...")
		}
	}
	var steps []engine.Step
	if *direct {
		steps = engine.DirectSteps(newGit, eff.cfg.Envs.Production, true, onWaiting)
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

	err = driveToCompletion(ctx, steps, s, save, cfg.Poll, stderr)
	return reportDriveResult(stdout, stderr, "hoist deploy", s.SourceEnv, s.TargetEnv, s, err)
}
