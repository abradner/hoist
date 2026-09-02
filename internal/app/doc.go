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
// The stack has push only today: pop arrives with the first screen that opens on top of the
// matrix, so there is no dead code to keep honest until then.
package app
