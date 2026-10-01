package session

import (
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
)

// Event is the private message shape every Controller-issued tea.Cmd produces. It satisfies
// tea.Msg like any Go value, but is deliberately not exported with a Msg suffix and carries an
// unexported method — see doc.go's own "Design" section for why: internal/parity's own parser
// only ever looks for `case pkg.XMsg` in internal/app/app.go, and treating this package's
// internal plumbing as a navigation message the operator can trigger would need a parity row
// naming something that plumbs nowhere. internal/app/app.go's Update has exactly one
// `case session.Event:` for it; nothing outside this
// package ever needs to construct or type-switch on one of the concrete types below.
type Event interface {
	sessionEvent()
}

// builtMsg is delivered once a Start or Resume call's background command finishes — the
// StartPromotion/Resume round trip itself (preflight, the claim, the first durable save).
// build/gen identify which entry this belongs to: a gen that no longer matches that entry's own
// current generation means it was re-armed (Poke, an override) or superseded/abandoned since —
// dropped outright, never adopted, mirroring app.go's own buildGen guard one layer down.
type builtMsg struct {
	build BuildID
	gen   uint64
	state engine.PromotionState
	drive Driver
	err   error
}

func (builtMsg) sessionEvent() {}

// progressMsg carries one preflight/drive progress line for one build, off the channel
// Hooks.Progress writes into — delivered as an Event rather than written directly into
// Controller state from whatever goroutine StartPromotion/Resume/Step is running in (no
// goroutine ever writes Controller state directly).
type progressMsg struct {
	build BuildID
	gen   uint64
	line  string
	ok    bool
}

func (progressMsg) sessionEvent() {}

// stepMsg is delivered once one Driver.Step call returns.
type stepMsg struct {
	build BuildID
	gen   uint64
	tick  service.Tick
	err   error
}

func (stepMsg) sessionEvent() {}

// pollMsg fires the next Step call for one entry, after its own Tick.Wait (capped to this
// package's own MinTick floor and to what's left of the entry's deadline).
type pollMsg struct {
	build BuildID
	gen   uint64
}

func (pollMsg) sessionEvent() {}

// listMsg carries one Backend.List call's answer.
type listMsg struct {
	gen  uint64
	list []service.Listed
	err  error
}

func (listMsg) sessionEvent() {}

// listTickMsg fires the next List call, at Config.ListEvery — dropped if gen no longer matches
// Controller's own current listing generation (Relist bumped it since this was scheduled), so a
// superseded tick chain dies rather than running alongside a newer one forever.
type listTickMsg struct{ gen uint64 }

func (listTickMsg) sessionEvent() {}

// abandonWaitMsg re-checks whether a busy entry has actually gone idle after Abandon signalled
// its ctx — mirrors app.go's own abandonWaitMsg one layer down: a plain context cancel is not a
// join, so this polls until the entry's own busy flag clears (or AbandonTimeout elapses) before
// Backend.Abandon is actually called.
type abandonWaitMsg struct {
	build   BuildID
	gen     uint64
	attempt int
}

func (abandonWaitMsg) sessionEvent() {}

// abandonedMsg carries Backend.Abandon's answer for one entry.
type abandonedMsg struct {
	build BuildID
	gen   uint64
	id    string
	lines []string
	err   error
}

func (abandonedMsg) sessionEvent() {}

var _ tea.Msg = builtMsg{} // every Event must also satisfy tea.Msg — any value does; documents the intent
