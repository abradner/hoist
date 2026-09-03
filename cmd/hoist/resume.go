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
)

// repoConfigFor finds cfg's repos[] entry whose GitHub name matches repoFullName — how both
// runPromotions and runResume locate the credential/CloneDir context a stored PromotionState
// doesn't carry a config reference for (state files are repo-agnostic beyond RepoFullName
// itself, on purpose — AGENTS.md §4.1: the state file is an index, not a second copy of config).
func repoConfigFor(cfg *config.Config, repoFullName string) (config.RepoConfig, bool) {
	for _, r := range cfg.Repos {
		if r.GitHub == repoFullName {
			return r, true
		}
	}
	return config.RepoConfig{}, false
}

// runPromotions is `hoist promotions`: lists every promotion state file under
// $XDG_STATE_HOME/hoist/promotions/, with phase-as-observed (re-observed against the forge and
// worktree, never the state file's own possibly-stale Phase field — AGENTS.md §4.1). A
// promotion whose repo is no longer in the config file is listed with its last-recorded phase
// and a note, since there is nothing to re-observe it against.
func runPromotions(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist promotions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	states, err := engine.ListStates()
	if err != nil {
		fmt.Fprintf(stderr, "hoist promotions: %v\n", err)
		return exitFailure
	}
	if len(states) == 0 {
		fmt.Fprintln(stdout, "hoist promotions: no promotions found")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for _, s := range states {
		rc, ok := repoConfigFor(cfg, s.RepoFullName)
		if !ok {
			fmt.Fprintf(stdout, "%s  %-20s  %s (last recorded; repo %s is not in the config file, cannot re-observe)\n", s.ID, s.TargetEnv, s.Phase, s.RepoFullName)
			continue
		}
		f, err := newForge(rc.GitHub)
		if err != nil {
			fmt.Fprintf(stdout, "%s  %-20s  ? (could not build a forge client: %v)\n", s.ID, s.TargetEnv, err)
			continue
		}
		done, status, err := engine.ObserveAll(ctx, engine.AllSteps(newGit, f, nil), s)
		switch {
		case err != nil:
			fmt.Fprintf(stdout, "%s  %-20s  ? (%v)\n", s.ID, s.TargetEnv, err)
		case done:
			fmt.Fprintf(stdout, "%s  %-20s  done (%s)\n", s.ID, s.TargetEnv, statusDetail(status.Observation))
		default:
			fmt.Fprintf(stdout, "%s  %-20s  %s: %s\n", s.ID, s.TargetEnv, status.Step, statusDetail(status.Observation))
		}
	}
	return 0
}

// runResume is `hoist resume`: re-drives a specific promotion, identified either positionally
// by id or by --env <target-env> (erroring if more than one non-terminal promotion matches that
// env — ambiguous, and AGENTS.md invariant 5 says there should never legitimately be two).
// Re-drives through AllSteps exactly like `hoist promote`, from wherever Observe actually finds
// it — never from the recorded Phase.
func runResume(args []string, cfg *config.Config, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hoist resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	env := fs.String("env", "", "resume the (single, non-terminal) promotion targeting this env, instead of naming an id")
	overrideCINone := fs.Bool("override-ci-none", false, "when ci.none is prompt, treat a PR with no reported checks as passing after the grace period anyway")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	id := fs.Arg(0)
	if (id == "") == (*env == "") {
		fmt.Fprintln(stderr, "hoist resume: give exactly one of <id> or --env <target-env>")
		fs.Usage()
		return exitUsage
	}

	states, err := engine.ListStates()
	if err != nil {
		fmt.Fprintf(stderr, "hoist resume: %v\n", err)
		return exitFailure
	}

	var s *engine.PromotionState
	if id != "" {
		for _, st := range states {
			if st.ID == id {
				s = st
				break
			}
		}
		if s == nil {
			fmt.Fprintf(stderr, "hoist resume: no promotion %s found\n", id)
			return exitFailure
		}
	} else {
		var matches []*engine.PromotionState
		// obsErrs collects a re-observation failure per candidate instead of silently filtering
		// it out of consideration (a transient GitHub/git error must never be indistinguishable
		// from "this candidate simply isn't in flight" — that could misleadingly report "no
		// in-flight promotion" with one candidate, or silently resolve to a different one with
		// several, without ever confirming the choice was actually unambiguous).
		var obsErrs []string
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		for _, st := range states {
			if st.TargetEnv != *env {
				continue
			}
			rc, ok := repoConfigFor(cfg, st.RepoFullName)
			if !ok {
				// A state naming a repo no longer in config (removed, or renamed since this
				// promotion started) is exactly the same "can't confirm" case obsErrs already
				// exists for below — silently skipping it here would let `resume --env`
				// misleadingly report "no in-flight promotion" (one candidate) or resolve to a
				// different match without ever establishing that choice was unambiguous
				// (several candidates, one of which is unconfirmable).
				obsErrs = append(obsErrs, fmt.Sprintf("%s: repo %q is not in the current config; restore it or name this promotion's id explicitly", st.ID, st.RepoFullName))
				continue
			}
			f, ferr := newForge(rc.GitHub)
			if ferr != nil {
				obsErrs = append(obsErrs, fmt.Sprintf("%s: building a forge client: %v", st.ID, ferr))
				continue
			}
			done, _, oerr := engine.ObserveAll(ctx, engine.AllSteps(newGit, f, nil), st)
			if oerr != nil {
				obsErrs = append(obsErrs, fmt.Sprintf("%s: %v", st.ID, oerr))
				continue
			}
			if !done {
				matches = append(matches, st)
			}
		}
		stop()
		if len(obsErrs) > 0 {
			sort.Strings(obsErrs)
			fmt.Fprintf(stderr, "hoist resume: could not confirm whether %d candidate(s) for %s are in flight (never silently excluded): %s\n", len(obsErrs), *env, strings.Join(obsErrs, "; "))
			return exitFailure
		}
		switch len(matches) {
		case 0:
			fmt.Fprintf(stderr, "hoist resume: no in-flight promotion targets %s\n", *env)
			return exitFailure
		case 1:
			s = matches[0]
		default:
			var ids []string
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			sort.Strings(ids)
			fmt.Fprintf(stderr, "hoist resume: %d in-flight promotions target %s (%s); name one by id\n", len(matches), *env, joinIDs(ids))
			return exitUsage
		}
	}

	rc, ok := repoConfigFor(cfg, s.RepoFullName)
	if !ok {
		fmt.Fprintf(stderr, "hoist resume: %s: repo %s is not in the config file\n", s.ID, s.RepoFullName)
		return exitFailure
	}
	f, err := newForge(rc.GitHub)
	if err != nil {
		fmt.Fprintf(stderr, "hoist resume: %v\n", err)
		return exitFailure
	}

	// s.CINone/CIGrace/Approval/Approvers/Collaborators are deliberately NOT re-read from rc
	// here: PromotionState's own doc comment (internal/engine/state.go) states the invariant
	// that these are policy "as of when this promotion started", carried forward so "a promotion
	// never straddles two different policies mid-flight" — re-reading them from the current
	// config file on every resume would let an operator's mid-flight config edit do exactly that
	// (change what CI/approval policy this specific promotion enforces after it has already
	// started), which is the bug this comment replaces a fix for. The persisted state file's
	// values (loaded by engine.ListStates/LoadState above) are trusted as-is. Only
	// --override-ci-none is an explicit, one-shot operator instruction for *this* invocation, so
	// it still always wins over whatever was persisted.
	if *overrideCINone {
		s.CINoneOverride = true
	}

	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		fmt.Fprintf(stderr, "hoist resume: %v\n", err)
		return exitFailure
	}
	save := func(st *engine.PromotionState) error { return engine.SaveState(statePath, st) }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	waited := false
	onWaiting := func() {
		if !waited {
			waited = true
			fmt.Fprintln(stderr, "hoist resume: waiting for signing approval...")
		}
	}
	steps := engine.AllSteps(newGit, f, onWaiting)
	err = driveToCompletion(ctx, steps, s, save, cfg.Poll, stderr)
	return reportDriveResult(stdout, stderr, "hoist resume", s.SourceEnv, s.TargetEnv, s, err)
}

func joinIDs(ids []string) string {
	out := ids[0]
	for _, id := range ids[1:] {
		out += ", " + id
	}
	return out
}
