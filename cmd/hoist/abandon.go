package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
)

// reorderConfirmAbandonFirst moves --confirm-abandon (and its value) ahead of every other
// argument, so the flag parses regardless of whether the operator types it before or after the
// promotion id. Go's flag.Parse stops scanning at the first non-flag token — abandon's own
// positional <id> is exactly that — so the natural reading order "abandon <id>
// --confirm-abandon=<id>" (name the promotion, then confirm it) would otherwise leave
// --confirm-abandon completely unparsed, silently falling back to its empty default and always
// refusing. `--confirm-abandon=<id> abandon <id>` already worked; this makes the other order
// work identically rather than documenting a trap.
func reorderConfirmAbandonFirst(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "--confirm-abandon=") {
			out := make([]string, 0, len(args))
			out = append(out, a)
			out = append(out, args[:i]...)
			out = append(out, args[i+1:]...)
			return out
		}
		if a == "--confirm-abandon" && i+1 < len(args) {
			out := make([]string, 0, len(args))
			out = append(out, a, args[i+1])
			out = append(out, args[:i]...)
			out = append(out, args[i+2:]...)
			return out
		}
	}
	return args
}

// runAbandon is `hoist abandon <id>`: the CLI face of abandonPromotion, gated on
// --confirm-abandon repeating the id (a second, distinct confirming argument — AGENTS.md §8,
// the same shape --confirm-direct/--confirm-production use).
func runAbandon(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist abandon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	confirm := fs.String("confirm-abandon", "", "repeat the promotion's own id exactly to confirm — required, never inferred from <id> alone")
	if err := fs.Parse(reorderConfirmAbandonFirst(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(stderr, "hoist abandon: give the promotion's id")
		fs.Usage()
		return exitUsage
	}
	if *confirm != id {
		fmt.Fprintf(stderr, "hoist abandon: --confirm-abandon must repeat %s exactly to confirm — nothing was touched\n", id)
		return exitUsage
	}

	set := promotionsSettings(cfg, "")
	svc := service.New(set, serviceDeps())
	ctx, stop := boundedCommandContext(set.Deadline)
	defer stop()

	lines, err := svc.Abandon(ctx, id)
	for _, line := range lines {
		fmt.Fprintf(stdout, "hoist abandon: %s\n", line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "hoist abandon: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "hoist abandon: %s abandoned\n", id)
	return 0
}
