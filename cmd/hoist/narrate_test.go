package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/redact"
)

// inOrder reports the first of want that does not appear in out after the one before it.
func inOrder(out string, want ...string) (missing string, ok bool) {
	rest := out
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			return w, false
		}
		rest = rest[i+len(w):]
	}
	return "", true
}

// TestPromoteNarratesEachPhaseAsItHappens is the regression test for a promote that printed
// nothing until its first wait: every phase before that — resolution, the freshness check, the
// claim, the branch, the commit, the push, the PR — now has a line on stderr, in the order they
// happen, and the PR's URL is printed when the PR is opened rather than first appearing in the
// approval instructions.
func TestPromoteNarratesEachPhaseAsItHappens(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 1 || f.PRs()[0].URL == "" {
		t.Fatalf("control: want one PR with a URL, got %+v", f.PRs())
	}
	url := f.PRs()[0].URL
	stderr := errOut.String()

	if missing, ok := inOrder(stderr,
		"hoist: resolving what app-staging runs to digests (",
		"hoist: checking your checkout against origin/main",
		"hoist: claiming app-production",
		"hoist: saving promotion state",
		"hoist: branched: creating branch hoist/app-production/",
		"hoist: branched: done",
		"hoist: committed: committing",
		"hoist: committed: done, commit ",
		"hoist: pushed: pushing hoist/app-production/",
		"hoist: pushed: done",
		"hoist: pr-opened: opening the pull request",
		"hoist: pr-opened: done, "+url,
		"hoist: merged: merging PR #",
		"hoist: merged: done, merged as ",
	); !ok {
		t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, stderr)
	}
	// The PR's URL comes before anything hoist waits on.
	if w := strings.Index(stderr, "waiting: "); w >= 0 && w < strings.Index(stderr, url) {
		t.Errorf("the PR URL was first printed after a wait:\n%s", stderr)
	}
	// stdout is the summary scripts read, and only that.
	if strings.Contains(out.String(), "hoist: ") || !strings.HasPrefix(out.String(), "hoist promote: app-staging -> app-production\n") {
		t.Errorf("progress leaked onto stdout, or the summary changed:\n%s", out.String())
	}
}

// --quiet drops the progress lines and nothing else: the run still succeeds and stdout is
// unchanged.
func TestPromoteQuietPrintsNoProgress(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--quiet"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	for _, line := range strings.Split(errOut.String(), "\n") {
		for _, progress := range []string{"resolving what", "checking your checkout", "claiming ", "saving promotion state", "creating branch", ": committing", ": pushing", "opening the pull request", ": merging", ": done", "hoist: still "} {
			if strings.Contains(line, progress) {
				t.Errorf("--quiet printed a progress line: %q", line)
			}
		}
	}
	// Positive control: the run did write to stderr — the wait it sat through is still there.
	if !strings.Contains(errOut.String(), "hoist: ci-green: waiting: ") {
		t.Errorf("--quiet must leave the wait lines alone:\n%s", errOut.String())
	}
	if !strings.HasPrefix(out.String(), "hoist promote: app-staging -> app-production\n") {
		t.Errorf("stdout changed under --quiet:\n%s", out.String())
	}
}

func fixedClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	t := start
	return func() time.Time { return t }, func(d time.Duration) { t = t.Add(d) }
}

