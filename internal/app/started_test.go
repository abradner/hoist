package app

import (
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
)

// TestStartedHasOneOriginInHeaderAndPane: preflight lines are logged before any History exists,
// so the first History entry is NOT when the promotion started. The flight header and the
// matrix pane's Summary must both count from the earlier preflight line — otherwise the same
// promotion reads "started 1m ago" in one place and "2m ago" in the other.
func TestStartedHasOneOriginInHeaderAndPane(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	snap := session.Snapshot{
		Build: 1, ID: "abcd1234", Phase: session.Waiting, Source: "app-staging", Target: "app-production",
		State: engine.PromotionState{
			ID: "abcd1234", SourceEnv: "app-staging", TargetEnv: "app-production",
			History: []engine.HistoryEntry{{Step: engine.StepBranched, At: now.Add(-60 * time.Second), Detail: "acted"}},
		},
		Log: []session.LogLine{{At: now.Add(-125 * time.Second), Text: "checking your checkout against origin/main"}},
	}
	want := now.Add(-125 * time.Second)

	if got := summaryForSnapshot(snap).StartedAt; !got.Equal(want) {
		t.Errorf("pane Summary.StartedAt = %v, want the first preflight line %v", got, want)
	}
	header := flight.NewAttached(snap, flight.PollDurations{}).WithNow(func() time.Time { return now }).SetStyles(ui.NewStyles(true)).SetSize(100, 30).View()
	if !strings.Contains(header, "started "+ui.Ago(now, want)) {
		t.Errorf("flight header does not say started %s:\n%s", ui.Ago(now, want), header)
	}
	if ui.Ago(now, want) == ui.Ago(now, snap.State.History[0].At) {
		t.Fatal("fixture is not a control: the two origins render the same")
	}
}
