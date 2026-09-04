// Package engine drives one promotion's four steps — branch, commit, push, PR — to
// completion, re-observing the remote before every action (AGENTS.md §4.1: "the world is the
// state"). No step trusts PromotionState.Phase or any other recorded flag to decide whether
// its work is done; Observe always re-derives that from the local worktree and the remote
// (origin, the forge) before Act is allowed to run. Killing the process at any point and
// re-running the same command must therefore produce exactly one branch, one commit and one
// PR — proven in resume_test.go against a real local git remote and an in-memory forge.
//
// internal/engine is the one package in this milestone allowed to import both pkg/git and
// pkg/forge together with internal/config and pkg/gitops — it is the orchestration layer
// AGENTS.md §4.3 describes pkg/* as never containing.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abradner/hoist/pkg/redact"
)

// StepName identifies one of the four steps a promotion drives through, in order.
type StepName string

// The four steps a promotion drives through, in order: a worktree and branch, a commit on
// it, a push of that commit to origin, and a pull request for that branch.
const (
	StepBranched  StepName = "branched"
	StepCommitted StepName = "committed"
	StepPushed    StepName = "pushed"
	StepPROpened  StepName = "pr-opened"
)

// Observation is what Observe reports before Drive decides whether to call Act.
//
//   - Satisfied: the step's goal already holds in the world; Act must not run.
//   - Waiting: the step is in progress on something external and interactive (signing
//     approval) — Drive stops here without error so the caller can retry later; it is not a
//     failure.
//   - Blocked: the step cannot proceed and retrying will not help — a real conflict (someone
//     else moved the branch to different content), not a transient failure. Drive stops and
//     reports it as a terminal *BlockedError, distinct from a plumbing error from Observe
//     itself.
//   - Detail: a short human-readable note for History and CLI output; never a substitute for
//     Satisfied/Waiting/Blocked in code that branches on the result.
type Observation struct {
	Satisfied bool
	Waiting   bool
	Detail    string
	Blocked   string
}

// Step is one stage of a promotion. Observe must have no side effects beyond querying the
// local worktree and/or the remote; only Act is allowed to change anything.
type Step interface {
	Name() StepName
	Observe(ctx context.Context, s *PromotionState) (Observation, error)
	Act(ctx context.Context, s *PromotionState) error
}

// BlockedError is returned by Drive when a step's Observe reports Blocked: a genuine conflict
// that retrying will not resolve (AGENTS.md named adversary: a same-name branch already on
// origin with different content). The caller should surface Reason to the operator rather
// than retry automatically.
type BlockedError struct {
	Step   StepName
	Reason string
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%s: blocked: %s", e.Step, e.Reason)
}

// ErrWaiting is returned by Drive when a step is Waiting: the caller should report Detail
// (e.g. "waiting for signing approval") and may retry later. It is not a failure.
var ErrWaiting = errors.New("engine: waiting")

