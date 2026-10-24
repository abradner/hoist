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
// work identically rather than documenting a trap. A "--" ends the flags, so a
// --confirm-abandon typed after one is a positional and stays where it is (#254).
func reorderConfirmAbandonFirst(args []string) []string {
	for i, a := range args {
		if a == "--" {
			break
		}
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

// parseWithID parses a command line that takes one positional id among its flags, in either
// order. flag.Parse stops at the first non-flag argument, so a flag typed after the id — the
// order the guide itself shows for `hoist resume <id> --override-ci-none` — would be left
// unparsed and silently ignored; this parses what follows the id as well. A second positional
// is refused rather than dropped, for the same reason: an argument hoist did not act on should
// not look accepted. id is "" when none was given.
//
// A "--" ends the flags, and everything after it is positional — the id included. When the
// first parse consumed one, what follows the id is therefore a second positional to refuse, not
// something to parse again: `resume -- <id> --override-ci-none` must not set the override (#254).
func parseWithID(fs *flag.FlagSet, args []string) (id string, err error) {
	refuse := func(err error) (string, error) {
		fs.Usage()
		fmt.Fprintf(fs.Output(), "%s: %v\n", fs.Name(), err)
		return "", err
	}
	// parse is flag.Parse behind the same check parseFlagsOnly makes: a flag given as the next
	// argument of a value-taking flag was swallowed, not set.
	parse := func(args []string) error {
		if name, value, swallowed := flagTakenAsValue(fs, args); swallowed {
			_, err := refuse(fmt.Errorf("--%s takes a value, but what follows it is %s, which looks like a flag; nothing was run. If that really is the value, write --%s=%s", name, value, name, value))
			return err
		}
		return fs.Parse(args)
	}
	if err := parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", nil
	}
	id = fs.Arg(0)
	// flag.Parse drops a "--" it consumed from fs.Args(), so it is the argument just before
	// them. A flag's value cannot be it: flagTakenAsValue refuses a value that starts with a dash.
	delimited := len(args) > fs.NArg() && args[len(args)-fs.NArg()-1] == "--"
	rest := fs.Args()[1:]
	if !delimited {
		if err := parse(rest); err != nil {
			return "", err
		}
		rest = fs.Args()
	}
	if len(rest) > 0 {
		return refuse(fmt.Errorf("unexpected argument %q after %s", rest[0], id))
	}
	return id, nil
}

// runAbandon is `hoist abandon <id>`: the CLI face of abandonPromotion, gated on
// --confirm-abandon repeating the id (a second, distinct confirming argument — AGENTS.md §8,
// the same shape --confirm-direct/--confirm-production use).
func runAbandon(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist abandon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	confirm := fs.String("confirm-abandon", "", "repeat the promotion's own id exactly to confirm — required, never inferred from <id> alone")
	quiet := fs.Bool("quiet", false, quietUsage)
	id, err := parseWithID(fs, reorderConfirmAbandonFirst(args))
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if id == "" {
		fmt.Fprintln(stderr, "hoist abandon: give the promotion's id")
		fs.Usage()
		return exitUsage
	}
	if *confirm != id {
		return failf(stderr, "hoist abandon", exitUsage, "--confirm-abandon must repeat %s exactly to confirm — nothing was touched", id)
	}

	set := promotionsSettings(cfg, "")
	svc := service.New(set, serviceDeps())
	ctx, stop := commandContext(set.Deadline)
	defer stop()

	n := newNarrator(stderr, *quiet)
	stopNarrator := n.watch()
	defer stopNarrator()
	stderr = n

	lines, err := svc.AbandonWith(ctx, id, n.hooks(nil))
	n.idle()
	for _, line := range lines {
		fmt.Fprintf(stdout, "hoist abandon: %s\n", line)
	}
	if err != nil {
		return fail(stderr, "hoist abandon", exitFailure, err)
	}
	fmt.Fprintf(stdout, "hoist abandon: %s abandoned\n", id)
	return 0
}
