// Package activity is the root's own record of what has happened this session — every
// promotion started, landed, blocked or failed, every abandon, every browser-launch outcome —
// kept as an append-only, capped Log (AGENTS.md §4.8) rather than the
// single transient "notice" string app.Model carried before this package existed.
//
// The old convention (app.Model.notice) showed exactly one message, cleared unconditionally on
// the operator's very next keypress — so a real refusal (an in-flight conflict, a missing
// repos[].github, a claim conflict) that arrived a beat before an unrelated "j" or "k" was
// simply gone, with no way to read it again short of re-running the equivalent command on the
// CLI (#164 was the first half of this; this package is the second). Log.Add never drops an
// entry on a keypress — only once the log's own cap (50) is exceeded, oldest first — and
// Model is the screen (l on the matrix, AGENTS.md §9 entry 10's own "one verb per concept"
// keymap) that reads every entry back in full, redacted and wrapped but never truncated.
//
// app.Model's own bottom row (View, replacing the old notice rows) shows only the latest
// entry's text, one line, plus a "l: activity (N)" hint — not cleared by a keypress either,
// only ever replaced by a newer entry or dismissed by opening the log itself.
package activity
