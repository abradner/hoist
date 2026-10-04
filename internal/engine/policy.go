package engine

import (
	"errors"
	"time"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/rollout"
)

// PollIntervals is the plain-value slice of internal/config's poll section that drive policy
// actually needs. It lives here, not as a direct use of config.PollConfig, because
// internal/engine must not import internal/config (§4.3-adjacent: engine is imported by both
// cmd/hoist and internal/app/flight, and neither caller's own config shape belongs inside the
// package that computes policy from it) — each caller converts its own config type into this
// one at its own boundary.
type PollIntervals struct {
	CI, Approval, Argo, Rollout time.Duration
}

// PollInterval picks the configured poll interval for whichever step a drive loop most recently
// stopped at. CIGreen and Approved each read their own knob; ArgoRefreshed and ArgoSynced share
// one (both wait on Argo CD's own reconcile loop — refresh landing, then sync/health converging
// — so the same remote, the same knob); RolledOut reads its own. Every other step only ever
// waits on the interactive signing prompt or a single merge/branch-delete retry, so it has no
// configured knob and gets a fixed 2s default (a knob with no real use is a knob nobody needed).
// The configured value is returned unchanged, whatever it is — this function applies no floor
// and no fallback for a configured knob; a caller that needs one (a UI tick loop that must never
// spin on a zero interval) applies it itself, on top of this result, at its own boundary.
func PollInterval(p PollIntervals, phase StepName) time.Duration {
	switch phase {
	case StepCIGreen:
		return p.CI
	case StepApproved:
		return p.Approval
	case StepArgoRefreshed, StepArgoSynced:
		return p.Argo
	case StepRolledOut:
		return p.Rollout
	default:
		return 2 * time.Second
	}
}

// IsNotFound reports whether err is either Argo's or the rollout adaptor's own "the object is
// genuinely gone" sentinel — a structural condition no amount of waiting resolves, never a
// transient plumbing hiccup, regardless of which retryable step's Act call happened to surface
// it. A step-specific ErrNotFound is normally handled by the step's own Observe (Blocked, not an
// error reaching a drive loop's retry classifier at all); this exists for the race where Act
// itself discovers the same absence (an Application or Deployment deleted or moved between
// Observe and Act), which Act has no way to turn into a *BlockedError of its own.
func IsNotFound(err error) bool {
	return errors.Is(err, argo.ErrNotFound) || errors.Is(err, rollout.ErrNotFound)
}

// RetryableStep is CIGreen, Approved, and the three M5 polling steps (ArgoRefreshed,
// ArgoSynced, RolledOut): the steps whose Observe calls out to a remote (a forge endpoint for
// the first two; the Kubernetes API for the Argo/rollout adaptors) that can transiently 404,
// scope-error or connection-reset without the underlying condition (CI status, an approval, an
// Application's or Deployment's status) actually being answerable yet. Every other step's error
// is terminal from a drive loop's point of view.
func RetryableStep(step StepName) bool {
	switch step {
	case StepCIGreen, StepApproved, StepArgoRefreshed, StepArgoSynced, StepRolledOut:
		return true
	default:
		return false
	}
}

// Retryable is the full retry decision a drive loop makes on an error Drive (or Status) returns:
// true for a *LandingUnknownError, and otherwise only for a *StepError naming a RetryableStep
// whose underlying error is not one of IsNotFound's structural sentinels. Anything else — an error that isn't a *StepError at all, a
// *StepError on a step with no configured retry, ctx cancellation, a rejected push, a broken git
// binary — is terminal: waiting for it will not make it succeed, so a drive loop reports it
// immediately rather than polling until the deadline.
func Retryable(err error) bool {
	// A landed promotion whose landing could not be confirmed this poll: a failed lookup while
	// waiting on Argo or the rollout, whatever step's guard reported it. The wait goes on.
	var unknown *LandingUnknownError
	if errors.As(err, &unknown) {
		return !IsNotFound(unknown.Err)
	}
	var stepErr *StepError
	if !errors.As(err, &stepErr) || !RetryableStep(stepErr.Step) {
		return false
	}
	return !IsNotFound(stepErr.Err)
}
