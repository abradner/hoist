package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/rollout"
)

// tuiBase and tuiOverrideCINone are the defaults the TUI's own confirm path uses for
// buildPromotionForConfirm's base/overrideCINone parameters. `hoist promote` takes these as
// --base (default "main") and --override-ci-none (default false) flags; the TUI has no
// equivalent flags yet (no milestone has designed that UI), so it uses the exact same
// defaults runPromote's own flags fall back to rather than inventing a different one.
const (
	tuiBase           = "main"
	tuiOverrideCINone = false
)

// buildStartPromotion adapts buildPromotionForConfirm (promote.go) and the engine's own drive
// primitives (engine.AllSteps, engine.Drive, engine.Status) into an app.StartPromotionFunc the
// TUI can call without importing pkg/git, pkg/forge or internal/config itself (AGENTS.md
// §4.8) — the same shape buildResolveFunc already gives the plan screen. g and f are built
// once by runTUI and reused for every confirm, since they are pure adaptors with no
// per-promotion state (mirroring newGit/newForge's own package-level reuse across runPromote's
// whole run); forgeErr is newForge's own error building f, deferred to here (rather than
// failing runTUI outright) since a repo with no github configured never needs f at all — see
// the eff.cfg check below, which reports that more specific case first.
func buildStartPromotion(eff effective, r *gitops.Repo, g git.Git, f forge.Forge, forgeErr error, a argo.Argo, ro rollout.Rollout, clusterErr error) app.StartPromotionFunc {
	return func(ctx context.Context, p gitops.Plan, opts app.StartOpts) (engine.PromotionState, flight.DriveFunc, error) {
		if eff.cfg == nil || eff.cfg.GitHub == "" {
			// The same check runPromote itself makes before ever calling
			// buildPromotionForConfirm (which assumes eff.cfg.GitHub is non-empty: it's
			// used verbatim as the forge repo id and DeriveID's own hash input) — worded
			// identically so the TUI and the CLI never disagree about what's missing.
			return engine.PromotionState{}, nil, errors.New("the selected repo has no github: owner/name configured; add repos[].github to the config file")
		}
		if forgeErr != nil {
			return engine.PromotionState{}, nil, forgeErr
		}

		// The same all-NoOp fast path runPromote's own body applies (promote.go, "changed"
		// loop plus checkNoOpAgainstBase) before ever calling buildPromotionForConfirm —
		// mirrored here rather than inherited from it, since buildPromotionForConfirm itself
		// has never carried this guard (only runPromote's caller-side body does). Without it,
		// confirming a plan whose ticked edits are all no-ops (every source ref already
		// matches the target — invisible in the confirm screen's own diff, which already
		// skips NoOp edits) would still claim, build a worktree and save a non-terminal state
		// before the commit step ever rejected or blocked the empty change — a state file
		// that could then block a real future promotion to the same target env (Codex review,
		// PR #50).
		// M6 replaced checkNoOpAgainstBase (all-no-op path only) with checkCloneCurrentForBase,
		// which is strictly stronger and runs unconditionally: gitops.Discover read this plan's
		// occurrences off eff.repo's own disk, so it is worth confirming that disk is still
		// current against --base's freshly fetched origin tip before trusting ANY plan built
		// from it, not only one that happens to come out all-no-op. runPromote does exactly
		// this, in the same order (promote.go) — kept identical here so the CLI and TUI cannot
		// disagree about when a plan is trustworthy.
		if err := checkCloneCurrentForBase(ctx, g, eff.repo, tuiBase, p.Edits); err != nil {
			return engine.PromotionState{}, nil, err
		}
		if !anyRealEdit(p.Edits) {
			if p.SourceEnv == "" {
				return engine.PromotionState{}, nil, fmt.Errorf("%s is already current; nothing to deploy", p.TargetEnv)
			}
			return engine.PromotionState{}, nil, fmt.Errorf("%s -> %s is already current; nothing to promote", p.SourceEnv, p.TargetEnv)
		}

		// The Argo/Deployment adaptors every promotion needs, deferred to here exactly like
		// forgeErr: a session that only browses the matrix never opens a cluster connection
		// and should not fail to start because one could not be built. Checked AFTER the
		// no-op fast path above, so confirming an already-current tag on a machine with a
		// broken kubeconfig says "nothing to deploy" rather than blaming the cluster for a
		// promotion that was never going to touch it — the order the CLI already uses
		// (Copilot, PR #72).
		if clusterErr != nil {
			return engine.PromotionState{}, nil, clusterErr
		}

		// checkCloneCurrentForBase above only validates the files THIS plan already knows
		// about, which is enough for the PR path: a PR is reviewed against origin's own tree
		// before it merges. A direct push has no such second look, so if origin/Base has since
		// gained an occurrence of this image repo in a file the local checkout cannot see, the
		// push would silently update a subset of the family and leave the rest behind. The CLI
		// runs this for --direct on both promote and deploy; the TUI's D gesture is the same
		// write with the same blind spot, so it runs the same check (Copilot, PR #72).
		if opts.Direct {
			buildFresh := func(fresh *gitops.Repo) (gitops.Plan, error) {
				if p.IsDeploy() {
					return gitops.BuildDeployPlan(fresh, p.TargetEnv, deployRefOf(p), eff.promotable)
				}
				// The digest overrides are recovered from the confirmed plan's own edits
				// rather than re-resolved: the point of this check is whether origin's tree
				// has an occurrence THIS plan cannot see, so the two plans must differ only
				// in the tree they were built from — re-running resolution here could also
				// move the refs and turn a resolution change into a phantom missing
				// occurrence. Edit.New is the resolved ref for its repo by construction.
				digests := make(map[string]image.Ref, len(p.Edits))
				for _, e := range p.Edits {
					digests[e.New.Repo] = e.New
				}
				return gitops.BuildPlan(fresh, p.SourceEnv, p.TargetEnv, eff.promotable, digests)
			}
			if err := checkNoMissingOccurrenceAtFreshBase(ctx, g, eff.repo, tuiBase, eff.appsRoot, p, buildFresh); err != nil {
				return engine.PromotionState{}, nil, err
			}
		}

		// Recomputed per confirm rather than once in runTUI: it is derived from p.TargetEnv,
		// which is whatever plan the operator just confirmed.
		argoApps, err := engine.ArgoAppNames(r, p.TargetEnv, p.Edits)
		if err != nil {
			return engine.PromotionState{}, nil, err
		}

		s, release, err := buildPromotionForConfirm(ctx, eff, p, tuiBase, tuiOverrideCINone, g, f, argoApps)
		if err != nil {
			return engine.PromotionState{}, nil, err
		}
		statePath, err := engine.StatePath(s.ID)
		if err != nil {
			release()
			return engine.PromotionState{}, nil, err
		}

		// buildPromotionForConfirm's own doc comment: release must be called once the
		// returned state's first successful save lands, never held for the whole promotion.
		// runPromote's own body satisfies that with a defer spanning its single function call
		// (promote.go) — a scope that also happens to release the claim even if
		// engine.Drive's very first Observe fails before its own save is ever reached
		// (engine.Drive returns a *StepError immediately on an Observe error, without calling
		// save at all: see engine.Drive's own code, the Observe-error branch has no
		// saveIfSet). The TUI has no equivalent enclosing scope — driving happens across many
		// independent tea.Cmd calls over the flight screen's whole lifetime — so without this,
		// an operator backing out (Esc, abort, quit) before engine.Drive's own first per-step
		// save ever landed would leak the claim file forever (Copilot + Codex, PR #50: "release
		// claim can leak if first engine.Drive Observe fails before any save"). Saving the
		// initial state and releasing right here, before driveFn is ever returned to the
		// flight screen, closes that gap the same way runPromote's defer does, just earlier —
		// the claim's job (letting a future findInFlight scan see this promotion) is already
		// done the moment this state file exists on disk.
		// Before the first save, not after: flight.OrderFor and `hoist resume` both read the
		// mode off the state, so a state saved without it renders the PR path's ten steps for
		// a direct run, and a quit before DirectPushedStep ever ran would leave a state file
		// resume drives as a PR promotion — opening a branch and a PR for a change the
		// operator asked to push straight to base. The CLI's own callers set it here too;
		// DirectPushedStep still sets it independently, because the step that does the
		// landing is what makes it true (Copilot, PR #72).
		s.Direct = opts.Direct
		if err := engine.SaveState(statePath, s); err != nil {
			release()
			return engine.PromotionState{}, nil, fmt.Errorf("writing initial promotion state: %w", err)
		}
		release()
		save := func(st *engine.PromotionState) error {
			return engine.SaveState(statePath, st)
		}

		// onWaiting is nil here: the interactive "waiting for signing approval" text
		// runPromote prints to stderr has no analogue wired into flight.Model yet (it would
		// need a way to deliver a message mid-driveCmd, which this brief does not add) — the
		// flight screen's own spinner keeps animating for the whole Act call regardless, so
		// the operator still sees the screen is busy, just without that specific wording.
		// The full ten either way, now that direct mode converges too (issue #66). The TUI drove
		// only CoreSteps while three things were missing: DirectSteps stopped at the push,
		// flight.retryableStep classified only CIGreen/Approved so a transient Kubernetes Get
		// stopped the flight dead, and buildPollDurations carried neither poll.argo nor
		// poll.rollout so pollInterval fell back to 2s. All three are addressed, so the screen
		// now drives what it has always rendered (issue #64).
		var steps []engine.Step
		if opts.Direct {
			// eff.cfg.Envs.Production unfiltered — DirectSteps' own doc comment forbids a
			// caller narrowing it. Confirmed comes from the screen that ran the gesture.
			steps = engine.AllDirectSteps(g, a, ro, eff.cfg.Envs.Production, opts.Confirmed, nil)
		} else {
			steps = engine.AllSteps(g, f, a, ro, nil)
		}

		driveFn := func(ctx context.Context, cur engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
			next := cur
			driveErr := engine.Drive(ctx, steps, &next, save)
			// Waiting and Blocked are read from statuses (via engine.Status below), not
			// surfaced as err — see flight.DriveFunc's own doc comment. Any other error
			// from Drive (a plumbing hiccup on a retryable step, or a terminal Act/Observe
			// failure) is a genuine failure and is returned as err.
			var outErr error
			var blocked *engine.BlockedError
			if driveErr != nil && !errors.Is(driveErr, engine.ErrWaiting) && !errors.As(driveErr, &blocked) {
				outErr = driveErr
			}
			done, statuses, statusErr := engine.Status(ctx, steps, &next)
			if statusErr != nil && outErr == nil {
				outErr = statusErr
			}
			return next, done, statuses, outErr
		}

		return *s, driveFn, nil
	}
}

