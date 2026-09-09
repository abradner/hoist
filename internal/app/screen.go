package app

import (
	tea "charm.land/bubbletea/v2"

	appconfig "github.com/abradner/hoist/internal/app/config"
	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/ui"
)

// Screen is what the root drives. Screens are values: every method returns the updated
// screen rather than mutating, so the root model stays a pure tea.Model.
type Screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (Screen, tea.Cmd)
	View() string
	SetSize(width, height int) Screen
	SetStyles(ui.Styles) Screen
	// CapturesText reports whether the screen is currently in a mode where an ordinary
	// letter key like "q" is text the operator is typing — a filter query, a huh field's own
	// "/" filter — rather than a command. The root's global quit binding (app.go's Update)
	// checks this before treating "q" as quit, and only forwards the key to the screen as
	// usual when it's true; ctrl+c is unaffected and always quits (round 5, finding 3: the
	// global binding used to run unconditionally, before any screen's own key handling ever
	// saw the press, so typing "q" into the tag picker's filter box quit the whole program
	// instead of typing). A screen with no such mode returns false unconditionally.
	CapturesText() bool
}

// matrixScreen adapts matrix.Model, whose methods return the concrete type, to Screen. Each
// screen package gets one of these so it never has to import app.
type matrixScreen struct{ matrix.Model }

func (s matrixScreen) Init() tea.Cmd { return s.Model.Init() }

func (s matrixScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return matrixScreen{m}, cmd
}

func (s matrixScreen) SetSize(width, height int) Screen {
	return matrixScreen{s.Model.SetSize(width, height)}
}

func (s matrixScreen) SetStyles(st ui.Styles) Screen {
	return matrixScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen: true while the matrix's image chooser is open (its filter
// takes letters).
func (s matrixScreen) CapturesText() bool { return s.Model.CapturesText() }

// planScreen adapts plan.Model the same way. It is pushed on top of the matrix when the
// operator asks to plan a promotion (matrix.OpenPlanMsg, handled in app.go) — the first
// screen doc.go's "pop arrives with the first screen that opens on top of the matrix" was
// written for.
type planScreen struct{ plan.Model }

func (s planScreen) Init() tea.Cmd { return s.Model.Init() }

func (s planScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return planScreen{m}, cmd
}

func (s planScreen) SetSize(width, height int) Screen {
	return planScreen{s.Model.SetSize(width, height)}
}

func (s planScreen) SetStyles(st ui.Styles) Screen {
	return planScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen, delegating to plan.Model's own query of whichever huh field
// (envSelect or multiSelect) is currently active — see plan.Model.CapturesText's own doc comment.
func (s planScreen) CapturesText() bool { return s.Model.CapturesText() }

// flightScreen adapts flight.Model the same way. It is pushed on top of the plan screen
// when the operator confirms a plan (plan.StartMsg, handled in app.go). app.go currently
// pushes it with a nil DriveFunc — read-only, not yet actually driving anything — since
// internal/app has no repoFullName, CI/approval policy, or git/forge adaptor to build a real
// engine.PromotionState from (cmd/hoist owns wiring that in, still a pending follow-up; see
// app.go's own comment on its plan.StartMsg handler for the full reasoning).
type flightScreen struct{ flight.Model }

func (s flightScreen) Init() tea.Cmd { return s.Model.Init() }

func (s flightScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return flightScreen{m}, cmd
}

func (s flightScreen) SetSize(width, height int) Screen {
	return flightScreen{s.Model.SetSize(width, height)}
}

func (s flightScreen) SetStyles(st ui.Styles) Screen {
	return flightScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen, delegating to flight.Model: true while the c gesture's
// confirmation is open, so the root's q does not quit mid-decision.
func (s flightScreen) CapturesText() bool { return s.Model.CapturesText() }

// tagsScreen adapts tags.Model the same way. It is pushed on top of the matrix when the
// operator asks to pick a new tag (matrix.OpenTagsMsg, handled in app.go).
type tagsScreen struct{ tags.Model }

func (s tagsScreen) Init() tea.Cmd { return s.Model.Init() }

func (s tagsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return tagsScreen{m}, cmd
}

func (s tagsScreen) SetSize(width, height int) Screen {
	return tagsScreen{s.Model.SetSize(width, height)}
}

func (s tagsScreen) SetStyles(st ui.Styles) Screen {
	return tagsScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen, delegating to tags.Model's own filtering flag — see
// tags.Model.CapturesText's own doc comment.
func (s tagsScreen) CapturesText() bool { return s.Model.CapturesText() }

// deployScreen adapts deploy.Model. It is pushed on top of the tag picker once the operator
// chooses a tag (tags.SelectedMsg/DirectRequestedMsg, handled in app.go) and is the last thing
// between that choice and a real write.
type deployScreen struct{ deploy.Model }

func (s deployScreen) Init() tea.Cmd { return s.Model.Init() }

func (s deployScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return deployScreen{m}, cmd
}

func (s deployScreen) SetSize(width, height int) Screen {
	return deployScreen{s.Model.SetSize(width, height)}
}

func (s deployScreen) SetStyles(st ui.Styles) Screen {
	return deployScreen{s.Model.SetStyles(st)}
}

// restartScreen adapts restart.Model. Pushed on top of the matrix by R, and unlike the deploy
// path the matrix stays beneath it: a restart is small and repeatable, and backing out should
// land on the cell it started from.
type restartScreen struct{ apprestart.Model }

func (s restartScreen) Init() tea.Cmd { return s.Model.Init() }

func (s restartScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return restartScreen{m}, cmd
}

func (s restartScreen) SetSize(width, height int) Screen {
	return restartScreen{s.Model.SetSize(width, height)}
}

func (s restartScreen) SetStyles(st ui.Styles) Screen {
	return restartScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen: true only while the production confirmation is open.
func (s restartScreen) CapturesText() bool { return s.Model.CapturesText() }

// configScreen adapts config.Model (internal/app/config). Pushed on top of the matrix by C
// (matrix.OpenConfigMsg, handled in app.go); read-only, so nothing beneath it changes while
// it is open and esc lands back where it started.
type configScreen struct{ appconfig.Model }

func (s configScreen) Init() tea.Cmd { return s.Model.Init() }

func (s configScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return configScreen{m}, cmd
}

func (s configScreen) SetSize(width, height int) Screen {
	return configScreen{s.Model.SetSize(width, height)}
}

func (s configScreen) SetStyles(st ui.Styles) Screen {
	return configScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen: the config screen has no text-entry mode.
func (s configScreen) CapturesText() bool { return false }

// watchScreen adapts watch.Model. Pushed on top of the matrix by w; the matrix stays beneath
// it, since watching changes nothing and esc should land on the cell it started from.
type watchScreen struct{ watch.Model }

func (s watchScreen) Init() tea.Cmd { return s.Model.Init() }

func (s watchScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, cmd := s.Model.Update(msg)
	return watchScreen{m}, cmd
}

func (s watchScreen) SetSize(width, height int) Screen {
	return watchScreen{s.Model.SetSize(width, height)}
}

func (s watchScreen) SetStyles(st ui.Styles) Screen {
	return watchScreen{s.Model.SetStyles(st)}
}

// CapturesText implements Screen, delegating to watch.Model: false today, since the watch
// screen has no text-entry mode — but the model, not this adapter, is what says so.
func (s watchScreen) CapturesText() bool { return s.Model.CapturesText() }
