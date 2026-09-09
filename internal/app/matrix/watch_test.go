package matrix

import (
	"testing"

	"github.com/abradner/hoist/internal/ui/uitest"
)

// TestWatchKeyEmitsOpenWatchMsg: w names the cursor cell — the family under the row cursor
// in the env under the column cursor — the way R does for a restart.
func TestWatchKeyEmitsOpenWatchMsg(t *testing.T) {
	m := uitest.Keys(newFixture(), update, "right", "down")
	msg, ok := emitted(t, m, "w").(OpenWatchMsg)
	if !ok || msg.Family != "drift" || msg.Target != "b" {
		t.Fatalf("w emitted %+v, want {Family: drift, Target: b}", msg)
	}
}
