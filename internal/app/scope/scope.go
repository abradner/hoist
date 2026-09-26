package scope

import (
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ID names one screen instance, or one reload of the same instance, that asked for something
// asynchronous. The zero value never equals anything New returns (New starts at 1), so a Model
// left with a zero-value ID by an oversight drops every stamped result rather than silently
// accepting one it never asked for.
type ID uint64

var next atomic.Uint64

// New allocates the next ID, process-wide unique. A screen calls it once at construction, and
// again whenever it reloads itself in place rather than being replaced by a fresh Model — see
// the package doc.
func New() ID {
	return ID(next.Add(1))
}

// Stamped is implemented by an async result message that carries the ID of whoever asked for
// it. Foreign is the only thing that reads it.
type Stamped interface {
	StampedBy() ID
}

// Result wraps a plain value with the ID that asked for it — the carrier Do and After return.
type Result[T any] struct {
	From ID
	V    T
}

// StampedBy implements Stamped.
func (r Result[T]) StampedBy() ID { return r.From }

// Do runs f and stamps its result with id, wrapped as a Result[T] — the scope-aware analogue of
// `func() tea.Msg { return f() }` for an async call whose result message has no field of its
// own to carry the id in.
func Do[T any](id ID, f func() T) tea.Cmd {
	return func() tea.Msg {
		return Result[T]{From: id, V: f()}
	}
}

// After schedules v to arrive after d, stamped with id — the scope-aware analogue of tea.Tick
// for a fixed value rather than a func of the fire time.
func After[T any](id ID, d time.Duration, v T) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return Result[T]{From: id, V: v}
	})
}

// Foreign reports whether msg is a Stamped result whose ID differs from id — the guard every
// adopting screen's Update starts with. A msg that isn't Stamped at all (a key press, a window
// resize, a navigation message this screen itself just emitted) is never foreign; Foreign only
// ever says yes about a message this package's own convention applies to.
func Foreign(id ID, msg tea.Msg) bool {
	s, ok := msg.(Stamped)
	return ok && s.StampedBy() != id
}
