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
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
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
		fmt.Fprintf(stderr, "commands:\n  plan           build a promotion plan for one env pair; --dry-run prints it and touches nothing\n  promote        drive a promotion to completion: worktree, commit, push, PR, CI, approval, merge, Argo refresh, Argo sync, rollout (resumable)\n  deploy         write one named image into one env and drive the same pipeline (--env, --image repo:tag@sha256:...); the image-bump half of promote\n  restart        roll an env's Deployments without changing the refs they declare (--env, optional --family); patches the live pod template like kubectl, writes nothing to git\n  promotions     list every promotion state file, with phase re-observed against the forge\n  resume <id>    re-drive a specific promotion (or --env <target-env>) from wherever it actually is\n  abandon <id>   retire a promotion that never landed (--confirm-abandon=<id>); closes its PR and deletes its branch if it opened one\n  watch --app    read-only: an Argo Application's sync/health/revision and its Deployments' rollout progress\n  config show    print the effective config (defaults filled in, secrets redacted)\n  config path    print where the config file is read from\n\n")
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
	case "abandon":
		return runAbandon(fs.Args()[1:], cfg, stdout, stderr)
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
// operator's override from the selected repo's own default (svc.List/svc.Resume, whose
// promotions may belong to another repo with another context). A subcommand copies its own
// --base/--kube-context into selection before selectRepo, so both fields hold that
// subcommand's answer, not only the root's. resolve carries the digest-resolution flags
// (#132) the same way — the flags as given, "" meaning "the config decides", exactly what
// service.NewResolveOptions takes as its own trailing string arguments.
type effective struct {
	repo, appsRoot    string
	promotable        []string
	base, kubeContext string
	kubeOverride      string
	resolve           resolveFlags
	cfg               *config.RepoConfig
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

// settingsFor builds the service.Settings a command's own Service is constructed from, out of
// the same effective flag/config precedence selectRepo already computed — the one place
// cmd/hoist reads eff into the shape internal/service takes, so a command never re-derives its
// own copy of the kube-context/promotable/poll precedence eff already settled.
func settingsFor(cfg *config.Config, eff effective) service.Settings {
	s := service.Settings{
		RepoDir:      eff.repo,
		AppsRoot:     eff.appsRoot,
		Base:         eff.base,
		Promotable:   eff.promotable,
		KubeContext:  eff.kubeContext,
		KubeOverride: eff.kubeOverride,
		Repo:         eff.cfg,
		Config:       cfg,
	}
	if cfg != nil {
		s.Poll = pollIntervals(cfg.Poll)
		s.Deadline = time.Duration(cfg.Poll.Deadline)
		s.Retain = time.Duration(cfg.State.Retain)
	}
	return s
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
	opts, err := service.NewResolveOptions(cfg, eff.cfg, rf.digestSources, rf.registryAuth, rf.clusterSecret, rf.opRef, rf.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitUsage
	}

	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	set := settingsFor(cfg, eff)
	set.Resolve = opts
	svc := service.New(set, serviceDeps())
	if err := svc.CheckOverrides(r, *from, digests); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	// Signal/deadline-aware, exactly like runPromote/runDeploy's own ctx (promote.go's own
	// comment on this same call): resolution can talk to the cluster and the registry, so ^C
	// or the poll deadline must be able to interrupt it here too, not just the write path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	pc, err := svc.Plan(ctx, service.PlanRequest{Repo: r, Source: *from, Target: *to, Overrides: digests})
	if err != nil {
		// The CLI printer's own guard (R-002): a cluster or registry error is already
		// redacted at its adaptor, but this is the last stop before stderr, so a value
		// registered anywhere in the process is scrubbed here too.
		fmt.Fprintf(stderr, "hoist plan: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	plan, rep := pc.Plan, pc.Resolution
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
		if service.AnyRealEdit(plan.Edits) {
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

// printPlan renders the plan read-only: files are read from disk, edits applied in memory,
// verified, and diffed. Nothing is written. rep, when non-nil, adds the resolution section
// before the warnings; with nil the output is M1's, byte for byte.
func printPlan(w io.Writer, r *gitops.Repo, plan *gitops.Plan, prefixes, configured []string, rep *service.Resolution) error {
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
		printResolution(w, rep)
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
// user quits. cfg is the whole loaded config file (service.NewResolveOptions needs it to find the
// matching registries[] entry); eff.cfg is the selected repo's own entry, nil on flags
// alone — the plan screen then runs in "digest sources: none" mode with default resolution
// options and an empty envs config, matching what M1 offered before this milestone.
func runTUI(eff effective, cfg *config.Config, stdout, stderr io.Writer) int {
	// The root --digest-sources/--registry-auth/--cluster-secret/--op-ref (#132) are resolved
	// once, here, before the Service exists at all: the plan screen resolves with them, and the
	// credential-chain overrides reach the tag picker's and the history's registry clients too
	// (svc.RegistryFor). A malformed one is refused here, before the screen opens, exactly as
	// `hoist plan` refuses it.
	regOpts, err := service.NewResolveOptions(cfg, eff.cfg, eff.resolve.digestSources, eff.resolve.registryAuth, eff.resolve.clusterSecret, eff.resolve.opRef, eff.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitUsage
	}
	set := settingsFor(cfg, eff)
	set.Resolve = regOpts
	svc := service.New(set, serviceDeps())

	// The repo view is a cached worktree at origin/<base> (internal/service's repo.go),
	// fetched fresh here and on every F5 — never eff.repo's own working tree directly, and
	// never the operator's own checkout touched to get it (AGENTS.md §4.6). Falls back to
	// eff.repo itself (today's exact pre-#PR7 behavior — a pure local disk read, no network
	// needed) when origin can't be reached at all: browsing the matrix must stay possible
	// offline, warn-don't-block (principle 5) rather than a new hard requirement a read-only
	// screen never had before.
	view, err := svc.LoadRepo(context.Background(), service.RepoFromOrigin)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	if view.Fallback != nil {
		fmt.Fprintf(stderr, "hoist: could not read origin/%s (%v) — showing %s's own local content instead\n", eff.base, view.Fallback, eff.repo)
	}
	r := view.Repo
	var envs config.EnvsConfig
	if eff.cfg != nil {
		envs = eff.cfg.Envs
	}
	// The root --kube-context (#105), already reconciled with the repo's kube.context by
	// selectRepo, reaches every cluster-touching adaptor the TUI builds — the same value a
	// subcommand's own flag would carry. The drift column (buildDriftFunc) asks the pods
	// alone and takes none of the digest-resolution overrides above.

	// git.Exec{} and the forge adaptor are pure, stateless clients. f/forgeErr here are for
	// buildHistoryFuncs below (svc's own StartPromotion/List/Resume build and memoize their own
	// forge client through serviceDeps' Forge closure instead, never this one) — built once and
	// reused for every history lookup in this TUI session, mirroring newGit/newForge's own
	// package-level reuse across a single runPromote call. newForge is called even when eff.cfg
	// is nil or has no GitHub configured (github.New("") fails fast on the owner/name parse
	// alone, before ever touching gh's own auth or the network) so buildHistoryFuncs always has
	// a forge value to close over; its own eff.cfg check runs first and reports the
	// missing-config case before this error would ever matter.
	githubRepo := ""
	if eff.cfg != nil {
		githubRepo = eff.cfg.GitHub
	}
	f, forgeErr := newForge(githubRepo)
	promo := app.Promotion{
		Poll:       buildPollDurations(cfg.Poll),
		OpenURL:    browserOpener(time.Duration(cfg.Preferences.BrowserLaunchTimeout)),
		OpenPRMode: cfg.Preferences.OpenPR,
	}
	tagsFn := buildTagsFunc(eff.cfg, svc)
	// buildRestartFuncs/buildWatchFunc/buildHistoryFuncs below read the Argo/rollout clients and
	// the current repo through svc itself, per call, rather than a client or a repo captured
	// once here at boot (Train 2 design PR 7): svc.Argo/svc.Rollout memoize only a success, so a
	// cluster unreachable this instant is retried on the next w/R/F5 rather than wedged for the
	// rest of the session, and svc.Repo() always answers with whatever LoadRepo/RefreshRepo most
	// recently stored, so a family an F5 refresh just added is visible immediately.
	restartFn := buildRestartFuncs(svc, eff.kubeContext, cfg.Poll)
	historyFn := buildHistoryFuncs(eff.cfg, f, forgeErr, eff.base, svc)
	configText, err := configViewText(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	root := app.New(r, eff.promotable, envs, svc.Plan, svc, promo, tagsFn, restartFn).
		WithConfigView(cfg.File, cfg.Found, configText).
		WithHistory(historyFn).
		WithDrift(buildDriftFunc(eff.kubeContext)).
		WithRefreshRepo(buildRefreshRepoFunc(svc)).
		WithWatch(buildWatchFunc(svc, eff.kubeContext, argoNamespaceOf(eff.cfg), cfg.Poll)).
		WithRun(eff.base, eff.kubeContext)
	final, err := tea.NewProgram(root, tea.WithOutput(stdout)).Run()
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	// Keymap rule 4: q (with a confirm) and ctrl+c (immediate) both leave any running drive
	// tracked exactly where it was — nothing is cancelled, only this process's own watch of it
	// ends — so on the way out, name every id still in flight and how to pick it back up.
	if fm, ok := final.(app.Model); ok {
		for _, id := range fm.InFlightIDs() {
			fmt.Fprintf(stdout, "in flight: %s — hoist resume %s\n", id, id)
		}
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

// buildTagsFunc adapts svc.RegistryFor's own per-repo credential scoping (F4), plus (when
// RepoConfig.Apps maps the requested image repo) a pkg/forge/github.Client for that app repo,
// into a tags.BuildFunc the TUI's tag picker calls per image repo — never importing registry/
// forge/config policy itself (AGENTS.md §4.8). M6's tag picker has no gitops.Repo occurrence to
// scope credentials by (it is "a direct caller that skips resolve", per resolve.go's own
// multiRegistry doc comment) — svc.RegistryFor picks the one registries[] entry (if any)
// covering imageRepo the identical way Plan's own resolution does.
func buildTagsFunc(rc *config.RepoConfig, svc *service.Service) tags.BuildFunc {
	return func(imageRepo string) (bool, tags.RegTagsFunc, tags.GitTagsFunc, tags.MetaFunc) {
		reg, err := svc.RegistryFor(imageRepo)
		if err != nil {
			failedRegTags := func(context.Context) ([]string, error) { return nil, err }
			return false, failedRegTags, nil, nil
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

		// #PR8: the single blocking listFn this used to be is now two independent calls, so
		// the picker can render regTagsFn's own fast answer without waiting on gitTagsFn's
		// slow N+1 crawl (pkg/forge.Forge.Tags' own doc comment) — internal/app/tags.Model's
		// Init fires both as separate tea.Cmds (regTagsCmd/gitTagsCmd) and backfills the
		// ordering whenever gitTagsFn actually answers, in whichever order the two land.
		regTagsFn := func(ctx context.Context) ([]string, error) {
			return reg.Tags(ctx, imageRepo)
		}
		// nil when this repo was never mapped at all — tags.Model.gitTagsCmd treats a nil
		// GitTagsFunc as "nothing to eventually ask", never a call that runs and reports
		// mapped=false (that shape is for a config mapping that fails at RUNTIME, below, not
		// for a repo with no mapping in the config to begin with).
		var gitTagsFn tags.GitTagsFunc
		if mapped {
			// mapped is this call's own observed answer (GitTagsFunc's doc comment) — closed
			// over as the value fc was actually built with (finding 3, round 2, carried over
			// from the pre-split design): the caller (internal/app/tags.Model.onGitTagsLoaded)
			// trusts THIS return, every call, over whatever BuildFunc's own static mapped
			// result said when the picker was opened.
			gitTagsFn = func(ctx context.Context) ([]forge.GitTag, bool, error) {
				gitTags, err := fc.Tags(ctx)
				if err != nil {
					// Degrade, not fail (AGENTS.md principle 5): the registry tags alone
					// (regTagsFn's own answer, already rendered by the time this can ever
					// return) are still a usable picker without invariant 3's own preferred
					// ordering.
					return nil, false, nil
				}
				return gitTags, true, nil
			}
		}
		metaFn := func(ctx context.Context, tag string) (registry.ImageMeta, error) {
			ref, err := image.Parse(imageRepo + ":" + tag)
			if err != nil {
				return registry.ImageMeta{}, err
			}
			return reg.Config(ctx, ref)
		}
		return mapped, regTagsFn, gitTagsFn, metaFn
	}
}
