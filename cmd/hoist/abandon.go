package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/app"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
)

// abandonPromotion retires promotion id for good: releases the state file and, if it opened a
// PR, closes it and deletes its remote branch. lines is one human-readable description per real
// action actually taken, in order (empty when nothing beyond the state file itself needed
// touching — a direct-mode promotion never opens a PR or pushes a branch to origin at all).
//
// Re-observes first and refuses outright if the promotion has already landed (merged, for the
// PR path; pushed, for direct mode) — abandoning is not a rollback: a landed promotion needs
// `hoist deploy` or a fresh promotion to undo, never a state-file delete. The observation is
// the identical git/forge-only core cmd/hoist/drive.go's findInFlight itself uses
// (engine.ObserveSteps with Argo/rollout both nil) — "has this promotion's own write happened"
// is exactly the question that answers, and it is also exactly the three-way
// intact/superseded/reverted judgment (AGENTS.md §4.1) DirectPushedStep/MergedStep already
// apply, so abandon inherits it rather than re-deriving a second opinion.
//
// No claim file is touched. engine/claim.go's own package doc explicitly forbids automatic
// reclaim by inspecting a claim's age (tried and removed three times) — and it would not even
// apply here: a promotion with a state file at all already released its own claim the moment
// that file was first saved (claim.go's documented lifecycle); the claim only ever guards the
// window before a state file exists.
//
// Shared by runAbandon (the CLI, which owns the --confirm-abandon gate — the equivalent
// confirmation on the TUI side is the flight screen's own X-then-huh.Confirm gesture, already
// answered by the time buildAbandonFunc's closure calls this) and buildAbandonFunc below
// (AGENTS.md §4.8: one core, two thin adapters, never two copies of the same write).
func abandonPromotion(ctx context.Context, cfg *config.Config, id string) ([]string, error) {
	states, err := engine.ListStates()
	if err != nil {
		return nil, err
	}
	var s *engine.PromotionState
	for _, st := range states {
		if st.ID == id {
			s = st
			break
		}
	}
	if s == nil {
		return nil, fmt.Errorf("no promotion %s found", id)
	}

	rc, ok := repoConfigFor(cfg, s.RepoFullName)
	if !ok {
		return nil, fmt.Errorf("%s: repo %s is not in the config file", s.ID, s.RepoFullName)
	}
	f, err := newForge(rc.GitHub)
	if err != nil {
		return nil, err
	}

	done, status, err := engine.ObserveAll(ctx, engine.ObserveSteps(s, newGit, f, nil, nil, nil), s)
	if err != nil {
		return nil, fmt.Errorf("checking whether %s has already landed: %w", id, err)
	}
	// `done` alone is not "has this landed": MergedStep.Observe (PR path) and DirectPushedStep
	// (direct path) both mutate s in place with the real landed sha the moment their own
	// merge/push is CONFIRMED — even when they go on to report Satisfied:false for something
	// that only ever happens AFTER landing (the PR path's own "merged as X; branch not yet
	// deleted" case: AGENTS.md §6.1 gotcha 7, `gh pr merge --delete-branch` failing in a
	// worktree AFTER a real merge already succeeded). Trusting `done` alone here let this
	// function "abandon" — delete the state file, discard the History — a promotion that had
	// genuinely merged to production, only because its own branch-delete cleanup hadn't
	// finished yet (round-2 adversarial review). s.LandedSHA() asks the direct, mode-agnostic
	// question this actually needs answered; `done` stays as a second, independent gate
	// (AGENTS.md §8 "layered checks" — deleting either must only ever change politeness, never
	// possibility, not that either one alone is known sufficient forever).
	if done || s.LandedSHA() != "" {
		return nil, fmt.Errorf("%s has already landed (%s); abandoning is not a rollback — use `hoist deploy` or a fresh promotion to undo it", id, statusDetail(status.Observation))
	}

	var lines []string
	// s.PR is never stale here, even on a retry after ClosePR succeeded but a LATER step in
	// this same function failed (DeleteRemoteBranch, say — nothing re-saves s in between, so
	// the reloaded state's own PR.Closed would otherwise still read false): ObserveAll above
	// always probes MergedStep first when it's in the step list (engine.go's own phaseIndex
	// short-circuit), and MergedStep.Observe's own findOwnPR unconditionally re-fetches the
	// live PR (GetPR, falling back to FindPR — steps_m4.go) and assigns it back to s.PR before
	// this ever runs, regardless of merged/closed state. A round-2 review raised this as a
	// possible re-close-on-retry gap and an earlier version of this fix added a second,
	// explicit GetPR call here to guard against it directly; traced and confirmed genuinely
	// redundant (TestAbandonRetryAfterBranchDeleteFailureDoesNotRecloseThePR passes with that
	// extra call removed) — removed rather than kept as an inaccurately-justified duplicate of
	// a check ObserveAll already performs (AGENTS.md principle 1).
	if s.PR != nil && !s.PR.Merged && !s.PR.Closed {
		if _, err := f.ClosePR(ctx, s.PR.Number); err != nil {
			return lines, fmt.Errorf("closing PR #%d: %w", s.PR.Number, err)
		}
		lines = append(lines, fmt.Sprintf("closed PR #%d", s.PR.Number))
	}
	// Direct mode never pushes s.Branch to origin at all (§6: "no separate branch left on
	// origin, no PR") — nothing to delete there, and no PR to close either (s.PR is always nil
	// for a direct promotion), so the two blocks here are both no-ops for it by construction;
	// this guard just skips the pointless remote call rather than relying on
	// DeleteRemoteBranch's own idempotency to make it harmless.
	if !s.Direct && s.Branch != "" {
		if err := newGit.DeleteRemoteBranch(ctx, s.CloneDir, "origin", s.Branch); err != nil {
			return lines, fmt.Errorf("deleting branch %s: %w", s.Branch, err)
		}
		lines = append(lines, "deleted branch "+s.Branch)
	}

	statePath, err := engine.StatePath(id)
	if err != nil {
		return lines, err
	}
	if err := engine.DeleteState(statePath); err != nil {
		return lines, fmt.Errorf("deleting state file: %w", err)
	}
	return lines, nil
}

// buildAbandonFunc wraps abandonPromotion as the plain function type the TUI's flight screen
// takes (app.AbandonFunc, AGENTS.md §4.8: the app package never sees pkg/git/pkg/forge or cfg
// itself) — installed via app.Model.WithAbandon in runTUI's own launcher wiring.
func buildAbandonFunc(cfg *config.Config) app.AbandonFunc {
	return func(ctx context.Context, id string) error {
		_, err := abandonPromotion(ctx, cfg, id)
		return err
	}
}

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	lines, err := abandonPromotion(ctx, cfg, id)
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
