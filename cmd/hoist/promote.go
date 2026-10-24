package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/forge/github"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
)

// newGit and newForge are variables so tests substitute fakes/local fixtures: no test in this
// repo points promote at a real GitHub repo or lets it touch a checkout other than a fixture
// it created itself.
var (
	newGit   git.Git = git.Exec{}
	newForge         = func(ownerRepo string) (forge.Forge, error) { return github.New(ownerRepo) }
)

// runPromote is `hoist promote`: builds the same gitops.Plan `hoist plan --dry-run` would
// print, then drives internal/engine's steps through internal/service.StartPromotion to
// actually commit it, push it and open a PR (or, in --direct mode, push straight to --base).
// Named "promote" rather than "push": the command's whole point is the promotion — commit +
// push + PR together — and "push" would read as only the git step, one of several this command
// actually performs.
//
// Preflight (checkDirectPreflight), planning (svc.Plan) and taking the plan the rest of the way
// (svc.StartPromotion) are now the same three calls `hoist deploy` and the TUI's own confirm
// screens make: the CLI and the TUI used to assemble the freshness/claim/save/preflight
// sequence by hand, in two different orders, which is exactly what let the TUI reach the
// production direct-commit gate only after its own claim and initial state save (AGENTS.md
// §4.5 trust boundary). Unifying on internal/service.StartPromotion closes that gap for both
// callers at once.
func runPromote(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	from := fs.String("from", "", "source env: the Argo destination namespace to read digests from (required)")
	to := fs.String("to", "", "target env: the Argo destination namespace to rewrite (required)")
	promotable := fs.String("promotable", sel.promotable, "comma-separated image repo prefixes hoist may promote (see hoist plan -h)")
	base := fs.String("base", sel.base, "the GitOps repo's default branch: what the promotion branch is created from and the PR targets (may also be given before the command)")
	direct := fs.Bool("direct", false, "commit straight to --base with no PR — non-production envs only. internal/engine.DirectCommitGateStep refuses this outright for any env listed in the selected repo's envs.production, regardless of this flag: this flag is not itself the gate, only how the CLI reaches it. Requires --confirm-direct=<env> too")
	confirmDirect := fs.String("confirm-direct", "", "the operator's explicit second acknowledgement required alongside --direct: must repeat --to's exact value (refused otherwise) — the CLI's stronger keypress-then-confirm shape (the TUI's equivalent is internal/app/tags' own keypress + huh.Confirm dialog, which names the same env in its prompt)")
	digests := digestFlag{}
	fs.Var(digests, "digest", "repo=repo:tag@sha256:<64 hex> — plan this reference for repo instead of what --from runs (see hoist plan -h)")
	var rf resolveFlags
	fs.StringVar(&rf.kubeContext, "kube-context", sel.kubeContext, "kubeconfig context whose pods supply digests (see hoist plan -h)")
	fs.StringVar(&rf.digestSources, "digest-sources", sel.resolve.digestSources, "comma-separated digest sources, first wins (see hoist plan -h; may also be given before the command)")
	fs.StringVar(&rf.registryAuth, "registry-auth", sel.resolve.registryAuth, "comma-separated registry credential sources tried in order (see hoist plan -h; may also be given before the command)")
	fs.StringVar(&rf.clusterSecret, "cluster-secret", sel.resolve.clusterSecret, "namespace/name of a pull secret for the cluster credential source (see hoist plan -h; may also be given before the command)")
	fs.StringVar(&rf.opRef, "op-ref", sel.resolve.opRef, "op://vault/item/field for the op credential source (see hoist plan -h; may also be given before the command)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway (has no effect on ci.none: block, which has no override)")
	quiet := fs.Bool("quiet", false, quietUsage)
	if err := parseFlagsOnly(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.promotable, sel.base, sel.kubeContext, sel.resolve = *repo, *appsRoot, *promotable, *base, rf.kubeContext, rf
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	// The same refusal `hoist plan` and the root give an explicit empty
	// --digest-sources/--registry-auth (#132's followup: promote had silently fallen back
	// to the config's chain).
	if msg, bad := emptyResolveFlag(sel.given, rf); bad {
		return failf(stderr, "hoist promote", exitUsage, "%s", msg)
	}
	set, err := selectRepo(cfg, sel)
	if err != nil {
		return fail(stderr, "hoist promote", exitFailure, err)
	}
	if set.RepoDir == "" || *from == "" || *to == "" {
		fmt.Fprintln(stderr, "hoist promote: --repo, --from and --to are required")
		fs.Usage()
		return exitUsage
	}
	if code := checkDirectPreflight("hoist promote", set, *direct, *confirmDirect, *to, stderr); code != 0 {
		return code
	}

	opts, err := service.NewResolveOptions(cfg, set.Repo, rf.digestSources, rf.registryAuth, rf.clusterSecret, rf.opRef, rf.kubeContext)
	if err != nil {
		return fail(stderr, "hoist promote", exitUsage, err)
	}
	r, err := gitops.Discover(set.RepoDir, set.AppsRoot)
	if err != nil {
		return fail(stderr, "hoist promote", exitFailure, err)
	}
	set.Resolve = opts
	svc := service.New(set, serviceDeps())
	if err := svc.CheckOverrides(r, *from, digests); err != nil {
		return fail(stderr, "hoist promote", exitFailure, err)
	}
	ctx, stop := commandContext(time.Duration(cfg.Poll.Deadline))
	defer stop()

	// Plan prepends resolve.Warnings itself now (service.Plan's own doc comment) — runPlan and
	// the TUI plan screen go through the identical call, so a pods/manifest digest disagreement
	// can never reach one and not the other again. It runs
	// under the same signal/deadline ctx as everything after it, so ^C or the poll deadline can
	// interrupt resolution too, not just the drive that follows it.
	// From here every line on stderr goes through the narrator, whose "still …" line is written
	// from its own goroutine; stop runs before this function's own final messages are read.
	n := newNarrator(stderr, *quiet)
	stopNarrator := n.watch()
	defer stopNarrator()
	stderr = n

	pc, err := svc.Plan(ctx, service.PlanRequest{Repo: r, Source: *from, Target: *to, Overrides: digests, Progress: n.progress})
	if err != nil {
		return fail(stderr, "hoist promote", exitFailure, err)
	}
	plan := pc.Plan

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist promote: waiting for signing approval...")
		}
	}
	req := pc.Request(service.Mode{Direct: *direct, Confirmed: true, OverrideCINone: *overrideCINone})
	d, err := svc.StartPromotion(ctx, req, n.hooks(onWaiting))
	if code, done := renderStartError(stdout, stderr, "hoist promote", err); done {
		return code
	}
	// Preflight is over; what runs next is the drive's first walk, which announces its own Acts.
	n.idle()

	err = d.Run(ctx, n.runHooks())
	s := d.State()
	return reportDriveResult(stdout, stderr, "hoist promote", plan.SourceEnv, plan.TargetEnv, &s, err)
}

