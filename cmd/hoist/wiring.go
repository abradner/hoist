package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// buildStartPromotion adapts internal/service.StartPromotion into an app.StartPromotionFunc the
// TUI can call without importing pkg/git, pkg/forge, pkg/argo, pkg/rollout or internal/config
// itself (AGENTS.md §4.8) — the same shape buildResolveFunc already gives the plan screen. svc is
// the one, session-lifetime Service runTUI builds; StartPromotion itself now does everything this
// adaptor used to assemble by hand (the freshness check, the forge/Argo/rollout adaptors, the
// claim-then-rescan, the state save and its release, and — new in this PR, the fix
// AGENTS.md's own design doc for this train calls out as its trust-boundary reason for existing
// — the production direct-commit gate, called BEFORE any of that rather than only inside the
// first Drive step that happened to run after the claim and the initial save).
//
// Repo and View are left nil on the request below, which is what fixes the boot-frozen-repo
// defect this adaptor used to have (FB-M1): StartPromotion reads svc.Repo() ITSELF, at call
// time, rather than trusting whatever *gitops.Repo/viewDir this closure was built with back at
// TUI boot — so an Argo Application renamed since boot, or a repo view F5 has refreshed since,
// is what the Argo Application lookup and the freshness check actually see.
func buildStartPromotion(svc *service.Service) app.StartPromotionFunc {
	return func(ctx context.Context, p gitops.Plan, opts app.StartOpts, progress func(string)) (engine.PromotionState, flight.Driver, error) {
		var onWaiting func()
		if progress != nil {
			// onWaiting carries the "waiting for signing approval" wait into the drive, the
			// same callback preflight reports progress through — so preflight and drive read
			// as one log (defect B).
			onWaiting = func() { progress("waiting for signing approval") }
		}
		d, err := svc.StartPromotion(ctx, service.StartRequest{
			Plan: p,
			Mode: service.Mode{Direct: opts.Direct, Confirmed: opts.Confirmed},
		}, service.Hooks{Progress: progress, OnWaiting: onWaiting})
		if err != nil {
			return engine.PromotionState{}, nil, err
		}
		return d.State(), d, nil
	}
}

// buildInFlightFuncs is the TUI's `hoist promotions` and `hoist resume <id>` (M10), now a thin
// adapter over svc.List/svc.Resume (internal/service/promotions.go) — the same re-observation
// both the CLI's runPromotions/runResume and the matrix's in-flight pane now share, rather than
// two separately-maintained loops that could (and once did, per the aggregate review of stack
// #137) disagree about the very same state file. svc's own Settings.KubeOverride is what both
// List's re-observation and Resume's drive open their Argo/rollout adaptors against — set once
// at construction (runTUI), the operator's explicit --kube-context (#105) when given, else each
// promotion's own repo's kube.context, so one TUI session runs against one cluster throughout.
func buildInFlightFuncs(svc *service.Service) app.InFlight {
	if svc == nil || svc.Settings().Config == nil {
		return app.InFlight{}
	}
	return app.InFlight{
		List: func(ctx context.Context) ([]flight.Summary, error) {
			listed, err := svc.List(ctx, service.ListOpts{})
			if err != nil {
				return nil, err
			}
			out := make([]flight.Summary, 0, len(listed))
			for _, l := range listed {
				out = append(out, summaryFor(l))
			}
			return out, nil
		},
		Resume: func(ctx context.Context, id string) (engine.PromotionState, flight.Driver, error) {
			// No live progress channel wired for a resumed promotion before this PR
			// (FB-M2) — svc.Resume's own DriverHooks{Progress: ...} now gives the flight
			// screen the same per-step progress a freshly started promotion already had.
			d, err := svc.Resume(ctx, id, service.ResumeOpts{})
			if err != nil {
				return engine.PromotionState{}, nil, err
			}
			return d.State(), d, nil
		},
	}
}

