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
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/resolve"
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
	configPath := fs.String("config", "", "config file (default $XDG_CONFIG_HOME/hoist/config.yaml, else ~/.config/hoist/config.yaml; a missing default is fine, a missing explicit path is not)")
	repo := fs.String("repo", "", "path to the GitOps repo checkout, or the name or path of a repos[] entry in the config file; with no command, opens the env/family matrix. Optional when the config file lists exactly one repo")
	appsRoot := fs.String("apps-root", gitops.DefaultAppsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	promotable := fs.String("promotable", "ghcr.io/", "comma-separated image repo prefixes that count as first-party (the selected repo's promotable when configured)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hoist [flags] [<command> [command flags]]\n\n")
		fmt.Fprintf(stderr, "no command: open the env/family matrix for --repo\n\n")
		fmt.Fprintf(stderr, "commands:\n  plan           build a promotion plan for one env pair; --dry-run prints it and touches nothing\n  promote        drive a promotion to completion: worktree, commit, push, PR, CI, approval, merge, Argo refresh, Argo sync, rollout (resumable; see AGENTS.md §4.1)\n  promotions     list every promotion state file, with phase re-observed against the forge\n  resume <id>    re-drive a specific promotion (or --env <target-env>) from wherever it actually is\n  watch --app    read-only: an Argo Application's sync/health/revision and its Deployments' rollout progress\n  config show    print the effective config (defaults filled in, secrets redacted)\n  config path    print where the config file is read from\n\n")
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
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	sel := selection{repo: *repo, appsRoot: *appsRoot, promotable: *promotable, given: map[string]bool{}}
	fs.Visit(func(f *flag.Flag) { sel.given[f.Name] = true })
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
	case "promotions":
		return runPromotions(fs.Args()[1:], cfg, stdout, stderr)
	case "resume":
		return runResume(fs.Args()[1:], cfg, stdout, stderr)
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
	given                      map[string]bool
}

// effective is what the config file and the flags agree the run is about. cfg is the
// selected repos[] entry, nil when the run is on flags alone.
type effective struct {
	repo, appsRoot string
	promotable     []string
	cfg            *config.RepoConfig
}

// selectRepo applies the precedence: a flag given on the command line wins; otherwise the
// selected repo's config value; otherwise the flag's M1 default. The repo is selected by
// --repo (a repos[] name or path) or, with no --repo, as the only configured repo. A
// --repo that matches no entry is a plain checkout path and takes the flag defaults, so
// the config file never changes what an explicit command line means.
func selectRepo(cfg *config.Config, sel selection) (effective, error) {
	eff := effective{repo: sel.repo, appsRoot: sel.appsRoot, promotable: splitList(sel.promotable)}
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
	fs.StringVar(&rf.kubeContext, "kube-context", "", "kubeconfig context whose pods supply digests (default: the selected repo's kube.context when configured, else the kubeconfig's current context; the name in use is printed)")
	fs.StringVar(&rf.digestSources, "digest-sources", "", "comma-separated digest sources, first wins: pods (what --from is running), manifest (its own pin), registry (HEAD of its tag); none plans from the manifests alone, exactly as M1 did (default: the selected repo's digest_sources when configured, else pods,manifest,registry)")
	fs.StringVar(&rf.registryAuth, "registry-auth", "", "comma-separated registry credential sources tried in order: env, keychain, cluster, op; the one that worked is reported by name (default: the matching registries[] entry's auth when configured, else env,keychain,cluster,op)")
	fs.StringVar(&rf.clusterSecret, "cluster-secret", "", "namespace/name of a kubernetes.io/dockerconfigjson pull secret for the cluster credential source (default: the matching registries[] entry's cluster when configured; unset skips the source)")
	fs.StringVar(&rf.opRef, "op-ref", "", "op://vault/item/field for the op credential source, read with `op read` (default: the matching registries[] entry's op when configured; unset skips the source and runs nothing)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	sel.repo, sel.appsRoot, sel.promotable = *repo, *appsRoot, *promotable
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
	for name, val := range map[string]string{"digest-sources": rf.digestSources, "registry-auth": rf.registryAuth} {
		if sel.given[name] && strings.TrimSpace(val) == "" {
			fmt.Fprintf(stderr, "hoist plan: --%s: empty; %s\n", name, map[string]string{"digest-sources": "use none to plan without resolution", "registry-auth": "list at least one of env, keychain, cluster, op"}[name])
			return exitUsage
		}
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
	plan, err := gitops.BuildPlan(r, *from, *to, prefixes, planDigests)
	if err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if rep != nil {
		plan.Warnings = append(resolve.Warnings(rep.res), plan.Warnings...)
	}
	if err := printPlan(stdout, r, &plan, prefixes, rep); err != nil {
		fmt.Fprintf(stderr, "hoist plan: %v\n", err)
		return exitFailure
	}
	if !*dryRun {
		fmt.Fprintln(stderr, "hoist plan: promotion without --dry-run lands in a later milestone; nothing was written. The output above is what it would change.")
		return exitNotImplemented
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
func printPlan(w io.Writer, r *gitops.Repo, plan *gitops.Plan, prefixes []string, rep *resolutionReport) error {
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
		fmt.Fprintf(w, "  %s  (%s)\n", ref, untouchedReason(ref, plan.SourceEnv, prefixes))
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

func untouchedReason(ref image.Ref, src string, prefixes []string) string {
	if gitops.IsPromotable(ref.Repo, prefixes) {
		return "not running in " + src
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
	resolveFn := buildResolveFunc(cfg, eff.cfg, eff.promotable)

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
	// The Argo/Deployment adaptors are built here for the same reason f is, and deferred the
	// same way: buildPromotionForConfirm's one-in-flight scan re-observes OTHER promotions with
	// engine.AllSteps, which from M5 on includes the Argo/rollout steps, so it needs real
	// adaptors even though this screen never drives past Merged itself (see
	// buildStartPromotion's own steps list, and buildPromotionForConfirm's doc comment for why
	// nil would panic on an already-merged state file rather than degrade). kubeContext is
	// resolved from the selected repo the same way resolutionOptions does it for the plan
	// screen's own digest resolution.
	kubeContext := ""
	if eff.cfg != nil {
		kubeContext = eff.cfg.Kube.Context
	}
	a, _, argoErr := newArgo(kubeContext)
	ro, _, rolloutErr := newRollout(kubeContext)
	promo := app.Promotion{
		Start:      buildStartPromotion(eff, r, newGit, f, forgeErr, a, ro, errors.Join(argoErr, rolloutErr)),
		Poll:       buildPollDurations(cfg.Poll),
		OpenURL:    browserOpener(time.Duration(cfg.Preferences.BrowserLaunchTimeout)),
		OpenPRMode: cfg.Preferences.OpenPR,
	}
	if _, err := tea.NewProgram(app.New(r, eff.promotable, envs, resolveFn, promo), tea.WithOutput(stdout)).Run(); err != nil {
		fmt.Fprintf(stderr, "hoist: %v\n", err)
		return exitFailure
	}
	return 0
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
	opts, optsErr := resolutionOptions(cfg, rc, resolveFlags{})
	return func(ctx context.Context, r *gitops.Repo, source string) (plan.ResolveOutcome, error) {
		if optsErr != nil {
			return plan.ResolveOutcome{}, optsErr
		}
		if len(opts.order) == 0 {
			return plan.ResolveOutcome{}, nil // digest sources: none
		}
		rep, err := runResolution(ctx, r, source, prefixes, opts, nil)
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