// renderStartError renders svc.StartPromotion's own error outcomes: the no-op "already current"
// success case (exit 0, the exact wording runPromote/runDeploy have always printed), a usage
// error (exitUsage), or anything else — an in-flight conflict, a genuinely blocked direct commit,
// a claim or forge failure — through the same redact.Strings final boundary reportDriveResult's
// own default branch uses (R-002: a step's Act error can embed a registered credential verbatim).
// done is false only when err is nil, telling the caller to proceed to Run.
func renderStartError(stdout, stderr io.Writer, cmdName string, err error) (code int, done bool) {
	if err == nil {
		return 0, false
	}
	var already *service.AlreadyCurrentError
	if errors.As(err, &already) {
		if already.Deploy {
			fmt.Fprintf(stdout, "%s: %s already runs %s; nothing to deploy.\n", cmdName, already.TargetEnv, already.Ref)
		} else {
			fmt.Fprintf(stdout, "%s: %s -> %s is already current; nothing to promote.\n", cmdName, already.SourceEnv, already.TargetEnv)
		}
		return 0, true
	}
	var usage *service.UsageError
	if errors.As(err, &usage) {
		return failf(stderr, cmdName, exitUsage, "%s", usage.Msg), true
	}
	fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(err.Error()))
	return exitFailure, true
}

