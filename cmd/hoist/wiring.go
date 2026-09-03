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
func buildStartPromotion(eff effective, g git.Git, f forge.Forge, forgeErr error) app.StartPromotionFunc {
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
		s, release, err := buildPromotionForConfirm(ctx, eff, p, tuiBase, tuiOverrideCINone, g, f)
		if err != nil {
			return engine.PromotionState{}, nil, err
		}
		statePath, err := engine.StatePath(s.ID)
		if err != nil {
			release()
			return engine.PromotionState{}, nil, err
		}

		// released and save mirror runPromote's own wrapper exactly (promote.go): the claim
		// only needs to outlive the gap up to the first durable state write, since
		// findInFlight's own re-observation of the real state file is what enforces
		// invariant 5 for the rest of the (possibly hours-long) promotion.
		released := false
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

		// onWaiting is nil here: the interactive "waiting for signing approval" text
		// runPromote prints to stderr has no analogue wired into flight.Model yet (it would
		// need a way to deliver a message mid-driveCmd, which this brief does not add) — the
		// flight screen's own spinner keeps animating for the whole Act call regardless, so
		// the operator still sees the screen is busy, just without that specific wording.
		steps := engine.AllSteps(g, f, nil)

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

// browserOpener is a variable so tests substitute a fake: no test in this repo launches a real
// browser (the hard constraint against contacting a real external endpoint in a test extends to
// spawning arbitrary OS processes a CI sandbox may not even have, and may not even have a
// display or an `open`/`xdg-open` binary at all).
var browserOpener = defaultOpenBrowser

// browserCommand is the pure half of defaultOpenBrowser: for a given runtime.GOOS value, it
// picks the program and arguments that would open url in the operator's default browser —
// `open` on macOS, `xdg-open` on Linux, `cmd /c start "" <url>` on Windows (the empty "" is the
// window-title argument `start` expects before the URL; without it, a URL containing quotes or
// starting with certain characters would itself be misread as the title) — without ever
// touching os/exec. Any other GOOS falls back to xdg-open's Unix convention rather than erroring
// outright, on the theory that a BSD or other Unix hoist is only run on is more likely to have
// xdg-open than not. Taking goos as a parameter (rather than reading runtime.GOOS itself) is the
// seam wiring_test.go uses to exercise every branch on every OS, without a build tag per branch
// and without ever calling exec.Command in a test (no test in this repo launches a real
// browser).
func browserCommand(goos, url string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		return "cmd", []string{"/c", "start", "", url}
	default:
		return "xdg-open", []string{url}
	}
}

// defaultOpenBrowser opens url in the operator's default browser (no new dependency, AGENTS.md
// §4.7; e.g. github.com/pkg/browser is exactly this well-known exec.Command-per-platform idiom
// in a package no heavier than browserCommand's dozen lines plus this one exec.Command call
// already covers). Start, not Run: the browser process outlives this one and this call must not
// block the TUI waiting for the user to close their browser tab.
func defaultOpenBrowser(url string) error {
	name, args := browserCommand(runtime.GOOS, url)
	if err := exec.Command(name, args...).Start(); err != nil {
		return fmt.Errorf("opening %s in a browser: %w", url, err)
	}
	return nil
}
