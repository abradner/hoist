// Command hoist is a terminal UI that promotes container images between environments
// in an Argo CD GitOps repository and follows the change through PR, merge and rollout.
//
// Subcommands land milestone by milestone (see AGENTS.md §1). Today: the matrix screen
// (no command), plan --dry-run with digest resolution from the source env's pods, its
// manifests and the registry, and config show/path.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
)

// version is overwritten at build time by -ldflags "-X main.version=…" (goreleaser sets it to
// the tag). A `go install …@v0.1.0` build has no ldflags but Go embeds the module version, which
// versionString falls back to; only a build from a checkout is "dev".
var version = "dev"

func versionString() string {
	bi, ok := debug.ReadBuildInfo()
	return resolveVersion(version, bi, ok)
}

// resolveVersion picks what --version prints: the ldflag when a build set it, else the module
// version Go embedded (a `go install …@vX.Y.Z` build), else "dev". A checkout build embeds
// "(devel)", which is no version at all.
func resolveVersion(ldflag string, bi *debug.BuildInfo, ok bool) string {
	if ldflag != "dev" {
		return ldflag
	}
	if ok && bi != nil && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Exit codes. 1 is a runtime failure, 2 a usage error, 3 "asked a read-only command to write".
const (
	exitFailure       = 1
	exitUsage         = 2
	exitReadOnlyWrite = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	configPath := fs.String("config", "", "config file (default $XDG_CONFIG_HOME/hoist/config.yaml, else ~/.config/hoist/config.yaml; a missing default is fine, a missing explicit path is not)")
	repo := fs.String("repo", "", "path to the GitOps repo checkout, or the name or path of a repos[] entry in the config file; with no command, opens the env/family matrix. Optional when the config file lists exactly one repo")
	appsRoot := fs.String("apps-root", gitops.DefaultAppsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	promotable := fs.String("promotable", "ghcr.io/", "comma-separated image repo prefixes that count as first-party (the selected repo's promotable when configured)")
	base := fs.String("base", defaultBase, "the GitOps repo's default branch: what a promotion or deploy branch is created from and the PR targets; the matrix's confirm path uses it too, and names it in the title when it is not main")
	kubeContext := fs.String("kube-context", "", "kubeconfig context for everything the matrix asks the cluster — drift, restarts, the promotions it drives (default: the selected repo's kube.context when configured, else the kubeconfig's current context; the title names the flag's or the config's context, never an address)")
	// The digest-resolution flags (#132): the root's value is what the matrix's plan screen
	// resolves with and what the tag picker's and history's registry clients authenticate
	// with, and every subcommand's own flag of the same name defaults to it, as --base and
	// --kube-context do (#105).
	var rf resolveFlags
	fs.StringVar(&rf.digestSources, "digest-sources", "", "comma-separated digest sources, first wins: pods, manifest, registry; none plans from the manifests alone (default: the selected repo's digest_sources when configured, else pods,manifest,registry; see hoist plan -h)")
	fs.StringVar(&rf.registryAuth, "registry-auth", "", "comma-separated registry credential sources tried in order: env, keychain, cluster, op — for the matrix's plan screen, the tag picker and the commit history alike (default: the matching registries[] entry's auth when configured, else env,keychain,cluster,op; see hoist plan -h)")
	fs.StringVar(&rf.clusterSecret, "cluster-secret", "", "namespace/name of a kubernetes.io/dockerconfigjson pull secret for the cluster credential source (default: the matching registries[] entry's cluster when configured; see hoist plan -h)")
	fs.StringVar(&rf.opRef, "op-ref", "", "op://vault/item/field for the op credential source (default: the matching registries[] entry's op when configured; see hoist plan -h)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hoist [flags] [<command> [command flags]]\n\n")
		fmt.Fprintf(stderr, "no command: open the env/family matrix for --repo\n\n")
		fmt.Fprintf(stderr, "commands:\n  plan           build a promotion plan for one env pair; --dry-run prints it and touches nothing\n  promote        drive a promotion to completion: worktree, commit, push, PR, CI, approval, merge, Argo refresh, Argo sync, rollout (resumable)\n  deploy         write one named image into one env and drive the same pipeline (--env, --image repo:tag@sha256:...); the image-bump half of promote\n  restart        roll an env's Deployments without changing the refs they declare (--env, optional --family); patches the live pod template like kubectl, writes nothing to git\n  promotions     list every promotion state file, with phase re-observed against the forge\n  resume <id>    re-drive a specific promotion (or --env <target-env>) from wherever it actually is\n  watch --app    read-only: an Argo Application's sync/health/revision and its Deployments' rollout progress\n  config show    print the effective config (defaults filled in, secrets redacted)\n  config path    print where the config file is read from\n\n")
		fmt.Fprintf(stderr, "hoist %s\n\n", versionString())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintln(stdout, versionString())
		return 0
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	sel := selection{repo: *repo, appsRoot: *appsRoot, promotable: *promotable, base: *base, kubeContext: *kubeContext, resolve: rf, given: map[string]bool{}}
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	// The same refusal `hoist plan` gives an explicit empty --digest-sources/--registry-auth,
	// here at the root so the no-command launch never resolves with an empty chain.
	if msg, bad := emptyResolveFlag(sel.given, rf); bad {
		fmt.Fprintf(stderr, "hoist: %s\n", msg)
		return exitUsage
	}
	if fs.NArg() == 0 {
		eff, err := selectRepo(cfg, sel)
		if err != nil {
			fmt.Fprintf(stderr, "hoist: %v\n", err)
			return exitFailure
		}
		if eff.repo == "" {
			fs.Usage()
			return exitUsage
		}
		return tuiRunner(eff, cfg, stdout, stderr)
	}
	switch cmd := fs.Arg(0); cmd {
	case "plan":
		return runPlan(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "promote":
		return runPromote(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "deploy":
		return runDeploy(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "restart":
		return runRestart(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "promotions":
		return runPromotions(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "resume":
		return runResume(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "watch":
		return runWatch(fs.Args()[1:], cfg, sel, stdout, stderr)
	case "config":
		return runConfig(fs.Args()[1:], cfg, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hoist: unknown command %q\n\n", cmd)
		fs.Usage()
		return exitUsage
	}
}

// digestFlag is the repeatable --digest repo=repo:tag@sha256:… flag: per-repo overrides
// handed to gitops.BuildPlan as its digests argument. Set applies image.ParseOverride — the
// one predicate the plan screen's `o` dialog applies too (#102), so the CLI and the TUI
// refuse the same inputs with the same words — and refuses a repo given twice rather than
// letting the last one silently win. BuildPlan still enforces pinned-and-tagged itself;
// ParseOverride's check is the polite early refusal (AGENTS.md §8, layered checks).
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
	ref, err := image.ParseOverride(s)
	if err != nil {
		return err
	}
	if _, dup := d[ref.Repo]; dup {
		return fmt.Errorf("repo %s given more than once", ref.Repo)
	}
	d[ref.Repo] = ref
	return nil
}

// loadConfig reads the config file: the explicit --config path, which must exist, or the
// default location, which may not (then the CLI runs on flags alone, as in M1).
func loadConfig(path string) (*config.Config, error) {
	explicit := path != ""
	if !explicit {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if explicit && !cfg.Found {
		return nil, fmt.Errorf("--config %s: no such file", path)
	}
	return cfg, nil
}

// selection carries the values of the flags the root flagset shares with a command, so
// that hoist --repo X plan … and hoist plan --repo X … mean the same thing, plus which of
// them were actually given: a command re-parses its own flagset with these as the
// defaults, a flag given at the command level still wins, and only a flag nobody gave
// falls back to the config file.
type selection struct {
	repo, appsRoot, promotable string
	// base and kubeContext are the root --base/--kube-context (#105): a subcommand's own
	// flag of the same name defaults to them, so `hoist --base dev promote` and
	// `hoist promote --base dev` mean the same thing, and the no-command TUI launch has them
	// at all.
	base, kubeContext string
	// resolve holds the root --digest-sources/--registry-auth/--cluster-secret/--op-ref
	// (#132), the same way: a subcommand's own flag of each name defaults to the root's and
	// copies its answer back here. resolve.kubeContext is unused — kubeContext above is the
	// one field for that flag.
	resolve resolveFlags
	given   map[string]bool
}

// kubeOverride is the root --kube-context when it was given, else "" — the operator's
// explicit choice as distinct from a repo's configured default (see effective.kubeOverride).
func (s selection) kubeOverride() string {
	if s.given["kube-context"] {
		return s.kubeContext
	}
	return ""
}

// defaultBase is what --base means when nobody gives it, on every face.
const defaultBase = "main"

// effective is what the config file and the flags agree the run is about. cfg is the
// selected repos[] entry, nil when the run is on flags alone. base is never empty;
// kubeContext is the flag when given, else the selected repo's kube.context, else "" (the
// kubeconfig's current context, resolved by whoever opens the cluster); kubeOverride is the
// flag alone — "" unless --kube-context was given — for the one consumer that must tell an
// operator's override from the selected repo's own default (buildInFlightFuncs, whose
// promotions may belong to another repo with another context). A subcommand copies its own
// --base/--kube-context into selection before selectRepo, so both fields hold that
// subcommand's answer, not only the root's. resolve carries the digest-resolution flags
// (#132) the same way — the flags as given, "" meaning "the config decides", exactly what
// resolutionOptions takes; resolveFlags() is the value to hand it.
type effective struct {
	repo, appsRoot    string
	promotable        []string
	base, kubeContext string
	kubeOverride      string
	resolve           resolveFlags
	cfg               *config.RepoConfig
}

// resolveFlags is what resolutionOptions and buildResolveFuncWith take for this run: the
// reconciled kube context beside the digest-resolution overrides as given.
func (e effective) resolveFlags() resolveFlags {
	rf := e.resolve
	rf.kubeContext = e.kubeContext
	return rf
}

// selectRepo applies the precedence: a flag given on the command line wins; otherwise the
// selected repo's config value; otherwise the flag's M1 default. The repo is selected by
// --repo (a repos[] name or path) or, with no --repo, as the only configured repo. A
// --repo that matches no entry is a plain checkout path and takes the flag defaults, so
// the config file never changes what an explicit command line means.
func selectRepo(cfg *config.Config, sel selection) (effective, error) {
	eff := effective{repo: sel.repo, appsRoot: sel.appsRoot, promotable: splitList(sel.promotable), base: sel.base, kubeContext: sel.kubeContext, resolve: sel.resolve}
	eff.resolve.kubeContext = ""
	if eff.base == "" {
		eff.base = defaultBase
	}
	if sel.given["kube-context"] {
		eff.kubeOverride = sel.kubeContext
	}
	if len(cfg.Repos) == 0 {
		return eff, nil
	}
	rc, err := cfg.Repo(sel.repo)
	if errors.Is(err, config.ErrUnknownRepo) && sel.given["repo"] {
		return eff, nil
	}
	if err != nil {
		return effective{}, err
	}
	if rc.Dir == "" {
		return effective{}, fmt.Errorf("%s: %s.path is required to open %s; add it or pass --repo <path>", cfg.File, rc.Key, rc.Name)
	}
	eff.repo = rc.Dir
	eff.cfg = &rc
	if !sel.given["apps-root"] {
		eff.appsRoot = rc.AppsRoot
	}
	if !sel.given["promotable"] && rc.Promotable != nil {
		eff.promotable = rc.Promotable
	}
	if !sel.given["kube-context"] {
		eff.kubeContext = rc.Kube.Context
	}
	return eff, nil
}

func runPlan(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	from := fs.String("from", "", "source env: the Argo destination namespace to read digests from (required)")
	to := fs.String("to", "", "target env: the Argo destination namespace to rewrite (required)")
	promotable := fs.String("promotable", sel.promotable, "comma-separated image repo prefixes hoist may promote; anything else is third-party and only reported. The default is a placeholder — set repos[].promotable in the config file or pass this flag with your registry path. An empty list is an error, not \"everything\"")
	digests := digestFlag{}
	fs.Var(digests, "digest", "repo=repo:tag@sha256:<64 hex> — plan this reference for repo instead of what --from runs; it must carry both a tag and a digest, and wins over the source env; a repo that --from does not run is an error (repeatable, one per repo)")
	dryRun := fs.Bool("dry-run", false, "print the diff, untouched images and warnings; write nothing")
	var rf resolveFlags
	fs.StringVar(&rf.kubeContext, "kube-context", sel.kubeContext, "kubeconfig context whose pods supply digests (default: the selected repo's kube.context when configured, else the kubeconfig's current context; the name in use is printed; may also be given before the command)")
	fs.StringVar(&rf.digestSources, "digest-sources", sel.resolve.digestSources, "comma-separated digest sources, first wins: pods (what --from is running), manifest (its own pin), registry (HEAD of its tag); none plans from the manifests alone, exactly as M1 did (default: the selected repo's digest_sources when configured, else pods,manifest,registry; may also be given before the command)")
	fs.StringVar(&rf.registryAuth, "registry-auth", sel.resolve.registryAuth, "comma-separated registry credential sources tried in order: env, keychain, cluster, op; the one that worked is reported by name (default: the matching registries[] entry's auth when configured, else env,keychain,cluster,op; may also be given before the command)")
	fs.StringVar(&rf.clusterSecret, "cluster-secret", sel.resolve.clusterSecret, "namespace/name of a kubernetes.io/dockerconfigjson pull secret for the cluster credential source (default: the matching registries[] entry's cluster when configured; unset skips the source; may also be given before the command)")
	fs.StringVar(&rf.opRef, "op-ref", sel.resolve.opRef, "op://vault/item/field for the op credential source, read with `op read` (default: the matching registries[] entry's op when configured; unset skips the source and runs nothing; may also be given before the command)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.promotable, sel.kubeContext, sel.resolve = *repo, *appsRoot, *promotable, rf.kubeContext, rf
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
	eff, err := selectRepo(cfg, sel)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *from == "" || *to == "" {
		fmt.Fprintln(stderr, "hoist plan: --repo, --from and --to are required")
		fs.Usage()
		return exitUsage
	}
	prefixes := eff.promotable
	// An empty list given explicitly is an error, as --promotable "" is: "" means "use the
	// default" only when the flag was not given at all.
	if msg, bad := emptyResolveFlag(sel.given, rf); bad {
		fmt.Fprintf(stderr, "hoist plan: %s\n", msg)
		return exitUsage
	}
	opts, err := resolutionOptions(cfg, eff.cfg, rf)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitUsage
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if err := checkOverrides(r, *from, prefixes, digests); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	planDigests := map[string]image.Ref(digests)
	var rep *resolutionReport
	if len(opts.order) > 0 {
		rep, err = runResolution(context.Background(), r, *from, prefixes, opts, digests)
		if err != nil {
			// The CLI printer's own guard (R-002): a cluster or registry error is already
			// redacted at its adaptor, but this is the last stop before stderr, so a value
			// registered anywhere in the process is scrubbed here too.
			fmt.Fprintf(stderr, "hoist plan: %s\n", redact.Strings(err.Error()))
			return exitFailure
		}
		planDigests = rep.digests(digests)
	}
	plan, err := gitops.BuildPlanWith(r, *from, *to, prefixes, planDigests, rep.reasons())
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if rep != nil {
		plan.Warnings = append(resolve.Warnings(rep.res), plan.Warnings...)
	}
	var configured []string
	if eff.cfg != nil {
		configured = eff.cfg.Promotable
	}
	if err := printPlan(stdout, r, &plan, prefixes, configured, rep); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	// The id promote would give this exact plan (issue #15): it needs the forge repo name,
	// which is config, so a flags-only run — no repos[].github to hash — prints none rather
	// than an id derived from a checkout directory name that would drift.
	if eff.cfg != nil && eff.cfg.GitHub != "" {
		id := engine.DeriveID(eff.cfg.GitHub, plan)
		if anyRealEdit(plan.Edits) {
			fmt.Fprintf(stdout, "\nPromotion id: %s (branch %s)\n", id, engine.BranchName(plan.TargetEnv, id))
		} else {
			// promote stops at "already current" before it creates anything for an all-no-op
			// plan, so naming a branch here would name one that never exists.
			fmt.Fprintf(stdout, "\nPromotion id: %s (already current; promote would create no branch)\n", id)
		}
	}
	if !*dryRun {
		fmt.Fprintln(stderr, "hoist plan: plan never writes — nothing was written. The output above is what it would change; run `hoist promote` to act on it.")
		return exitReadOnlyWrite
	}
	return 0
}

// runConfig is `hoist config show|path`. show prints the effective config — defaults
// filled in, paths as written, secret-ish values redacted — so it can be pasted into an
// issue; path prints where the file is read from, whether or not it exists.
func runConfig(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "path" && len(args) == 1:
		fmt.Fprintln(stdout, cfg.File)
		if !cfg.Found {
			fmt.Fprintln(stderr, "hoist config path: no file there; running on flags and defaults")
		}
		return 0
	case sub == "show" && len(args) == 1:
		if !cfg.Found {
			fmt.Fprintf(stderr, "hoist config show: no file at %s; showing defaults\n", cfg.File)
		}
		out, err := cfg.Redacted().Marshal()
		if err != nil {
			fmt.Fprintf(stderr, "hoist config show: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "# %s\n%s", cfg.File, out)
		return 0
	default:
		fmt.Fprintf(stderr, "usage: hoist config show | hoist config path\n")
		return exitUsage
	}
}

// checkOverrides refuses a --digest override BuildPlan would never consult: one for a repo
// outside the promotable prefixes (BuildPlan iterates promotable repos only, so an override
// for a third-party image would be accepted and change nothing), or one for a repo that has
// no occurrence in the source env (a typo in the repo name would plan the source env's ref
// instead of the caller's). Either way -h promises the override is planned, so silence is
// wrong. An unknown source env and an empty prefix list are left for BuildPlan to report.
func checkOverrides(r *gitops.Repo, from string, prefixes []string, digests digestFlag) error {
	if len(digests) == 0 {
		return nil
	}
	if len(prefixes) > 0 {
		var outside []string
		for repo := range digests {
			if !gitops.IsPromotable(repo, prefixes) {
				outside = append(outside, repo)
			}
		}
		if len(outside) > 0 {
			sort.Strings(outside)
			return fmt.Errorf("override for %s is not a promotable repo; prefixes: %s", strings.Join(outside, ", "), strings.Join(prefixes, ", "))
		}
	}
	env, ok := r.Envs[from]
	if !ok {
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
// verified, and diffed. Nothing is written. rep, when non-nil, adds the resolution section
// before the warnings; with nil the output is M1's, byte for byte.
func printPlan(w io.Writer, r *gitops.Repo, plan *gitops.Plan, prefixes, configured []string, rep *resolutionReport) error {
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
	if plan.IsDeploy() {
		fmt.Fprintf(w, "hoist deploy: -> %s (%d edits in %d files)\n\n", plan.TargetEnv, changes, len(files))
	} else {
		fmt.Fprintf(w, "hoist plan: %s -> %s (%d edits in %d files)\n\n", plan.SourceEnv, plan.TargetEnv, changes, len(files))
	}
	for _, f := range files {
		p, err := gitops.ResolvePath(r.Root, f)
		if err != nil {
			return err
		}
		before, err := os.ReadFile(p)
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
		fmt.Fprintf(w, "  %s  (%s)\n", ref, untouchedReason(ref, plan, prefixes, configured))
	}
	fmt.Fprintln(w)
	if rep != nil {
		rep.print(w)
	}
	fmt.Fprintf(w, "Warnings (%d):\n", len(plan.Warnings))
	for _, wn := range plan.Warnings {
		// Every warning is already redacted at its source package; this is the CLI
		// printer's own guard (R-002) so a value registered anywhere in the process is
		// still caught here even if some future warning path forgets to.
		fmt.Fprintf(w, "  [%s] %s\n", wn.Code, redact.Strings(strings.ReplaceAll(wn.Message, "\n", "\n  ")))
	}
	if len(r.Unmanaged) > 0 {
		fmt.Fprintf(w, "\nUnmanaged (%d): directories with manifests but no Application wrapper; not scanned:\n", len(r.Unmanaged))
		for _, d := range r.Unmanaged {
			fmt.Fprintf(w, "  %s\n", d)
		}
	}
	return nil
}

// untouchedReason explains, per reference, why an occurrence in the target env was left alone.
// The two plan variants leave things alone for different reasons, and saying the wrong one is
// worse than saying nothing: a promotion skips a repo because the source env does not run it,
// while a deploy skips every repo that simply is not the one image it was asked to write — those
// repos are running perfectly well in the env, so a promotion's wording would flatly misreport
// them (and, with a deploy's empty SourceEnv, would read "not running in ").
//
// prefixes is what this invocation may promote; configured is the repo's own promotable list
// from the config file (nil on a flags-only run). They differ when --promotable narrowed the
// run to one family, and a first-party repo left out by that narrowing is not third-party —
// saying it was undercut the very output an operator scoping a production promotion is
// reading for safety (issue #65). "third-party" is reserved for a repo matching no
// configured prefix at all.
func untouchedReason(ref image.Ref, plan *gitops.Plan, prefixes, configured []string) string {
	if !gitops.IsPromotable(ref.Repo, prefixes) {
		if len(configured) > 0 && gitops.IsPromotable(ref.Repo, configured) {
			return "first-party, outside --promotable " + strings.Join(prefixes, ",")
		}
		return "third-party: outside " + strings.Join(prefixes, ",")
	}
	if plan.IsDeploy() {
		return "not this deploy's image"
	}
	return "not running in " + plan.SourceEnv
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

// tuiRunner is what a bare `hoist --repo` dispatches to. It is a variable so the dispatch
// can be tested without starting a terminal program.
var tuiRunner = runTUI

// runTUI discovers the repo and runs the matrix (and, from it, the plan) screen until the
// user quits. cfg is the whole loaded config file (buildResolveFunc needs it to find the
// matching registries[] entry); eff.cfg is the selected repo's own entry, nil on flags
// alone — the plan screen then runs in "digest sources: none" mode with default resolution
// options and an empty envs config, matching what M1 offered before this milestone.
func runTUI(eff effective, cfg *config.Config, stdout, stderr io.Writer) int {
	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	var envs config.EnvsConfig
	if eff.cfg != nil {
		envs = eff.cfg.Envs
	}
	// The root --kube-context (#105), already reconciled with the repo's kube.context by
	// selectRepo, reaches every cluster-touching adaptor the TUI builds — the same value a
	// subcommand's own flag would carry. The root --digest-sources/--registry-auth/
	// --cluster-secret/--op-ref (#132) ride along the same way: the plan screen resolves
	// with them, and the credential-chain overrides reach the tag picker's and the
	// history's registry clients too. A malformed one is refused here, before the screen
	// opens, exactly as `hoist plan` refuses it. The drift column (buildDriftFunc) asks the
	// pods alone and takes none of them.
	regOpts, err := resolutionOptions(cfg, eff.cfg, eff.resolveFlags())
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitUsage
	}
	resolveFn := buildResolveFuncWith(cfg, eff.cfg, eff.promotable, eff.resolveFlags())

	// git.Exec{} and the forge adaptor are pure, stateless clients — built once here and
	// reused for every promotion the operator confirms in this TUI session, mirroring
	// newGit/newForge's own package-level reuse across a single runPromote call. newForge is
	// called even when eff.cfg is nil or has no GitHub configured (github.New("") fails fast
	// on the owner/name parse alone, before ever touching gh's own auth or the network) so
	// buildStartPromotion always has a forge value to close over; its own eff.cfg check runs
	// first and reports the missing-config case before this error would ever matter.
	githubRepo := ""
	if eff.cfg != nil {
		githubRepo = eff.cfg.GitHub
	}
	f, forgeErr := newForge(githubRepo)
	// The Argo/Deployment adaptors, built once alongside f and deferred the same way. Every
	// promotion the flight screen drives now reaches the Argo/rollout steps — both modes
	// (issues #64, #66) — so these are needed for any confirm, but a session that only browses
	// the matrix should not fail to open because the cluster is unreachable.
	a, _, argoErr := newArgo(eff.kubeContext)
	ro, _, rolloutErr := newRollout(eff.kubeContext)
	promo := app.Promotion{
		Start:      buildStartPromotion(eff, r, newGit, f, forgeErr, a, ro, errors.Join(argoErr, rolloutErr)),
		Poll:       buildPollDurations(cfg.Poll),
		OpenURL:    browserOpener(time.Duration(cfg.Preferences.BrowserLaunchTimeout)),
		OpenPRMode: cfg.Preferences.OpenPR,
	}
	tagsFn := buildTagsFunc(cfg, eff.cfg, eff.kubeContext, regOpts)
	restartFn := buildRestartFuncs(ro, rolloutErr, cfg.Poll)
	// The checkout's HEAD is what the plan's line numbers were read from, so it is what the
	// live-age blame asks the forge about; the default branch is the fallback for a HEAD that
	// was never pushed. Resolving it is one local git call and never a reason not to open.
	blameRef := ""
	if sha, ok, err := newGit.RevParse(context.Background(), r.Root, "HEAD"); err == nil && ok {
		blameRef = sha
	}
	historyFn := buildHistoryFuncs(cfg, eff.cfg, r, f, forgeErr, blameRef, eff.base, eff.kubeContext, regOpts)
	configText, err := configViewText(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	root := app.New(r, eff.promotable, envs, resolveFn, promo, tagsFn, restartFn).
		WithConfigView(cfg.File, cfg.Found, configText).
		WithHistory(historyFn).
		WithInFlight(buildInFlightFuncs(cfg, eff.kubeOverride)).
		WithDrift(buildDriftFunc(eff.kubeContext)).
		WithWatch(buildWatchFunc(r, a, ro, errors.Join(argoErr, rolloutErr), argoNamespaceOf(eff.cfg), cfg.Poll)).
		WithRun(eff.base, eff.kubeContext)
	if _, err := tea.NewProgram(root, tea.WithOutput(stdout)).Run(); err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	return 0
}

// configViewText is what the TUI's config screen (C, #104) shows: exactly the bytes `hoist
// config show` prints, redacted before they leave this package — the screen takes the
// string and never sees the loader (AGENTS.md §4.8).
func configViewText(cfg *config.Config) (string, error) {
	out, err := cfg.Redacted().Marshal()
	if err != nil {
		return "", fmt.Errorf("config view: %w", err)
	}
	return string(out), nil
}

// buildResolveFunc adapts the plan command's own resolution adaptors (resolution.go:
// resolutionOptions, runResolution — kube context, registry credential chain) into a
// plan.ResolveFunc the TUI can call without importing cmd itself (AGENTS.md §4.8). Each
// call builds a fresh cluster/registry connection for the requested source env, exactly as
// `hoist plan` does; resolutionOptions runs once here since it does not depend on the
// source env. An unreachable cluster or misconfigured registry is not caught ahead of time —
// there is no source env to try it against yet — so the plan screen's own tea.Cmd catches
// the error per call and degrades to "digest sources: none" with a warning line (AGENTS.md
// principle 5), rather than this function failing to open the TUI at all.
func buildResolveFunc(cfg *config.Config, rc *config.RepoConfig, prefixes []string) plan.ResolveFunc {
	return buildResolveFuncWith(cfg, rc, prefixes, resolveFlags{})
}

// buildDriftFunc is the matrix's cluster question: the raw pod observations for one env
// (the env is the namespace, AGENTS.md §1), straight from pkg/k8s, keyed by canonical
// image repo with every distinct running reference kept — never the planning resolver,
// which picks one digest per repo and would collapse a partial rollout, and which grafts
// the manifest's tag onto the digest it picked (#122). Pods only, whatever digest_sources
// the config orders for planning: a manifest or registry answer is not "what the cluster
// runs". Each running reference carries the pod's digest and, when the pod reported the
// image it was started from, that image's tag — so a bare manifest tag is compared with
// what the pod pulled, not with a tag the resolver assumed. The cluster is opened per
// call, so an unreachable one is one env's "cluster not asked" sentence, never a reason
// the TUI fails to open. This is the adapter §4.8 reserves for cmd/hoist: the matrix
// takes the function, never pkg/k8s.
func buildDriftFunc(kubeContext string) matrix.DriftFunc {
	return func(ctx context.Context, env string) (map[string][]image.Ref, error) {
		cluster, _, err := newCluster(kubeContext)
		if err != nil {
			return nil, err
		}
		imgs, err := cluster.RunningImages(ctx, env)
		if err != nil {
			return nil, err
		}
		return runningRefs(imgs), nil
	}
}

// runningRefs groups pod observations by canonical image repo, one entry per distinct
// (digest, tag) the namespace runs. The tag comes from the pod's reported image when its
// repo is the same one as the imageID's; a pod that reported none, or a different repo (a
// mirror pulled under another name), contributes its digest alone.
func runningRefs(imgs []k8s.RunningImage) map[string][]image.Ref {
	out := map[string][]image.Ref{}
	seen := map[string]bool{}
	for _, ri := range imgs {
		ref := ri.Ref
		ref.Tag = ""
		if ri.Image.Tag != "" && image.Canonical(ri.Image.Repo) == image.Canonical(ri.Ref.Repo) {
			ref.Tag = ri.Image.Tag
		}
		key := image.Canonical(ref.Repo)
		if id := key + "\n" + ref.String(); !seen[id] {
			seen[id] = true
			out[key] = append(out[key], ref)
		}
	}
	return out
}

func buildResolveFuncWith(cfg *config.Config, rc *config.RepoConfig, prefixes []string, rf resolveFlags) plan.ResolveFunc {
	opts, optsErr := resolutionOptions(cfg, rc, rf)
	return func(ctx context.Context, r *gitops.Repo, source string, overrides map[string]image.Ref) (plan.ResolveOutcome, error) {
		if optsErr != nil {
			return plan.ResolveOutcome{}, optsErr
		}
		if len(opts.order) == 0 {
			return plan.ResolveOutcome{}, nil // digest sources: none
		}
		// overrides reach resolve.Resolve exactly as runPlan's --digest map does, so an
		// override from the plan screen's o dialog is reported as [override] with the same
		// alternatives and disagreement warnings the CLI's Resolution section shows (#102).
		rep, err := runResolution(ctx, r, source, prefixes, opts, overrides)
		if err != nil {
			return plan.ResolveOutcome{}, err
		}
		var used string
		var consulted bool
		if ar, ok := rep.registry.(registry.AuthReporter); ok && rep.registry != nil {
			used, consulted = ar.AuthSourceUsed(), ar.Consulted()
		}
		authTried := make([]string, 0, len(rep.auth))
		for _, a := range rep.auth {
			authTried = append(authTried, string(a))
		}
		return plan.ResolveOutcome{
			Resolutions:       rep.res,
			KubeContext:       rep.kubeContext,
			RegistryAuth:      used,
			RegistryConsulted: consulted,
			RegistryAuthTried: authTried,
		}, nil
	}
}

// buildTagsFunc adapts the same registries[]-entry credential chain buildResolveFunc uses,
// plus (when RepoConfig.Apps maps the requested image repo) a pkg/forge/github.Client for
// that app repo, into a tags.BuildFunc the TUI's tag picker calls per image repo — never
// importing registry/forge/config policy itself (AGENTS.md §4.8, mirroring buildResolveFunc
// exactly). Unlike buildResolveFunc, this has no gitops.Repo occurrence to scope credentials
// by (M6's tag picker is "a direct caller that skips resolve", per resolution.go's own
// multiRegistry doc comment) — registryEntryFor/entryAuthConfig still pick the one
// registries[] entry (if any) covering imageRepo, so a repo a different entry covers, or none
// does, never borrows another entry's credentials (F4's rule, unchanged here). reg carries
// the root --registry-auth/--cluster-secret/--op-ref (#132): given, they win for every image
// repo exactly as they do in runResolution; only its auth, clusterSecret and opRef are read.
func buildTagsFunc(cfg *config.Config, rc *config.RepoConfig, kubeContext string, reg resolveOptions) tags.BuildFunc {
	var registries []config.RegistryConfig
	if cfg != nil {
		registries = cfg.Registries
	}
	return func(imageRepo string) (bool, tags.ListFunc, tags.MetaFunc) {
		entry := registryEntryFor(registries, imageRepo)
		auth, clusterSecret, opRef := entryAuthConfig(entry, reg)
		regCfg := registry.AuthConfig{Order: auth, OpRef: opRef}
		if clusterSecret != "" && has(auth, registry.AuthCluster) {
			kctx := kubeContext
			if kctx == "" && rc != nil {
				kctx = rc.Kube.Context
			}
			if cluster, _, err := newCluster(kctx); err == nil {
				regCfg.ClusterSecret, regCfg.Cluster = clusterSecret, cluster
			}
			// An unreachable cluster here just means the cluster credential source will
			// itself fail and the chain falls through to the next one (pkg/registry's own
			// documented behavior) — never a reason to fail opening the picker.
		}
		reg, err := newRegistry(regCfg)
		if err != nil {
			failed := func(context.Context) ([]string, []forge.GitTag, bool, error) { return nil, nil, false, err }
			return false, failed, nil
		}

		appRepo, mapped := "", false
		if rc != nil {
			appRepo, mapped = rc.Apps[imageRepo]
		}
		var fc forge.Forge
		if mapped {
			fc, err = newForge(appRepo)
			if err != nil {
				// Degrade to Created-based ordering (AGENTS.md principle 5) rather than
				// failing the whole picker over a forge the operator may not have configured
				// `gh` access to yet.
				mapped = false
			}
		}

		// listFn's own mapped return (finding 3, round 2) is this call's actually-observed
		// answer, not the outer mapped closed over above: the outer value can already be false
		// here (no config mapping, or newForge failed above), but it can also start true and
		// still need to degrade below, once fc.Tags is actually asked and fails at runtime —
		// the caller (internal/app/tags.Model.onListLoaded) trusts THIS return, every call,
		// over whatever BuildFunc's own static mapped result said when the picker was opened.
		listFn := func(ctx context.Context) ([]string, []forge.GitTag, bool, error) {
			regTags, err := reg.Tags(ctx, imageRepo)
			if err != nil {
				return nil, nil, false, err
			}
			if !mapped {
				return regTags, nil, false, nil
			}
			gitTags, err := fc.Tags(ctx)
			if err != nil {
				// Same degrade-not-fail call as above, now that the forge has actually been
				// asked: the registry tags are still useful without git-tag ordering — and
				// mapped=false here is what tells the caller to actually fall back to
				// Created-based ordering (invariant 3), not merely to receive an empty git-tag
				// list while still believing every row is "mapped but unmatched".
				return regTags, nil, false, nil
			}
			return regTags, gitTags, true, nil
		}
		metaFn := func(ctx context.Context, tag string) (registry.ImageMeta, error) {
			ref, err := image.Parse(imageRepo + ":" + tag)
			if err != nil {
				return registry.ImageMeta{}, err
			}
			return reg.Config(ctx, ref)
		}
		return mapped, listFn, metaFn
	}
}