// buildPollDurations translates config.PollConfig's CI/Approval/Deadline into
// flight.PollDurations — the plain-value shape the flight screen actually needs (AGENTS.md
// §4.8: the screen never imports internal/config itself). All five are translated now that the
// screen drives the Argo and rollout steps too; it previously stopped at Merged, so poll.argo
// and poll.rollout had no analogue to carry (issue #64).
func buildPollDurations(poll config.PollConfig) flight.PollDurations {
	return flight.PollDurations{
		CI:       time.Duration(poll.CI),
		Approval: time.Duration(poll.Approval),
		Argo:     time.Duration(poll.Argo),
		Rollout:  time.Duration(poll.Rollout),
		Deadline: time.Duration(poll.Deadline),
	}
}

// browserOpener builds the launch mechanism flight.OpenPRMsg's handler calls when
// preferences.open_pr is "launch" or "both" (see internal/app.Promotion.OpenURL and
// config.PreferencesConfig.OpenPR's own doc comment) — a variable, not a plain function call,
// so tests substitute a fake: no test in this repo launches a real browser (the hard constraint
// against contacting a real external endpoint in a test extends to spawning arbitrary OS
// processes a CI sandbox may not even have, and may not even have a display or an
// `open`/`xdg-open` binary at all).
var browserOpener = func(timeout time.Duration) func(url string) error {
	return func(url string) error { return defaultOpenBrowser(timeout, url) }
}

