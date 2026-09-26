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
// screens make — see internal/service's own design doc for why unifying them was worth a whole
// PR: the CLI and the TUI used to assemble the freshness/claim/save/preflight sequence by hand,
// in two different orders, which is exactly what let the TUI reach the production direct-commit
// gate only after its own claim and initial state save (AGENTS.md §4.5 trust boundary).
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
	if err := fs.Parse(args); err != nil {
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
		fmt.Fprintf(stderr, "hoist promote: %s\n", msg)
		return exitUsage
	}
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *from == "" || *to == "" {
		fmt.Fprintln(stderr, "hoist promote: --repo, --from and --to are required")
		fs.Usage()
		return exitUsage
	}
	if code := checkDirectPreflight("hoist promote", eff, *direct, *confirmDirect, *to, stderr); code != 0 {
		return code
	}

	opts, err := service.NewResolveOptions(cfg, eff.cfg, rf.digestSources, rf.registryAuth, rf.clusterSecret, rf.opRef, rf.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitUsage
	}
	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	set := settingsFor(cfg, eff)
	set.Resolve = opts
	svc := service.New(set, serviceDeps())
	if err := svc.CheckOverrides(r, *from, digests); err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	// Plan prepends resolve.Warnings itself now (service.Plan's own doc comment) — runPlan and
	// the TUI plan screen go through the identical call, so a pods/manifest digest disagreement
	// can never reach one and not the other again (AGENTS.md §4's Divergences, item 10).
	pc, err := svc.Plan(context.Background(), service.PlanRequest{Repo: r, Source: *from, Target: *to, Overrides: digests})
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	plan := pc.Plan

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist promote: waiting for signing approval...")
		}
	}
	req := pc.Request(service.Mode{Direct: *direct, Confirmed: true, OverrideCINone: *overrideCINone})
	d, err := svc.StartPromotion(ctx, req, service.Hooks{OnWaiting: onWaiting})
	if code, done := renderStartError(stdout, stderr, "hoist promote", err); done {
		return code
	}

	err = d.Run(ctx, runHooksForCLI(stderr))
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
		fmt.Fprintf(stderr, "%s: %s\n", cmdName, usage.Msg)
		return exitUsage, true
	}
	fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(err.Error()))
	return exitFailure, true
}

// reportDriveResult renders driveToCompletion's outcome the same way for hoist promote and
// hoist resume: the branch/commit/PR/merge summary on success, the specific messages
// AGENTS.md's "waiting for signing approval" / ErrWaiting / ctx deadline / Blocked cases call
// for, and the redact.Strings final boundary for anything else (Finding B: a step's Act error
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

// historyDetail is the most recent History entry's Detail, for a deadline message that says
// what it was last observed waiting on rather than just naming the phase.
func historyDetail(s *engine.PromotionState) string {
	if len(s.History) == 0 {
		return ""
	}
	return s.History[len(s.History)-1].Detail
}

// checkDirectPreflight is the CLI's single --direct gate, shared by `hoist promote` and
// `hoist deploy`. It exists as one function rather than a copy per command because everything
// it enforces is an invariant (AGENTS.md §4.5, invariants 5 and 6) and a second copy is how
// those drift: the two commands must refuse a production env, an unconfigured repo, and a
// mismatched confirmation identically, or the weaker one becomes the way around the stronger.
// The `--confirm-direct` string match itself is a CLI-only UX gesture with no service-layer
// analogue (the TUI's own equivalent is its keypress + huh.Confirm dialog) and stays here for
// that reason; the actual production refusal this function ends with is the SAME
// engine.DirectCommitGateStep call service.Preflight makes again, unconditionally, from inside
// StartPromotion — calling the one enforcement point twice, never adding a second one (AGENTS.md
// §8, layered checks: the deletion test).
//
// Returns 0 to proceed, or the exit code to return. cmdName prefixes every message so each
// command still speaks in its own name.
func checkDirectPreflight(cmdName string, eff effective, direct bool, confirmDirect, targetEnv string, stderr io.Writer) int {
	if !direct {
		// The non-direct branch still needs a forge identity.
		if eff.cfg == nil || eff.cfg.GitHub == "" {
			fmt.Fprintf(stderr, "%s: the selected repo has no github: owner/name configured; add repos[].github to the config file\n", cmdName)
			return exitUsage
		}
		return 0
	}
	// Direct mode's whole safety rests on knowing envs.production (AGENTS.md invariant
	// 6): a flags-only run has no such list, which would make internal/engine.
	// DirectCommitGateStep's ProductionEnvs empty and every env look non-production by
	// omission. Fail fast rather than silently treat "unconfigured" as "safe" (AGENTS.md
	// §8: never provide a fallback default for required configuration).
	if eff.cfg == nil {
		fmt.Fprintf(stderr, "%s: --direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)\n", cmdName)
		return exitUsage
	}
	// Direct mode still needs the same github: owner/name the non-direct branch below
	// requires — not for the forge (direct mode never opens one), but because
	// engine.DeriveID(eff.cfg.GitHub, plan) hashes it into the promotion's id, which names
	// the state path, the branch and the worktree directory. Two repos both configured
	// without github: that promote the same env+digest set would otherwise derive the
	// IDENTICAL id — a real identity collision (one repo's run could overwrite the
	// other's state file or remove the other's still-active worktree as "unregistered
	// stale"), not merely a cosmetic gap. A git-only, no-PR operation still needs a stable
	// repo identity for that reason alone.
	if eff.cfg.GitHub == "" {
		fmt.Fprintf(stderr, "%s: the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)\n", cmdName)
		return exitUsage
	}
	if confirmDirect == "" {
		fmt.Fprintf(stderr, "%s: --direct requires --confirm-direct=<env> too — naming the env twice is the acknowledgement, so a direct write is never one flag\n", cmdName)
		return exitUsage
	}
	if confirmDirect != targetEnv {
		fmt.Fprintf(stderr, "%s: --confirm-direct=%q does not match the target env %q; repeat the exact target env to confirm\n", cmdName, confirmDirect, targetEnv)
		return exitUsage
	}
	// Round-N finding (Codex, P2): DirectCommitGateStep — internal/engine/direct.go's own
	// "sole enforcement point" for AGENTS.md invariant 5/6 — used to be constructed only
	// after BuildPlan and the all-no-op fast path further down this function, so a
	// --direct run against a production env whose plan happened to already be current
	// exited 0 claiming success ("already current") without the gate ever running, and a
	// resolution failure for a production target surfaced as an unrelated resolution
	// error instead of the required refusal — either way masking the refusal AGENTS.md
	// §4.5 promises "outright". Call the identical step here, first — before resolving
	// digests, building the plan, or reaching the no-op fast path — so a production
	// target is refused before anything else can mask or bypass it. Confirmed is always
	// true here: reaching this point already required --confirm-direct to equal --to
	// exactly, checked immediately above. service.Preflight calls this same step again
	// once svc.StartPromotion runs (unreachable through it whenever this refuses, per that
	// step's own doc comment) — this calls the one enforcement point twice, it does not
	// add a second one (AGENTS.md §8, layered checks: the deletion test).
	gate := engine.DirectCommitGateStep{ProductionEnvs: eff.cfg.Envs.Production, Confirmed: true}
	obs, err := gate.Observe(context.Background(), &engine.PromotionState{TargetEnv: targetEnv})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)
		return exitFailure
	}
	if obs.Blocked != "" {
		blocked := &engine.BlockedError{Step: engine.StepDirectGate, Reason: obs.Blocked}
		fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(blocked.Error()))
		return exitFailure
	}
	return 0
}
