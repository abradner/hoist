package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/matrix"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
)

// flightPoll is the flight screen's slice of the poll config (AGENTS.md §4.8: the screen never
// imports internal/config). The app root reads two of its values: Deadline bounds a promotion that
// is still being built, and Approval is how often the in-flight listing re-observes
// (session.Config.ListEvery, internal/app/app.go). The CI, Argo and rollout cadences the other
// fields once carried belong to the session controller and the engine now, and are left zero (T7
// removes the dead fields from flight.PollDurations, AR-L4).
func flightPoll(poll config.PollConfig) flight.PollDurations {
	return flight.PollDurations{Approval: time.Duration(poll.Approval), Deadline: time.Duration(poll.Deadline)}
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
// picks the program and arguments that would open url in the operator's default browser — `open`
// on macOS, `xdg-open` on everything else — without ever touching os/exec. hoist is released for
// darwin and linux only (.goreleaser.yaml), so there is no Windows branch to maintain; any other
// GOOS gets xdg-open's Unix convention rather than an error, on the theory that a BSD hoist is
// built on by hand is more likely to have it than not. Taking goos as a parameter (rather than
// reading runtime.GOOS itself) is the seam wiring_test.go uses to exercise every branch on every
// OS, without a build tag per branch and without ever calling exec.Command in a test (no test in
// this repo launches a real browser).
func browserCommand(goos, url string) (name string, args []string) {
	if goos == "darwin" {
		return "open", []string{url}
	}
	return "xdg-open", []string{url}
}

// defaultOpenBrowser opens url in the operator's default browser (no new dependency, AGENTS.md
// §4.7; e.g. github.com/pkg/browser is exactly this well-known exec.Command-per-platform idiom
// in a package no heavier than browserCommand's dozen lines plus this one exec.Command call
// already covers). Run, not Start-and-reap: the LAUNCHER (open/xdg-open/rundll32) is what this
// call waits on, not the browser itself, which stays running long after the launcher — designed
// to hand off and exit — has already returned. An earlier Start-and-reap shape (a
// startAndReap helper, since removed — its only caller was this function, and Run reaps the
// launcher itself, no background goroutine needed) could not observe the launcher's own exit
// status at all: cmd.Start() only errors if the binary itself couldn't even be found, and the
// background goroutine reaping cmd.Wait() discarded whatever it returned, so a launcher that
// started but then failed at runtime (no browser installed, a bad DISPLAY, xdg-open's own
// failure) reported nil here — flight.OpenPRMsg's handler showed no notice at all, even though
// nothing actually opened. timeout (preferences.browser_launch_timeout,
// default 5s) bounds the wait so a genuinely wedged launcher cannot block the TUI's whole event
// loop indefinitely — generous for what should normally be a near-instant fork+exec-and-return
// (open/xdg-open/rundll32 are all designed as fire-and-forget dispatchers that hand off and exit
// immediately, never blocking for the browser's own lifetime), while still bounding the worst
// case (no DISPLAY set, a genuinely wedged launcher) to a few seconds of TUI unresponsiveness
// rather than forever.
//
// The only caller today is flight.OpenPRMsg's handler (app.go), whose url is always
// PRURL(state) — a PR URL the forge itself returned when this promotion opened it, not
// something an attacker gets to choose in the common case. The URL is passed as one argument
// to a launcher that runs no shell, so no metacharacter in it can split into a second command
// whoever supplies it.
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

// buildWatchFunc is the watch screen's adapter: svc.OpenWatchFamily does the work (the family's
// Application, its workloads, the clients, the cadence — all `hoist watch` makes too), and this
// only maps its plain snapshot onto the screen's own type, which the screen package cannot import
// the service for (it pins its import list). Each w opens afresh, so a cluster unreachable the
// last time is retried and a family an F5 just added is found.
func buildWatchFunc(svc *service.Service) watch.BuildFunc {
	return func(family, env string) (watch.Funcs, error) {
		w, err := svc.OpenWatchFamily(env, family)
		if err != nil {
			return watch.Funcs{}, err
		}
		return watch.Funcs{
			Read: func(ctx context.Context) (watch.Snapshot, error) {
				snap, err := w.Read(ctx)
				if err != nil {
					return watch.Snapshot{}, err
				}
				return watchSnapshot(snap), nil
			},
			Interval: w.Interval,
		}, nil
	}
}

func watchSnapshot(s service.WatchSnapshot) watch.Snapshot {
	out := watch.Snapshot{
		App: s.App, Namespace: s.Namespace,
		SyncStatus: s.SyncStatus, HealthStatus: s.HealthStatus, Revision: s.Revision,
		OperationPhase: s.OperationPhase, ReconciledAt: s.ReconciledAt,
	}
	for _, w := range s.Workloads {
		out.Workloads = append(out.Workloads, watch.Workload(w))
	}
	return out
}

// buildRestartFuncs hands the restart screen the service's restart use case as the plain function
// values it takes, so internal/app never reaches for pkg/rollout or a kubeconfig itself (AGENTS.md
// §4.8). The service types are the screen's own (restart.Plan, restart.Progress), so these are
// method values, not adapters.
func buildRestartFuncs(svc *service.Service) apprestart.Funcs {
	return apprestart.Funcs{
		Read:     svc.RestartRead,
		Do:       svc.RestartDo,
		Observe:  svc.RestartObserve,
		Interval: svc.RestartInterval(),
	}
}

// buildHistoryFuncs hands the history use case to the screens as the plain function values they
// take (service/history.go): the tag picker and both confirm screens share one service, so one
// migrate.Cache. A zero Funcs is "no history available" — a flags-only run has no repos[].apps to
// map — and a repo with an empty repos[].apps keeps the live age, which blames the gitops repo and
// needs no mapping.
func buildHistoryFuncs(svc *service.Service) history.Funcs {
	rc := svc.Settings().Repo
	if rc == nil {
		return history.Funcs{}
	}
	if len(rc.Apps) == 0 {
		return history.Funcs{Mapped: func(string) bool { return false }, LiveAge: svc.LiveAge}
	}
	return history.Funcs{
		Mapped:   svc.HistoryMapped,
		Revision: svc.HistoryRevision,
		Delta:    svc.HistoryDelta,
		LiveAge:  svc.LiveAge,
	}
}

// buildTagsFunc adapts service.TagSource to the tag picker's BuildFunc, called once per image repo
// the picker opens. A registry that cannot be built reports the real error from the registry list
// and offers no git tags or metadata; an unmapped repo, or one whose forge cannot be built, has
// no git tags and the picker orders by the registry's own creation dates (principle 5).
func buildTagsFunc(svc *service.Service) tags.BuildFunc {
	return func(imageRepo string) (bool, tags.RegTagsFunc, tags.GitTagsFunc, tags.MetaFunc) {
		src := svc.OpenTagSource(imageRepo)
		if src.Err() != nil {
			return false, src.RegistryTags, nil, nil
		}
		var gitTags tags.GitTagsFunc
		if src.HasGitTags() {
			gitTags = src.GitTags
		}
		return src.Mapped, src.RegistryTags, gitTags, src.Meta
	}
}

// buildRefreshRepoFunc is F5's own re-read of origin (matrix.RefreshRepoFunc), adapted from
// svc.RefreshRepo (internal/service/repo.go) — the same refreshRepoView + gitops.Discover pair
// runTUI's own boot does, through the one Service both share, so the matrix and a plan built
// moments later never disagree about which origin/<base> either was reading. The returned rev
// is view.SHA — "" when the fetch fell back to the clone — so F5's own success note (#PR8) can
// name the exact tip it just read.
func buildRefreshRepoFunc(svc *service.Service) matrix.RefreshRepoFunc {
	return func(ctx context.Context) (*gitops.Repo, string, error) {
		view, err := svc.RefreshRepo(ctx)
		if err != nil {
			return nil, "", err
		}
		return view.Repo, view.SHA, nil
	}
}