// TestNarratorSaysItIsStillBusy pins the silence clock: a phase that runs silenceEvery without
// any other output gets a "still" line with how long it has run, repeated at that interval,
// and nothing once hoist is waiting on someone else or the phase has finished.
func TestNarratorSaysItIsStillBusy(t *testing.T) {
	var buf bytes.Buffer
	n := newNarrator(&buf, false)
	now, advance := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	n.now = now

	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("nothing is running yet, got %q", buf.String())
	}

	n.progress("claiming app-production and checking for a conflicting promotion")
	buf.Reset()
	advance(silenceEvery - time.Second)
	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("spoke before silenceEvery had passed: %q", buf.String())
	}
	advance(time.Second)
	n.stillBusy()
	if got, want := buf.String(), "hoist: still claiming app-production and checking for a conflicting promotion (15s so far)\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	buf.Reset()
	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("repeated itself immediately: %q", buf.String())
	}
	advance(silenceEvery)
	n.stillBusy()
	if !strings.Contains(buf.String(), "(30s so far)") {
		t.Fatalf("the second line counts from when the phase started, got %q", buf.String())
	}

	// Other output through the narrator restarts the clock without changing what is running.
	buf.Reset()
	advance(silenceEvery - time.Second)
	if _, err := n.Write([]byte("hoist promote: waiting for signing approval...\n")); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	advance(2 * time.Second)
	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("spoke %s after other output: %q", 2*time.Second, buf.String())
	}

	// A wait is someone else's turn.
	n.idle()
	advance(10 * silenceEvery)
	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("spoke while idle: %q", buf.String())
	}

	// A finished Act leaves nothing running either.
	n.act(service.ActEvent{Step: engine.StepPushed, State: engine.PromotionState{Branch: "hoist/env/abc"}})
	n.history(engine.HistoryEntry{Step: engine.StepPushed, Detail: "acted"}, engine.PromotionState{})
	buf.Reset()
	advance(10 * silenceEvery)
	n.stillBusy()
	if buf.Len() != 0 {
		t.Fatalf("spoke after the act finished: %q", buf.String())
	}
}

func TestNarratorQuietKeepsOtherOutput(t *testing.T) {
	var buf bytes.Buffer
	n := newNarrator(&buf, true)
	now, advance := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	n.now = now

	n.progress("checking your checkout against origin/main")
	n.act(service.ActEvent{Step: engine.StepCommitted})
	advance(10 * silenceEvery)
	n.stillBusy()
	n.history(engine.HistoryEntry{Step: engine.StepCommitted, Detail: "acted"}, engine.PromotionState{CommitSHA: "abc"})
	if buf.Len() != 0 {
		t.Fatalf("--quiet printed progress: %q", buf.String())
	}
	if _, err := n.Write([]byte("hoist: ci-green: waiting: CI: 1/2 checks complete\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hoist: ci-green: waiting: CI: 1/2 checks complete\n" {
		t.Fatalf("--quiet must leave waits and errors alone, got %q", got)
	}
}

// Every narrated line passes through pkg/redact (AGENTS.md §4.10), whichever hook it came in by.
func TestNarratorRedactsWhatItPrints(t *testing.T) {
	const secret = "narrator-test-credential-9f3b2c"
	redact.Register(secret)
	var buf bytes.Buffer
	n := newNarrator(&buf, false)
	n.now = time.Now

	n.progress("fetching with " + secret)
	n.act(service.ActEvent{Step: engine.StepPushed, State: engine.PromotionState{Branch: "b-" + secret}})
	n.history(engine.HistoryEntry{Step: engine.StepPROpened, Detail: "acted"}, engine.PromotionState{PR: &forge.PR{URL: "https://git.example.test/pr/1?t=" + secret}})
	n.last = n.last.Add(-time.Hour)
	n.progress("retrying with " + secret)
	n.last = n.last.Add(-time.Hour)
	n.stillBusy()

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("a registered secret reached stderr:\n%s", buf.String())
	}
	if got := strings.Count(buf.String(), "\n"); got != 5 {
		t.Fatalf("control: want 5 lines printed (so the absence above means something), got %d:\n%s", got, buf.String())
	}
}

func TestActLinesNameWhatEachStepDoes(t *testing.T) {
	st := engine.PromotionState{Branch: "hoist/env/abc", Base: "main", CommitSHA: "c0ffee", MergeSHA: "beef", PR: &forge.PR{Number: 7, URL: "https://git.example.test/pr/7"}}
	for _, tc := range []struct {
		step        engine.StepName
		start, done string
	}{
		{engine.StepBranched, "creating branch hoist/env/abc in its own worktree", "done"},
		{engine.StepCommitted, "committing", "done, commit c0ffee"},
		{engine.StepPushed, "pushing hoist/env/abc", "done"},
		{engine.StepDirectPushed, "pushing to main", "done"},
		{engine.StepPROpened, "opening the pull request", "done, https://git.example.test/pr/7"},
		{engine.StepMerged, "merging PR #7", "done, merged as beef"},
		{engine.StepArgoSynced, "working", "done"},
		{engine.StepArgoRefreshed, "asking Argo CD to refresh", "done"},
		{engine.StepName("some-later-step"), "working", "done"},
	} {
		if got := actStarting(service.ActEvent{Step: tc.step, State: st}); got != tc.start {
			t.Errorf("%s starting: got %q, want %q", tc.step, got, tc.start)
		}
		if got := actDone(tc.step, st); got != tc.done {
			t.Errorf("%s done: got %q, want %q", tc.step, got, tc.done)
		}
	}
}

// savePlannedState saves a promotion state as it stands, for a later command to find.
func savePlannedState(t *testing.T, s *engine.PromotionState) {
	t.Helper()
	path, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(path, s); err != nil {
		t.Fatal(err)
	}
}

// deploy and resume drive the same pipeline promote does and narrate it the same way.
func TestDeployAndResumeNarrateTheirActs(t *testing.T) {
	t.Run("deploy", func(t *testing.T) {
		cfgPath, _, f := newPromoteFixture(t)
		rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "deploy", "--env", "app-production", "--image", "ghcr.io/example/app:v3@" + digestThird}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if missing, ok := inOrder(errOut.String(),
			"hoist: checking your checkout against origin/main",
			"hoist: branched: creating branch hoist/app-production/",
			"hoist: committed: done, commit ",
			"hoist: pr-opened: done, "+f.PRs()[0].URL,
		); !ok {
			t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, errOut.String())
		}
		// A deploy resolves nothing: the reference is the caller's.
		if strings.Contains(errOut.String(), "resolving what") {
			t.Errorf("a deploy reported digest resolution it never does:\n%s", errOut.String())
		}
	})
	t.Run("resume", func(t *testing.T) {
		cfgPath, clone, f := newPromoteFixture(t)
		s := buildPROpenedState(t, clone)
		savePlannedState(t, s)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "resume", s.ID}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if missing, ok := inOrder(errOut.String(),
			"hoist: branched: creating branch "+s.Branch,
			"hoist: pushed: done",
			"hoist: pr-opened: done, "+f.PRs()[0].URL,
			"hoist: merged: done, merged as ",
		); !ok {
			t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, errOut.String())
		}
	})
	t.Run("resume --quiet", func(t *testing.T) {
		cfgPath, clone, _ := newPromoteFixture(t)
		s := buildPROpenedState(t, clone)
		savePlannedState(t, s)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "resume", "--quiet", s.ID}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if strings.Contains(errOut.String(), "branched:") || !strings.Contains(out.String(), "merged:") {
			t.Fatalf("--quiet must drop the act lines and still drive to the end:\nstderr:\n%s\nstdout:\n%s", errOut.String(), out.String())
		}
	})
}

