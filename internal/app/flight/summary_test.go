package flight

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
)

func sat(step engine.StepName) engine.StepStatus {
	return engine.StepStatus{Step: step, Observation: engine.Observation{Satisfied: true}}
}

func parkedOnApproval() engine.PromotionState {
	return engine.PromotionState{
		ID: "5pr6sd333t", SourceEnv: "app-staging", TargetEnv: "app-production",
		PR:      &forge.PR{Number: 103, URL: "https://forge.example.invalid/pr/103"},
		History: []engine.HistoryEntry{{Step: engine.StepBranched, At: time.Date(2026, 3, 5, 11, 48, 0, 0, time.UTC)}},
	}
}

func approvalStatuses() []engine.StepStatus {
	return []engine.StepStatus{
		sat(engine.StepBranched), sat(engine.StepCommitted), sat(engine.StepPushed), sat(engine.StepPROpened), sat(engine.StepCIGreen),
		{Step: engine.StepApproved, Observation: engine.Observation{Waiting: true, Detail: "no approval comment yet"}},
	}
}

func TestSummarizeParkedOnApproval(t *testing.T) {
	s := Summarize(parkedOnApproval(), false, approvalStatuses(), nil)
	if s.ID != "5pr6sd333t" || s.Source != "app-staging" || s.Target != "app-production" || s.PR.Number != 103 {
		t.Fatalf("identity = %+v", s)
	}
	if got := s.StepStrip(); got != "● branch  ● commit  ● push  ● PR #103  ● CI  ◍ approval  ○ merge  ○ argo refresh  ○ argo sync  ○ rollout" {
		t.Fatalf("strip = %q", got)
	}
	if got := s.Verdict(); got != "blocked on approval" {
		t.Fatalf("verdict = %q", got)
	}
	text, command := s.Action()
	if text != "blocked on you — comment on PR #103 to release it:" || command != "hoist approve 5pr6sd333t" {
		t.Fatalf("action = %q / %q", text, command)
	}
}

func TestSummarizeOtherShapes(t *testing.T) {
	st := parkedOnApproval()
	// Waiting on CI: a verdict, no command.
	ci := Summarize(st, false, []engine.StepStatus{sat(engine.StepBranched), sat(engine.StepCommitted), sat(engine.StepPushed), sat(engine.StepPROpened), {Step: engine.StepCIGreen, Observation: engine.Observation{Waiting: true, Detail: "2 of 4 checks pending"}}}, nil)
	if ci.Verdict() != "waiting on CI" {
		t.Errorf("CI verdict = %q", ci.Verdict())
	}
	if text, cmd := ci.Action(); text != "2 of 4 checks pending" || cmd != "" {
		t.Errorf("CI action = %q / %q", text, cmd)
	}
	// Blocked: the conflict text is the action.
	blocked := Summarize(st, false, []engine.StepStatus{sat(engine.StepBranched), {Step: engine.StepCommitted, Observation: engine.Observation{Blocked: "worktree has uncommitted changes"}}}, nil)
	if blocked.Verdict() != "blocked at commit" || !strings.Contains(blocked.StepStrip(), "✗ commit") {
		t.Errorf("blocked = %q / %q", blocked.Verdict(), blocked.StepStrip())
	}
	if text, _ := blocked.Action(); text != "worktree has uncommitted changes" {
		t.Errorf("blocked action = %q", text)
	}
	// Done.
	done := Summarize(st, true, []engine.StepStatus{sat(engine.StepRolledOut)}, nil)
	if done.Verdict() != "done" || strings.Contains(done.StepStrip(), "○") {
		t.Errorf("done = %q / %q", done.Verdict(), done.StepStrip())
	}
	// Direct: the direct step order, no PR step at all.
	st.Direct = true
	direct := Summarize(st, false, []engine.StepStatus{sat(engine.StepDirectGate), sat(engine.StepBranched), {Step: engine.StepCommitted}}, nil)
	if strip := direct.StepStrip(); strings.Contains(strip, "PR") || !strings.Contains(strip, "● gate") || !strings.HasSuffix(strip, "○ rollout") {
		t.Errorf("direct strip = %q", strip)
	}
	if direct.Verdict() != "at commit" {
		t.Errorf("direct verdict = %q", direct.Verdict())
	}
	// Could not re-observe: the reason is the action, the verdict says so, no step is claimed.
	failed := Summarize(st, false, nil, errors.New("HTTP 403 the gh token may be missing the repo scope"))
	if failed.Verdict() != "cannot re-observe" || !strings.Contains(failed.Err, "403") {
		t.Errorf("failed = %+v", failed)
	}
	if strings.Contains(failed.StepStrip(), "●") {
		t.Errorf("a failed observation must not claim any step done: %q", failed.StepStrip())
	}
}
