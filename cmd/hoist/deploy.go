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
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
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
// Everything after "which ref" is shared through svc.StartPromotion: the same freshness checks
// (including direct mode's own fresh-base cross-check), the same claim-then-rescan, the same
// steps, the same drive loop, and artifacts rendered from the same templates (which know to
// describe a deploy rather than a promotion — see gitops.Plan.Variant).
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
	base := fs.String("base", sel.base, "the GitOps repo's default branch: what the deploy branch is created from and the PR targets (may also be given before the command)")
	direct := fs.Bool("direct", false, "commit straight to --base with no PR — non-production envs only. internal/engine.DirectCommitGateStep refuses this outright for any env listed in the selected repo's envs.production, regardless of this flag: this flag is not itself the gate, only how the CLI reaches it. Requires --confirm-direct=<env> too")
	confirmDirect := fs.String("confirm-direct", "", "the operator's explicit second acknowledgement required alongside --direct: must repeat --env's exact value (refused otherwise)")
	kubeContext := fs.String("kube-context", sel.kubeContext, "kubeconfig context for the Argo/rollout steps (the selected repo's kube.context when configured; may also be given before the command)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway (has no effect on ci.none: block)")
	dryRun := fs.Bool("dry-run", false, "print the diff this deploy would make and exit without touching git, the forge or the cluster")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.promotable, sel.base, sel.kubeContext = *repo, *appsRoot, *promotable, *base, *kubeContext
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
	// --dry-run`, the same operation for a promotion, has never demanded one.
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
	set := settingsFor(cfg, eff)
	set.KubeContext = *kubeContext
	svc := service.New(set, serviceDeps())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	// service.Plan attaches WarnDeployIntoProduction itself for every deploy it builds, so the
	// dry-run output, the confirm screen and the PR body all carry it by construction — the CLI
	// and the TUI's own deploy path (internal/app's openDeploy) no longer attach it separately.
	// It runs under the same signal/deadline ctx as everything after it (harmless for the
	// dry-run return below, which does nothing more with ctx).
	pc, err := svc.Plan(ctx, service.PlanRequest{Repo: r, Target: *env, Deploy: &ref})
	if err != nil {
		fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
		return exitFailure
	}
	plan := pc.Plan

	if *dryRun {
		// The configured promotable list travels here as it does for `hoist plan`, so a deploy
		// narrowed by --promotable does not call the operator's other first-party repos
		// third-party in its Untouched section (issue #65).
		var configured []string
		if eff.cfg != nil {
			configured = eff.cfg.Promotable
		}
		if err := printPlan(stdout, r, &plan, eff.promotable, configured, nil); err != nil {
			fmt.Fprintf(stderr, "hoist deploy: %v\n", err)
			return exitFailure
		}
		return 0
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist deploy: waiting for signing approval...")
		}
	}
	req := pc.Request(service.Mode{Direct: *direct, Confirmed: true, OverrideCINone: *overrideCINone})
	d, err := svc.StartPromotion(ctx, req, service.Hooks{OnWaiting: onWaiting})
	if code, done := renderStartError(stdout, stderr, "hoist deploy", err); done {
		return code
	}

	err = d.Run(ctx, runHooksForCLI(stderr))
	s := d.State()
	return reportDriveResult(stdout, stderr, "hoist deploy", s.SourceEnv, s.TargetEnv, &s, err)
}
