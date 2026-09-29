package flight

import (
	"fmt"
	"strings"
	"time"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/redact"
)

// Summary is one promotion as the matrix's in-flight pane shows it (M10, #85 screen 05): the
// identity, the step rows engine.Status produced for it, and what it is waiting for — all
// re-observed from the forge and the cluster the moment it was listed, never read from the
// state file's recorded phase (AGENTS.md §4.1). Built by Summarize from the same
// (done, statuses) shape DeriveRows takes, so the pane and the flight screen agree.
type Summary struct {
	ID             string
	Source, Target string
	Direct         bool
	StartedAt      time.Time
	PR             *forge.PR
	Rows           []Row
	Done           bool
	// Err is why this promotion could not be re-observed (a forge scope gap, a repo no
	// longer in config), redacted at the render boundary like every other upstream string.
	// The Rows are then every step not-reached and the pane says so rather than guessing.
	Err string
	// Live is true when this Summary was built from this session's own session.Controller entry
	// (app.go's summaryForSnapshot) rather than purely from a fresh listing (summaryFor) — the
	// pane's own way to say "hoist itself is driving this, right now" versus "re-observed, last
	// seen at this state" (Train 2 design PR 3's own "the pane marks drives running here").
	Live bool
	// Build identifies the session.Controller entry this Summary was built from
	// (summaryForSnapshot, app.go) — the only handle a still-Building entry has, since Summarize
	// leaves ID empty until a real promotion id exists. Zero (session.BuildID's own zero value)
	// for anything built purely from a listing (summaryFor), which never has a live Controller
	// entry to point at.
	Build session.BuildID
	// NextPoll mirrors session.Snapshot.NextPoll — when this session will next re-observe this
	// promotion on its own, the same field flight.Model's own waiting() reads. Zero for anything
	// not Live (a plain listing has no controller entry telling it when it will next be asked
	// for again), and zero for a Live entry that isn't in the plain "waiting on the next
	// scheduled check" state either (building, busy, done, stopped — session.Controller only
	// ever sets it for a Waiting entry). The matrix's own in-flight pane is this field's one
	// consumer today (commit 1 of the T3-06 train): each Waiting entry's "next check in Ns" text
	// is worded straight off it, never recomputed from a duration this package would have to
	// keep in step with session.Controller's own poll interval by hand.
	NextPoll time.Time
}

// Summarize builds a Summary from a state and engine.Status's result for it.
func Summarize(s engine.PromotionState, done bool, statuses []engine.StepStatus, err error) Summary {
	out := Summary{
		ID: s.ID, Source: s.SourceEnv, Target: s.TargetEnv, Direct: s.Direct,
		StartedAt: StartedAt(s), PR: s.PR, Done: done,
	}
	if err != nil {
		out.Err = redact.Strings(err.Error())
		out.Rows = DeriveRows(OrderFor(s), false, nil)
		return out
	}
	out.Rows = DeriveRows(OrderFor(s), done, statuses)
	return out
}

// Compact glyphs for the one-line step row the pane draws: done, active or waiting, blocked,
// not reached — the same states Row.Glyph carries, in a form that reads as a strip.
const (
	StripDone       = "●"
	StripActive     = "◍"
	StripBlocked    = "✗"
	StripNotReached = "○"
)

// StepStrip is the whole pipeline on one line: "● branch ● commit ● push ● PR #103 ● CI ◍
// approval ○ merge ○ argo refresh ○ argo sync ○ rollout". The PR step names its number once
// one exists.
func (s Summary) StepStrip() string {
	parts := make([]string, 0, len(s.Rows))
	for _, r := range s.Rows {
		glyph := StripNotReached
		switch r.Glyph {
		case GlyphDone:
			glyph = StripDone
		case GlyphActive, GlyphWaiting:
			glyph = StripActive
		case GlyphBlocked:
			glyph = StripBlocked
		}
		label := Label(r.Step)
		if r.Step == engine.StepPROpened && s.PR != nil && s.PR.Number > 0 {
			label = fmt.Sprintf("PR #%d", s.PR.Number)
		}
		parts = append(parts, glyph+" "+label)
	}
	return strings.Join(parts, "  ")
}

// Verdict is the one phrase that survives every width (docs/tui/mockups.html: "degrade by
// dropping evidence, never the verdict"): "done", "blocked on approval", "waiting on CI",
// "blocked: <reason>" for a real conflict, or "cannot re-observe" when Err is set.
func (s Summary) Verdict() string {
	switch {
	case s.Err != "":
		return "cannot re-observe"
	case s.Done:
		return "done"
	}
	for _, r := range s.Rows {
		if !r.Active {
			continue
		}
		switch r.Glyph {
		case GlyphBlocked:
			return "blocked at " + Label(r.Step)
		case GlyphWaiting:
			if r.Step == engine.StepApproved {
				return "blocked on approval"
			}
			return "waiting on " + Label(r.Step)
		default:
			return "at " + Label(r.Step)
		}
	}
	return "starting"
}

// ApprovalCopy is Action() with one wording fix (v2·04a, UX-H10): a promotion blocked on the
// approval step reads "waiting for an approver to comment `hoist approve <id>` on PR #N" instead
// of Action()'s own "blocked on you — comment ... to release it:", which reads like an accusal
// rather than a status (AGENTS.md §9's own regression note on this fix). Exported (T3-06) so
// this package's own flight.Model and the matrix's in-flight pane render the identical sentence
// from one place, rather than each keeping its own copy of the fix in step by hand — the matrix
// carried it alone from T3-05 until this package had its own PR to land it in too.
func ApprovalCopy(s Summary) (text, command string) {
	text, command = s.Action()
	if command == "" || !strings.HasPrefix(command, "hoist approve") {
		return text, command
	}
	if s.PR != nil && s.PR.Number > 0 {
		return fmt.Sprintf("waiting for an approver to comment `%s` on PR #%d", command, s.PR.Number), ""
	}
	return fmt.Sprintf("waiting for an approver to comment `%s` on the PR", command), ""
}

// Action is what the operator can do about it, when it is theirs to do: the approval
// command for a promotion parked on approval (the id was previously only in the PR body,
// which is how a healthy promotion sat indistinguishable from a hang), the conflict text for
// a blocked step, "" when there is nothing to type. Second return is the command itself,
// for the accent style, "" when the action is prose only.
func (s Summary) Action() (text, command string) {
	if s.Err != "" {
		return s.Err, ""
	}
	for _, r := range s.Rows {
		if !r.Active {
			continue
		}
		switch {
		case r.Glyph == GlyphBlocked:
			return r.Detail, ""
		case r.Step == engine.StepApproved && r.Glyph == GlyphWaiting:
			where := "on the PR"
			if s.PR != nil && s.PR.Number > 0 {
				where = fmt.Sprintf("on PR #%d", s.PR.Number)
			}
			return "blocked on you — comment " + where + " to release it:", "hoist approve " + s.ID
		default:
			return r.Detail, ""
		}
	}
	return "", ""
}
