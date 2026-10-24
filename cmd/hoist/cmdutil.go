package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/redact"
)

// failf is how a command reports a refusal or failure: "<who>: <message>" on stderr, then the
// exit code to return. The whole line passes through redact.Strings (AGENTS.md §4.10, R-002) —
// every command's last stop before the terminal, so a credential registered anywhere in the
// process is scrubbed whether or not the call site remembered to. A command writes
// `return failf(stderr, "hoist plan", exitFailure, "%v", err)` and cannot forget.
//
// who is the command's name as the operator types it ("hoist plan"), or "hoist" for the root.
func failf(stderr io.Writer, who string, code int, format string, args ...any) int {
	fmt.Fprintf(stderr, "%s: %s\n", who, redact.Strings(fmt.Sprintf(format, args...)))
	return code
}

// fail is failf for the common case of reporting an error as is.
func fail(stderr io.Writer, who string, code int, err error) int {
	return failf(stderr, who, code, "%s", err)
}

// commandContext is the context a command runs under: interrupted by ^C and, when deadline is
// positive, bounded by it (poll.deadline), so talking to a real forge, git remote or cluster can
// never hang a command forever on one bad candidate. The returned func releases both.
func commandContext(deadline time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if deadline <= 0 {
		return ctx, stop
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	return ctx, func() { cancel(); stop() }
}

// pairedEnvs resolves --from for plan and promote: --to is the required end, and with --to given
// and --from not, the source is the one env envs.pairs promotes into it (service.Settings.PairEnvs,
// #173). A lone --from is refused: the target is never derived. With both or neither given they are
// returned unchanged (the caller's own "required" refusal covers neither). The caller checks that
// each flag was GIVEN, not that its value is non-empty, so `--to "$UNSET"` stays the refusal it
// always was (§9 entry 16). note is the stderr line that names the pair used, so the operator sees
// which promotion they are about to plan or start; it is returned, not printed, so the caller
// prints it only once every other refusal has been passed and the note never announces a command
// that is then refused. ok is false when the config could not fill the source in; code is then the
// exit code.
func pairedEnvs(who string, set service.Settings, from, to string, stderr io.Writer) (f, t, note string, code int, ok bool) {
	pf, pt, err := set.PairEnvs(from, to)
	if err != nil {
		return "", "", "", refusal(stderr, who, err), false
	}
	if from == "" && pf != "" {
		note = fmt.Sprintf("%s: --from not given; envs.pairs promotes %s into %s\n", who, pf, pt)
	}
	return pf, pt, note, 0, true
}

// refusal renders a refusal error and returns a non-zero exit code. renderStartError returns 0 for
// the one error that is a success (service.AlreadyCurrentError), and prints that one to stdout,
// which refusal discards; no refusal produces it, but a refusal that read as "proceed" or exited
// silently would be the worst way for one to start, so a zero code is turned into a failure that
// says what it was.
func refusal(stderr io.Writer, cmdName string, err error) int {
	if code, _ := renderStartError(io.Discard, stderr, cmdName, err); code != 0 {
		return code
	}
	return failf(stderr, cmdName, exitFailure, "%v", err)
}
