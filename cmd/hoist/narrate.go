package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/redact"
)

// silenceEvery is how long hoist may be busy on one thing without saying so again. Short next
// to heartbeatEvery, which paces a wait on someone else: this one covers the stretches where
// hoist itself is working — a push over a slow link, a commit parked on a signing prompt, the
// scan of earlier promotions — and silence there reads as a hang within seconds. A variable
// only so a test can shrink it.
var silenceEvery = 15 * time.Second

// silenceTick is how often the narrator checks whether silenceEvery has passed. A variable for
// the same reason.
var silenceTick = time.Second

// quietUsage is --quiet's help text, the same on every command that has it.
const quietUsage = "print no progress lines on stderr; waits, the signing notice, the approval instructions and errors are still printed"

// narrator is everything a drive command writes to stderr while it works: one line per phase
// as it starts (service.Hooks.Progress), one per step's Act as it starts (service.Hooks.OnAct)
// and one when its outcome is saved (service.Hooks.OnHistory), and a "still …" line when the thing last announced has run silenceEvery
// without any other output. It is also the io.Writer the command's other reporters write
// through — the signing notice, waitingReporter, the retry line — because the "still" line
// comes from another goroutine, and two writers on one stderr would interleave.
//
// stdout is never touched: what a script parses stays exactly what it was.
//
// --quiet drops the phase lines, the act lines — an Act starting and its "done" — and the "still"
// lines, and nothing else. That includes the "done" line carrying the PR's URL, which under
// --quiet first appears in the approval instructions or the final summary.
type narrator struct {
	mu    sync.Mutex
	w     io.Writer
	quiet bool
	now   func() time.Time
	doing string    // what was last announced as under way; "" while hoist is waiting or done
	since time.Time // when doing was announced
	last  time.Time // when anything was last written
	// said is the last start line and the last outcome line printed for each step: a step that
	// acts again and would print the same line again says nothing.
	said map[engine.StepName]actLines
}

type actLines struct{ start, done string }

func newNarrator(stderr io.Writer, quiet bool) *narrator {
	return &narrator{w: stderr, quiet: quiet, now: time.Now, said: map[engine.StepName]actLines{}}
}

