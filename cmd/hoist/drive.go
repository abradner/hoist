package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/redact"
)

// pollIntervals converts internal/config's poll section into engine.PollIntervals, the plain
// value engine.PollInterval actually reads — internal/engine must not import internal/config
// (it is shared with internal/app/flight, which has its own, differently-shaped config view),
// so this conversion happens once, here, at the one place allowed to know both sides.
func pollIntervals(poll config.PollConfig) engine.PollIntervals {
	return engine.PollIntervals{
		CI:       time.Duration(poll.CI),
		Approval: time.Duration(poll.Approval),
		Argo:     time.Duration(poll.Argo),
		Rollout:  time.Duration(poll.Rollout),
	}
}

// runHooksForCLI builds the service.RunHooks a CLI drive command runs a *service.Driver's Run
// through — the same waitingReporter/heartbeatEvery reporting cmd/hoist's own driveToCompletion
// gave every promotion before this PR, now wired as hooks Run itself calls rather than logic
// duplicated in a loop this package no longer owns (AGENTS.md invariant 4: the actual waiting
// still lives in Run's own loop, not here — this is only what gets printed while it waits).
func runHooksForCLI(stderr io.Writer) service.RunHooks {
	w := &waitingReporter{w: stderr, now: time.Now}
	return service.RunHooks{
		OnTick:      func(t service.Tick) { w.report(&t.State) },
		OnRetry:     func(err error) { fmt.Fprintf(stderr, "hoist: %s (retrying)\n", redact.Strings(err.Error())) },
		Heartbeat:   heartbeatEvery,
		OnHeartbeat: func(t service.Tick) { w.report(&t.State) },
	}
}

// heartbeatEvery is how long an unchanged wait goes before the CLI says it is still alive. It
// is deliberately far slower than the default poll.* intervals (a configured one may be
// longer, which is why the sleep below is taken in heartbeat-sized pieces): the line exists to
// tell a healthy hour-long approval wait apart from a hung process, not to narrate every tick. A variable
// only so a test can shrink it; nothing else assigns it.
var heartbeatEvery = 10 * time.Minute

// waitingReporter prints why the CLI is waiting, once per distinct reason, plus a heartbeat
// while the reason stays the same. Before it existed a promotion parked at Approved re-derived
// seven satisfied steps every 30 seconds for up to poll.deadline and printed nothing at all —
// indistinguishable from a hang from the outside, with the reason sitting unread in the state
// file's History (issue #68, found on the first real promotion). The reason is exactly what
// Drive just recorded there — the newest "waiting: …" entry — so the terminal and the state
// file say the same thing. It is found by searching back rather than read off the end: Drive
// saves after recording the wait, and a failed save appends its own entry after it.
type waitingReporter struct {
	w           io.Writer
	now         func() time.Time
	last        string    // the last reason printed, "" before the first
	startedAt   time.Time // when last was first printed: the heartbeat's "so far" counts from here
	lastBeat    time.Time // when last was last printed (first print or heartbeat), for cadence
	hintedToken bool      // the approval instructions are printed once per run
}

func (r *waitingReporter) report(s *engine.PromotionState) {
	var e engine.HistoryEntry
	found := false
	for i := len(s.History) - 1; i >= 0; i-- {
		if strings.HasPrefix(s.History[i].Detail, "waiting: ") {
			e, found = s.History[i], true
			break
		}
	}
	if !found {
		return
	}
	reason := string(e.Step) + ": " + e.Detail
	now := r.now()
	if reason == r.last {
		if now.Sub(r.lastBeat) >= heartbeatEvery {
			fmt.Fprintf(r.w, "hoist: still %s (%s so far)\n", redact.Strings(reason), now.Sub(r.startedAt).Round(time.Minute))
			r.lastBeat = now
		}
		return
	}
	r.last, r.startedAt, r.lastBeat = reason, now, now
	fmt.Fprintf(r.w, "hoist: %s\n", redact.Strings(reason))
	// The approval wait has a second gap the first real run hit: the only place the token
	// was printed was the PR body. Say exactly what to post, and where, the first time.
	if e.Step == engine.StepApproved && !r.hintedToken && s.PR != nil {
		r.hintedToken = true
		fmt.Fprintf(r.w, "hoist: to approve, comment `hoist approve %s` on %s\n", s.ID, s.PR.URL)
	}
}

// findInFlight looks for a promotion state other than skipID targeting repoFullName/targetEnv
// that engine.ObserveAll reports as not yet done — AGENTS.md §4.1's own re-observe rule, applied
// to "is there already a promotion running for this env" rather than trusted from the state
// file's own Phase or presence alone (invariant 5: one in-flight promotion per target env).
// found is nil when no conflicting in-flight promotion exists. An error re-observing a
// candidate is treated conservatively — reported rather than silently skipped — since a
// promotion this call can't verify is done must not be treated as safely finished.
//
// Deliberately observes only the git/forge core (through Merged for the PR path, through the
// push for a direct one — engine.ObserveSteps picks by the state's own mode, since a direct
// state can never satisfy the PR path's steps and would otherwise be in flight forever), not
// the full engine.AllSteps —
// this is a considered call, not an oversight. Invariant 5 exists to prevent exactly one thing:
// two promotions racing to create separate branches/PRs/merges for the same target env (a real
// git/forge conflict). That risk is fully retired the moment a merge lands — a second promotion
// for the same env gets its own id, its own branch and its own PR (§4.1's deterministic id is
// keyed on the image set, so a later promotion for the same env necessarily differs), so nothing
// about this promotion's own Argo refresh/sync or rollout convergence can still collide with it.
// Blocking a brand-new promotion until a prior one's rollout finishes converging would be a
// tightening with no matching risk to justify it — Argo/rollout convergence can run long (a slow
// or stuck Deployment), and there is no reason a legitimate follow-up promotion for the same env
// (e.g. a hotfix) should have to wait on it. `hoist promote`/`hoist resume` still drive every
// promotion through the full ten steps via AllSteps (below) — only this in-flight check stops
// short. See TestFindInFlightDoesNotBlockAfterMergeWithRolloutPending in inflight_test.go for the
// scenario this guards.
func findInFlight(ctx context.Context, g git.Git, f forge.Forge, repoFullName, targetEnv, skipID string) (found *engine.PromotionState, status engine.StepStatus, err error) {
	states, err := engine.ListStates()
	if err != nil {
		return nil, engine.StepStatus{}, err
	}
	for _, prev := range states {
		if prev.ID == skipID || prev.RepoFullName != repoFullName || prev.TargetEnv != targetEnv {
			continue
		}
		done, last, oerr := engine.ObserveAll(ctx, engine.ObserveSteps(prev, g, f, nil, nil, nil), prev)
		if oerr != nil {
			return prev, last, fmt.Errorf("checking whether promotion %s is still in flight: %w", prev.ID, oerr)
		}
		if !done {
			return prev, last, nil
		}
	}
	return nil, engine.StepStatus{}, nil
}
