// Package session is the controller that owns every drive a promotion or deploy goes through
// once the operator confirms it: starting it (internal/service.Service.StartPromotion), resuming
// one already on disk (Resume), stepping it to completion one poll at a time, abandoning it, and
// keeping the matrix's in-flight pane listed — all of it re-observed, never trusted from a
// screen's own memory (AGENTS.md §4.1).
//
// internal/app's root model holds one Controller and routes every command and Event through it
// (app.go's Update, apply), and the flight screen mirrors one entry rather than driving a
// promotion itself — so a background drive no longer dies with the screen that started it.
//
// # Design
//
// Controller is a value type: every state change
// happens inside Update or one of Controller's own methods, each returning a new Controller
// rather than mutating one in place, exactly as internal/app/matrix.Model and
// internal/app/plan.Model already do (AGENTS.md §4.8). Bubble Tea keeps only whatever a model's
// Update returns, so a value stored there survives the copy; nothing here needs a mutex, and
// View has nothing to race with. Every map Controller holds (its own tracked entries, and the id
// index onto them) is copied with maps.Clone before a mutating method writes to it — the same
// discipline internal/app/matrix.Model.stack already follows — so an older Controller value a
// caller is still holding (a screen's own stale copy, a test's "before" snapshot) never observes
// a write a newer copy made.
//
// Background work — a StartPromotion/Resume round trip, one Driver.Step poll, a List call, an
// Abandon call, or draining one progress line off a build's channel — is always a plain tea.Cmd
// that returns one of this package's own unexported Event values; nothing here ever writes
// Controller state from a goroutine. Every Event these commands can produce is listed in
// events.go.
//
// Event is deliberately not exported with a Msg suffix and is never itself treated as a
// navigation message: internal/parity's own parser only ever looks for `case pkg.XMsg` in
// internal/app/app.go, and this package's internal plumbing has nothing for the operator to
// trigger directly the way a screen's own *Msg does. The wiring PR that follows this one adds
// exactly one `case session.Event:` to that switch.
package session
