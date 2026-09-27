package scope

import (
	"testing"
	"time"
)

func TestNewIsUniqueAndNeverZero(t *testing.T) {
	a, b := New(), New()
	if a == 0 || b == 0 {
		t.Fatalf("New() returned a zero ID: a=%d b=%d", a, b)
	}
	if a == b {
		t.Fatalf("two New() calls returned the same ID: %d", a)
	}
}

func TestDoStampsTheResultWithID(t *testing.T) {
	id := New()
	cmd := Do(id, func() string { return "hello" })
	msg := cmd()
	res, ok := msg.(Result[string])
	if !ok {
		t.Fatalf("Do's cmd returned %T, want Result[string]", msg)
	}
	if res.From != id || res.V != "hello" {
		t.Fatalf("Result = %+v, want From=%d V=hello", res, id)
	}
}

func TestAfterStampsTheResultWithID(t *testing.T) {
	id := New()
	// tea.Tick's own Cmd blocks for the given duration (real wall time, AGENTS.md §8: no fake
	// clock is threaded through this package — the caller's own Config.After-shaped seam is
	// what tests further up inject) then calls the wrapped func directly as the tea.Msg, so a
	// short duration is enough to prove the value it fires is stamped.
	cmd := After(id, time.Millisecond, 42)
	msg := cmd()
	res, ok := msg.(Result[int])
	if !ok {
		t.Fatalf("After's cmd returned %T, want Result[int]", msg)
	}
	if res.From != id || res.V != 42 {
		t.Fatalf("Result = %+v, want From=%d V=42", res, id)
	}
}

func TestForeignDropsAnotherIDsResult(t *testing.T) {
	mine, theirs := New(), New()
	msg := Result[int]{From: theirs, V: 1}
	if !Foreign(mine, msg) {
		t.Error("a Result stamped by another ID should be Foreign")
	}
	if Foreign(mine, Result[int]{From: mine, V: 1}) {
		t.Error("a Result stamped by my own ID must not be Foreign")
	}
}

func TestForeignIgnoresUnstampedMessages(t *testing.T) {
	id := New()
	if Foreign(id, "not stamped") {
		t.Error("a message that isn't Stamped at all must never read as Foreign")
	}
}
