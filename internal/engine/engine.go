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
func Drive(ctx context.Context, steps []Step, s *PromotionState, save func(*PromotionState) error) error {
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		obs, err := step.Observe(ctx, s)
		if err != nil {
			return fmt.Errorf("%s: observe: %w", step.Name(), err)
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
				appendHistory(s, step.Name(), "act failed: "+err.Error())
				saveIfSet(s, save)
				return fmt.Errorf("%s: act: %w", step.Name(), err)
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

func appendHistory(s *PromotionState, name StepName, detail string) {
	s.History = append(s.History, HistoryEntry{Step: name, At: time.Now(), Detail: detail})
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
