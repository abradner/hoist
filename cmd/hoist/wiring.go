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
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
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
func buildStartPromotion(eff effective, r *gitops.Repo, g git.Git, f forge.Forge, forgeErr error) app.StartPromotionFunc {
	return func(ctx context.Context, p gitops.Plan) (engine.PromotionState, flight.DriveFunc, error) {
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
		if !anyRealEdit(p.Edits) {
			if err := checkNoOpAgainstBase(ctx, g, eff.repo, tuiBase, p.Edits); err != nil {
				return engine.PromotionState{}, nil, err
			}
			return engine.PromotionState{}, nil, fmt.Errorf("%s -> %s is already current; nothing to promote", p.SourceEnv, p.TargetEnv)
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
		// engine.CoreSteps, the seven M4 already drove, not M5's full ten. The flight screen
		// renders all ten (flight.StepOrder), so M5's three show as never-reached and the
		// operator finishes the promotion with `hoist resume` — deliberately, for now.
		// Driving them from here needs more than passing AllSteps: flight.retryableStep
		// classifies only CIGreen/Approved as retryable, so a transient Kubernetes Get would
		// stop the flight dead instead of polling again, and buildPollDurations carries neither
		// poll.argo nor poll.rollout, so flight.pollInterval would fall back to its 2s default
		// and hammer the API regardless of what the operator configured. Wiring all three
		// together is its own piece of work, tracked as a follow-up issue rather than smuggled
		// into M5's rebase (Codex review, PR #51; issue #64).
		steps := engine.CoreSteps(g, f, nil)

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
// §4.8: the screen never imports internal/config itself). Argo/Rollout have no flight-screen
// analogue (M4's four-then-three engine steps stop at Merged; there is no Argo/rollout step
// yet), so they are deliberately not translated here.
func buildPollDurations(poll config.PollConfig) flight.PollDurations {
	return flight.PollDurations{
		CI:       time.Duration(poll.CI),
		Approval: time.Duration(poll.Approval),
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
