package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
)

// runGC is `hoist gc [--dry-run]`: removes what finished promotions left on this machine — the
// worktree and local branch of every promotion observed landed, orphaned worktrees under the
// hoist cache directory, and long-unused registry cache entries (service.GC has the rules).
// --dry-run makes every check a real run makes and prints what it would remove.
//
// It reads the config file's repos[] for the checkouts it may act in, like `hoist promotions`
// and `hoist abandon`, not the root --repo flag: with no config file there is no configured
// checkout, and everything it finds is reported kept for that reason.
//
// It takes no confirmation flag: nothing it removes is state. A worktree and its branch are
// scratch space hoist rebuilds from a plan, a cache entry is refetched, and anything that is
// not provably hoist's own leftover is kept and named. What it never does is decide a promotion
// is finished from a state file — a promotion is only cleaned up once re-observed landed.
func runGC(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "list what would be removed, and what would be kept and why, without removing anything (it still observes: origin's base branch is fetched)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hoist gc: unexpected argument %q — gc takes no arguments, only --dry-run\n", fs.Arg(0))
		return exitUsage
	}

	set := promotionsSettings(cfg, "")
	svc := service.New(set, serviceDeps())
	ctx, stop := boundedCommandContext(set.Deadline)
	defer stop()

	rep, err := svc.GC(ctx, service.GCOpts{DryRun: *dryRun})
	for _, line := range rep.Removed {
		fmt.Fprintf(stdout, "hoist gc: %s\n", line)
	}
	for _, line := range rep.Kept {
		fmt.Fprintf(stdout, "hoist gc: %s\n", line)
	}
	for _, line := range rep.Failed {
		fmt.Fprintf(stderr, "hoist gc: %s\n", line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "hoist gc: %v\n", err)
		return exitFailure
	}
	// A failure is not a decision to keep something: the summary still says what was done, and
	// the exit code says not everything could be.
	code := 0
	if len(rep.Failed) > 0 {
		code = exitFailure
	}
	switch {
	case len(rep.Removed) == 0:
		fmt.Fprintln(stdout, "hoist gc: nothing to remove")
	case *dryRun:
		fmt.Fprintf(stdout, "hoist gc: %d to remove — nothing was touched; run `hoist gc` to remove them\n", len(rep.Removed))
	default:
		fmt.Fprintf(stdout, "hoist gc: %d removed\n", len(rep.Removed))
	}
	return code
}
