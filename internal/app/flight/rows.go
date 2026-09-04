// Package flight is the flight screen: shown once a promotion starts driving through
// engine.AllSteps (branch, commit, push, PR, CI green, approved, merged, then (M5) Argo
// refresh, Argo sync, rollout), it lists every step with a glyph for its current state,
// the active step's own human detail text, a
// stopwatch since the promotion started, and a togglable scrollback of
// PromotionState.History. rows.go derives the step rows from a PromotionState and the
// ordered per-step statuses engine.Status produces, with no terminal dependency (AGENTS.md
// §4.8); model.go lays that data out.
package flight

import (
	"time"

	"github.com/abradner/hoist/internal/engine"
)

// The glyph set every Row.Glyph renders as: done, active (Drive/Status is currently sitting
// on this one, with nothing external to wait on — it either just acted or is about to),
// waiting (active, but blocked on something external: CI running, an approval comment not
// posted yet), blocked (a real conflict, terminal until an operator resolves it), and not
// yet reached (every step after the active one — Status never got far enough to observe it
// this pass, or did not need to, because of its own short-circuit; see engine.Status's doc
// comment).
const (
	GlyphDone       = "✓"
	GlyphActive     = "▶"
	GlyphWaiting    = "…"
	GlyphBlocked    = "✗"
	GlyphNotReached = "·"
)

// StepOrder is the fixed order flight renders steps in — engine.AllSteps' own order,
// branch/commit/push/PR then CI green/approved/merged, then (M5) Argo refresh/sync and
// rollout. It is a plain literal rather than derived from engine.AllSteps(nil, nil, nil,
// nil, nil) at runtime (technically possible: nil satisfies every interface parameter
// without this package importing pkg/git/pkg/forge/pkg/argo/pkg/rollout, and every
// Step.Name() implementation ignores its receiver's fields) because a literal is more
// legible here and does not depend on every future Step implementation continuing to
// ignore its own fields in Name(). TestStepOrderMatchesAllSteps in model_test.go is what
// keeps this from silently drifting if AllSteps' own order ever changes.
var StepOrder = []engine.StepName{
	engine.StepBranched,
	engine.StepCommitted,
	engine.StepPushed,
	engine.StepPROpened,
	engine.StepCIGreen,
	engine.StepApproved,
	engine.StepMerged,
	engine.StepArgoRefreshed,
	engine.StepArgoSynced,
	engine.StepRolledOut,
}

// stepLabels is the human-readable name shown for each step in the list — short, so the
// glyph column stays aligned regardless of terminal width.
var stepLabels = map[engine.StepName]string{
	engine.StepBranched:      "branch",
	engine.StepCommitted:     "commit",
	engine.StepPushed:        "push",
	engine.StepPROpened:      "PR",
	engine.StepCIGreen:       "CI",
	engine.StepApproved:      "approval",
	engine.StepMerged:        "merge",
	engine.StepArgoRefreshed: "argo refresh",
	engine.StepArgoSynced:    "argo sync",
	engine.StepRolledOut:     "rollout",
}

// Label is the human-readable name for a step; falls back to the raw StepName for anything
// stepLabels doesn't know (defensive — every step engine.AllSteps returns today is listed
// above, and TestStepOrderMatchesAllSteps would catch a new one silently falling back).
func Label(name engine.StepName) string {
	if l, ok := stepLabels[name]; ok {
		return l
	}
	return string(name)
}

// Row is one step in the flight list, derived with no terminal dependency.
type Row struct {
	Step  engine.StepName
	Glyph string
	// Active is true for exactly the one row DeriveRows considers "current" — the step
	// Status stopped at (Blocked, Waiting, or plainly not yet Satisfied). Never true when
	// done is true: every row is Done then.
	Active bool
	// Detail is the human string shown under an active row: Observation.Detail for a
	// waiting or not-yet-acted step, Observation.Blocked for a blocked one. It is never
	// invented here — always exactly what the engine step itself produced (AGENTS.md §4.1).
	// It is NOT already redacted by the time it reaches this package: engine.Status calls
	// each Step's own Observe directly and returns its Observation as-is (engine.go's
	// doc comment on Status/ObserveAll) — it never goes through engine.go's appendHistory,
	// which only wraps Drive's own history-writing path, a different call this package's
	// data never travels through. No Step.Observe in internal/engine/steps*.go redacts its
	// own Detail/Blocked text either. The actual guarantee here is model.go's View(), which
	// passes its whole assembled output (this Detail included) through redact.Strings once
	// at the final boundary before anything reaches the terminal — the same
	// belt-and-suspenders convention as plan.Model's own View(). That is the one place, not
	// two, where this package's data is scrubbed; DeriveRows and this struct carry it
	// unredacted up to that point, same as engine.Status hands it over.
	Detail string
}

