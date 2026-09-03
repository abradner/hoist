package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
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

// flightScreen adapts flight.Model the same way. It is pushed on top of the plan screen
// when the operator confirms a plan (plan.StartMsg, handled in app.go) — the screen that
// shows the promotion actually driving through engine.AllSteps.
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
