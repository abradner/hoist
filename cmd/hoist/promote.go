package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/forge/github"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
)

// newGit and newForge are variables so tests substitute fakes/local fixtures: no test in this
// repo points promote at a real GitHub repo or lets it touch a checkout other than a fixture
// it created itself.
var (
	newGit   git.Git = git.Exec{}
	newForge         = func(ownerRepo string) (forge.Forge, error) { return github.New(ownerRepo) }
)

// anyRealEdit reports whether edits contains at least one edit that is not a NoOp (the target
// already carries exactly the planned reference) — the all-NoOp fast-path guard both runPromote
// (below) and cmd/hoist/wiring.go's buildStartPromotion share, so a plan whose every edit is
// already satisfied is recognized identically on the CLI and TUI paths rather than only where
// each was written to check it.
func anyRealEdit(edits []gitops.Edit) bool {
	for _, e := range edits {
		if !e.NoOp() {
			return true
		}
	}
	return false
}

// runPromote is `hoist promote`: builds the same gitops.Plan `hoist plan --dry-run` would
// print, then drives internal/engine's four steps to actually commit it, push it and open a
// PR. Named "promote" rather than "push": the command's whole point is the promotion — commit
// + push + PR together — and "push" would read as only the git step, one of four this command
// actually performs.

// checkCloneCurrentForBase confirms, for every distinct file the plan's edits reference, that
// planning against the clone's (cloneDir's) on-disk content — exactly what gitops.Discover and
// BuildPlan already read, above, to build this plan — still matches what a brand-new
// promotion's worktree will actually be built from. Two independent questions, neither of which
// this package can repair directly (AGENTS.md §4.6 forbids ever bringing cloneDir's own
// checked-out files up to date via `git reset --hard`/`merge --ff-only` — that would touch "the
// user's own checked-out branch, working tree or index" even when that checkout happens to be on
// base itself — so a mismatch is only ever refused, never silently fixed):
//
//  1. Is cloneDir's disk clean relative to what its local base branch has actually committed?
//     (This check's original, round-1 scope: an uncommitted local edit means the plan was built
//     from content nothing in git — local or origin — has ever recorded.)
//  2. Does the local base branch's own committed content agree with origin/<base>'s CURRENT
//     tip — fetched fresh by this function itself, first, via the same git.Git.FetchBranch
//     direct mode's own publish step already calls? A stale remote-tracking ref used to be this
//     function's own gap (round 5, finding 1: nothing ever fetched before this ran, so
//     "as last fetched" was unbounded staleness, not a documented limitation) — closed here by
//     never trusting a cached ref at all.
//
// Question 2 runs unconditionally, never gated by which side is "ahead": pkg/git.Exec.Worktree's
// own resolveBase prefers origin/<base> over the local branch of the same name whenever that ref
// exists, full stop — it does not care whether local is behind, caught up, or itself ahead with
// an unpushed commit. A previous revision of this function trusted local's bytes whenever local
// was ahead of (or equal to) origin, which was exactly backwards (round 5, finding 2): an
// unpushed local commit changing a planned file is just as untrustworthy as origin having moved
// independently, because the worktree this promotion actually commits into is seeded from origin,
// not from that unpushed local content, regardless. A mismatch is refused unless it equals
// exactly what applying this promotion's own edits to the clone's current content would produce
// (the resume-safety carve-out: that shape is this exact promotion's own prior, successful
// direct-mode push, or any other route to the identical end state — not foreign drift — and
// Drive's own re-observation, AGENTS.md §4.1, is what correctly reports it done rather than this
// function refusing a legitimate resume).
func checkCloneCurrentForBase(ctx context.Context, g git.Git, cloneDir, base string, edits []gitops.Edit) error {
	if _, _, err := g.FetchBranch(ctx, cloneDir, "origin", base); err != nil {
		return fmt.Errorf("fetching origin/%s to confirm the clone is current: %w", base, err)
	}

	// localOK is deliberately ignored: base's own local branch not resolving at all is caught
	// per-file below (LsTreeBlob against base fails, which the dirty bucket already treats as
	// untrustworthy) — nothing here needs it as a bool in its own right.
	localSHA, _, err := g.RevParse(ctx, cloneDir, base)
	if err != nil {
		return err
	}
	originRef := "origin/" + base
	originSHA, originOK, err := g.RevParse(ctx, cloneDir, originRef)
	if err != nil {
		return err
	}
	// targetRef is exactly what pkg/git.Exec.Worktree's own resolveBase would build a brand-new
	// promotion branch from: origin/<base> whenever that ref exists at all — the bare local
	// branch name only when there is none to prefer (no "origin" remote configured, or nothing
	// ever fetched from it — some tests construct exactly that; resolveBase's own fallback).
	targetRef, targetSHA := base, localSHA
	if originOK {
		targetRef, targetSHA = originRef, originSHA
	}

	byFile := map[string][]gitops.Edit{}
	var files []string
	for _, e := range edits {
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)

	var dirty, stale []string
	for _, f := range files {
		p, err := gitops.ResolvePath(cloneDir, f)
		if err != nil {
			return err
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading %s from %s: %w", f, cloneDir, err)
		}
		curBlob, err := g.HashObject(ctx, cloneDir, cur)
		if err != nil {
			return err
		}
		localBlob, ok, err := g.LsTreeBlob(ctx, cloneDir, base, f)
		if err != nil {
			return err
		}
		if !ok || curBlob != localBlob {
			dirty = append(dirty, f)
			continue // already refusing this file; no need to also check it against the target.
		}
		if targetRef == base {
			continue // no origin/<base> ref exists at all: the local branch IS the target.
		}
		targetBlob, ok, err := g.LsTreeBlob(ctx, cloneDir, targetRef, f)
		if err != nil {
			return err
		}
		if ok && targetBlob == curBlob {
			continue // local already matches what the worktree will actually be built from.
		}
		after, err := gitops.ApplyBytes(cur, byFile[f])
		if err != nil {
			return err
		}
		afterBlob, err := g.HashObject(ctx, cloneDir, after)
		if err != nil {
			return err
		}
		if !ok || targetBlob != afterBlob {
			stale = append(stale, f)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%s has uncommitted local changes not yet in %q for: %s — a plan built from that content can't be trusted; commit, stash or discard the local changes and re-run", cloneDir, base, strings.Join(dirty, ", "))
	}
	if len(stale) > 0 {
		// Deliberately one neutral phrasing regardless of which side is "ahead": a directional
		// word ("fallen behind", "has an unpushed commit") would be wrong exactly when local and
		// origin have each moved independently (a real divergence, not a simple lead/lag) — this
		// question no longer cares which direction the mismatch runs, only that it exists, so
		// the message doesn't claim a direction the mechanism itself doesn't distinguish.
		return fmt.Errorf(
			"%s's local %q (%s) disagrees with %s (%s) — which is what a new promotion's worktree is actually built from — for: %s; reconcile (fetch/push as appropriate) and re-run",
			cloneDir, base, shortRev(localSHA), targetRef, shortRev(targetSHA), strings.Join(stale, ", "),
		)
	}
	return nil
}

// shortRev truncates a commit sha for a readable error message; used only for display.
func shortRev(sha string) string {
	const n = 12
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}

// discoverAtFreshBase fetches origin/base fresh (never a cached ref) and checks out its exact
// current tip into a throwaway, detached worktree — never cloneDir's own checked-out branch or
// working files (AGENTS.md §4.6) — so a caller can discover and plan against exactly what
// origin/base currently holds, independent of whatever cloneDir's own local disk happens to
// show. Used only by checkNoMissingOccurrenceAtFreshBase below, as a cross-check: planning
// itself still reads from cloneDir, exactly as it always has (see checkCloneCurrentForBase's own
// doc comment for why that source, and its own validate-and-refuse dance, is kept — resolving a
// promotable digest from the "manifest" source, in particular, means the source env's own local
// content is meant to be authoritative, unpushed edits included, not silently overridden by
// whatever origin happens to hold).
//
// Returns the snapshot directory and a cleanup func the caller must call once done reading from
// it (nothing here needs to survive past that one comparison; cleanup is idempotent-safe to call
// more than once or after a partial failure).
func discoverAtFreshBase(ctx context.Context, g git.Git, cloneDir, base string) (dir string, cleanup func(), err error) {
	if _, _, err := g.FetchBranch(ctx, cloneDir, "origin", base); err != nil {
		return "", nil, fmt.Errorf("fetching origin/%s: %w", base, err)
	}
	// Mirrors pkg/git.Exec's own resolveBase: prefer the remote-tracking ref whenever it
	// exists, falling back to the bare branch name only for a repo with no "origin" configured
	// at all, or one nothing has ever fetched from — resolveBase's own doc comment explains why
	// (some tests construct exactly that; a real clone always has one after the fetch above).
	ref := base
	if _, ok, rerr := g.RevParse(ctx, cloneDir, "origin/"+base); rerr != nil {
		return "", nil, rerr
	} else if ok {
		ref = "origin/" + base
	}
	tmp, err := os.MkdirTemp("", "hoist-direct-discover-*")
	if err != nil {
		return "", nil, err
	}
	snap := filepath.Join(tmp, "base")
	cleanup = func() {
		_ = g.RemoveWorktree(context.Background(), cloneDir, snap)
		_ = os.RemoveAll(tmp)
	}
	if err := g.WorktreeAtRef(ctx, cloneDir, snap, ref); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("checking out a throwaway snapshot of %s: %w", ref, err)
	}
	return snap, cleanup, nil
}

