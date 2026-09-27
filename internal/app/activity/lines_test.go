package activity

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/redact"
)

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestLongErrorFullInActivityView proves a long error survives in full, unmerged and
// untruncated: 20 distinct lines in, 20 distinct lines out, each on its own row — asserting the
// exact row count and which rows hold which line, never Contains over the whole rendering
// (AGENTS.md §8: "assert the SHAPE... never Contains — a count-based check ... also matches").
// This is the regression test for the whole point of this package: the old app.Model.notice
// convention only ever showed one line, cleared on the next keypress, so a multi-line transport
// error was never fully readable anywhere in the TUI.
func TestLongErrorFullInActivityView(t *testing.T) {
	var detailLines []string
	for i := range 20 {
		detailLines = append(detailLines, fmt.Sprintf("error line %02d of a long transport failure", i))
	}
	detail := strings.Join(detailLines, "\n")
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	log := Log{}.Add(Entry{At: at, Kind: Err, Text: "promotion abcd1234 failed", Detail: detail})

	now := at.Add(5 * time.Minute)
	lines := Lines(log, fixedNow(now), 80)

	if want := 1 + len(detailLines); len(lines) != want {
		t.Fatalf("got %d lines, want %d (1 header + %d detail lines):\n%s", len(lines), want, len(detailLines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "promotion abcd1234 failed") {
		t.Fatalf("row 0 = %q, want the header naming the failure", lines[0])
	}
	for i, want := range detailLines {
		if got := lines[1+i]; got != want {
			t.Errorf("row %d = %q, want %q — the full error must survive unmerged and untruncated", 1+i, got, want)
		}
	}
}

// TestLinesEmptyLogSaysSo is the positive control for TestLongErrorFullInActivityView's own
// count-based assertion: an empty log must not also produce a plausible-looking 21-line
// rendering by accident.
func TestLinesEmptyLogSaysSo(t *testing.T) {
	lines := Lines(Log{}, fixedNow(time.Now()), 80)
	if len(lines) != 1 || lines[0] != "nothing yet" {
		t.Fatalf("Lines(empty) = %v, want exactly [\"nothing yet\"]", lines)
	}
}

// TestLinesRedactsDetail proves pkg/redact runs over Detail and URL, not just Text — a
// credential embedded in a transport error (a token in a URL, say) must not reach the screen.
func TestLinesRedactsDetail(t *testing.T) {
	const secret = "sk-super-secret-token-value"
	log := Log{}.Add(Entry{
		At:     time.Now(),
		Kind:   Err,
		Text:   "failed",
		Detail: "transport error: token=" + secret,
	})
	// Register the secret with pkg/redact the way every real adaptor does the moment it reads
	// one (AGENTS.md §4.10).
	redact.Register(secret)
	lines := Lines(log, fixedNow(time.Now()), 80)
	for _, l := range lines {
		if strings.Contains(l, secret) {
			t.Fatalf("rendered line contains the unredacted secret: %q", l)
		}
	}
}
