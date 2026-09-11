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
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
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
// viewDir is what the plan was actually discovered from — the TUI's own cached view of
// origin/<base> (repoview.go, runTUI), not eff.repo's working tree. checkRepoViewCurrent below
// replaces checkCloneCurrentForBase's old job here: since r (and therefore every plan built
// from it) already comes from origin's own committed content, there is no local-disk-vs-origin
// gap left to reconcile the way checkCloneCurrentForBase exists to catch (its own comparison
// against refs/heads/<base>, the LOCAL branch, would misfire here — viewDir is deliberately
// checked out from origin, never the local branch, so the two are expected to differ whenever
// local is behind, which is exactly the case #PR7 exists to stop refusing). What's left to
// check is narrower and simpler: has origin/<base> moved again since viewDir was last
// refreshed. Every OTHER read in this function still goes through eff.repo unchanged:
// buildPromotionForConfirm's worktree is built from the operator's own clone (that is what
// makes a signed commit possible at all), and checkNoMissingOccurrenceAtFreshBase's own
// fresh-base check is a separate, later-timed freshness check unrelated to which content the
// plan itself was built from.
func buildStartPromotion(eff effective, r *gitops.Repo, viewDir string, g git.Git, f forge.Forge, forgeErr error, a argo.Argo, ro rollout.Rollout, clusterErr error) app.StartPromotionFunc {
	return func(ctx context.Context, p gitops.Plan, opts app.StartOpts, progress func(string)) (engine.PromotionState, flight.DriveFunc, error) {
		// report is progress with the nil check made once, here, rather than at every call
		// site below — mirrors onWaiting's own nil-safety convention throughout
		// internal/engine (a nil hook is exactly as valid as a real one, never a special
		// case a caller has to guard against itself).
		report := func(string) {}
		if progress != nil {
			report = progress
		}
		if eff.cfg == nil || eff.cfg.GitHub == "" {
			// The same check runPromote itself makes before ever calling
			// buildPromotionForConfirm (which assumes eff.cfg.GitHub is non-empty: it's
			// used verbatim as the forge repo id and DeriveID's own hash input) — worded
			// identically so the TUI and the CLI never disagree about what's missing.
			return engine.PromotionState{}, nil, errors.New("the selected repo has no github: owner/name configured; add repos[].github to the config file")
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
		report("checking your checkout against origin/" + eff.base)
		if err := checkRepoViewCurrent(ctx, g, eff.repo, eff.base, viewDir); err != nil {
			return engine.PromotionState{}, nil, err
		}
		if !anyRealEdit(p.Edits) {
			if p.SourceEnv == "" {
				return engine.PromotionState{}, nil, fmt.Errorf("%s is already current; nothing to deploy", p.TargetEnv)
			}
			return engine.PromotionState{}, nil, fmt.Errorf("%s -> %s is already current; nothing to promote", p.SourceEnv, p.TargetEnv)
		}
		// The forge, like the cluster adaptors below, is checked only once a plan has proven
		// to need one: runPromote and runDeploy both build newForge after their own all-no-op
		// fast path, so confirming an already-current plan on a machine whose `gh` login has
		// lapsed says "already current" from every entry point rather than a GitHub auth
		// failure from this one (issue #55).
		if forgeErr != nil {
			return engine.PromotionState{}, nil, forgeErr
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
				reasons := make(map[string]string, len(p.Edits))
				for _, e := range p.Edits {
					digests[e.New.Repo] = e.New
					reasons[e.New.Repo] = "the confirmed plan's own edit"
				}
				return gitops.BuildPlanWith(fresh, p.SourceEnv, p.TargetEnv, eff.promotable, digests, reasons)
			}
			report("checking origin/" + eff.base + " for occurrences your checkout hasn't seen")
			if err := checkNoMissingOccurrenceAtFreshBase(ctx, g, eff.repo, eff.base, eff.appsRoot, p, buildFresh); err != nil {
				return engine.PromotionState{}, nil, err
			}
		}

		// Recomputed per confirm rather than once in runTUI: it is derived from p.TargetEnv,
		// which is whatever plan the operator just confirmed.
		argoApps, err := engine.ArgoAppNames(r, p.TargetEnv, p.Edits)
		if err != nil {
			return engine.PromotionState{}, nil, err
		}

		// overrideCINone is false here and has no TUI launch flag or config knob on purpose:
		// the confirm path never treats a PR with no checks as green (AGENTS.md §4.5 — a
		// default may not weaken a gate). The TUI's override is per promotion and after the
		// fact: `c` on the flight screen, behind a huh.Confirm, sets CINoneOverride on that
		// one promotion's state (flight.Model.ApplyCINoneOverride, answering
		// flight.OverrideCINoneMsg in internal/app), which the DriveFunc below carries into
		// engine.Drive and CIGreenStep.Observe — the same field `hoist resume
		// --override-ci-none` sets (#103). A re-confirm of the same id keeps a prior run's
		// override, as buildPromotionForConfirm's own prev-state merge already does for the CLI.
		report("claiming " + p.TargetEnv + " and checking for a conflicting promotion")
		s, release, err := buildPromotionForConfirm(ctx, eff, p, eff.base, false, g, f, argoApps)
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
		report("saving promotion state")
		if err := engine.SaveState(statePath, s); err != nil {
			release()
			return engine.PromotionState{}, nil, fmt.Errorf("writing initial promotion state: %w", err)
		}
		release()
		save := func(st *engine.PromotionState) error {
			return engine.SaveState(statePath, st)
		}
		report("preflight complete, driving")

		// onWaiting used to be nil unconditionally here: the interactive "waiting for signing
		// approval" text runPromote prints to stderr had no analogue wired into flight.Model
		// (it would need a way to deliver a message mid-driveCmd) — the flight screen's own
		// spinner kept animating for the whole Act call regardless, so the operator still saw
		// the screen was busy, just without that specific wording. progress is that way now
		// (defect B): the same callback this preflight reported through carries the wait into
		// the drive too.
		var onWaiting func()
		if progress != nil {
			onWaiting = func() { progress("waiting for signing approval") }
		}
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
			steps = engine.AllDirectSteps(g, a, ro, eff.cfg.Envs.Production, opts.Confirmed, onWaiting)
		} else {
			steps = engine.AllSteps(g, f, a, ro, onWaiting)
		}

		return *s, driveFuncFor(steps, save, progress), nil
	}
}

