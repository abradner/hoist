package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
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

// findInFlight (AGENTS.md invariant 5: one in-flight promotion per target env) moved to
// internal/service.FindInFlight — see its own doc comment there. The regression tests that used
// to live in this package's findinflight_test.go moved with it, to
// internal/service/inflight_test.go.