// browserCommand is the pure half of defaultOpenBrowser: for a given runtime.GOOS value, it
// picks the program and arguments that would open url in the operator's default browser —
// `open` on macOS, `xdg-open` on Linux, `rundll32 url.dll,FileProtocolHandler <url>` on Windows —
// without ever touching os/exec. The Windows branch deliberately avoids `cmd /c start`: cmd.exe
// parses metacharacters like `&|<>` in its command line, so a URL containing one would be split
// and reinterpreted rather than passed through verbatim — a real command-injection risk in
// general, even though today's one caller only ever passes a forge-returned PR URL (see
// defaultOpenBrowser's own doc comment). rundll32's FileProtocolHandler entry point takes the
// URL as a single opaque argument and never invokes a shell, so no such splitting can happen
// (Copilot, PR #50 round 4). Any other GOOS falls back to xdg-open's Unix convention rather than
// erroring outright, on the theory that a BSD or other Unix system hoist happens to run on is
// more likely to have xdg-open than not. Taking goos as a parameter (rather than reading runtime.GOOS
// itself) is the seam wiring_test.go uses to exercise every branch on every OS, without a build
// tag per branch and without ever calling exec.Command in a test (no test in this repo launches
// a real browser).
func browserCommand(goos, url string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}

// defaultOpenBrowser opens url in the operator's default browser (no new dependency, AGENTS.md
// §4.7; e.g. github.com/pkg/browser is exactly this well-known exec.Command-per-platform idiom
// in a package no heavier than browserCommand's dozen lines plus this one exec.Command call
// already covers). Run, not Start-and-reap: the LAUNCHER (open/xdg-open/rundll32) is what this
// call waits on, not the browser itself, which stays running long after the launcher — designed
// to hand off and exit — has already returned. Round-4's original Start-and-reap shape (a
// startAndReap helper, since removed — its only caller was this function, and Run reaps the
// launcher itself, no background goroutine needed) could not observe the launcher's own exit
// status at all: cmd.Start() only errors if the binary itself couldn't even be found, and the
// background goroutine reaping cmd.Wait() discarded whatever it returned, so a launcher that
// started but then failed at runtime (no browser installed, a bad DISPLAY, xdg-open's own
// failure) reported nil here — flight.OpenPRMsg's handler showed no notice at all, even though
// nothing actually opened (Copilot review, PR #50). timeout (preferences.browser_launch_timeout,
// default 5s) bounds the wait so a genuinely wedged launcher cannot block the TUI's whole event
// loop indefinitely — generous for what should normally be a near-instant fork+exec-and-return
// (open/xdg-open/rundll32 are all designed as fire-and-forget dispatchers that hand off and exit
// immediately, never blocking for the browser's own lifetime), while still bounding the worst
// case (no DISPLAY set, a genuinely wedged launcher) to a few seconds of TUI unresponsiveness
// rather than forever.
//
// The only caller today is flight.OpenPRMsg's handler (app.go), whose url is always
// PRURL(state) — a PR URL the forge itself returned when this promotion opened it, not
// something an attacker gets to choose in the common case. browserCommand's own Windows
// hardening (no cmd.exe metacharacter parsing) is worth having regardless: it is free, and
// this function's contract ("open this url") should not depend on trusting every caller to
// have vetted url first.
func defaultOpenBrowser(timeout time.Duration, url string) error {
	name, args := browserCommand(runtime.GOOS, url)
	if err := runLauncher(timeout, name, args...); err != nil {
		return fmt.Errorf("opening %s in a browser: %w", url, err)
	}
	return nil
}

// runLauncher runs name/args to completion (bounded by timeout) and surfaces whatever
// exec.Cmd.Run reports — including a non-zero exit from the launcher itself, not only the
// "binary not found" failure a bare Start would have reported. Split out from defaultOpenBrowser
// so a test can exercise this exact mechanism against a command it fully owns and controls,
// without ever launching a real browser or a process it doesn't own (this repo's own hard
// constraint, AGENTS.md §4.7 / newPromoteFixture's own comment).
func runLauncher(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

// deployRefOf recovers the single image reference a deploy plan writes. Exact, not a guess:
// gitops.BuildDeployPlan sets Edit.New to the one caller-named ref on every edit it produces,
// and refuses to produce a plan with no edits at all — so the first edit's New is that ref
// whenever p.IsDeploy() holds. Recovered from the plan rather than threaded through
// app.StartOpts because the plan is what actually crosses this boundary; a second copy of the
// ref could disagree with the edits it is supposed to describe.
func deployRefOf(p gitops.Plan) image.Ref {
	if len(p.Edits) == 0 {
		return image.Ref{}
	}
	return p.Edits[0].New
}
