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
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// buildArgoRollout constructs this run's Argo/Rollout adaptors from rc's kube context —
// exactly the pair `hoist promote` builds alongside its forge client (promote.go), needed here
// too since runPromotions/runResume also drive/observe AllSteps, which always wires all ten
// steps regardless of which one a given promotion currently sits at.
func buildArgoRollout(rc config.RepoConfig) (argo.Argo, rollout.Rollout, error) {
	a, _, err := newArgo(rc.Kube.Context)
	if err != nil {
		return nil, nil, err
	}
	ro, _, err := newRollout(rc.Kube.Context)
	if err != nil {
		return nil, nil, err
	}
	return a, ro, nil
}

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

// ensureArgoApps repairs a state file written before M5 added PromotionState.ArgoApps: JSON
// decoding an older file leaves the field empty (round-1 review finding), and
// ArgoRefreshedStep/ArgoSyncedStep both take their `len(apps) == 0` "no Argo Application in this
// promotion's plan" success path on an empty ArgoApps — so an upgraded, already-in-flight
// promotion could be reported complete having never actually checked the Application it edited.
//
// An empty ArgoApps alongside a non-empty Edits is unambiguous evidence of exactly that: engine.
// ArgoAppNames' own contract (see its doc comment) means a real call against a non-empty edit set
// can never itself return an empty, non-error slice — every edit's directory maps to exactly one
// family/Application, or the call errors naming the orphan edit. So this only ever fires for a
// state genuinely predating ArgoApps' introduction, never for a fresh, post-M5 promotion that
// legitimately touches no Argo Application (which also has no Edits at all, since BuildPlan
// produces edits only from what an env's own families declare).
//
// s.ArgoApps is otherwise left untouched — state.go's own doc comment ("computed once ... then
// carried unchanged across every resume") still governs every other case, matching Edits/
// CommitMessage/PRTitle/PRBody's own carried-not-recomputed treatment.
func ensureArgoApps(s *engine.PromotionState, rc config.RepoConfig) error {
	if len(s.ArgoApps) > 0 || len(s.Edits) == 0 {
		return nil
	}
	r, err := gitops.Discover(s.CloneDir, rc.AppsRoot)
	if err != nil {
		return fmt.Errorf("rebuilding Argo Applications for a pre-M5 state file: %w", err)
	}
	apps, err := engine.ArgoAppNames(r, s.TargetEnv, s.Edits)
	if err != nil {
		return fmt.Errorf("rebuilding Argo Applications for a pre-M5 state file: %w", err)
	}
	s.ArgoApps = apps
	return nil
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
	// Bounded the same way promote/resume's own drive path already is (round-6 finding): each
	// candidate's re-observation talks to a real forge/git, and a hung call here had no bound
	// at all beyond an interrupt — `hoist promotions` could stall forever on one bad candidate
	// instead of listing the rest.
	if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
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
		a, ro, err := buildArgoRollout(rc)
		if err != nil {
			fmt.Fprintf(stdout, "%s  %-20s  ? (could not build an Argo/rollout client: %s)\n", s.ID, s.TargetEnv, redact.Strings(err.Error()))
			continue
		}
		if err := ensureArgoApps(s, rc); err != nil {
			fmt.Fprintf(stdout, "%s  %-20s  ? (%v)\n", s.ID, s.TargetEnv, err)
			continue
		}
		done, status, err := engine.ObserveAll(ctx, engine.AllSteps(newGit, f, a, ro, nil), s)
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
		// Bounded the same way runPromotions' own re-observe loop already is (round-6 finding,
		// applied there but missed here): a hung forge/git call on one candidate must not stall
		// `hoist resume --env` forever before it even selects an id.
		if deadline := time.Duration(cfg.Poll.Deadline); deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, deadline)
			defer cancel()
		}
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
			a, ro, ferr := buildArgoRollout(rc)
			if ferr != nil {
				obsErrs = append(obsErrs, fmt.Sprintf("%s: building Argo/rollout clients: %v", st.ID, ferr))
				continue
			}
			if ferr := ensureArgoApps(st, rc); ferr != nil {
				obsErrs = append(obsErrs, fmt.Sprintf("%s: %v", st.ID, ferr))
				continue
			}
			done, _, oerr := engine.ObserveAll(ctx, engine.AllSteps(newGit, f, a, ro, nil), st)
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
	a, ro, err := buildArgoRollout(rc)
	if err != nil {
		fmt.Fprintf(stderr, "hoist resume: %s\n", redact.Strings(err.Error()))
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
	//
	// ArgoNamespace is different in kind, not just carried along by analogy: it doesn't gate a
	// decision against historical events the way the M4 fields above do (a re-read there could
	// silently re-judge an already-recorded approval/CI comment against a changed policy), it
	// only names where a *live* Get for this env's Argo Applications lands. If an operator moves
	// those Applications to a different namespace while a promotion is mid-flight, re-reading it
	// here means ArgoRefreshedStep/ArgoSyncedStep keep finding them; a stale value would instead
	// fail loudly (Application not found) rather than misjudge anything quietly. So, unlike the
	// M4 fields, it's re-read from the current config on every resume, same as a fresh `hoist
	// promote` would compute it (AGENTS.md §4.9). ArgoApps does not follow either rule either — it
	// is a structural fact about the plan already committed to, exactly like Edits, and this
	// function does not recompute it for a state that already carries it. The one exception is
	// ensureArgoApps just below: a state file written before M5 added the field never had a
	// chance to carry it at all, which is a gap in "already committed to", not an instance of it
	// (round-1 review finding — see ensureArgoApps' own doc comment).
	s.ArgoNamespace = rc.Kube.ArgoNamespace
	if *overrideCINone {
		s.CINoneOverride = true
	}
	if err := ensureArgoApps(s, rc); err != nil {
		fmt.Fprintf(stderr, "hoist resume: %v\n", err)
		return exitFailure
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
	steps := engine.AllSteps(newGit, f, a, ro, onWaiting)
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
