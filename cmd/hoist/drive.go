package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// pollInterval picks the poll interval for whichever step Drive most recently stopped at
// (s.Phase, set by Drive itself before returning ErrWaiting — never trusted for anything but
// this, which only chooses how long to sleep before the next re-observe, not what is true).
// internal/config's poll section (AGENTS.md §4.9's one-place-for-defaults rule already fills
// these in via Normalize) is what AGENTS.md invariant 4 means by "per internal/config's poll
// section if one exists" — it already existed as of M3, unused until now.
func pollInterval(poll config.PollConfig, phase engine.StepName) time.Duration {
	switch phase {
	case engine.StepCIGreen:
		return time.Duration(poll.CI)
	case engine.StepApproved:
		return time.Duration(poll.Approval)
	case engine.StepArgoRefreshed, engine.StepArgoSynced:
		// Both wait on Argo CD's own reconcile loop (refresh landing, then sync/health
		// converging) — the same remote, so the same knob.
		return time.Duration(poll.Argo)
	case engine.StepRolledOut:
		return time.Duration(poll.Rollout)
	default:
		// Branched/Committed/Pushed/PROpened/Merged only ever wait on the interactive signing
		// prompt (handled separately via onWaiting) or a single merge/branch-delete retry — a
		// short, fixed interval is plenty; there is no config knob for it because there is
		// nothing to tune (AGENTS.md §4.9: a knob with no real use is a knob nobody needed).
		return 2 * time.Second
	}
}

// driveToCompletion runs engine.Drive repeatedly, sleeping pollInterval's answer between
// attempts, until it succeeds, hits a *engine.BlockedError, or ctx is done (AGENTS.md §4.9's
// poll.deadline, applied by the caller wrapping ctx with a timeout — never a constant this
// function invents on its own). AGENTS.md invariant 4: the actual waiting lives here, in the
// CLI driver's own loop calling Drive (which itself only calls Observe, never sleeps) —
// nothing about a Step's Act ever blocks on a poll interval.
//
// A plain error from Drive (not ErrWaiting, not *BlockedError) is treated as potentially
// transient — Known bug classes: a 404 or permissions hiccup on Checks/Comments must be
// retried, not read as a hard failure — logged once and retried after the poll interval, rather
// than aborting on the first hiccup; ctx cancellation is what actually bounds that retry loop.
func driveToCompletion(ctx context.Context, steps []engine.Step, s *engine.PromotionState, save func(*engine.PromotionState) error, poll config.PollConfig, stderr io.Writer) error {
	for {
		err := engine.Drive(ctx, steps, s, save)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, engine.ErrWaiting):
			// fall through to sleep below
		default:
			var blocked *engine.BlockedError
			if errors.As(err, &blocked) {
				return err
			}
			var stepErr *engine.StepError
			if !errors.As(err, &stepErr) || !retryableStep(stepErr.Step) || isNotFoundErr(stepErr.Err) {
				// Not a step this loop knows to be transient (Known bug classes: a 404/scope
				// hiccup on CIGreen's Checks or Approved's Comments/IsAllowedAuthor calls) — a
				// git/GitHub operation on an earlier step failing terminally (a rejected push, a
				// broken git binary) will not fix itself by waiting, so report it immediately
				// exactly as pre-M4 Drive callers did. isNotFoundErr closes a gap retryableStep's
				// own doc comment assumes doesn't exist: ArgoRefreshedStep/RolledOutStep's own
				// Observe already Blocks cleanly on a missing Application/Deployment (never
				// reaching here as a plain StepError at all), but their Act calls (Refresh, or
				// any future write) can independently discover the same absence — a race between
				// Observe and Act, an Application/Deployment deleted or moved in between — and
				// Act has no way to produce a *BlockedError itself (Drive always wraps an Act
				// error as a plain retryable-looking *StepError, engine.go's own Drive). Without
				// this check, that race silently retries every poll.argo/poll.rollout interval
				// instead of reporting Blocked immediately (Copilot review, PR #51).
				return err
			}
			fmt.Fprintf(stderr, "hoist: %s (retrying)\n", redact.Strings(err.Error()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval(poll, s.Phase)):
		}
	}
}