// checkNoMissingOccurrenceAtFreshBase is direct mode's own addition alongside
// checkCloneCurrentForBase (round-N finding, "base-advanced-with-new-occurrence"): it
// independently discovers and plans from a throwaway, freshly-fetched snapshot of origin/base's
// current tree, then refuses if that discovers any occurrence — identified by file/line/column,
// never by its current value, since a differing value at an ALREADY-known position is exactly
// what checkCloneCurrentForBase already validates — that plan (built from cloneDir's own disk,
// same as always) does not already know about at all.
//
// Only direct mode needs this: only direct mode's own prior pushes can put origin/base ahead of
// cloneDir's local disk in a way cloneDir itself never observes (PushHeadTo never advances
// cloneDir's own checked-out branch — AGENTS.md §4.6 — and nothing else refreshes it either).
// The PR flow's own worktree is always built directly from cloneDir's content, whatever it is,
// and any staleness there surfaces as an ordinary merge conflict on GitHub — a softer failure
// this check does not need to guard against.
//
// planDigests is reused as-is from the caller's own already-completed resolution (pods/manifest/
// registry): this is only about which occurrences BuildPlan finds, never a second, independent
// digest resolution against the fresh snapshot — the source env's own local content stays
// authoritative for what digest is "current" (discoverAtFreshBase's own doc comment).
func checkNoMissingOccurrenceAtFreshBase(ctx context.Context, g git.Git, cloneDir, base, appsRoot string, plan gitops.Plan, buildFresh func(*gitops.Repo) (gitops.Plan, error)) error {
	snap, cleanup, err := discoverAtFreshBase(ctx, g, cloneDir, base)
	if err != nil {
		return err
	}
	defer cleanup()

	freshRepo, err := gitops.Discover(snap, appsRoot)
	if err != nil {
		return fmt.Errorf("discovering origin/%s's own current tree: %w", base, err)
	}
	// buildFresh is how this plan would be built against origin's tree rather than the local
	// one — BuildPlan for a promotion, BuildDeployPlan for a deploy. Passed in rather than
	// branched on here so the occurrence comparison below stays a single implementation: this
	// check is direct mode's only defence against writing a file the local clone cannot see,
	// and a second copy of it is how one command silently loses that defence.
	freshPlan, err := buildFresh(freshRepo)
	if err != nil {
		return fmt.Errorf("planning against origin/%s's own current tree: %w", base, err)
	}

	known := make(map[string]bool, len(plan.Edits))
	for _, e := range plan.Edits {
		known[occurrencePositionKey(e.Occurrence)] = true
	}
	var missing []string
	for _, e := range freshPlan.Edits {
		if !known[occurrencePositionKey(e.Occurrence)] {
			missing = append(missing, fmt.Sprintf("%s (line %d)", e.File, e.Line))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"origin/%s has occurrence(s) %s's own local checkout doesn't know about at all: %s — fetch/merge to update your clone and re-run; direct mode never writes a file it can't already see locally",
		base, cloneDir, strings.Join(missing, ", "),
	)
}

// occurrencePositionKey identifies an occurrence by WHERE it is, never by its current value —
// the same physical scalar discovered from two different snapshots (cloneDir's disk, origin's
// fresh tree) shares this key even when the two disagree on content, which is exactly the case
// checkCloneCurrentForBase already handles separately.
func occurrencePositionKey(o gitops.Occurrence) string {
	return fmt.Sprintf("%s#%d:%d", o.File, o.Line, o.Col)
}

func runPromote(args []string, cfg *config.Config, sel selection, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", sel.repo, "path to the GitOps repo checkout, or a configured repo's name (required unless the config file lists exactly one repo; may also be given before the command)")
	appsRoot := fs.String("apps-root", sel.appsRoot, "directory of Argo Application wrappers, relative to --repo (the selected repo's apps_root when configured)")
	from := fs.String("from", "", "source env: the Argo destination namespace to read digests from (required)")
	to := fs.String("to", "", "target env: the Argo destination namespace to rewrite (required)")
	promotable := fs.String("promotable", sel.promotable, "comma-separated image repo prefixes hoist may promote (see hoist plan -h)")
	base := fs.String("base", "main", "the GitOps repo's default branch: what the promotion branch is created from and the PR targets")
	direct := fs.Bool("direct", false, "commit straight to --base with no PR — non-production envs only. internal/engine.DirectCommitGateStep refuses this outright for any env listed in the selected repo's envs.production, regardless of this flag (AGENTS.md M6 'Direct mode'): this flag is not itself the gate, only how the CLI reaches it. Requires --confirm-direct=<env> too")
	confirmDirect := fs.String("confirm-direct", "", "the operator's explicit second acknowledgement required alongside --direct: must repeat --to's exact value (refused otherwise) — the CLI's stronger keypress-then-confirm shape (the TUI's equivalent is internal/app/tags' own keypress + huh.Confirm dialog, which names the same env in its prompt)")
	digests := digestFlag{}
	fs.Var(digests, "digest", "repo=repo:tag@sha256:<64 hex> — plan this reference for repo instead of what --from runs (see hoist plan -h)")
	var rf resolveFlags
	fs.StringVar(&rf.kubeContext, "kube-context", "", "kubeconfig context whose pods supply digests (see hoist plan -h)")
	fs.StringVar(&rf.digestSources, "digest-sources", "", "comma-separated digest sources, first wins (see hoist plan -h)")
	fs.StringVar(&rf.registryAuth, "registry-auth", "", "comma-separated registry credential sources tried in order (see hoist plan -h)")
	fs.StringVar(&rf.clusterSecret, "cluster-secret", "", "namespace/name of a pull secret for the cluster credential source (see hoist plan -h)")
	fs.StringVar(&rf.opRef, "op-ref", "", "op://vault/item/field for the op credential source (see hoist plan -h)")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway (has no effect on ci.none: block, which has no override; see AGENTS.md invariant 1)")
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
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	if eff.repo == "" || *from == "" || *to == "" {
		fmt.Fprintln(stderr, "hoist promote: --repo, --from and --to are required")
		fs.Usage()
		return exitUsage
	}
	if code := checkDirectPreflight("hoist promote", eff, *direct, *confirmDirect, *to, stderr); code != 0 {
		return code
	}
	prefixes := eff.promotable

	opts, err := resolutionOptions(cfg, eff.cfg, rf)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitUsage
	}
	r, err := gitops.Discover(eff.repo, eff.appsRoot)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	if err := checkOverrides(r, *from, prefixes, digests); err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	planDigests := map[string]image.Ref(digests)
	if len(opts.order) > 0 {
		rep, err := runResolution(context.Background(), r, *from, prefixes, opts, digests)
		if err != nil {
			fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(err.Error()))
			return exitFailure
		}
		planDigests = rep.digests(digests)
	}
	plan, err := gitops.BuildPlan(r, *from, *to, prefixes, planDigests)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}

	// gitops.Discover, above, read every occurrence's position and content from eff.repo's own
	// disk — whatever was there at the moment this process started. Confirm that's still
	// trustworthy relative to --base's origin tip — fetched fresh by checkCloneCurrentForBase
	// itself, never a cached ref — before trusting the plan built from it at all: unconditionally,
	// not only when the plan turns out all-no-op (checkNoOpAgainstBase's original, round-1
	// scope). A false "already current" from a stale no-op is this package's worst failure mode
	// (nothing downstream calls gitops.Apply/Verify to catch it — no worktree exists yet on that
	// path); a real edit built from stale content deserves this clearer, earlier answer too,
	// rather than only a confusing verification failure deep in the engine.
	if err := checkCloneCurrentForBase(context.Background(), newGit, eff.repo, *base, plan.Edits); err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}

	// Direct mode's own additional gap (round-N finding, "base-advanced-with-new-occurrence"):
	// checkCloneCurrentForBase above only validates files plan.Edits already names — it cannot
	// catch an occurrence origin/*base has gained that eff.repo's own local disk never had at
	// all, since gitops.Discover above never saw that file to begin with. This is specific to
	// direct mode: only direct mode's own prior pushes can put origin/*base ahead of eff.repo's
	// local disk in the first place (PushHeadTo never advances eff.repo's own checked-out
	// branch — AGENTS.md §4.6 — and nothing else refreshes it either), so only direct mode
	// needs this cross-check.
	if *direct {
		buildFresh := func(fresh *gitops.Repo) (gitops.Plan, error) {
			return gitops.BuildPlan(fresh, *from, *to, prefixes, planDigests)
		}
		if err := checkNoMissingOccurrenceAtFreshBase(context.Background(), newGit, eff.repo, *base, eff.appsRoot, plan, buildFresh); err != nil {
			fmt.Fprintf(stderr, "hoist promote: %v\n", err)
			return exitFailure
		}
	}

	if !anyRealEdit(plan.Edits) {
		fmt.Fprintf(stdout, "hoist promote: %s -> %s is already current; nothing to promote.\n", plan.SourceEnv, plan.TargetEnv)
		return 0
	}

	f, err := newForge(eff.cfg.GitHub)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	// Both modes reach the Argo/rollout steps now that DirectSteps converges too (issue #66),
	// so these are unconditional. An earlier revision built them only for the PR path, because
	// direct mode stopped at the push and would otherwise have demanded a cluster for work it
	// never did; converging is the real fix, and it retires that gate.
	argoApps, err := engine.ArgoAppNames(r, plan.TargetEnv, plan.Edits)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	a, _, err := newArgo(opts.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	ro, _, err := newRollout(opts.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	s, release, err := buildPromotionForConfirm(ctx, eff, plan, *base, *overrideCINone, newGit, f, argoApps)
	// Recorded from this invocation's own flag, not carried from any prior state: like
	// --override-ci-none, an operator re-running with (or without) --direct is asking for that
	// mode now. DirectCommitGateStep re-derives the production refusal independently either
	// way, so this only ever selects the step list, never relaxes a gate.
	if s != nil {
		s.Direct = *direct
	}
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(err.Error()))
		return exitFailure
	}
	// released tracks whether the claim has already been let go so the deferred cleanup below
	// is a no-op once the first successful state save has released it for real (see the save
	// wrapper further down) — mirrors buildPromotionForConfirm's own internal release-once
	// guard, which only covers its own error paths.
	released := false
	defer func() {
		if !released {
			released = true
			release()
		}
	}()

	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist promote: waiting for signing approval...")
		}
	}
	var steps []engine.Step
	if *direct {
		// eff.cfg.Envs.Production, unfiltered — DirectSteps'/DirectCommitGateStep's own doc
		// comments explain why this must be exactly that list, verbatim, never narrowed.
		// Confirmed is true only because both --direct and --confirm-direct were given,
		// checked above (the CLI's keypress-then-confirm equivalent).
		steps = engine.AllDirectSteps(newGit, a, ro, eff.cfg.Envs.Production, true, onWaiting)
	} else {
		steps = engine.AllSteps(newGit, f, a, ro, onWaiting)
	}
	// The claim only needs to outlive the gap up to the first durable state write — once that
	// lands, findInFlight's own re-observation of the real state file is what enforces invariant
	// 5 for the rest of the (possibly hours-long) promotion, so release it here rather than
	// holding it for the whole run.
	save := func(st *engine.PromotionState) error {
		if err := engine.SaveState(statePath, st); err != nil {
			return err
		}
		if !released {
			released = true
			release()
		}
		return nil
	}

	err = driveToCompletion(ctx, steps, s, save, cfg.Poll, stderr)
	return reportDriveResult(stdout, stderr, "hoist promote", plan.SourceEnv, plan.TargetEnv, s, err)
}

