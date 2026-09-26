// Package app is the root Bubble Tea model for the hoist TUI.
//
// Shape (the convention in AGENTS.md §4.8, first adopted here):
//
//   - internal/app holds the root tea.Model: the screen stack, the window size, the theme
//     (built once from tea.BackgroundColorMsg) and the global keys (q / ctrl+c quit). It is
//     the only tea.Model in the program; everything else is a screen.
//   - internal/app/<screen> holds one screen as its own model with a New(...) constructor,
//     a pure Update that returns the screen's concrete type, a View() string, and
//     SetSize/SetStyles setters the root calls on resize and theme change. The root wraps
//     each screen in a tiny adapter (see screen.go) so screens never import this package.
//   - A screen's derived data — what it shows, before any styling — lives in a separate
//     file with no terminal dependency (matrix/cells.go) so it is unit-testable as plain
//     values; the model file only lays that data out.
//   - internal/ui holds the shared Styles palette, the frame chrome (ui.Frame, ui.Box,
//     ui.Columns, ui.Dialog), relative time and the status-bar helper; it imports Lip Gloss
//     and x/ansi (width and strip), no Bubbles.
//   - No layout library (AGENTS.md §4.7) means no flexbox-for-terminals dependency: a
//     screen's View is ui.Frame{...}.Render(styles, w, h), built on lipgloss's own borders
//     and joins, with the footer always the last line. Hand-assembled box characters are the
//     thing that rule forbids (§4.8, the M10 amendment).
//   - Every screen's tests render through internal/ui/uitest: goldens at 80×24 and 120×40,
//     and keypresses driven through uitest.Keys rather than fields set by hand.
//
// The stack gained pop with the first screen that opens on top of the matrix
// (internal/app/plan): a screen never calls back into app to push or pop itself — that
// would mean every screen importing app, which is exactly the cycle this package's shape
// exists to avoid. Instead a screen emits a message of its own concrete type (matrix's
// OpenPlanMsg to push the plan screen, plan's BackMsg to pop it) and the root recognizes
// those types in its own Update switch, since app is the one package that already imports
// every screen. New navigation should follow the same shape rather than growing a second
// one: define the message where the emitting screen lives, handle it in app.go.
//
// # A background drive belongs to the session, not to the screen watching it
//
// internal/app/session.Controller — a value type, held as one field on the root Model — is
// the one place a promotion or deploy is actually started, resumed, stepped, abandoned or
// listed. Every background command it issues (a Start/Resume round trip, one Driver.Step
// poll, a listing) reaches the root as a session.Event, and the root's own Update has
// exactly one case for it (kept out of internal/parity's navigation registry on purpose —
// session.Event is deliberately not named *Msg, since it plumbs nowhere the operator can
// trigger directly; see internal/app/session's own doc comment). The flight screen that
// shows a drive's progress owns none of it: it is built already attached
// (flight.NewAttached) to one Controller entry, and every later change reaches that same
// instance through Mirror, driven by the root's own apply(app.go) routing session.Change
// values by BuildID. A screen requests a step (flight.ReobserveMsg, OverrideCINoneMsg,
// AbandonMsg) rather than taking one, so nothing about driving a promotion is owned by
// whichever screen instance happens to be showing it. Today (Train 2's wiring PR) the root
// still stops that entry's drive when its screen is popped (Esc calls session.Controller.Stop
// or CancelBuild, x calls Stop) — deliberately unchanged behaviour, so this PR is wiring only.
// A later PR in the same train lets a drive outlive its screen and re-attach on demand,
// which this shape is what makes possible: the Controller, not the screen, already owns the
// ctx and the goroutine.
package app