// summaryFor turns one service.Listed into the flight.Summary the in-flight pane renders —
// Unconfigured and Err both become a Summary whose own Err names why re-observation could not
// happen at all (never dropped), exactly as observeForList's pre-move wording did.
func summaryFor(l service.Listed) flight.Summary {
	switch {
	case l.Unconfigured:
		return flight.Summarize(l.State, false, nil, fmt.Errorf("repo %s is not in the config file", l.State.RepoFullName))
	case l.Err != nil:
		return flight.Summarize(l.State, false, nil, l.Err)
	default:
		return flight.Summarize(l.State, l.Done, l.Statuses, nil)
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

// buildWatchFunc is the watch screen's adapter (AGENTS.md §4.8: cmd/hoist owns the adapter
// from adaptors to a screen's plain function type): for one family in one env it resolves the
// family's Application (gitops.Family.App — the same wrapper `hoist watch --app` looks up by
// name) and the workloads it declares (familyWorkloads, shared with `hoist watch`), and returns
// a watch.Func that makes the same Get/Deployment/JobLike reads `hoist watch` makes
// (readWatchSnapshot) at the same cadence (watchInterval). It never calls Refresh: the Func
// closes over readWatchSnapshot only, and the screen package cannot name argo.Argo at all.
// A missing cluster is a nil builder, and w on the matrix says so instead of opening a screen.
func buildWatchFunc(r *gitops.Repo, a argo.Argo, ro rollout.Rollout, clusterErr error, argoNamespace string, poll config.PollConfig) watch.BuildFunc {
	if clusterErr != nil || a == nil || ro == nil {
		return nil
	}
	return func(family, env string) (watch.Funcs, error) {
		e, ok := r.Envs[env]
		if !ok {
			return watch.Funcs{}, fmt.Errorf("no env %q in the repo", env)
		}
		fam, ok := e.Families[family]
		if !ok || fam == nil {
			return watch.Funcs{}, fmt.Errorf("no family %q in %s", family, env)
		}
		if fam.App == "" {
			return watch.Funcs{}, fmt.Errorf("%s in %s has no Argo Application", family, env)
		}
		app := argo.Application{Namespace: argoNamespace, Name: fam.App}
		deployments, jobLikes := familyWorkloads(fam)
		return watch.Funcs{
			Read: func(ctx context.Context) (watch.Snapshot, error) {
				return readWatchSnapshot(ctx, a, ro, app, env, deployments, jobLikes)
			},
			Interval: watchInterval(poll),
		}, nil
	}
}

// buildRestartFuncs adapts internal/restart's core into the plain function values the restart
// screen takes, so internal/app never reaches for pkg/rollout or a kubeconfig itself
// (AGENTS.md §4.8) — the same shape buildResolveFunc and buildTagsFunc already give the plan
// and picker screens.
//
// A zero Funcs when the cluster could not be reached: the screen's own Read is then nil, and
// the root says so on the matrix rather than opening a screen that can do nothing.
func buildRestartFuncs(ro rollout.Rollout, rolloutErr error, poll config.PollConfig) apprestart.Funcs {
	if rolloutErr != nil || ro == nil {
		return apprestart.Funcs{}
	}
	interval := time.Duration(poll.Rollout)
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return apprestart.Funcs{
		Read: func(ctx context.Context, env string, names []string) (restart.Plan, error) {
			return restart.Read(ctx, ro, env, names)
		},
		Do: func(ctx context.Context, p restart.Plan, at time.Time) ([]string, error) {
			return restart.Do(ctx, ro, p, at)
		},
		Observe: func(ctx context.Context, env string, names []string, at time.Time) ([]restart.Progress, error) {
			return restart.Observe(ctx, ro, env, names, at)
		},
		Interval: interval,
	}
}

// buildRefreshRepoFunc is F5's own re-read of origin (matrix.RefreshRepoFunc), adapted from
// svc.RefreshRepo (internal/service/repo.go) — the same refreshRepoView + gitops.Discover pair
// runTUI's own boot does, through the one Service both share, so the matrix and a plan built
// moments later never disagree about which origin/<base> either was reading.
func buildRefreshRepoFunc(svc *service.Service) matrix.RefreshRepoFunc {
	return func(ctx context.Context) (*gitops.Repo, error) {
		view, err := svc.RefreshRepo(ctx)
		if err != nil {
			return nil, err
		}
		return view.Repo, nil
	}
}
