// Package scope stamps an async command's result with the screen instance that issued it, so
// Update can tell a request its own current instance is still waiting on from one an earlier
// instance of the same kind of screen never got to see land.
//
// A screen is a value (AGENTS.md §4.8): esc, then reopening the same key, builds a brand new
// Model rather than mutating the old one. That does not by itself stop the old instance's
// outstanding tea.Cmd — a real network or cluster call fired from Init or a keypress keeps
// running, and nothing in bubbletea reaches into a Cmd already in flight to cancel it (that is
// a separate, later piece of work: an owned context.Context, not this package's job). Its result
// message still arrives, is still routed to whichever screen is now on top by concrete type
// (app.go's own default forward-to-top-screen case), and — because a new instance of the same
// screen kind has since been pushed in the old one's place — can land on a Model that never
// asked for it. plan.Model's loadedMsg racing a second p press and watch.Model's snapshotMsg
// racing w on a different family are the same shape: an Update that trusts whatever tea.Msg
// arrives shows the earlier instance's stale answer as if it were the current one's.
//
// ID is a lightweight scope: New allocates one, process-wide unique, once at construction — and
// again wherever a Model reloads itself in place rather than being replaced by a fresh instance
// (plan's own override rebuild is today's one example), since a stale in-flight result for the
// request that reload superseded is exactly the same hazard, one instance re-asking a newer
// question of itself. Do and After build a tea.Cmd whose result carries that ID via Stamped;
// Result[T] is the carrier for a message with no field of its own to hold one — every adopting
// screen still owns its own named message types, just returned as scope.Result[thatType] from
// Do or After instead of a bare `func() tea.Msg`.
//
// Every adopting screen's Update starts with `if scope.Foreign(m.id, msg) { return m, nil }`:
// drop it and read nothing else out of it, and let whatever request the current instance itself
// issued answer in its own time. Never stamp a spinner or cursor-blink tick — internal/ui/uitest's
// own Drain already drops those unconditionally wherever a test unwinds a batch, and a screen
// that read its own current id from inside a long recurring tick chain would only ever see the
// id frozen into that chain's first tea.Tick, not the instance's actual current one.
package scope
