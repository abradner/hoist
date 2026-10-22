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

// pairedEnvs resolves --from and --to for plan and promote: with both or neither given they are
// returned unchanged (the caller's own "required" refusal covers neither), and with exactly one
// given — the caller checks that the flag was GIVEN, not that its value is non-empty, so
// `--to "$UNSET"` stays the refusal it always was (§9 entry 16) — the other is the pair
// envs.pairs configures (service.Settings.PairEnvs, #173). A pair filled in
// from the config is named on stderr, so the operator sees which promotion they are about to
// plan or start. ok is false when the config could not fill it in; code is then the exit code.
func pairedEnvs(who string, set service.Settings, from, to string, stderr io.Writer) (f, t string, code int, ok bool) {
	pf, pt, err := set.PairEnvs(from, to)
	if err != nil {
		c, _ := renderStartError(io.Discard, stderr, who, err)
		return "", "", c, false
	}
	switch {
	case from == "" && pf != "":
		fmt.Fprintf(stderr, "%s: --from not given; envs.pairs promotes %s into %s\n", who, pf, pt)
	case to == "" && pt != "":
		fmt.Fprintf(stderr, "%s: --to not given; envs.pairs promotes %s into %s\n", who, pf, pt)
	}
	return pf, pt, 0, true
}