// StepError wraps a plain (non-Blocked, non-Waiting) error a step's Observe or Act returned,
// naming which step and which of the two failed. Added in M4 so a caller (the CLI's poll loop)
// can tell a step apart before deciding whether the error is worth retrying — a Checks/Comments
// call erroring on CIGreen or Approved (Known bug classes: a 404 or permissions hiccup, which
// should be retried, not read as authoritative) looks very different from a git/GitHub
// operation failing on an earlier step (terminal — waiting will not fix a broken git binary or
// a rejected push). Error()'s text is unchanged from Drive's pre-M4 format ("<step>: observe: "
// / "<step>: act: " prefix), so nothing that only inspected the message is affected.
type StepError struct {
	Step StepName
	Op   string // "observe" or "act"
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("%s: %s: %v", e.Step, e.Op, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// HistoryEntry is one line of a promotion's audit trail: what Observe/Act found, and when.
// History is informational only — Observe never reads it to decide anything (AGENTS.md
// §4.1: the state file, History included, is an index of what to look at, never evidence of
// what happened).
type HistoryEntry struct {
	Step   StepName
	At     time.Time
	Detail string
}

// Drive runs steps in order against s, saving after every step that changes state (and
// stopping to save "waiting" too, so a concurrent `hoist config show`-style inspection can
// see it). save may be nil (tests that don't care about persistence). Drive stops at the
// first step that is Blocked, Waiting, or whose Act fails; a later re-invocation of Drive
// with the same steps and an s built the same way re-observes every step from the top —
// nothing here remembers where it left off beyond what Observe itself re-derives.
//
// One exception, and the reason for the probe below: without it, that "re-observes every step
// from the top" rule collides with MergedStep's own Act, which deletes the promotion's branch
// on origin. Once a later step (M5's ArgoRefreshed/ArgoSynced/RolledOut) leaves Drive Waiting
// for multiple polls, every one of those later polls would re-run the full loop from Branched
// — including PushedStep, whose Observe finds the now-deleted branch missing and (correctly, by
// its own contract) re-pushes it, which in turn makes MergedStep's Observe see "branch not yet
// deleted" and re-run its own delete. That is a real, if individually harmless, re-push/re-delete
// cycle on every poll tick for as long as the rollout takes to converge — first noticed, and
// worked around locally with a widened poll.argo, in TestResumeRebuildsArgoAppsForALegacyStateFile
// (cmd/hoist/resume_test.go). The fix mirrors ObserveAll/Status's own short-circuit (see their
// doc comments): MergedStep's Observe re-verifies, from the world, that the whole core chain up
// to and including the merge still genuinely holds (FindPR, ancestry via mergeWasReverted, and
// the branch's absence) — that is self-contained proof, and re-deriving Branched..Approved on
// top of it adds nothing. So: probe MergedStep's Observe once, out of order, before the main
// loop, and when it comes back cleanly Satisfied, skip straight past it — Branched through
// Merged are not re-Observed or re-Acted this pass. The probe only runs once s.Phase (an
// advisory hint only, same as pollInterval's own use of it — never trusted as proof) shows a
// prior Drive call already reached Merged or beyond, so the earlier, still-in-progress ticks
// (waiting on CI or approval) pay no extra Observe call for a step that cannot possibly be
// satisfied yet. If the probe comes back anything other than cleanly Satisfied — including a
// real revert caught by mergeWasReverted's ancestry check — its Observation is reused rather
// than re-fetched when the main loop reaches that step in its own turn, so the probe never
// costs a duplicate real call either way.
func Drive(ctx context.Context, steps []Step, s *PromotionState, save func(*PromotionState) error) error {
	mergedIdx := -1
	for i, step := range steps {
		if step.Name() == StepMerged {
			mergedIdx = i
			break
		}
	}
	start := 0
	probedIdx := -1
	var probed Observation
	if mergedIdx >= 0 && mergedIdx < len(steps)-1 && phaseIndex(steps, s.Phase) > mergedIdx {
		if obs, err := steps[mergedIdx].Observe(ctx, s); err == nil {
			probedIdx, probed = mergedIdx, obs
			if obs.Blocked == "" && !obs.Waiting && obs.Satisfied {
				start = mergedIdx + 1
			}
		}
		// A probe error is deliberately not handled here — it falls through to the main loop,
		// which re-Observes this same step in its own turn and reports the error through the
		// normal *StepError path, exactly as if no probe had run at all.
	}
	for i := start; i < len(steps); i++ {
		step := steps[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		var obs Observation
		var err error
		if i == probedIdx {
			obs = probed
		} else {
			obs, err = step.Observe(ctx, s)
		}
		if err != nil {
			s.Phase = step.Name()
			return &StepError{Step: step.Name(), Op: "observe", Err: err}
		}
		if obs.Blocked != "" {
			s.Phase = step.Name()
			appendHistory(s, step.Name(), "blocked: "+obs.Blocked)
			saveIfSet(s, save)
			return &BlockedError{Step: step.Name(), Reason: obs.Blocked}
		}
		if obs.Waiting {
			s.Phase = step.Name()
			appendHistory(s, step.Name(), "waiting: "+obs.Detail)
			saveIfSet(s, save)
			return ErrWaiting
		}
		if !obs.Satisfied {
			if err := step.Act(ctx, s); err != nil {
				s.Phase = step.Name()
				appendHistory(s, step.Name(), "act failed: "+err.Error())
				saveIfSet(s, save)
				return &StepError{Step: step.Name(), Op: "act", Err: err}
			}
			appendHistory(s, step.Name(), "acted")
		} else {
			appendHistory(s, step.Name(), "already satisfied: "+obs.Detail)
		}
		s.Phase = step.Name()
		if save != nil {
			if err := save(s); err != nil {
				return fmt.Errorf("%s: saving state: %w", step.Name(), err)
			}
		}
	}
	return nil
}

// phaseIndex returns the index within steps whose Name() matches phase, or -1 if phase is empty,
// unrecognized, or not present in this particular steps slice (e.g. CoreSteps, which has no M5
// steps to match an M5 phase against). Used only as an optimization hint by Drive's own Merged
// short-circuit above — never as a substitute for a real Observe call.
func phaseIndex(steps []Step, phase StepName) int {
	if phase == "" {
		return -1
	}
	for i, step := range steps {
		if step.Name() == phase {
			return i
		}
	}
	return -1
}

// StepStatus is one step's re-observed state — never read from Phase, always from a fresh
// Observe call. Returned by ObserveAll for a listing (`hoist resume`'s startup listing) or a
// one-in-flight-per-target-env check (`hoist promote`'s refusal), neither of which should call
// Act.
type StepStatus struct {
	Step StepName
	Observation
}

// ObserveAll re-derives every step's Observation in order, stopping at the first that is not
// cleanly Satisfied (Waiting or Blocked included) — the read-only half of what Drive does,
// without ever calling Act. done reports whether every step was Satisfied; last is the
// stopping point's own status (or the final step's, when done). A promotion is "in flight"
// exactly when done is false: still has real work ahead of it, is waiting on something
// external, or is blocked and needs operator attention — any of which means a second promotion
// for the same target env must not start (AGENTS.md §4.1: re-observe, never trust the state
// file's own Phase, for this question too).
//
// The last step is checked first, as a short-circuit: MergedStep's own Observe (merged AND its
// branch deleted) is self-contained proof the whole promotion finished, and is deliberately the
// only step whose Act *removes* something an earlier step's Observe depends on for its own
// Satisfied condition (PushedStep's Observe requires the branch to still exist on origin — true
// throughout the promotion, false forever after MergedStep's cleanup runs). Without this
// short-circuit, ObserveAll would report a fully completed, cleaned-up promotion as stuck at
// Pushed — wrong, and exactly backwards from what a one-in-flight-per-env check needs: it would
// make a *finished* promotion block every future one for the same env, forever. ObserveAll never
// calls Act, so its short-circuit probes the *last* step. Drive does call Act, and for a while
// (pre-M5) that meant it never needed a short-circuit of its own: Merged was always the last
// step, so Drive simply finished the moment Merged was satisfied and was never called again.
// M5 added steps after Merged, so a promotion now sits waiting there across many further Drive
// calls — each of which, without its own short-circuit, would re-hit exactly the problem this
// paragraph describes for ObserveAll, except with Act: PushedStep re-pushing the branch Merged
// just deleted, then MergedStep deleting it again, every poll tick. Drive now carries the same
// probe, aimed one step earlier (at Merged rather than the last step, since Merged's own Observe
// is the self-contained proof either way) — see Drive's own doc comment.
func ObserveAll(ctx context.Context, steps []Step, s *PromotionState) (done bool, last StepStatus, err error) {
	var finalProbe *StepStatus
	if n := len(steps); n > 0 {
		final := steps[n-1]
		obs, oerr := final.Observe(ctx, s)
		if oerr != nil {
			return false, StepStatus{Step: final.Name()}, fmt.Errorf("%s: observe: %w", final.Name(), oerr)
		}
		if obs.Satisfied {
			return true, StepStatus{Step: final.Name(), Observation: obs}, nil
		}
		finalProbe = &StepStatus{Step: final.Name(), Observation: obs}
	}
	for i, step := range steps {
		if err := ctx.Err(); err != nil {
			return false, last, err
		}
		if finalProbe != nil && i == len(steps)-1 {
			// Same step the short-circuit probe already observed above (round-6 regression:
			// this doubled every final step's Observe — real remote/git work for MergedStep —
			// on every findInFlight/promotions/resume --env call; Status already carries this
			// fix, ObserveAll had not been given it) — reuse that Observation instead of
			// calling Observe on it again.
			last = *finalProbe
		} else {
			obs, oerr := step.Observe(ctx, s)
			if oerr != nil {
				return false, StepStatus{Step: step.Name()}, fmt.Errorf("%s: observe: %w", step.Name(), oerr)
			}
			last = StepStatus{Step: step.Name(), Observation: obs}
		}
		if last.Blocked != "" || last.Waiting || !last.Satisfied {
			return false, last, nil
		}
	}
	return true, last, nil
}

// Status is ObserveAll's sibling for a caller that needs to render every step's own
// standing, not only the stopping point: the internal/app/flight screen's step list (glyph
// per step, detail on whichever one is active) needs a StepStatus for each step already
// passed as well as the one it stopped at, which ObserveAll's single "last" return cannot
// carry. Status shares ObserveAll's two rules verbatim — the final-step short-circuit (see
// ObserveAll's doc comment: MergedStep's own Observe is self-contained proof the whole
// promotion finished, since PushedStep's Observe would otherwise falsely read as stuck once
// Merged's own Act has deleted the branch it depends on) and the stopping rule (the first
// step that is not cleanly Satisfied ends the walk) — only the shape of what is returned
// differs: statuses accumulates one entry per step actually observed, in order, rather than
// discarding every entry but the last.
//
// When the short-circuit fires, statuses is the single-element slice {final step's own
// StepStatus} — deliberately not one entry per step, since re-observing every earlier step
// individually is exactly what the short-circuit exists to avoid (an already-merged,
// already-cleaned-up promotion would read PushedStep's Observe as false: the branch it
// checks is gone). A caller rendering a fully done list (done == true) should treat every
// step as done regardless of how many entries statuses carries, using the final entry only
// for its Detail — see flight.DeriveRows.
//
// Otherwise, statuses holds exactly the steps Status reached: every entry before the last is
// Satisfied (the walk only continues past a step that cleanly is), and the last entry is
// the one Status stopped at — Blocked, Waiting, or plainly not yet Satisfied. A step whose
// name never appears in statuses has not been reached at all this call. The short-circuit
// probe above already called Observe once on the final step before the walk started
// (needed to even know whether to short-circuit); when the walk isn't short-circuited it
// reaches that same final step again in its own turn, but reuses the probe's Observation
// there rather than calling Observe a second time — every poll that isn't yet fully done
// would otherwise cost one extra, wasted remote call on the final step, every tick of the
// flight screen's own poll loop (PR #39 review finding #3).
// Both of Status's own Observe errors are returned as *StepError (Op: "observe"), not a bare
// fmt.Errorf, even though nothing here ever calls Act: internal/app/flight.Model's retry
// classifier (retryableErr, mirroring cmd/hoist/drive.go's own driveToCompletion) only retries
// automatically on a *StepError naming StepCIGreen or StepApproved — the two steps whose
// Observe alone can transiently 404/scope-error on a Checks or Comments call without the
// underlying condition (CI status, an approval) actually being answerable yet. A bare wrapped
// error carries the same message (StepError.Error()'s "<step>: <op>: <err>" format matches this
// function's own pre-existing "%s: observe: %w" text exactly, and Unwrap still reaches oerr, so
// errors.Is/the message text are both unchanged) but cannot be told apart by errors.As, which is
// all that classifier can use. Before this, a transient hiccup on the immediately-following
// Status call — after engine.Drive had itself already observed the very same step successfully
// as Waiting or Blocked, in cmd/hoist/wiring.go's own DriveFunc — surfaced as a plain error the
// flight screen read as terminal and stopped polling on for good, unlike the CLI's own
// driveToCompletion, which retries the identical shape of failure when Drive's own Observe hits
// it directly (Codex review, PR #50 round 4).
func Status(ctx context.Context, steps []Step, s *PromotionState) (done bool, statuses []StepStatus, err error) {
	var finalProbe *StepStatus
	if n := len(steps); n > 0 {
		final := steps[n-1]
		obs, oerr := final.Observe(ctx, s)
		if oerr != nil {
			return false, nil, &StepError{Step: final.Name(), Op: "observe", Err: oerr}
		}
		if obs.Satisfied {
			return true, []StepStatus{{Step: final.Name(), Observation: obs}}, nil
		}
		finalProbe = &StepStatus{Step: final.Name(), Observation: obs}
	}
	for i, step := range steps {
		if cerr := ctx.Err(); cerr != nil {
			return false, statuses, cerr
		}
		var st StepStatus
		if finalProbe != nil && i == len(steps)-1 {
			// Same step the short-circuit probe already observed above; reuse that
			// Observation instead of calling Observe on it again.
			st = *finalProbe
		} else {
			obs, oerr := step.Observe(ctx, s)
			if oerr != nil {
				return false, statuses, &StepError{Step: step.Name(), Op: "observe", Err: oerr}
			}
			st = StepStatus{Step: step.Name(), Observation: obs}
		}
		statuses = append(statuses, st)
		if st.Blocked != "" || st.Waiting || !st.Satisfied {
			return false, statuses, nil
		}
	}
	return true, statuses, nil
}

// appendHistory is the one place a HistoryEntry.Detail is ever written, and therefore the
// last boundary before it is persisted to the state file on disk (SaveState marshals History
// verbatim). A step's Act error can embed a registered credential verbatim — pkg/git's own
// error wrapping folds a failed command's stderr in, and a git hook or the signing helper
// could echo one there — so detail passes through redact.Strings here even though pkg/git
// already does its own best-effort scrubbing where it can (the same belt-and-suspenders
// pattern as internal/app/plan/model.go's View(), which redacts its assembled output once at
// the final boundary in addition to per-field calls).
func appendHistory(s *PromotionState, name StepName, detail string) {
	s.History = append(s.History, HistoryEntry{Step: name, At: time.Now(), Detail: redact.Strings(detail)})
}

// saveIfSet is best-effort: a save failure on a terminal (blocked/waiting) path is logged into
// history but must not shadow the real reason Drive is stopping.
func saveIfSet(s *PromotionState, save func(*PromotionState) error) {
	if save == nil {
		return
	}
	if err := save(s); err != nil {
		appendHistory(s, s.Phase, "state save failed: "+err.Error())
	}
}