// abandon and promotions say what they are re-observing before they go quiet doing it, on
// stderr; what each prints on stdout is unchanged.
func TestAbandonAndPromotionsNarrate(t *testing.T) {
	prOpened := func(t *testing.T) (cfgPath string, s *engine.PromotionState) {
		t.Helper()
		cfgPath, clone, f := newPromoteFixture(t)
		s = buildPROpenedState(t, clone)
		if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
			t.Fatalf("driving to PR-opened: %v", err)
		}
		savePlannedState(t, s)
		return cfgPath, s
	}

	t.Run("promotions", func(t *testing.T) {
		cfgPath, s := prOpened(t)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "promotions"}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if got, want := errOut.String(), "hoist: re-observing 1 promotion(s) against the forge and the cluster\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		if !strings.HasPrefix(out.String(), s.ID+"  ") || strings.Contains(out.String(), "re-observing") {
			t.Errorf("stdout is the listing and only the listing:\n%s", out.String())
		}

		out.Reset()
		errOut.Reset()
		if got := run([]string{"--config", cfgPath, "promotions", "--quiet"}, &out, &errOut); got != 0 || errOut.Len() != 0 || !strings.HasPrefix(out.String(), s.ID+"  ") {
			t.Errorf("--quiet: exit %d, stderr %q, stdout %q", got, errOut.String(), out.String())
		}
	})
	t.Run("abandon", func(t *testing.T) {
		cfgPath, s := prOpened(t)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "abandon", s.ID, "--confirm-abandon", s.ID}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		want := "hoist: re-observing " + s.ID + " to confirm it has not landed\nhoist: closing PR #1\nhoist: deleting branch " + s.Branch + " on origin\n"
		if errOut.String() != want {
			t.Errorf("stderr = %q, want %q", errOut.String(), want)
		}
		if !strings.Contains(out.String(), "hoist abandon: closed PR #1") || !strings.Contains(out.String(), s.ID+" abandoned") {
			t.Errorf("stdout changed:\n%s", out.String())
		}
	})
	t.Run("abandon --quiet after the id", func(t *testing.T) {
		cfgPath, s := prOpened(t)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "abandon", s.ID, "--confirm-abandon", s.ID, "--quiet"}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if errOut.Len() != 0 || !strings.Contains(out.String(), s.ID+" abandoned") {
			t.Errorf("--quiet typed after the id must still be honoured: stderr %q, stdout %q", errOut.String(), out.String())
		}
	})
}