// driveFuncFor is the flight.DriveFunc both a confirmed plan and a resumed promotion drive
// through: one engine.Drive, then engine.Status for the full step list. Waiting and Blocked
// are read from statuses, not surfaced as err — see flight.DriveFunc's own doc comment. Any
// other error from Drive (a plumbing hiccup on a retryable step, or a terminal Act/Observe
// failure) is a genuine failure and is returned as err.
//
// progress, when non-nil, turns engine.Drive's own per-step save calls into a live line the
// flight screen shows as it happens (defect C: previously the screen only learned anything
// once a WHOLE Drive call returned — and Drive can walk through several already-satisfied or
// newly-acted steps in one call before it stops, so a first drive that branches, commits,
// pushes and opens a PR before hitting CI's own Waiting could render nothing at all for the
// whole time that took). save's own persistence still happens on every call; progress is
// layered on top of it, never instead of it. buildStartPromotion passes the operator's
// preflight progress callback through unchanged so preflight and drive read as one log; the
// TUI's resumed-promotion path (buildInFlightFuncs.Resume) has no progress channel wired yet
// and passes nil here, same as before this change — its own live streaming is a natural,
// separately-scoped followup once this lands.
func driveFuncFor(steps []engine.Step, save func(*engine.PromotionState) error, progress func(string)) flight.DriveFunc {
	if progress != nil {
		wrapped := save
		save = func(st *engine.PromotionState) error {
			if n := len(st.History); n > 0 {
				h := st.History[n-1]
				progress(fmt.Sprintf("%s: %s", h.Step, h.Detail))
			}
			return wrapped(st)
		}
	}
	return func(ctx context.Context, cur engine.PromotionState) (engine.PromotionState, bool, []engine.StepStatus, error) {
		next := cur
		driveErr := engine.Drive(ctx, steps, &next, save)
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
}

// buildInFlightFuncs is the TUI's `hoist promotions` and `hoist resume <id>` (M10): List
// re-observes every state file the way runPromotions does — the step list the state itself
// implies, against a forge and Argo/rollout clients built from the repo config it names —
// and Resume builds the same state and DriveFunc runResume would. A state whose repo is not
// in the config file, or whose clients cannot be built, is listed with that as its Err rather
// than dropped: a promotion that cannot be confirmed is not one that is not there.
// kubeOverride, when non-empty, is the operator's explicit --kube-context (#105) and is what
// both List's re-observation and Resume's drive open their Argo/rollout adaptors against,
// instead of each promotion's own repo's kube.context, so one TUI session runs against one
// cluster throughout and the pane and the flight screen agree. Empty keeps runResume's rule
// — the promotion's repo's kube.context, which is not necessarily the selected repo's: the
// list is every state file, whichever repo it belongs to (review of #105).
func buildInFlightFuncs(cfg *config.Config, kubeOverride string) app.InFlight {
	if cfg == nil {
		return app.InFlight{}
	}
	return app.InFlight{
		List: func(ctx context.Context) ([]flight.Summary, error) {
			states, err := engine.ListStates()
			if err != nil {
				return nil, err
			}
			out := make([]flight.Summary, 0, len(states))
			for _, s := range states {
				out = append(out, observeForList(ctx, cfg, s, kubeOverride))
			}
			return out, nil
		},
		Resume: func(_ context.Context, id string) (engine.PromotionState, flight.DriveFunc, error) {
			states, err := engine.ListStates()
			if err != nil {
				return engine.PromotionState{}, nil, err
			}
			var s *engine.PromotionState
			for _, st := range states {
				if st.ID == id {
					s = st
					break
				}
			}
			if s == nil {
				return engine.PromotionState{}, nil, fmt.Errorf("no promotion %s found", id)
			}
			rc, ok := repoConfigFor(cfg, s.RepoFullName)
			if !ok {
				return engine.PromotionState{}, nil, fmt.Errorf("%s: repo %s is not in the config file", s.ID, s.RepoFullName)
			}
			f, err := newForge(rc.GitHub)
			if err != nil {
				return engine.PromotionState{}, nil, err
			}
			a, ro, err := buildArgoRolloutIn(rc, kubeOverride)
			if err != nil {
				return engine.PromotionState{}, nil, err
			}
			// The same carry-forward rules runResume applies (see its own comments): policy
			// fields stay as persisted, ArgoNamespace is re-read, a pre-M5 state is repaired.
			s.ArgoNamespace = rc.Kube.ArgoNamespace
			if err := ensureArgoApps(s, rc); err != nil {
				return engine.PromotionState{}, nil, err
			}
			statePath, err := engine.StatePath(s.ID)
			if err != nil {
				return engine.PromotionState{}, nil, err
			}
			save := func(st *engine.PromotionState) error { return engine.SaveState(statePath, st) }
			// Drive the mode this promotion actually is (runResume's own reasoning): a direct
			// promotion through AllSteps would push its branch and open a PR. Confirmed is
			// true because the state file exists only because the operator already confirmed.
			steps := engine.AllSteps(newGit, f, a, ro, nil)
			if s.Direct {
				steps = engine.AllDirectSteps(newGit, a, ro, rc.Envs.Production, true, nil)
			}
			// No live progress channel wired for a resumed promotion yet — driveFuncFor's
			// own doc comment names this as the scoped-out follow-up; nil here is unchanged
			// from before this PR.
			return *s, driveFuncFor(steps, save, nil), nil
		},
	}
}

// observeForList re-observes one state for the in-flight pane: engine.Status over the list
// the state implies, so the pane can draw every step, not only where it stopped.
func observeForList(ctx context.Context, cfg *config.Config, s *engine.PromotionState, kubeOverride string) flight.Summary {
	rc, ok := repoConfigFor(cfg, s.RepoFullName)
	if !ok {
		return flight.Summarize(*s, false, nil, fmt.Errorf("repo %s is not in the config file", s.RepoFullName))
	}
	f, err := newForge(rc.GitHub)
	if err != nil {
		return flight.Summarize(*s, false, nil, fmt.Errorf("could not build a forge client: %w", err))
	}
	a, ro, err := buildArgoRolloutIn(rc, kubeOverride)
	if err != nil {
		return flight.Summarize(*s, false, nil, fmt.Errorf("could not build an Argo/rollout client: %s", redact.Strings(err.Error())))
	}
	if err := ensureArgoApps(s, rc); err != nil {
		return flight.Summarize(*s, false, nil, errors.New(redact.Strings(err.Error())))
	}
	done, statuses, err := engine.Status(ctx, engine.ObserveSteps(s, newGit, f, a, ro, nil), s)
	if err != nil {
		// The same rule as the Argo/rollout client error above: every adaptor scrubs its own
		// output, and the pane still passes what it prints through redact.
		err = errors.New(redact.Strings(err.Error()))
	}
	return flight.Summarize(*s, done, statuses, err)
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