// DeriveRows turns (done, statuses) — engine.Status's own return shape for a promotion's
// steps, in engine.AllSteps' order — into the rows the step list renders, one per name in
// order.
//
// When done is true every row renders Glyph done: engine.Status's own short-circuit (see
// its doc comment) means statuses then carries only the final step's own StepStatus, not
// one per earlier step, so there is nothing to derive an individual glyph from for the
// steps before it — and nothing needs to be, since done already answers "is every step
// satisfied" for all of them at once. The last row's Detail is filled from that single
// entry (e.g. "merged as <sha>; branch deleted").
//
// Otherwise, order is walked once: a step present in statuses is Satisfied, Waiting,
// Blocked or "not yet acted on", read straight off its own Observation; a step absent from
// statuses has not been reached this pass (engine.Status stops at the first step that is
// not cleanly Satisfied, so nothing after it was observed) and renders not-yet-reached.
// Exactly one row is Active — whichever one Status actually stopped at — since every entry
// in statuses before the last one is, by construction, Satisfied.
func DeriveRows(order []engine.StepName, done bool, statuses []engine.StepStatus) []Row {
	if done {
		return doneRows(order, statuses)
	}
	byStep := make(map[engine.StepName]engine.StepStatus, len(statuses))
	for _, st := range statuses {
		byStep[st.Step] = st
	}
	rows := make([]Row, 0, len(order))
	for _, name := range order {
		rows = append(rows, deriveRow(name, byStep))
	}
	return rows
}

func deriveRow(name engine.StepName, byStep map[engine.StepName]engine.StepStatus) Row {
	st, observed := byStep[name]
	if !observed {
		return Row{Step: name, Glyph: GlyphNotReached}
	}
	switch {
	case st.Blocked != "":
		return Row{Step: name, Glyph: GlyphBlocked, Active: true, Detail: st.Blocked}
	case st.Waiting:
		return Row{Step: name, Glyph: GlyphWaiting, Active: true, Detail: st.Detail}
	case st.Satisfied:
		return Row{Step: name, Glyph: GlyphDone, Detail: st.Detail}
	default:
		// Not satisfied, not waiting, not blocked: Drive has not yet acted on this step
		// (or is about to) — the active row.
		return Row{Step: name, Glyph: GlyphActive, Active: true, Detail: st.Detail}
	}
}

func doneRows(order []engine.StepName, statuses []engine.StepStatus) []Row {
	var finalDetail string
	if len(statuses) > 0 {
		finalDetail = statuses[len(statuses)-1].Detail
	}
	rows := make([]Row, len(order))
	for i, name := range order {
		rows[i] = Row{Step: name, Glyph: GlyphDone}
	}
	if n := len(rows); n > 0 {
		rows[n-1].Detail = finalDetail
	}
	return rows
}

// ActiveStep is the row DeriveRows marked Active, if any — "" when every row is done or
// (defensively) when none is marked active at all.
func ActiveStep(rows []Row) (engine.StepName, bool) {
	for _, r := range rows {
		if r.Active {
			return r.Step, true
		}
	}
	return "", false
}

// BlockedStep is the row DeriveRows rendered with GlyphBlocked, if any. A blocked row is
// always the one Active row too (deriveRow's own Blocked case sets both), so this is really
// "ActiveStep, but only when that step is Blocked rather than merely Waiting or not-yet-acted
// — the distinction Model.onDriveResult needs to decide whether to keep polling: a Blocked
// step is terminal until an operator resolves the conflict (engine.BlockedError's own doc
// comment: "retrying will not help"), unlike a Waiting or not-yet-acted one, which is exactly
// what the poll loop exists to keep re-observing.
func BlockedStep(rows []Row) (engine.StepName, bool) {
	for _, r := range rows {
		if r.Glyph == GlyphBlocked {
			return r.Step, true
		}
	}
	return "", false
}

// StartedAt is the promotion's start time: History[0].At, the first entry Drive ever
// appends (BranchedStep's own first Observe/Act). PromotionState carries no separate
// started-at field (state.go) — the audit trail is the only place this is recorded
// (AGENTS.md §4.1: "the state file... is an index of what to look at"). The zero time when
// History is empty, i.e. a promotion that has not been driven even once yet.
func StartedAt(s engine.PromotionState) time.Time {
	if len(s.History) == 0 {
		return time.Time{}
	}
	return s.History[0].At
}

// PRURL is s.PR's URL, when a PR has been observed at all — what the 'o' key opens.
func PRURL(s engine.PromotionState) (string, bool) {
	if s.PR == nil || s.PR.URL == "" {
		return "", false
	}
	return s.PR.URL, true
}
