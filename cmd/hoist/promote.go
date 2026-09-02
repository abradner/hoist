package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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

// runPromote is `hoist promote`: builds the same gitops.Plan `hoist plan --dry-run` would
// print, then drives internal/engine's four steps to actually commit it, push it and open a
// PR. Named "promote" rather than "push": the command's whole point is the promotion — commit
// + push + PR together — and "push" would read as only the git step, one of four this command
// actually performs.
// checkNoOpAgainstBase guards the all-NoOp fast path: it re-derives, for every distinct file
// the plan's edits reference, whether the clone (cloneDir, no worktree exists yet at this
// point) is dirty relative to base for that file — the clone's current on-disk blob (computed
// with the same HashObject the engine uses for ExpectedBlobs) must match the blob committed
// at base (read with the same LsTreeBlob the engine uses to Observe HEAD). A mismatch means an
// uncommitted local change to a file this promotion cares about, which makes "all no-op"
// unverifiable — refuse by naming the file(s) rather than reporting a false success.
func checkNoOpAgainstBase(ctx context.Context, g git.Git, cloneDir, base string, edits []gitops.Edit) error {
	seen := map[string]bool{}
	var files []string
	for _, e := range edits {
		if !seen[e.File] {
			seen[e.File] = true
			files = append(files, e.File)
		}
	}
	sort.Strings(files)

	var dirty []string
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
		baseBlob, ok, err := g.LsTreeBlob(ctx, cloneDir, base, f)
		if err != nil {
			return err
		}
		if !ok || baseBlob != curBlob {
			dirty = append(dirty, f)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("the plan looks already current, but %s has uncommitted local changes not yet in %q for: %s — a no-op computed against dirty content can't be trusted; commit, stash or discard the local changes and re-run", cloneDir, base, strings.Join(dirty, ", "))
	}
	return nil
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
	if *direct {
		// Direct mode's whole safety rests on knowing envs.production (AGENTS.md invariant
		// 6): a flags-only run has no such list, which would make internal/engine.
		// DirectCommitGateStep's ProductionEnvs empty and every env look non-production by
		// omission. Fail fast rather than silently treat "unconfigured" as "safe" (AGENTS.md
		// §8: never provide a fallback default for required configuration).
		if eff.cfg == nil {
			fmt.Fprintln(stderr, "hoist promote: --direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)")
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
			fmt.Fprintln(stderr, "hoist promote: the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)")
			return exitUsage
		}
		if *confirmDirect == "" {
			fmt.Fprintln(stderr, "hoist promote: --direct requires --confirm-direct=<env> too (the keypress-then-confirm shape AGENTS.md's M6 brief requires, at the CLI)")
			return exitUsage
		}
		if *confirmDirect != *to {
			fmt.Fprintf(stderr, "hoist promote: --confirm-direct=%q does not match --to=%q; repeat the exact target env to confirm\n", *confirmDirect, *to)
			return exitUsage
		}
	} else if eff.cfg == nil || eff.cfg.GitHub == "" {
		fmt.Fprintln(stderr, "hoist promote: the selected repo has no github: owner/name configured; add repos[].github to the config file")
		return exitUsage
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

	changed := false
	for _, e := range plan.Edits {
		if !e.NoOp() {
			changed = true
			break
		}
	}
	if !changed {
		// An all-NoOp plan means "the target already carries the planned ref" — but Edit.NoOp
		// only knows about the bytes gitops.Discover already read from the clone. Nothing
		// downstream of here calls gitops.Apply/Verify (there is no worktree yet on this
		// path), so this is the one place a dirty clone can otherwise slip through: if the
		// user has an uncommitted local edit to a file the plan touches, the plan can compute
		// all-NoOp against that dirty content while it would NOT be all-NoOp against what's
		// actually committed at --base. Confirm the clone isn't dirty for those paths before
		// trusting the conclusion (AGENTS.md §4.2, "Verify runs before git add, always").
		if err := checkNoOpAgainstBase(context.Background(), newGit, eff.repo, *base, plan.Edits); err != nil {
			fmt.Fprintf(stderr, "hoist promote: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "hoist promote: %s -> %s is already current; nothing to promote.\n", plan.SourceEnv, plan.TargetEnv)
		return 0
	}

	id := engine.DeriveID(eff.cfg.GitHub, plan)
	branch := engine.BranchName(plan.TargetEnv, id)
	worktreeDir, err := engine.WorktreeDir(id)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}

	f, err := newForge(eff.cfg.GitHub)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	// Invariant 5: one in-flight promotion per target env. A different image set for the same
	// target env gets a different id (§4.1), so this can only find *another* promotion's state
	// file — re-observed fresh, never trusted from its own Phase (findInFlight/ObserveAll).
	if conflict, status, ferr := findInFlight(ctx, newGit, f, eff.cfg.GitHub, plan.TargetEnv, id); ferr != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(ferr.Error()))
		return exitFailure
	} else if conflict != nil {
		fmt.Fprintf(stderr, "hoist promote: promotion %s targeting %s is still in flight (at %s: %s); run `hoist resume %s` instead of starting a second one\n",
			conflict.ID, plan.TargetEnv, status.Step, statusDetail(status.Observation), conflict.ID)
		return exitFailure
	}

	// findInFlight above is read-only (it only ever looks at state files already on disk), so a
	// second `hoist promote` racing this one for the same target env could pass that check too,
	// before either process has written its own state file — engine.ClaimInFlight closes that
	// window with an atomic filesystem claim; a concurrent loser gets a clear conflict error here
	// rather than silently proceeding to open a second branch/PR for the same env. released
	// tracks whether it's already been let go so the deferred cleanup below is a no-op once the
	// first successful state save has released it for real (see the save wrapper further down).
	release, err := engine.ClaimInFlight(eff.cfg.GitHub, plan.TargetEnv, id)
	if err != nil {
		fmt.Fprintf(stderr, "hoist promote: %v\n", err)
		return exitFailure
	}
	released := false
	defer func() {
		if !released {
			released = true
			release()
		}
	}()

	// The scan above and the claim just acquired are not one atomic operation: a second
	// `hoist promote` (a different id, targeting the same env) can finish its own scan before
	// this process claimed anything, then pause; this process claims, drives all the way to its
	// first durable state save, and releases the claim (the claim's job is done once the state
	// file itself can be found by a future scan); only then does the second process resume and
	// successfully claim the now-free slot, without ever repeating its scan — so it never sees
	// the state file this process just wrote. Re-running the same scan now, while still holding
	// the claim, closes that window: nothing else can win the claim while this check runs, and
	// anything that raced into existence on disk between the first scan and now is caught here.
	if conflict, status, ferr := findInFlight(ctx, newGit, f, eff.cfg.GitHub, plan.TargetEnv, id); ferr != nil {
		fmt.Fprintf(stderr, "hoist promote: %s\n", redact.Strings(ferr.Error()))
		return exitFailure
	} else if conflict != nil {
		fmt.Fprintf(stderr, "hoist promote: promotion %s targeting %s is still in flight (at %s: %s); run `hoist resume %s` instead of starting a second one\n",
			conflict.ID, plan.TargetEnv, status.Step, statusDetail(status.Observation), conflict.ID)
		return exitFailure
	}

	s := &engine.PromotionState{
		ID:             id,
		RepoFullName:   eff.cfg.GitHub,
		SourceEnv:      plan.SourceEnv,
		TargetEnv:      plan.TargetEnv,
		Branch:         branch,
		CloneDir:       eff.repo,
		WorktreeDir:    worktreeDir,
		Base:           *base,
		Edits:          plan.Edits,
		CommitMessage:  engine.RenderCommitMessage(id, plan),
		PRTitle:        engine.PRTitle(plan),
		PRBody:         engine.RenderPRBody(id, plan),
		CINone:         eff.cfg.CI.None,
		CIGrace:        time.Duration(eff.cfg.CI.Grace),
		CINoneOverride: *overrideCINone,
		Approval:       eff.cfg.Approval(plan.TargetEnv),
		Approvers:      eff.cfg.Approvers,
		Collaborators:  eff.cfg.Collaborators,
	}
	// The state file is an index of what to look at, never evidence of what happened
	// (AGENTS.md §4.1) — every Observe below re-derives truth from the worktree and the
	// remote regardless of what's loaded here. Carrying History forward is purely so
	// `hoist promote`'s own output can show it; a missing or unreadable file never blocks
	// the run. An override flag given on a later re-run always wins over what a previous run
	// persisted, since the operator just asked for it again.
	if prev, err := engine.LoadState(statePath); err == nil && prev != nil && prev.ID == id {
		s.History = prev.History
		// Policy fields (CINone/CIGrace/Approval/Approvers/Collaborators), set above from
		// eff.cfg, are overwritten here with what the existing state file already persisted —
		// mirroring runResume's own fix (cmd/hoist/resume.go, commit f3b1c53) for the identical
		// bug at this sibling call site: PromotionState's doc comment (internal/engine/state.go)
		// states these are policy "as of when this promotion started", carried forward so a
		// promotion never straddles two different policies mid-flight. Re-invoking `hoist
		// promote` for an id that already has a state file is still a resume of THAT promotion,
		// not a new one — leaving these fresh from current config would let an operator's
		// mid-flight config edit retroactively change what policy an already-started promotion
		// enforces, exactly the bug runResume was fixed for.
		s.CINone = prev.CINone
		s.CIGrace = prev.CIGrace
		s.Approval = prev.Approval
		s.Approvers = prev.Approvers
		s.Collaborators = prev.Collaborators
		if !*overrideCINone {
			s.CINoneOverride = prev.CINoneOverride
		}
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
		steps = engine.DirectSteps(newGit, eff.cfg.Envs.Production, true, onWaiting)
	} else {
		steps = engine.AllSteps(newGit, f, onWaiting)
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
		fmt.Fprintf(stdout, "%s: %s -> %s\n", cmdName, sourceEnv, targetEnv)
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
