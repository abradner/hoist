package service

import (
	"fmt"
	"strings"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/image"
)

// AlreadyCurrentError is StartPromotion's own no-op signal: every edit in the confirmed plan
// already carries what it would write, so there is nothing left to commit, push or open a PR
// for. It carries plain fields rather than one canonical message because the CLI's own wording
// (which names the deployed ref for a deploy) and the TUI's (which never has, for a deploy —
// see wiring.go's old buildStartPromotion) have never agreed word for word; unifying that text
// is out of this PR's scope (it is not one of the design's own listed divergences), so each
// caller still builds its own final line from these fields. Error() gives a reasonable default
// for a caller that only wants *a* message — the TUI's own notice line uses exactly it today.
type AlreadyCurrentError struct {
	SourceEnv, TargetEnv string
	// Deploy is true for a deploy plan (SourceEnv is always "" there); Ref is the one reference
	// that plan would have written, valid only when Deploy is true.
	Deploy bool
	Ref    image.Ref
}

func (e *AlreadyCurrentError) Error() string {
	if e.Deploy {
		return fmt.Sprintf("%s is already current; nothing to deploy", e.TargetEnv)
	}
	return fmt.Sprintf("%s -> %s is already current; nothing to promote", e.SourceEnv, e.TargetEnv)
}

// UsageError is a caller-fixable misconfiguration Preflight/StartPromotion refuse before ever
// touching git, the forge or the cluster — a missing repos[].github, or --direct without a
// configured repo at all (AGENTS.md §4.5/§8: never provide a fallback default for required
// configuration). Its own type, rather than a plain fmt.Errorf, so a caller (cmd/hoist) can map
// it to its own "usage" exit code without string-matching the message.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// InFlightConflictError is AGENTS.md invariant 5's own refusal: another promotion, other than
// the one being started, already targets the same repo/env and has not yet re-observed as done.
// Error() is claimTarget's/FindInFlight's own wording, moved unchanged from cmd/hoist/promote.go's
// inFlightConflictError.
type InFlightConflictError struct {
	Conflict *engine.PromotionState
	Env      string
	Status   engine.StepStatus
}

func (e *InFlightConflictError) Error() string {
	return fmt.Sprintf("promotion %s targeting %s is still in flight (at %s: %s); run `hoist resume %s` instead of starting a second one",
		e.Conflict.ID, e.Env, e.Status.Step, Detail(e.Status.Observation), e.Conflict.ID)
}

// NotFoundError is Find's own "no such promotion" (ID set) and FindInFlightForEnv's own "no
// in-flight promotion targets this env" (Env set) — one type for both, since both are the same
// shape of answer ("nothing matched") to the two different questions `hoist resume` can be
// asked (by id, or by --env). Exactly one of ID/Env is ever set. Error() reproduces
// cmd/hoist/resume.go's own pre-move wording verbatim, so a caller printing it unchanged (the
// CLI renderer) prints exactly what it always has.
type NotFoundError struct {
	ID  string
	Env string
}

func (e *NotFoundError) Error() string {
	if e.Env != "" {
		return fmt.Sprintf("no in-flight promotion targets %s", e.Env)
	}
	return fmt.Sprintf("no promotion %s found", e.ID)
}

// AmbiguousError is FindInFlightForEnv's own "more than one in-flight promotion targets this
// env" — AGENTS.md invariant 5 says there should never legitimately be two, so this is always a
// caller error (name one by id) rather than something FindInFlightForEnv resolves on its own.
// IDs is sorted before Error() is ever called, so the message is deterministic.
type AmbiguousError struct {
	Env string
	IDs []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%d in-flight promotions target %s (%s); name one by id", len(e.IDs), e.Env, strings.Join(e.IDs, ", "))
}

// UnconfirmedError is FindInFlightForEnv's own refusal to silently exclude a candidate it could
// not re-observe (a transient forge/git error, a repo removed from config since the promotion
// started): every candidate targeting Env that could not be confirmed either way is named in
// Errs (sorted before this error is built), rather than treated as "not in flight" — which could
// misleadingly report NotFoundError when the real reason is that the answer is simply unknown.
type UnconfirmedError struct {
	Env  string
	Errs []string
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("could not confirm whether %d candidate(s) for %s are in flight (never silently excluded): %s", len(e.Errs), e.Env, strings.Join(e.Errs, "; "))
}

// Detail picks whichever of Observation's message fields is set — Blocked takes precedence
// since it's the more actionable one when both could apply. Moved from cmd/hoist/promote.go's
// statusDetail, unchanged; exported because it is now shared by service's own inflight.go and by
// cmd/hoist's remaining (not-yet-moved) abandon.go/resume.go call sites.
func Detail(o engine.Observation) string {
	if o.Blocked != "" {
		return o.Blocked
	}
	return o.Detail
}
