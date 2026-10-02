package service

import (
	"context"
	"fmt"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
)

// FindInFlight looks for a promotion state other than skipID targeting repoFullName/targetEnv
// that engine.ObserveAll reports as not yet done — AGENTS.md §4.1's own re-observe rule, applied
// to "is there already a promotion running for this env" rather than trusted from the state
// file's own Phase or presence alone (invariant 5: one in-flight promotion per target env). found
// is nil when no conflicting in-flight promotion exists. An error re-observing a candidate is
// treated conservatively — reported rather than silently skipped — since a promotion this call
// can't verify is done must not be treated as safely finished.
//
// Deliberately observes only the git/forge core (through Merged for the PR path, through the
// push for a direct one — engine.ObserveSteps picks by the state's own mode, since a direct
// state can never satisfy the PR path's steps and would otherwise be in flight forever), not the
// full engine.AllSteps/AllDirectSteps — a considered call, not an oversight. Invariant 5 exists
// to prevent exactly one thing: two promotions racing to create separate branches/PRs/merges for
// the same target env (a real git/forge conflict). That risk is fully retired the moment a merge
// (or a direct push) lands — a second promotion for the same env gets its own id, its own branch
// and its own PR (§4.1's deterministic id is keyed on the image set), so nothing about this
// promotion's own Argo refresh/sync or rollout convergence can still collide with it. See
// AGENTS.md §9 entry 11 for why this needs a genuinely three-way landed verdict (intact/
// superseded/reverted), not a plain ancestry or blob check, to avoid wedging a real env for days
// (#165, #166) — that verdict lives in engine.observeLanded, called from inside the steps
// ObserveSteps builds here, not from this function itself.
//
// Moved unchanged from cmd/hoist/drive.go's findInFlight — g and f are now built from Deps
// rather than taken as parameters: repoFullName is always the SAME forge identity every step
// list built from prev would use, and s.Git()/s.ForgeFor give exactly that without a caller
// having to already hold one.
//
// Every candidate is observed through observeThroughScan: a finished promotion — the kind an
// env accumulates — is recognised from one listing of origin's branches and one fetch of the
// base shared across the whole scan, and anything not finished is observed again live. The
// snapshot is built here and dropped on return, so every call asks origin afresh.
func (s *Service) FindInFlight(ctx context.Context, repoFullName, targetEnv, skipID string) (*engine.PromotionState, engine.StepStatus, error) {
	live, scan := s.Git(), newScanGit(s.Git())
	f, err := s.ForgeFor(repoFullName)
	if err != nil {
		return nil, engine.StepStatus{}, err
	}
	states, err := s.deps.Store.List()
	if err != nil {
		return nil, engine.StepStatus{}, err
	}
	for _, prev := range states {
		if prev.ID == skipID || prev.RepoFullName != repoFullName || prev.TargetEnv != targetEnv {
			continue
		}
		done, last, oerr := observeThroughScan(prev, scan, live, func(g git.Git, st *engine.PromotionState) (bool, engine.StepStatus, error) {
			return engine.ObserveAll(ctx, engine.ObserveSteps(st, g, f, nil, nil, nil), st)
		})
		if oerr != nil {
			return prev, last, fmt.Errorf("checking whether promotion %s is still in flight: %w", prev.ID, oerr)
		}
		if !done {
			return prev, last, nil
		}
	}
	return nil, engine.StepStatus{}, nil
}

// claimTarget is the one-in-flight-per-target-env claim: scan → engine.ClaimInFlight (via
// FileStore.Claim, an atomic filesystem claim) → rescan while holding — preserved exactly from
// cmd/hoist/promote.go's buildPromotionForConfirm (hardening). id is this promotion's own
// deterministic id (already computed by the caller — engine.DeriveID), used only to skip itself
// in each scan, so a resumed/retried promotion for the SAME id/target never conflicts with
// itself.
//
// The scan above and the claim just acquired are not one atomic operation: a second confirm (a
// different id, targeting the same env) can finish its own scan before this process claimed
// anything, then pause; this process claims, drives all the way to its first durable state save,
// and releases the claim (the claim's job is done once the state file itself can be found by a
// future scan); only then does the second process resume and successfully claim the now-free
// slot, without ever repeating its scan — so it never sees the state file this process just
// wrote. Re-running the same scan now, while still holding the claim, closes that window:
// nothing else can win the claim while this check runs, and anything that raced into existence
// on disk between the first scan and now is caught here.
//
// On success, release must be called by the caller exactly once the returned state's first
// successful save lands (ClaimInFlight's own doc comment: the claim's job is done once a durable
// state file exists for a future findInFlight/ObserveAll scan to see) — never held for the whole
// promotion. On error, any claim this call acquired has already been released.
func (s *Service) claimTarget(ctx context.Context, repoFullName, targetEnv, id string) (release func(), err error) {
	if conflict, status, ferr := s.FindInFlight(ctx, repoFullName, targetEnv, id); ferr != nil {
		return nil, fmt.Errorf("checking whether another promotion is already in flight: %w", ferr)
	} else if conflict != nil {
		return nil, &InFlightConflictError{Conflict: conflict, Env: targetEnv, Status: status}
	}

	claimRelease, err := s.deps.Store.Claim(repoFullName, targetEnv, id)
	if err != nil {
		return nil, err
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

	if conflict, status, ferr := s.FindInFlight(ctx, repoFullName, targetEnv, id); ferr != nil {
		return nil, fmt.Errorf("checking whether another promotion is already in flight: %w", ferr)
	} else if conflict != nil {
		return nil, &InFlightConflictError{Conflict: conflict, Env: targetEnv, Status: status}
	}

	ok = true
	return releaseOnce, nil
}
