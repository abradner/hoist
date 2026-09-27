package scope

import (
	"context"
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

// Scope is an owned context.Context paired with the ID that names it. Open ties both to a
// screen instance's own lifetime: Close cancels every DoCtx call still outstanding for it the
// moment the root actually removes that screen from the stack (internal/app's own pop/truncate),
// rather than leaving each one to run until its own per-call timeout fires — the two mechanisms
// are complementary, not redundant: a screen that is never popped (the matrix) still needs
// DoCtx's timeout, and a screen popped in the middle of a call still needs Close's immediate
// cancellation.
type Scope struct {
	ID
	ctx    context.Context
	cancel context.CancelFunc
}

// Open starts a new Scope: a fresh ID (New) and a cancellable context derived from
// context.Background() — the one place besides internal/app/session's own long-lived
// controller ctx that hoist's TUI is allowed to start a context from nothing, since a freshly
// constructed screen instance has no parent context to inherit (AGENTS.md §4.8, "each screen
// owns its ctx").
func Open() Scope {
	ctx, cancel := context.WithCancel(context.Background())
	return Scope{ID: New(), ctx: ctx, cancel: cancel}
}

// Close cancels this Scope's ctx. Safe to call more than once (context.CancelFunc already is)
// and safe on a zero Scope, where it is a no-op — a screen adapter that never got as far as
// opening one has nothing to cancel.
func (s Scope) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// Ctx returns this Scope's own context — context.Background() for a zero Scope, so a caller
// never has to nil-check before using it. Prefer DoCtx when building a single command; Ctx is
// for a caller with its own multi-command shape to fit the context into (internal/app/tags'
// several independent fetches, for instance).
func (s Scope) Ctx() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// DoCtx runs f with a context derived from s — bounded by both s's own cancellation and by
// timeout, whichever comes first — and stamps the result with s.ID exactly as Do does: the
// ctx-bound cousin of Do, for a call that must also die the moment its screen is closed.
func DoCtx[T any](s Scope, timeout time.Duration, f func(context.Context) T) tea.Cmd {
	id := s.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(s.Ctx(), timeout)
		defer cancel()
		return Result[T]{From: id, V: f(ctx)}
	}
}

// Timeout derives a bounded, standalone context (context.Background() wrapped by
// context.WithTimeout) for a caller with no Scope of its own to derive from — the matrix
// screen's per-env cluster reads and repo refresh, in practice: the matrix is never replaced by
// a fresh instance the way a pushed screen is, so it has no "closed" moment of its own to tie a
// Scope to, only a per-call ceiling. Kept inside this package so a bare context.Background()
// never has to appear in a screen package directly (AGENTS.md §4.8's own acceptance grep for
// this convention).
func Timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