func TestMergedActNamesTheCleanupWhenTheMergeAlreadyLanded(t *testing.T) {
	st := engine.PromotionState{PR: &forge.PR{Number: 7, Merged: true}}
	if got, want := actStarting(service.ActEvent{Step: engine.StepMerged, State: st}), "deleting the branch of merged PR #7"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// What other reporters write through the narrator is redacted there too, so an error printed
// with a bare %v cannot carry a registered secret to the terminal.
func TestNarratorRedactsWhatIsWrittenThroughIt(t *testing.T) {
	const secret = "narrator-write-credential-71c0de"
	redact.Register(secret)
	var buf bytes.Buffer
	n := newNarrator(&buf, true)
	msg := "hoist resume: fetching failed: token " + secret + "\n"
	if got, err := n.Write([]byte(msg)); err != nil || got != len(msg) {
		t.Fatalf("Write = %d, %v; an io.Writer reports the bytes it was given", got, err)
	}
	if strings.Contains(buf.String(), secret) || !strings.HasPrefix(buf.String(), "hoist resume: fetching failed: token ") {
		t.Fatalf("got %q", buf.String())
	}
}

// A wait, and a pause before a retry, both stop the silence clock: hoist is not working then.
func TestRunHooksMarkTheNarratorIdle(t *testing.T) {
	for name, fire := range map[string]func(service.RunHooks){
		"a wait":  func(h service.RunHooks) { h.OnTick(service.Tick{}) },
		"a retry": func(h service.RunHooks) { h.OnRetry(errors.New("transient")) },
	} {
		var buf bytes.Buffer
		n := newNarrator(&buf, false)
		now, advance := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		n.now = now
		n.progress("saving promotion state")
		fire(n.runHooks())
		buf.Reset()
		advance(10 * silenceEvery)
		n.stillBusy()
		if buf.Len() != 0 {
			t.Errorf("%s: the narrator still claimed to be busy: %q", name, buf.String())
		}
	}
}

// lockedBuffer is a stderr two goroutines may write to, which calls seen with everything
// written so far after each write.
type lockedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	seen func(string)
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if b.seen != nil {
		b.seen(b.buf.String())
	}
	return n, err
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingPushGit holds every Push until release is closed.
type blockingPushGit struct {
	git.Git
	release <-chan struct{}
}

func (g blockingPushGit) Push(ctx context.Context, worktreeDir, remote, branch string) error {
	select {
	case <-g.release:
	case <-time.After(30 * time.Second):
		return errors.New("test: the narrator never said it was still pushing")
	}
	return g.Git.Push(ctx, worktreeDir, remote, branch)
}

// TestPromoteSaysItIsStillPushingWhileThePushRuns drives the whole command with a push that
// does not return until stderr carries the "still" line for it: the line is written by the
// narrator's own goroutine, while the drive is blocked inside the Act, and the run then
// finishes normally.
func TestPromoteSaysItIsStillPushingWhileThePushRuns(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	prevEvery, prevTick, prevGit := silenceEvery, silenceTick, newGit
	silenceEvery, silenceTick = time.Millisecond, time.Millisecond
	t.Cleanup(func() { silenceEvery, silenceTick, newGit = prevEvery, prevTick, prevGit })

	release := make(chan struct{})
	var once sync.Once
	errOut := &lockedBuffer{seen: func(all string) {
		if strings.Contains(all, "hoist: still pushed: pushing hoist/app-production/") {
			once.Do(func() { close(release) })
		}
	}}
	newGit = blockingPushGit{Git: prevGit, release: release}

	var out bytes.Buffer
	if got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}, &out, errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	if missing, ok := inOrder(errOut.String(),
		"hoist: pushed: pushing hoist/app-production/",
		"hoist: still pushed: pushing hoist/app-production/",
		"hoist: pushed: done",
	); !ok {
		t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, errOut.String())
	}
}

