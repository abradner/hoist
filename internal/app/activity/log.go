package activity

import "time"

// Kind classifies one Entry for the activity screen's own styling and the bottom row's own
// short summary — Err in the error color, OK in the success color, Info plain.
type Kind int

const (
	// Info is a plain, non-error entry — a promotion started, a PR opened, a URL shown.
	Info Kind = iota
	// OK marks a successful outcome — a promotion landed, an abandon completed.
	OK
	// Err marks a refusal or failure — a build error, a blocked step, a failed drive.
	Err
)

// String names a Kind for rendering — never used to decide anything.
func (k Kind) String() string {
	switch k {
	case OK:
		return "ok"
	case Err:
		return "error"
	default:
		return "info"
	}
}

// Entry is one thing the session reported: a promotion started (naming its PR when one already
// exists), landed, blocked, failed or abandoned, or a browser-launch outcome. Text is the short
// summary the bottom row can show on one line; Detail is the full text a summary would otherwise
// lose — a complete transport error, in particular — shown only in the activity screen itself,
// wrapped but never truncated (AGENTS.md §8, "verify the output, not the instrument": a summary
// that silently ate the reason would be exactly the #164 shape this package exists to end). URL
// is set when the entry names a link (a PR) — both the bottom row and the activity screen show
// it. Every string here is redacted at render time (pkg/redact), never at construction, so
// Log.Add never has to know redaction happened.
type Entry struct {
	At     time.Time
	Kind   Kind
	Text   string
	Detail string
	URL    string
}

// Cap is the maximum number of entries Log keeps. Add drops the oldest once full.
const Cap = 50

// Log is an append-only, capped history of Entry values (doc.go's own "Design" section) — a
// value type, copy-on-append like session.Controller (AGENTS.md §4.8): Add never mutates the receiver's backing array, so an older Log a screen still holds
// (Model.New takes one by value, doc comment) never observes a later Add.
type Log struct {
	entries []Entry
}

// Add appends e and returns the updated Log, oldest entries dropped past Cap.
func (l Log) Add(e Entry) Log {
	entries := make([]Entry, 0, len(l.entries)+1)
	entries = append(entries, l.entries...)
	entries = append(entries, e)
	if len(entries) > Cap {
		entries = entries[len(entries)-Cap:]
	}
	return Log{entries: entries}
}

// Latest returns the most recently added Entry, or false if the log is empty.
func (l Log) Latest() (Entry, bool) {
	if len(l.entries) == 0 {
		return Entry{}, false
	}
	return l.entries[len(l.entries)-1], true
}

// Len reports how many entries the log holds.
func (l Log) Len() int { return len(l.entries) }

// Entries returns every entry, oldest first. The caller must not mutate the result.
func (l Log) Entries() []Entry {
	return append([]Entry(nil), l.entries...)
}