// Write implements io.Writer for the command's other stderr output. It counts as output for
// the silence clock but does not change what hoist is doing. What it is handed is redacted
// here, whatever its caller already did, so that nothing reaches stderr through a narrator
// without passing pkg/redact (AGENTS.md §4.10) — an error printed with a bare %v included.
func (n *narrator) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.last = n.now()
	if _, err := io.WriteString(n.w, redact.Strings(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// progress prints one phase line and records it as what hoist is now doing. Redacted like every
// other printed line (AGENTS.md §4.10): a phase line can carry a branch or env name today, and
// nothing stops a later one carrying a remote's error text.
func (n *narrator) progress(line string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.announce(redact.Strings(line))
}

// announce prints line and starts the silence clock on it. n.mu is held.
func (n *narrator) announce(line string) {
	now := n.now()
	n.doing, n.since = line, now
	if n.quiet {
		return
	}
	n.last = now
	fmt.Fprintf(n.w, "hoist: %s\n", line)
}

// act prints a step's Act starting. Its outcome is history's to print.
//
// A step that acts again prints nothing it has already said: ArgoRefreshedStep re-requests the
// refresh on every poll until Argo reconciles, and a start-and-done pair every five seconds
// reads as a loop and buries the one line saying what hoist is waiting for. A retried push says
// "pushing" once and "done" once, with the retry line between them.
func (n *narrator) act(e service.ActEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	line := redact.Strings(string(e.Step) + ": " + actStarting(e))
	said := n.said[e.Step]
	if said.start == line {
		return
	}
	said.start = line
	n.said[e.Step] = said
	n.announce(line)
}

// history hears every entry a walk records, as its save lands (service.Hooks.OnHistory), and
// prints the ones that are an Act's outcome — with what the Act produced: the commit, the PR's
// URL the moment it exists, the merge. The rest are already somebody's line: a wait is
// waitingReporter's, a block or a failed Act is the drive's own error, and "already satisfied"
// is a re-observation, not an event. Any entry means the thing last announced is no longer what
// is running. It is called under the Driver's lock, so it only formats and writes.
func (n *narrator) history(e engine.HistoryEntry, s engine.PromotionState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.doing = ""
	if n.quiet || e.Detail != "acted" {
		return
	}
	line := redact.Strings(string(e.Step) + ": " + actDone(e.Step, s))
	said := n.said[e.Step]
	if said.done == line {
		return
	}
	said.done = line
	n.said[e.Step] = said
	n.last = n.now()
	fmt.Fprintf(n.w, "hoist: %s\n", line)
}

// idle records that hoist is waiting on something else, which waitingReporter narrates at its
// own, much slower, pace.
func (n *narrator) idle() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.doing = ""
}

// stillBusy prints the "still …" line when the thing last announced is still running and
// nothing has been written for silenceEvery.
func (n *narrator) stillBusy() {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if n.quiet || n.doing == "" || now.Sub(n.last) < silenceEvery {
		return
	}
	n.last = now
	fmt.Fprintf(n.w, "hoist: still %s (%s so far)\n", n.doing, now.Sub(n.since).Round(time.Second))
}

// watch runs stillBusy every silenceTick until the returned stop is called; stop returns only once
// the goroutine has exited, so nothing is written through the narrator after it.
func (n *narrator) watch() (stop func()) {
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(silenceTick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				n.stillBusy()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// hooks are the service.Hooks a drive command starts or resumes a promotion with. onWaiting is
// the command's own signing notice.
func (n *narrator) hooks(onWaiting func()) service.Hooks {
	return service.Hooks{Progress: n.progress, OnWaiting: onWaiting, OnAct: n.act, OnHistory: n.history}
}

// runHooks are runHooksForCLI's, writing through the narrator, with every wait — and every
// pause before a retry — marking hoist idle so the silence clock stops while nothing is running.
func (n *narrator) runHooks() service.RunHooks {
	h := runHooksForCLI(n)
	onTick := h.OnTick
	h.OnTick = func(t service.Tick) {
		n.idle()
		onTick(t)
	}
	onRetry := h.OnRetry
	h.OnRetry = func(err error) {
		n.idle()
		onRetry(err)
	}
	return h
}

// actStarting says what a step's Act is about to do, in the operator's terms.
func actStarting(e service.ActEvent) string {
	s := e.State
	switch e.Step {
	case engine.StepBranched:
		return "creating branch " + s.Branch + " in its own worktree"
	case engine.StepCommitted:
		return "committing"
	case engine.StepPushed:
		return "pushing " + s.Branch
	case engine.StepDirectPushed:
		return "pushing to " + s.Base
	case engine.StepPROpened:
		return "opening the pull request"
	case engine.StepMerged:
		switch {
		case s.PR != nil && s.PR.Merged:
			// A resume after the merge landed but before its branch was deleted: the Act that
			// remains is the cleanup, not a second merge.
			return fmt.Sprintf("deleting the branch of merged PR #%d", s.PR.Number)
		case s.PR != nil:
			return fmt.Sprintf("merging PR #%d", s.PR.Number)
		}
		return "merging"
	case engine.StepArgoRefreshed:
		return "asking Argo CD to refresh"
	default:
		return "working"
	}
}

// actDone says what a step's Act produced, from the state saved with its History entry.
func actDone(step engine.StepName, s engine.PromotionState) string {
	switch {
	case step == engine.StepCommitted && s.CommitSHA != "":
		return "done, commit " + s.CommitSHA
	case step == engine.StepPROpened && s.PR != nil && s.PR.URL != "":
		return "done, " + s.PR.URL
	case step == engine.StepMerged && s.MergeSHA != "":
		return "done, merged as " + s.MergeSHA
	default:
		return "done"
	}
}
