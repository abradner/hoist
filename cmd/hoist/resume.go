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
	"github.com/abradner/hoist/pkg/redact"
)

// promotionsSettings builds the service.Settings runPromotions/runResume/runAbandon share:
// these three commands never select a single repo (a state file names its own, possibly
// different from whatever --repo would have selected), so this is deliberately narrower than
// settingsFor — just the config file, the operator's own --kube-context override, and the
// poll/deadline/retain knobs every one of svc.List/svc.Resume/svc.Abandon needs.
func promotionsSettings(cfg *config.Config, kubeOverride string) service.Settings {
	set := service.Settings{Config: cfg, KubeOverride: kubeOverride}
	if cfg != nil {
		set.Poll = pollIntervals(cfg.Poll)
		set.Deadline = time.Duration(cfg.Poll.Deadline)
		set.Retain = time.Duration(cfg.State.Retain)
	}
	return set
}

// boundedCommandContext gives runPromotions/runResume/runAbandon the same interruptible,
// deadline-bounded context: talking to a real
// forge/git/cluster (AGENTS.md §4.3) must never hang the command forever on one bad candidate.
func boundedCommandContext(deadline time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if deadline <= 0 {
		return ctx, stop
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	return ctx, func() { cancel(); stop() }
}

// runPromotions is `hoist promotions`: lists every promotion state file under
// $XDG_STATE_HOME/hoist/promotions/, with phase-as-observed (re-observed against the forge and
// worktree, never the state file's own possibly-stale Phase field — AGENTS.md §4.1). A
// promotion whose repo is no longer in the config file is listed with its last-recorded phase
// and a note, since there is nothing to re-observe it against.
//
// A terminal promotion (done, per this same re-observation) older than state.retain
// (StateConfig.Retain, default 30 days, measured from PromotionState.LastActivity) is archived
// — moved to engine.ArchiveDir, out of the live listing by default. This never changes what the
// re-observation just concluded: archiving only ever happens AFTER done is confirmed true, never
// inferred from age alone, so a promotion still genuinely in flight (however old) is untouched
// regardless of --repo/--archived. --archived additionally lists what is already archived,
// scoped by --repo exactly like the live listing.
func runPromotions(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist promotions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kubeContext := fs.String("kube-context", sel.kubeOverride(), "kubeconfig context to re-observe every promotion in, instead of each repo's own kube.context (may also be given before the command)")
	repoFilter := fs.String("repo", "", "only list promotions for this repo (owner/name, repos[].github) — default every configured repo")
	archived := fs.Bool("archived", false, "also list archived promotions (state.retain; still plain, readable JSON under the promotions/archive/ subdirectory)")
	if err := parseFlagsOnly(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	set := promotionsSettings(cfg, *kubeContext)
	svc := service.New(set, serviceDeps())

	ctx, stop := boundedCommandContext(set.Deadline)
	defer stop()

	listed, err := svc.List(ctx, service.ListOpts{RepoFullName: *repoFilter, ArchiveDoneOlderThan: set.Retain})
	if err != nil {
		fmt.Fprintf(stderr, "hoist promotions: %v\n", err)
		return exitFailure
	}
	if len(listed) == 0 && !*archived {
		if *repoFilter != "" {
			// Named explicitly rather than folded into the bare message below: a --repo typo
			// (repos[].github is owner/name, not the config entry's own name: or path:) would
			// otherwise print identically to "you have no promotions at all", with nothing to
			// suggest the filter itself might be the reason.
			fmt.Fprintf(stdout, "hoist promotions: no promotions found for --repo %s\n", *repoFilter)
			return 0
		}
		fmt.Fprintln(stdout, "hoist promotions: no promotions found")
		return 0
	}
	// archivedThisRun tracks ids this same invocation just moved to the archive (Listed.Archived),
	// so --archived's own listing below (which reads ArchiveDir fresh, after the loop) does not
	// print one of them a second time.
	archivedThisRun := map[string]bool{}
	for _, l := range listed {
		s := l.State
		switch {
		case l.Unconfigured:
			fmt.Fprintf(stdout, "%s  %-20s  %s (last recorded; repo %s is not in the config file, cannot re-observe)\n", s.ID, s.TargetEnv, s.Phase, s.RepoFullName)
		case l.Err != nil:
			fmt.Fprintf(stdout, "%s  %-20s  ? (%v)\n", s.ID, s.TargetEnv, l.Err)
		case l.Archived:
			fmt.Fprintf(stdout, "%s  %-20s  done (%s) — archived (older than %s)\n", s.ID, s.TargetEnv, service.Detail(l.Last.Observation), set.Retain)
			archivedThisRun[s.ID] = true
		case l.ArchiveErr != nil:
			fmt.Fprintf(stdout, "%s  %-20s  done (%s) — archiving failed: %v\n", s.ID, s.TargetEnv, service.Detail(l.Last.Observation), l.ArchiveErr)
		case l.Done:
			fmt.Fprintf(stdout, "%s  %-20s  done (%s)\n", s.ID, s.TargetEnv, service.Detail(l.Last.Observation))
		default:
			fmt.Fprintf(stdout, "%s  %-20s  %s: %s\n", s.ID, s.TargetEnv, l.Last.Step, service.Detail(l.Last.Observation))
		}
	}
	if *archived {
		arch, err := svc.ListArchived(*repoFilter)
		if err != nil {
			fmt.Fprintf(stderr, "hoist promotions: listing archived promotions: %v\n", err)
			return exitFailure
		}
		for _, s := range arch {
			if archivedThisRun[s.ID] {
				continue
			}
			fmt.Fprintf(stdout, "%s  %-20s  archived (last activity %s)\n", s.ID, s.TargetEnv, s.LastActivity().Format(time.RFC3339))
		}
	}
	return 0
}

// runResume is `hoist resume`: re-drives a specific promotion, identified either positionally
// by id or by --env <target-env> (erroring if more than one non-terminal promotion matches that
// env — ambiguous, and AGENTS.md invariant 5 says there should never legitimately be two).
// Re-drives through AllSteps exactly like `hoist promote`, from wherever Observe actually finds
// it — never from the recorded Phase.
func runResume(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := fs.String("env", "", "resume the (single, non-terminal) promotion targeting this env, instead of naming an id")
	kubeContext := fs.String("kube-context", sel.kubeOverride(), "kubeconfig context to observe and drive the promotion in, instead of its repo's own kube.context (may also be given before the command)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	id := fs.Arg(0)
	if (id == "") == (*env == "") {
		fmt.Fprintln(stderr, "hoist resume: give exactly one of <id> or --env <target-env>")
		fs.Usage()
		return exitUsage
	}

	set := promotionsSettings(cfg, *kubeContext)
	svc := service.New(set, serviceDeps())
	ctx, stop := boundedCommandContext(set.Deadline)
	defer stop()

	if id == "" {
		// obsErrs (service.UnconfirmedError) collects a re-observation failure per candidate
		// instead of silently filtering it out of consideration (a transient GitHub/git error
		// must never be indistinguishable from "this candidate simply isn't in flight" — that
		// could misleadingly report "no in-flight promotion" with one candidate, or silently
		// resolve to a different one with several, without ever confirming the choice was
		// actually unambiguous).
		st, err := svc.FindInFlightForEnv(ctx, *env)
		if err != nil {
			var ambiguous *service.AmbiguousError
			if errors.As(err, &ambiguous) {
				fmt.Fprintf(stderr, "hoist resume: %v\n", ambiguous)
				return exitUsage
			}
			fmt.Fprintf(stderr, "hoist resume: %v\n", err)
			return exitFailure
		}
		id = st.ID
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist resume: waiting for signing approval...")
		}
	}
	// Drive the mode this promotion actually is, not the one resume happens to know best. A
	// direct promotion never pushed its branch and has no PR, so driving it through the PR-path
	// steps would push the branch and open one — turning a deliberately PR-less deploy into a
	// PR, with two real writes. An earlier revision refused to resume these at
	// all, on the belief that runResume could not reach envs.production for the gate; it can, so
	// the honest fix is svc.Resume driving DirectSteps rather than declining (see its own doc
	// comment).
	d, err := svc.Resume(ctx, id, service.ResumeOpts{OverrideCINone: *overrideCINone, Hooks: service.Hooks{OnWaiting: onWaiting}})
	if err != nil {
		fmt.Fprintf(stderr, "hoist resume: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	err = d.Run(ctx, runHooksForCLI(stderr))
	s := d.State()
	return reportDriveResult(stdout, stderr, "hoist resume", s.SourceEnv, s.TargetEnv, &s, err)
}
