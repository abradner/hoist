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
//   - internal/ui holds the shared Styles palette and the status-bar helper; it imports
//     Lip Gloss and x/ansi (width and strip), no Bubbles.
//   - No layout library (AGENTS.md §4.7): screens compose strings with strings.Join and the
//     Bubbles components they embed.
//
// The stack gained pop with the first screen that opens on top of the matrix
// (internal/app/plan): a screen never calls back into app to push or pop itself — that
// would mean every screen importing app, which is exactly the cycle this package's shape
// exists to avoid. Instead a screen emits a message of its own concrete type (matrix's
// OpenPlanMsg to push the plan screen, plan's BackMsg to pop it) and the root recognizes
// those types in its own Update switch, since app is the one package that already imports
// every screen. New navigation should follow the same shape rather than growing a second
// one: define the message where the emitting screen lives, handle it in app.go.
package app
