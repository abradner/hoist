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
			if !errors.As(err, &stepErr) || !retryableStep(stepErr.Step) {
				// Not a step this loop knows to be transient (Known bug classes: a 404/scope
				// hiccup on CIGreen's Checks or Approved's Comments/IsAllowedAuthor calls) — a
				// git/GitHub operation on an earlier step failing terminally (a rejected push, a
				// broken git binary) will not fix itself by waiting, so report it immediately
				// exactly as pre-M4 Drive callers did.
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

// retryableStep is CIGreen and Approved: the only two steps whose Observe calls out to a forge
// endpoint (Checks, Comments, IsAllowedAuthor) that can transiently 404 or scope-error without
// the underlying condition (CI status, an approval) actually being answerable yet. Every other
// step's error is terminal from this loop's point of view.
func retryableStep(step engine.StepName) bool {
	return step == engine.StepCIGreen || step == engine.StepApproved
}

// findInFlight looks for a promotion state other than skipID targeting repoFullName/targetEnv
// that engine.ObserveAll reports as not yet done — AGENTS.md §4.1's own re-observe rule, applied
// to "is there already a promotion running for this env" rather than trusted from the state
// file's own Phase or presence alone (invariant 5: one in-flight promotion per target env).
// found is nil when no conflicting in-flight promotion exists. An error re-observing a
// candidate is treated conservatively — reported rather than silently skipped — since a
// promotion this call can't verify is done must not be treated as safely finished.
func findInFlight(ctx context.Context, g git.Git, f forge.Forge, a argo.Argo, ro rollout.Rollout, repoFullName, targetEnv, skipID string) (found *engine.PromotionState, status engine.StepStatus, err error) {
	states, err := engine.ListStates()
	if err != nil {
		return nil, engine.StepStatus{}, err
	}
	for _, prev := range states {
		if prev.ID == skipID || prev.RepoFullName != repoFullName || prev.TargetEnv != targetEnv {
			continue
		}
		done, last, oerr := engine.ObserveAll(ctx, engine.AllSteps(g, f, a, ro, nil), prev)
		if oerr != nil {
			return prev, last, fmt.Errorf("checking whether promotion %s is still in flight: %w", prev.ID, oerr)
		}
		if !done {
			return prev, last, nil
		}
	}
	return nil, engine.StepStatus{}, nil
}