// reportDriveResult renders Driver.Run's outcome the same way for hoist promote and
// hoist resume: the branch/commit/PR/merge summary on success, the specific messages
// AGENTS.md's "waiting for signing approval" / ErrWaiting / ctx deadline / Blocked cases call
// for, and the redact.Strings final boundary for anything else (a step's Act error
// can embed a registered credential verbatim via a failed git command's wrapped stderr).
func reportDriveResult(stdout, stderr io.Writer, cmdName, sourceEnv, targetEnv string, s *engine.PromotionState, err error) int {
	switch {
	case err == nil:
		// A deploy has no source env, so the promotion's "A -> B" would render as
		// ": " with a hole in it — the same empty-SourceEnv defect the templates and
		// printPlan were reworked for, on the most visible line hoist prints. The arrow goes
		// with it: "-> env" with nothing on its left is the same hole one character narrower,
		// and a deploy is not a movement between two places anyway. It names the one env.
		if sourceEnv == "" {
			fmt.Fprintf(stdout, "%s: %s\n", cmdName, targetEnv)
		} else {
			fmt.Fprintf(stdout, "%s: %s -> %s\n", cmdName, sourceEnv, targetEnv)
		}
		fmt.Fprintf(stdout, "  branch: %s\n", s.Branch)
		fmt.Fprintf(stdout, "  commit: %s\n", s.CommitSHA)
		switch {
		case s.PR == nil:
			// A successfully completed promotion with no PR at all is direct mode's own
			// signature (it never creates one) — reportDriveResult is shared with resume.go,
			// which has no --direct flag of its own to check, so this reads it off the
			// PromotionState itself rather than taking a parameter only promote.go could supply.
			fmt.Fprintf(stdout, "  pushed straight to %s (direct mode, no PR)\n", s.Base)
		case s.PR != nil:
			fmt.Fprintf(stdout, "  PR: %s\n", s.PR.URL)
		}
		if s.MergeSHA != "" {
			fmt.Fprintf(stdout, "  merged: %s\n", s.MergeSHA)
		}
		return 0
	case errors.Is(err, engine.ErrWaiting):
		fmt.Fprintf(stderr, "%s: still waiting for signing approval; re-run to resume\n", cmdName)
		return exitFailure
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "%s: %s at %s (poll.deadline elapsed); re-run to resume\n", cmdName, s.Phase, redact.Strings(historyDetail(s)))
		return exitFailure
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(stderr, "%s: interrupted while %s; re-run to resume\n", cmdName, s.Phase)
		return exitFailure
	default:
		var blocked *engine.BlockedError
		if errors.As(err, &blocked) {
			fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(blocked.Error()))
			return exitFailure
		}
		fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(err.Error()))
		return exitFailure
	}
}

// historyDetail is the newest History entry's Detail that is not "acted", for a deadline
// message that says what it was last observed waiting on rather than just naming the phase.
// "acted" is skipped because History records an unchanged wait once: a step that re-acts every
// poll (ArgoRefreshedStep) leaves its "acted" after the wait it is still in (#243).
func historyDetail(s *engine.PromotionState) string {
	for i := len(s.History) - 1; i >= 0; i-- {
		if s.History[i].Detail != "acted" {
			return s.History[i].Detail
		}
	}
	return ""
}

// checkDirectPreflight is the CLI's --direct gate, shared by `hoist promote` and `hoist deploy`:
// the service's own preflight (internal/service/policy.go — one statement of what a start needs,
// so the commands and the TUI refuse the same shapes in the same words, AR-M8) with the one thing
// that is CLI-only between its two halves: the --confirm-direct match, a second typed
// acknowledgement with no TUI analogue (the TUI's is its keypress plus a huh.Confirm naming the
// env). The production refusal it ends with is the same DirectCommitGateStep call StartPromotion
// makes again from inside, never a second rule (§8, layered checks: the deletion test).
//
// Returns 0 to proceed, or the exit code to return. cmdName prefixes every message.
func checkDirectPreflight(cmdName string, set service.Settings, direct bool, confirmDirect, targetEnv string, stderr io.Writer) int {
	mode := service.Mode{Direct: direct}
	if err := set.PreflightConfig(mode); err != nil {
		code, _ := renderStartError(io.Discard, stderr, cmdName, err)
		return code
	}
	if !direct {
		return 0
	}
	if confirmDirect == "" {
		return failf(stderr, cmdName, exitUsage, "--direct requires --confirm-direct=<env> too — naming the env twice is the acknowledgement, so a direct write is never one flag")
	}
	if confirmDirect != targetEnv {
		return failf(stderr, cmdName, exitUsage, "--confirm-direct=%q does not match the target env %q; repeat the exact target env to confirm", confirmDirect, targetEnv)
	}
	// Called before resolving digests, building the plan or reaching the no-op fast path, so a
	// production target is refused before anything else can mask or bypass it (§4.5). Confirmed
	// is true: reaching here required --confirm-direct to equal the target exactly.
	mode.Confirmed = true
	if err := set.PreflightGate(targetEnv, mode); err != nil {
		code, _ := renderStartError(io.Discard, stderr, cmdName, err)
		return code
	}
	return 0
}
