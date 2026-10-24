package main

import (
	"testing"

	"github.com/abradner/hoist/internal/engine"
)

// TestHistoryDetailSkipsActedAfterTheWait: History records an unchanged wait once, so a step that
// re-acts every poll (ArgoRefreshedStep) leaves its "acted" after the Argo wait it is still in.
// A deadline message built from History must name the wait, not "acted" (#243).
func TestHistoryDetailSkipsActedAfterTheWait(t *testing.T) {
	s := &engine.PromotionState{History: []engine.HistoryEntry{
		{Step: engine.StepArgoRefreshed, Detail: "acted"},
		{Step: engine.StepArgoSynced, Detail: "waiting: Argo has not synced the merge yet"},
		{Step: engine.StepArgoRefreshed, Detail: "acted"},
	}}
	if got, want := historyDetail(s), "waiting: Argo has not synced the merge yet"; got != want {
		t.Fatalf("historyDetail = %q, want %q", got, want)
	}
	if got := historyDetail(&engine.PromotionState{}); got != "" {
		t.Fatalf("empty History: %q", got)
	}
}