// buildPromotionForConfirm does everything runPromote does up through constructing (or loading
// and merging into) a real *engine.PromotionState for plan, before driveToCompletion's own
// polling loop ever starts: the id/branch/worktree/statePath derivation, the claim-then-rescan
// one-in-flight check (round-6 hardening — see the two findInFlight calls below, and
// ClaimInFlight's own doc comment for why a single scan-then-claim is not atomic), and the
// prior-state merge-in (the resume-safety fix, commit f3b1c53, that keeps a retried/resumed
// promotion from straddling two different CI/approval policies). This is real, hardened,
// several-review-rounds-deep logic; runPromote itself now calls this rather than inlining a copy
// (see its own body above), and cmd/hoist's TUI wiring (wiring.go's startPromotion) calls it too,
// so the CLI and TUI paths can never silently drift apart.
//
// base and overrideCINone are the CLI's own --base/--override-ci-none flag values; the TUI has
// no equivalent flags yet, so its own caller passes the same defaults ("main", false) runPromote
// itself defaults to.
//
// argoApps is M5's addition: the Argo Application names this promotion's own state records
// (engine.ArgoAppNames), computed by the caller from the same discovered repo BuildPlan already
// read — it needs a *gitops.Repo this function deliberately does not take. No Argo/rollout
// adaptor is needed here despite that: the two findInFlight scans below are scoped to
// engine.CoreSteps, never AllSteps, precisely so a merged-but-still-converging promotion does
// not block a new one (see findInFlight's own doc comment), which also means they never reach
// the steps that would need one.
//
// On success, release must be called by the caller exactly once the returned state's first
// successful save lands (ClaimInFlight's own doc comment: the claim's job is done once a durable
// state file exists for a future findInFlight/ObserveAll scan to see) — never held for the whole
// promotion. On error, any claim this call acquired has already been released; release is nil
// whenever err is non-nil.
func buildPromotionForConfirm(ctx context.Context, eff effective, plan gitops.Plan, base string, overrideCINone bool, g git.Git, f forge.Forge, argoApps []string) (*engine.PromotionState, func(), error) {
	id := engine.DeriveID(eff.cfg.GitHub, plan)
	branch := engine.BranchName(plan.TargetEnv, id)
	worktreeDir, err := engine.WorktreeDir(id)
	if err != nil {
		return nil, nil, err
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		return nil, nil, err
	}

	// Invariant 5: one in-flight promotion per target env. A different image set for the same
	// target env gets a different id (§4.1), so this can only find *another* promotion's state
	// file — re-observed fresh, never trusted from its own Phase (findInFlight/ObserveAll).
	if conflict, status, ferr := findInFlight(ctx, g, f, eff.cfg.GitHub, plan.TargetEnv, id); ferr != nil {
		return nil, nil, fmt.Errorf("checking whether another promotion is already in flight: %w", ferr)
	} else if conflict != nil {
		return nil, nil, inFlightConflictError(conflict, plan.TargetEnv, status)
	}

	// findInFlight above is read-only (it only ever looks at state files already on disk), so a
	// second confirm racing this one for the same target env could pass that check too, before
	// either process has written its own state file — engine.ClaimInFlight closes that window
	// with an atomic filesystem claim; a concurrent loser gets a clear conflict error here
	// rather than silently proceeding to open a second branch/PR for the same env. released
	// tracks whether it's already been let go so releaseOnce is idempotent for every error path
	// below and for a caller that later calls the returned release itself.
	claimRelease, err := engine.ClaimInFlight(eff.cfg.GitHub, plan.TargetEnv, id)
	if err != nil {
		return nil, nil, err
	}
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			claimRelease()
		}
	}
	ok := false
	defer func() {
		if !ok {
			releaseOnce()
		}
	}()

	// The scan above and the claim just acquired are not one atomic operation: a second
	// confirm (a different id, targeting the same env) can finish its own scan before this
	// process claimed anything, then pause; this process claims, drives all the way to its
	// first durable state save, and releases the claim (the claim's job is done once the state
	// file itself can be found by a future scan); only then does the second process resume and
	// successfully claim the now-free slot, without ever repeating its scan — so it never sees
	// the state file this process just wrote. Re-running the same scan now, while still holding
	// the claim, closes that window: nothing else can win the claim while this check runs, and
	// anything that raced into existence on disk between the first scan and now is caught here.
	if conflict, status, ferr := findInFlight(ctx, g, f, eff.cfg.GitHub, plan.TargetEnv, id); ferr != nil {
		return nil, nil, fmt.Errorf("checking whether another promotion is already in flight: %w", ferr)
	} else if conflict != nil {
		return nil, nil, inFlightConflictError(conflict, plan.TargetEnv, status)
	}

	s := &engine.PromotionState{
		ID:             id,
		RepoFullName:   eff.cfg.GitHub,
		SourceEnv:      plan.SourceEnv,
		TargetEnv:      plan.TargetEnv,
		Branch:         branch,
		CloneDir:       eff.repo,
		WorktreeDir:    worktreeDir,
		Base:           base,
		Edits:          plan.Edits,
		CommitMessage:  engine.RenderCommitMessage(id, plan),
		PRTitle:        engine.PRTitle(plan),
		PRBody:         engine.RenderPRBody(id, plan),
		CINone:         eff.cfg.CI.None,
		CIGrace:        time.Duration(eff.cfg.CI.Grace),
		CINoneOverride: overrideCINone,
		Approval:       eff.cfg.Approval(plan.TargetEnv),
		Approvers:      eff.cfg.Approvers,
		Collaborators:  eff.cfg.Collaborators,
		ArgoNamespace:  eff.cfg.Kube.ArgoNamespace,
		ArgoApps:       argoApps,
	}
	// The state file is an index of what to look at, never evidence of what happened
	// (AGENTS.md §4.1) — every Observe below re-derives truth from the worktree and the
	// remote regardless of what's loaded here. Carrying History forward is purely so the
	// caller's own output can show it; a missing or unreadable file never blocks the run. An
	// override flag given on a later re-run always wins over what a previous run persisted,
	// since the operator just asked for it again.
	if prev, err := engine.LoadState(statePath); err == nil && prev != nil && prev.ID == id {
		s.History = prev.History
		// Base, and the policy fields (CINone/CIGrace/Approval/Approvers/Collaborators), set
		// above from the caller's base/eff.cfg, are overwritten here with what the existing
		// state file already persisted — mirroring runResume's own fix (cmd/hoist/resume.go,
		// commit f3b1c53) for the identical bug at this sibling call site: PromotionState's doc
		// comment (internal/engine/state.go) states these are policy "as of when this promotion
		// started", carried forward so a promotion never straddles two different policies
		// mid-flight. Re-confirming a plan whose id already has a state file is still a resume
		// of THAT promotion, not a new one — leaving these fresh from the current base/config
		// would let a re-run with a different --base (or the TUI path's own implicit default)
		// retroactively redirect an already-started promotion, disagreeing with the PR/worktree
		// it already built against and later Blocking on pr.Base != s.Base (Copilot review).
		s.Base = prev.Base
		s.CINone = prev.CINone
		s.CIGrace = prev.CIGrace
		s.Approval = prev.Approval
		s.Approvers = prev.Approvers
		s.Collaborators = prev.Collaborators
		if !overrideCINone {
			s.CINoneOverride = prev.CINoneOverride
		}
		// The rendered artifacts are carried forward for the same reason, and one more. They
		// describe a commit and a PR that already exist: re-rendering them from this
		// invocation's plan would leave the state file narrating something the forge does not
		// say. That matters now that two differently-shaped commands can reach the same id —
		// DeriveID hashes (repo, target env, resulting refs) and deliberately treats a deploy
		// and a promotion landing identical refs as the same promotion (identity.go), so a
		// `promote` re-run against a `deploy`'s id would otherwise rewrite "deploy" wording to
		// "promote" while the actual commit and PR keep the original (self-review, PR #70).
		// Whoever described this promotion first is the one the world agrees with.
		if prev.CommitMessage != "" {
			s.CommitMessage = prev.CommitMessage
		}
		if prev.PRTitle != "" {
			s.PRTitle = prev.PRTitle
		}
		if prev.PRBody != "" {
			s.PRBody = prev.PRBody
		}
	}

	ok = true
	return s, releaseOnce, nil
}

