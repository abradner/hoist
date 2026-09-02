// Command hoist is a terminal UI that promotes container images between environments
// in an Argo CD GitOps repository and follows the change through PR, merge and rollout.
//
// Subcommands land milestone by milestone (see AGENTS.md §1). Today: plan --dry-run.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// version is overwritten at build time by -ldflags "-X main.version=…".
var version = "dev"

// Exit codes. 1 is a runtime failure, 2 a usage error, 3 "not implemented in this milestone".
const (
	exitFailure        = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hoist [flags] <command> [command flags]\n\n")
		fmt.Fprintf(stderr, "commands:\n  plan    build a promotion plan for one env pair; --dry-run prints it and touches nothing\n\n")
		fmt.Fprintf(stderr, "hoist %s\n\n", version)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return exitUsage
	}
	switch cmd := fs.Arg(0); cmd {
	case "plan":
		return runPlan(fs.Args()[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hoist: unknown command %q\n\n", cmd)
		fs.Usage()
		return exitUsage
	}
}

// digestFlag is the repeatable --digest repo=repo:tag@sha256:… flag: per-repo overrides
// handed to gitops.BuildPlan as its digests argument. Set parses the reference with
// image.Parse (so a malformed digest is refused before anything is read), requires the left
// side to name the reference's own repo, and refuses a repo given twice rather than letting
// the last one silently win. Whether the override is pinned and tagged is BuildPlan's call.
type digestFlag map[string]image.Ref

func (d digestFlag) String() string {
	parts := make([]string, 0, len(d))
	for repo, ref := range d {
		parts = append(parts, repo+"="+ref.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (d digestFlag) Set(s string) error {
	repo, rest, ok := strings.Cut(s, "=")
	repo = strings.TrimSpace(repo)
	if !ok || repo == "" {
		return fmt.Errorf("want repo=repo:tag@sha256:<digest>, got %q", s)
	}
	ref, err := image.Parse(strings.TrimSpace(rest))
	if err != nil {
		return err
	}
	if ref.Repo != repo {
		return fmt.Errorf("override for %s names a different repo %s", repo, ref.Repo)
	}
	if _, dup := d[repo]; dup {
		return fmt.Errorf("repo %s given more than once", repo)
	}
	d[repo] = ref
	return nil
}

func runPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "path to the GitOps repo checkout (required)")
	appsRoot := fs.String("apps-root", gitops.DefaultAppsRoot, "directory of Argo Application wrappers, relative to --repo")
	from := fs.String("from", "", "source env: the Argo destination namespace to read digests from (required)")
	to := fs.String("to", "", "target env: the Argo destination namespace to rewrite (required)")
	promotable := fs.String("promotable", "ghcr.io/", "comma-separated image repo prefixes hoist may promote; anything else is third-party and only reported. The default is a placeholder until config files are read (a later milestone) — set it to your registry path. An empty list is an error, not \"everything\"")
	digests := digestFlag{}
	fs.Var(digests, "digest", "repo=repo:tag@sha256:<64 hex> — plan this reference for repo instead of what --from runs; it must carry both a tag and a digest, and wins over the source env; a repo that --from does not run is an error (repeatable, one per repo)")
	dryRun := fs.Bool("dry-run", false, "print the diff, untouched images and warnings; write nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if *repo == "" || *from == "" || *to == "" {
		fmt.Fprintln(stderr, "hoist plan: --repo, --from and --to are required")
		fs.Usage()
		return exitUsage
	}
	prefixes := splitList(*promotable)

	r, err := gitops.Discover(*repo, *appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if err := checkOverridesExist(r, *from, digests); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	plan, err := gitops.BuildPlan(r, *from, *to, prefixes, digests)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if err := printPlan(stdout, r, &plan, prefixes); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if !*dryRun {
		fmt.Fprintln(stderr, "hoist plan: promotion without --dry-run lands in a later milestone; nothing was written. The output above is what it would change.")
		return exitNotImplemented
	}
	return 0
}

// checkOverridesExist refuses a --digest override for a repo that has no occurrence in the
// source env. BuildPlan only consults digests for repos it found there, so such an override
// would otherwise be silently ignored while -h promises it is planned — a typo in the repo
// name would plan the source env's ref instead of the caller's. An unknown source env is
// left for BuildPlan to report.
func checkOverridesExist(r *gitops.Repo, from string, digests digestFlag) error {
	env, ok := r.Envs[from]
	if !ok || len(digests) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, f := range env.Families {
		for _, o := range f.Occurrences {
			present[o.Ref.Repo] = true
		}
	}
	var missing, repos []string
	for repo := range digests {
		if !present[repo] {
			missing = append(missing, repo)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	for repo := range present {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return fmt.Errorf("override for %s matches no image in %s; images there: %s", strings.Join(missing, ", "), from, strings.Join(repos, ", "))
}

// printPlan renders the plan read-only: files are read from disk, edits applied in memory,
// verified, and diffed. Nothing is written.
func printPlan(w io.Writer, r *gitops.Repo, plan *gitops.Plan, prefixes []string) error {
	byFile := map[string][]gitops.Edit{}
	var files []string
	var noops []gitops.Edit
	changes := 0
	for _, e := range plan.Edits {
		if e.NoOp() {
			noops = append(noops, e)
			continue
		}
		changes++
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)
	fmt.Fprintf(w, "hoist plan: %s -> %s (%d edits in %d files)\n\n", plan.SourceEnv, plan.TargetEnv, changes, len(files))
	for _, f := range files {
		before, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(f)))
		if err != nil {
			return err
		}
		after, err := gitops.ApplyBytes(before, byFile[f])
		if err != nil {
			return err
		}
		if err := gitops.Verify(map[string][]byte{f: before}, map[string][]byte{f: after}, byFile[f]); err != nil {
			return err
		}
		fmt.Fprint(w, gitops.UnifiedDiff(f, before, after))
		fmt.Fprintln(w)
	}
	if len(noops) > 0 {
		fmt.Fprintf(w, "Already current (%d):\n", len(noops))
		for _, e := range noops {
			fmt.Fprintf(w, "  %s:%d %s/%s %s\n", e.File, e.Line, e.Kind, e.Container, e.Ref)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Untouched (%d):\n", len(plan.Untouched))
	for _, ref := range plan.Untouched {
		fmt.Fprintf(w, "  %s  (%s)\n", ref, untouchedReason(ref, plan.SourceEnv, prefixes))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Warnings (%d):\n", len(plan.Warnings))
	for _, wn := range plan.Warnings {
		fmt.Fprintf(w, "  [%s] %s\n", wn.Code, strings.ReplaceAll(wn.Message, "\n", "\n  "))
	}
	if len(r.Unmanaged) > 0 {
		fmt.Fprintf(w, "\nUnmanaged (%d): directories with manifests but no Application wrapper; not scanned:\n", len(r.Unmanaged))
		for _, d := range r.Unmanaged {
			fmt.Fprintf(w, "  %s\n", d)
		}
	}
	return nil
}

func untouchedReason(ref image.Ref, src string, prefixes []string) string {
	for _, p := range prefixes {
		if strings.HasPrefix(ref.Repo, p) {
			return "not running in " + src
		}
	}
	return "third-party: outside " + strings.Join(prefixes, ",")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
