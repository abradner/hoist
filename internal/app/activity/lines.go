package activity

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
)

// Lines renders every entry in log as plain wrapped text, oldest first — the activity screen's
// own derived data, with no terminal dependency (AGENTS.md §4.8: "a screen's derived data …
// lives in a file with no terminal dependency", the same split matrix/cells.go uses), so a test
// can assert an exact row count and which rows hold a given line without a viewport or a Frame
// in the way. Nothing here truncates: a long Detail (a full transport error) wraps onto as many
// rows as it needs — the screen scrolls, it never drops the tail (contrast ui.Frame's own
// section-trimming, AGENTS.md §9 entry 10, which is about a FIXED number of terminal rows, not
// this scrollable content inside one of them). now ages every entry (ui.Ago); a test pins it.
// Every string is redacted here, once, at this render boundary (pkg/redact), the same convention
// app.Model.View and flight.Model.View already use.
func Lines(log Log, now func() time.Time, width int) []string {
	if width <= 0 {
		width = 80
	}
	entries := log.Entries()
	if len(entries) == 0 {
		return []string{"nothing yet"}
	}
	when := now()
	var out []string
	for i, e := range entries {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, entryLines(e, when, width)...)
	}
	return out
}

// entryLines renders one Entry: a header line (age, kind, the short Text), then Detail and URL
// each wrapped onto their own lines when present.
func entryLines(e Entry, now time.Time, width int) []string {
	head := fmt.Sprintf("%s  %-4s  %s", ui.Ago(now, e.At), e.Kind, redact.Strings(e.Text))
	lines := wrap(head, width)
	if e.Detail != "" {
		lines = append(lines, wrap(redact.Strings(e.Detail), width)...)
	}
	if e.URL != "" {
		lines = append(lines, wrap(redact.Strings(e.URL), width)...)
	}
	return lines
}

func wrap(text string, width int) []string {
	return strings.Split(ansi.Wrap(text, width, ""), "\n")
}
