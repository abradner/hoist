package engine

import (
	"context"
	"sync"
)

// landedGuard wraps a step that a promotion only needs BEFORE it lands — the ones that read its
// worktree (BranchedStep, CommittedStep) or its remote branch (PushedStep) — so that once the
// landing step is satisfied, the wrapped step is too, whatever has since happened to the
// worktree, the local branch or the remote branch.
//
// Without it, "done" depended on never tidying up. A direct promotion is observed through
// BranchedStep and CommittedStep with nothing to short-circuit them, so one whose worktree was
// gone read as stopped at Branched: the one-in-flight-per-target-env rule then refused every
// later promotion into that env. The PR path had a partial answer — ObserveAll, Status and
// DriveStatus probe MergedStep first — but DriveStatus's probe is gated on the recorded Phase
// being PAST the merge, so a promotion saved at exactly "merged" (killed between the merge and
// the first Argo step, or an Argo client that errored) walked from the top on resume: with its
// worktree gone it rebuilt one from the base tip and blocked in CommittedStep on whatever commit
// happened to be there, and with it present it re-pushed the branch the merge had just deleted.
//
// The landing step's own Observe is the self-contained proof (AGENTS.md §4.1): MergedStep and
// DirectPushedStep both answer from origin and the clone's object database, never the worktree.
//
// What it costs, counted (AGENTS.md §9 entry 15: a step that talks to the remote gets its calls
// counted). The wrapped step is asked FIRST and the landing step only when the wrapped one is
// not cleanly satisfied and a commit is on record that could have landed; and a walk asks the
// landing step on a guard's behalf at most ONCE, however many guarded steps need the answer,
// and not at all when the walker's own up-front probe of that step already came back
// unsatisfied or failed (landingWalk). So:
//
//   - a promotion still on its way: one extra landing observation in its life, at the push
//     step between the commit and the first push (PR path); none on the direct path;
//   - a landed PR-path promotion: none — ObserveAll and Status probe the merge and skip past
//     the guards, and DriveStatus either does the same or, at Phase "merged", pays one;
//   - a landed direct promotion whose worktree is gone: one per walk, on top of
//     DirectPushedStep's own turn;
//   - an origin that cannot be reached: none beyond what the walk already paid — a failed
//     probe is not asked again.
type landedGuard struct {
	inner   Step
	landing Step
}

// guardLanded wraps inner so it reads satisfied once landing does.
func guardLanded(inner, landing Step) Step { return landedGuard{inner: inner, landing: landing} }

// Name implements Step: the guard is invisible in phases, history and step rows.
func (l landedGuard) Name() StepName { return l.inner.Name() }

// Observe implements Step.
func (l landedGuard) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	obs, err := l.inner.Observe(ctx, s)
	if err == nil && cleanlySatisfied(obs) {
		return obs, nil
	}
	if l.landed(ctx, s) {
		return Observation{Satisfied: true, Detail: "already landed on origin/" + s.Base + "; no longer needed"}, nil
	}
	return obs, err
}

// Act implements Step.
func (l landedGuard) Act(ctx context.Context, s *PromotionState) error { return l.inner.Act(ctx, s) }

// landed asks the landing step about a COPY of s — a short-circuit must not be what records a
// merge sha or a pushed sha on the promotion; the landing step does that itself when the walk
// reaches it — and counts only an answer that is about this promotion's own recorded commit.
func (l landedGuard) landed(ctx context.Context, s *PromotionState) bool {
	if !LandingObservable(s) {
		return false
	}
	// One answer per walk: a walk guards up to three steps with the same landing step, and
	// the walker itself may already have asked it (see landingWalk).
	walk := landingWalkFrom(ctx)
	if landed, known := walk.get(); known {
		return landed
	}
	landed := l.observeLanded(ctx, s)
	walk.set(landed)
	return landed
}

func (l landedGuard) observeLanded(ctx context.Context, s *PromotionState) bool {
	cp := *s
	obs, err := l.landing.Observe(ctx, &cp)
	if err != nil || !cleanlySatisfied(obs) {
		return false
	}
	// A merged PR found by this promotion's branch name is not necessarily THIS run's: the id
	// is deterministic, so promoting the same digest set into the same env twice reuses the
	// branch name, and the forge still holds the first run's merged PR (#41). When the forge
	// says which commit that PR merged, it has to be the one on record here.
	if !cp.Direct && cp.PR != nil && cp.PR.HeadSHA != "" && cp.PR.HeadSHA != s.CommitSHA {
		return false
	}
	return true
}

// LandingObservable reports whether s records enough for a landing step's Observe to be asked
// about it at all: a commit, and — for a direct promotion, whose landing is judged by content —
// the edits and blobs to judge it by. DirectPushedStep.Observe assumes CommittedStep ran before
// it and filled those in; asked about a state where they are empty, its content check has
// nothing to compare and is vacuously "intact". Every caller that asks a landing step out of
// order (landedGuard, internal/service's cleanup) checks this first.
func LandingObservable(s *PromotionState) bool {
	if s.CommitSHA == "" {
		return false
	}
	if s.Direct {
		return len(s.ExpectedBlobs) > 0 && len(s.Edits) > 0
	}
	return true
}

func cleanlySatisfied(o Observation) bool { return o.Satisfied && !o.Waiting && o.Blocked == "" }

// landingWalk is one walk's memo of "has this promotion landed", shared through the context by
// the walker (ObserveAll, Status, DriveStatus) and every landedGuard in the step list it walks.
// Step lists are built once and walked many times (a Driver polls), so the memo cannot live on
// the guard; it lives for exactly one walk. A nil *landingWalk — a guard observed outside any
// walk — remembers nothing.
type landingWalk struct {
	mu     sync.Mutex
	known  bool
	landed bool
}

type landingWalkKey struct{}

// withLandingWalk starts a walk's memo.
func withLandingWalk(ctx context.Context) context.Context {
	return context.WithValue(ctx, landingWalkKey{}, &landingWalk{})
}

func landingWalkFrom(ctx context.Context) *landingWalk {
	w, _ := ctx.Value(landingWalkKey{}).(*landingWalk)
	return w
}

func (w *landingWalk) get() (landed, known bool) {
	if w == nil {
		return false, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.landed, w.known
}

func (w *landingWalk) set(landed bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.known, w.landed = true, landed
}

// probedNotLanded records that the walker's own up-front probe of the landing step did not come
// back cleanly satisfied — unsatisfied, waiting, blocked, or an error. The guards then take that
// as the walk's answer instead of asking again: a second forge lookup and a second fetch per
// state, which against an unreachable origin is a second failed connection per state.
func probedNotLanded(ctx context.Context) { landingWalkFrom(ctx).set(false) }
