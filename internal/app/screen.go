package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/ui"
)

// Screen is what the root drives. Screens are values: every method returns the updated
// screen rather than mutating, so the root model stays a pure tea.Model.
type Screen interface {
	Update(tea.Msg) (Screen, tea.Cmd)
	View() string
	SetSize(width, height int) Screen
	SetStyles(ui.Styles) Screen
}

// matrixScreen adapts matrix.Model, whose methods return the concrete type, to Screen. Each
// screen package gets one of these so it never has to import app.
type matrixScreen struct{ matrix.Model }

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