// Flags typed after the id are parsed, on resume as on abandon: `resume <id> --quiet` is quiet.
func TestResumeParsesFlagsAfterTheID(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	savePlannedState(t, s)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "resume", s.ID, "--quiet"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	if strings.Contains(errOut.String(), "branched:") || !strings.Contains(out.String(), "merged:") {
		t.Fatalf("--quiet after the id was ignored:\nstderr:\n%s\nstdout:\n%s", errOut.String(), out.String())
	}

	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "resume", s.ID, "stray"}, &out, &errOut); got != exitUsage || !strings.Contains(errOut.String(), `unexpected argument "stray"`) {
		t.Fatalf("a second positional must be refused: exit %d, stderr %q", got, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "abandon", s.ID, "--confirm-abandon", s.ID, "--quiet=true", "stray"}, &out, &errOut); got != exitUsage {
		t.Fatalf("abandon must refuse a second positional too: exit %d, stderr %q", got, errOut.String())
	}
}

// resume --env narrates its candidate scan, and a direct promotion its push to the base branch.
func TestResumeByEnvAndDirectModeNarrate(t *testing.T) {
	t.Run("resume --env", func(t *testing.T) {
		cfgPath, clone, _ := newPromoteFixture(t)
		s := buildPROpenedState(t, clone)
		savePlannedState(t, s)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "resume", "--env", "app-production"}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if missing, ok := inOrder(errOut.String(),
			"hoist: re-observing every promotion into app-production to find the one in flight",
			"hoist: branched: creating branch "+s.Branch,
		); !ok {
			t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, errOut.String())
		}
	})
	t.Run("direct", func(t *testing.T) {
		cfgPath, _, f := newPromoteFixture(t)
		var out, errOut bytes.Buffer
		if got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "--direct", "--confirm-direct=app-production"}, &out, &errOut); got != 0 {
			t.Fatalf("exit %d; stderr: %s", got, errOut.String())
		}
		if missing, ok := inOrder(errOut.String(),
			"hoist: checking origin/main for occurrences your checkout hasn't seen",
			"hoist: committed: done, commit ",
			"hoist: direct-pushed: pushing to main",
			"hoist: direct-pushed: done",
		); !ok {
			t.Fatalf("stderr is missing %q, or has it out of order:\n%s", missing, errOut.String())
		}
		if len(f.PRs()) != 0 || strings.Contains(errOut.String(), "pr-opened") {
			t.Errorf("direct mode opens no PR and must not narrate one:\n%s", errOut.String())
		}
	})
}

// history prints an Act's outcome and nothing else: a wait, a block, a failed Act and a
// re-observation each already have a line of their own, or are not events.
func TestNarratorPrintsOnlyAnActsOutcomeFromHistory(t *testing.T) {
	var buf bytes.Buffer
	n := newNarrator(&buf, false)
	now, advance := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	n.now = now
	st := engine.PromotionState{PR: &forge.PR{Number: 7, URL: "https://git.example.test/pr/7"}}

	for _, detail := range []string{"waiting: CI: 1/2 checks complete", "blocked: head moved", "act failed: push rejected", "already satisfied: PR #7 already exists"} {
		n.progress("something under way")
		buf.Reset()
		n.history(engine.HistoryEntry{Step: engine.StepPROpened, Detail: detail}, st)
		advance(10 * silenceEvery)
		n.stillBusy()
		if buf.Len() != 0 {
			t.Errorf("%q: printed %q; it is not an Act's outcome, and it ends what was under way", detail, buf.String())
		}
	}
	n.history(engine.HistoryEntry{Step: engine.StepPROpened, Detail: "acted"}, st)
	if got, want := buf.String(), "hoist: pr-opened: done, https://git.example.test/pr/7\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
