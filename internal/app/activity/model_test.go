package activity

import (
	"testing"
	"time"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
)

var fixedAt = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func fixture() Log {
	return Log{}.
		Add(Entry{At: fixedAt.Add(-2 * time.Hour), Kind: Info, Text: "started app-staging → app-production"}).
		Add(Entry{At: fixedAt.Add(-90 * time.Minute), Kind: Err, Text: "abcd1234 blocked: ci.none: prompt"}).
		Add(Entry{At: fixedAt.Add(-1 * time.Minute), Kind: OK, Text: "abcd1234 landed", URL: "https://example.invalid/pr/1"})
}

func newFixture() Model {
	return New(fixture(), func() time.Time { return fixedAt }).SetStyles(ui.NewStyles(true))
}

func TestViewGolden(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m := newFixture().SetSize(size[0], size[1])
		uitest.Golden(t, "activity", m.View(), size[0], size[1])
	}
}

func TestEscEmitsBackMsg(t *testing.T) {
	m := newFixture().SetSize(80, 24)
	_, cmd := m.Update(uitest.Key("esc"))
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
}

func TestCapturesTextIsAlwaysFalse(t *testing.T) {
	if newFixture().CapturesText() {
		t.Fatal("activity screen has no text-entry mode")
	}
}

// TestEmptyLogGolden pins the "nothing yet" shape so an operator opening the log before any
// entry exists sees a stated reason, not a blank box.
func TestEmptyLogGolden(t *testing.T) {
	m := New(Log{}, func() time.Time { return fixedAt }).SetStyles(ui.NewStyles(true)).SetSize(80, 24)
	uitest.Golden(t, "activity-empty", m.View(), 80, 24)
}