// retryableStep is CIGreen, Approved, and the three M5 polling steps (ArgoRefreshed, ArgoSynced,
// RolledOut): the steps whose Observe calls out to a remote (a forge endpoint for the first two;
// the Kubernetes API for the Argo/rollout adaptors) that can transiently 404, scope-error or
// connection-reset without the underlying condition (CI status, an approval, an Application's
// or Deployment's status) actually being answerable yet. The M5 steps are exactly the same shape
// of problem CIGreen/Approved were already carved out for (round-1 review finding: a single
// connection reset or API timeout reading an Argo Application or Deployment status must not exit
// `promote`/`resume` outright when poll.argo/poll.rollout exist precisely to keep trying) — a
// step-specific ErrNotFound is handled by the step itself (Blocked, not an error reaching here at
// all); only a genuinely transient error surfaces as a *StepError this function is asked about.
// Every other step's error is terminal from this loop's point of view.
// isNotFoundErr reports whether err is either Argo or rollout adaptor's own "the object is
// genuinely gone" sentinel — a structural condition no amount of waiting resolves, never a
// transient plumbing hiccup, regardless of which retryable step's Act call happened to surface
// it (see retryableStep's own caller for why this matters: Observe already treats this the same
// way, but Act has no way to produce a *BlockedError of its own).
func isNotFoundErr(err error) bool {
	return errors.Is(err, argo.ErrNotFound) || errors.Is(err, rollout.ErrNotFound)
}

func retryableStep(step engine.StepName) bool {
	switch step {
	case engine.StepCIGreen, engine.StepApproved,
		engine.StepArgoRefreshed, engine.StepArgoSynced, engine.StepRolledOut:
		return true
	default:
		return false
	}
}

// findInFlight looks for a promotion state other than skipID targeting repoFullName/targetEnv
// that engine.ObserveAll reports as not yet done — AGENTS.md §4.1's own re-observe rule, applied
// to "is there already a promotion running for this env" rather than trusted from the state
// file's own Phase or presence alone (invariant 5: one in-flight promotion per target env).
// found is nil when no conflicting in-flight promotion exists. An error re-observing a
// candidate is treated conservatively — reported rather than silently skipped — since a
// promotion this call can't verify is done must not be treated as safely finished.
//
// Deliberately observes only the git/forge core (through Merged for the PR path, through the
// push for a direct one — engine.ObserveSteps picks by the state's own mode, since a direct
// state can never satisfy the PR path's steps and would otherwise be in flight forever), not
// the full engine.AllSteps —
// this is a considered call, not an oversight. Invariant 5 exists to prevent exactly one thing:
// two promotions racing to create separate branches/PRs/merges for the same target env (a real
// git/forge conflict). That risk is fully retired the moment a merge lands — a second promotion
// for the same env gets its own id, its own branch and its own PR (§4.1's deterministic id is
// keyed on the image set, so a later promotion for the same env necessarily differs), so nothing
// about this promotion's own Argo refresh/sync or rollout convergence can still collide with it.
// Blocking a brand-new promotion until a prior one's rollout finishes converging would be a
// tightening with no matching risk to justify it — Argo/rollout convergence can run long (a slow
// or stuck Deployment), and there is no reason a legitimate follow-up promotion for the same env
// (e.g. a hotfix) should have to wait on it. `hoist promote`/`hoist resume` still drive every
// promotion through the full ten steps via AllSteps (below) — only this in-flight check stops
// short. See TestFindInFlightDoesNotBlockAfterMergeWithRolloutPending in inflight_test.go for the
// scenario this guards.
func findInFlight(ctx context.Context, g git.Git, f forge.Forge, repoFullName, targetEnv, skipID string) (found *engine.PromotionState, status engine.StepStatus, err error) {
	states, err := engine.ListStates()
	if err != nil {
		return nil, engine.StepStatus{}, err
	}
	for _, prev := range states {
		if prev.ID == skipID || prev.RepoFullName != repoFullName || prev.TargetEnv != targetEnv {
			continue
		}
		done, last, oerr := engine.ObserveAll(ctx, engine.ObserveSteps(prev, g, f, nil, nil, nil), prev)
		if oerr != nil {
			return prev, last, fmt.Errorf("checking whether promotion %s is still in flight: %w", prev.ID, oerr)
		}
		if !done {
			return prev, last, nil
		}
	}
	return nil, engine.StepStatus{}, nil
}
