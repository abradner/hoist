// Command hoist is a terminal UI that promotes container images between environments
// in an Argo CD GitOps repository and follows the change through PR, merge and rollout.
//
// Subcommands land milestone by milestone (see AGENTS.md §1). Today the binary parses
// its arguments, prints usage, and exits: the scaffold exists so CI, lint and the
// public-safety gate run against a real module from the first commit.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

// version is overwritten at build time by -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("hoist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hoist [flags] [plan|resume|status|watch]\n\n")
		fmt.Fprintf(stderr, "hoist %s — nothing is wired up yet; see AGENTS.md for the milestone plan.\n\n", version)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	fs.Usage()
	return 2
}
