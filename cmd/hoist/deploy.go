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
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
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
// Everything after "which ref" is shared: the same freshness checks (including direct
// mode's own fresh-base cross-check), the same claim-then-rescan,
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
	//
	// A --dry-run still runs it for --direct — a dry run of something that would be refused
	// should say so, and the production refusal is the whole point of the gate. It does NOT
	// run for a plain dry run, whose only remaining check is "is repos[].github configured":
	// a non-direct dry run opens no PR and derives no id, so demanding a forge identity
	// refuses a read-only command for a reason that cannot apply to it — and `hoist plan
	// --dry-run`, the same operation for a promotion, has never demanded one (Copilot, PR #70).
	if *direct || !*dryRun {
		if code := checkDirectPreflight("hoist deploy", eff, *direct, *confirmDirect, *env, stderr); code != 0 {
			return code
		}
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
	if *direct {
		// Direct mode's own additional gap, identical to runPromote's: checkCloneCurrentForBase
		// only validates files this plan already names, so it cannot see an occurrence
		// origin/<base> has gained that the local checkout never had — gitops.Discover never
		// read that file. Only direct mode can put origin ahead of the local clone that way
		// (its own prior pushes), and only direct mode writes with no PR to catch it.
		buildFresh := func(fresh *gitops.Repo) (gitops.Plan, error) {
			return gitops.BuildDeployPlan(fresh, *env, ref, eff.promotable)
		}
		if err := checkNoMissingOccurrenceAtFreshBase(context.Background(), newGit, eff.repo, *base, eff.appsRoot, plan, buildFresh); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
			return exitFailure
		}
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
	// Both modes reach the Argo/rollout steps now that DirectSteps converges too (issue #66),
	// so these are unconditional. An earlier revision built them only for the PR path, because
	// direct mode stopped at the push and would otherwise have demanded a cluster for work it
	// never did; converging is the real fix, and it retires that gate.
	argoApps, err := engine.ArgoAppNames(r, plan.TargetEnv, plan.Edits)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	a, _, err := newArgo(kctx)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	ro, _, err := newRollout(kctx)
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %s\n", redact.Strings(err.Error()))
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

	err = driveToCompletion(ctx, steps, s, save, cfg.Poll, stderr)
	return reportDriveResult(stdout, stderr, "hoist deploy", s.SourceEnv, s.TargetEnv, s, err)
}