// inFlightConflictError is the one-in-flight-per-target-env refusal message, shared by both of
// buildPromotionForConfirm's findInFlight calls above (the CLI's original wording, unchanged).
func inFlightConflictError(conflict *engine.PromotionState, targetEnv string, status engine.StepStatus) error {
	return fmt.Errorf("promotion %s targeting %s is still in flight (at %s: %s); run `hoist resume %s` instead of starting a second one",
		conflict.ID, targetEnv, status.Step, statusDetail(status.Observation), conflict.ID)
}

// statusDetail picks whichever of Observation's message fields is set — Blocked takes
// precedence since it's the more actionable one when both could apply.
func statusDetail(o engine.Observation) string {
	if o.Blocked != "" {
		return o.Blocked
	}
	return o.Detail
}

// reportDriveResult renders driveToCompletion's outcome the same way for hoist promote and
// hoist resume: the branch/commit/PR/merge summary on success, the specific messages
// AGENTS.md's "waiting for signing approval" / ErrWaiting / ctx deadline / Blocked cases call
// for, and the redact.Strings final boundary for anything else (Finding B: a step's Act error
// can embed a registered credential verbatim via a failed git command's wrapped stderr).
func reportDriveResult(stdout, stderr io.Writer, cmdName, sourceEnv, targetEnv string, s *engine.PromotionState, err error) int {
	switch {
	case err == nil:
		// A deploy has no source env, so the promotion's "A -> B" would render as
		// ": " with a hole in it — the same empty-SourceEnv defect the templates and
		// printPlan were reworked for, on the most visible line hoist prints. The arrow goes
		// with it: "-> env" with nothing on its left is the same hole one character narrower,
		// and a deploy is not a movement between two places anyway. It names the one env.
		if sourceEnv == "" {
			fmt.Fprintf(stdout, "%s: %s\n", cmdName, targetEnv)
		} else {
			fmt.Fprintf(stdout, "%s: %s -> %s\n", cmdName, sourceEnv, targetEnv)
		}
		fmt.Fprintf(stdout, "  branch: %s\n", s.Branch)
		fmt.Fprintf(stdout, "  commit: %s\n", s.CommitSHA)
		switch {
		case s.PR == nil:
			// A successfully completed promotion with no PR at all is direct mode's own
			// signature (it never creates one) — reportDriveResult is shared with resume.go,
			// which has no --direct flag of its own to check, so this reads it off the
			// PromotionState itself rather than taking a parameter only promote.go could supply.
			fmt.Fprintf(stdout, "  pushed straight to %s (direct mode, no PR)\n", s.Base)
		case s.PR != nil:
			fmt.Fprintf(stdout, "  PR: %s\n", s.PR.URL)
		}
		if s.MergeSHA != "" {
			fmt.Fprintf(stdout, "  merged: %s\n", s.MergeSHA)
		}
		return 0
	case errors.Is(err, engine.ErrWaiting):
		fmt.Fprintf(stderr, "%s: still waiting for signing approval; re-run to resume\n", cmdName)
		return exitFailure
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "%s: %s at %s (poll.deadline elapsed); re-run to resume\n", cmdName, s.Phase, redact.Strings(historyDetail(s)))
		return exitFailure
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(stderr, "%s: interrupted while %s; re-run to resume\n", cmdName, s.Phase)
		return exitFailure
	default:
		var blocked *engine.BlockedError
		if errors.As(err, &blocked) {
			fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(blocked.Error()))
			return exitFailure
		}
		fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(err.Error()))
		return exitFailure
	}
}

// historyDetail is the most recent History entry's Detail, for a deadline message that says
// what it was last observed waiting on rather than just naming the phase.
func historyDetail(s *engine.PromotionState) string {
	if len(s.History) == 0 {
		return ""
	}
	return s.History[len(s.History)-1].Detail
}

// checkDirectPreflight is the CLI's single --direct gate, shared by `hoist promote` and
// `hoist deploy`. It exists as one function rather than a copy per command because everything
// it enforces is an invariant (AGENTS.md §4.5, invariants 5 and 6) and a second copy is how
// those drift: the two commands must refuse a production env, an unconfigured repo, and a
// mismatched confirmation identically, or the weaker one becomes the way around the stronger.
//
// Returns 0 to proceed, or the exit code to return. cmdName prefixes every message so each
// command still speaks in its own name.
func checkDirectPreflight(cmdName string, eff effective, direct bool, confirmDirect, targetEnv string, stderr io.Writer) int {
	if !direct {
		// The non-direct branch still needs a forge identity.
		if eff.cfg == nil || eff.cfg.GitHub == "" {
			fmt.Fprintf(stderr, "%s: the selected repo has no github: owner/name configured; add repos[].github to the config file\n", cmdName)
			return exitUsage
		}
		return 0
	}
	// Direct mode's whole safety rests on knowing envs.production (AGENTS.md invariant
	// 6): a flags-only run has no such list, which would make internal/engine.
	// DirectCommitGateStep's ProductionEnvs empty and every env look non-production by
	// omission. Fail fast rather than silently treat "unconfigured" as "safe" (AGENTS.md
	// §8: never provide a fallback default for required configuration).
	if eff.cfg == nil {
		fmt.Fprintf(stderr, "%s: --direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)\n", cmdName)
		return exitUsage
	}
	// Direct mode still needs the same github: owner/name the non-direct branch below
	// requires — not for the forge (direct mode never opens one), but because
	// engine.DeriveID(eff.cfg.GitHub, plan) hashes it into the promotion's id, which names
	// the state path, the branch and the worktree directory. Two repos both configured
	// without github: that promote the same env+digest set would otherwise derive the
	// IDENTICAL id — a real identity collision (one repo's run could overwrite the
	// other's state file or remove the other's still-active worktree as "unregistered
	// stale"), not merely a cosmetic gap. A git-only, no-PR operation still needs a stable
	// repo identity for that reason alone.
	if eff.cfg.GitHub == "" {
		fmt.Fprintf(stderr, "%s: the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)\n", cmdName)
		return exitUsage
	}
	if confirmDirect == "" {
		fmt.Fprintf(stderr, "%s: --direct requires --confirm-direct=<env> too (the keypress-then-confirm shape AGENTS.md's M6 brief requires, at the CLI)\n", cmdName)
		return exitUsage
	}
	if confirmDirect != targetEnv {
		fmt.Fprintf(stderr, "%s: --confirm-direct=%q does not match the target env %q; repeat the exact target env to confirm\n", cmdName, confirmDirect, targetEnv)
		return exitUsage
	}
	// Round-N finding (Codex, P2): DirectCommitGateStep — internal/engine/direct.go's own
	// "sole enforcement point" for AGENTS.md invariant 5/6 — used to be constructed only
	// after BuildPlan and the all-no-op fast path further down this function, so a
	// --direct run against a production env whose plan happened to already be current
	// exited 0 claiming success ("already current") without the gate ever running, and a
	// resolution failure for a production target surfaced as an unrelated resolution
	// error instead of the required refusal — either way masking the refusal AGENTS.md
	// §4.5 promises "outright". Call the identical step here, first — before resolving
	// digests, building the plan, or reaching the no-op fast path — so a production
	// target is refused before anything else can mask or bypass it. Confirmed is always
	// true here: reaching this point already required --confirm-direct to equal --to
	// exactly, checked immediately above. Drive still runs this same step again below
	// once steps is built (unreachable through Drive whenever this refuses, per the
	// step's own doc comment) — this calls the one enforcement point twice, it does not
	// add a second one (AGENTS.md §8, layered checks: the deletion test).
	gate := engine.DirectCommitGateStep{ProductionEnvs: eff.cfg.Envs.Production, Confirmed: true}
	obs, err := gate.Observe(context.Background(), &engine.PromotionState{TargetEnv: targetEnv})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)
		return exitFailure
	}
	if obs.Blocked != "" {
		blocked := &engine.BlockedError{Step: engine.StepDirectGate, Reason: obs.Blocked}
		fmt.Fprintf(stderr, "%s: %s\n", cmdName, redact.Strings(blocked.Error()))
		return exitFailure
	}
	return 0
}
