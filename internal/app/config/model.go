// Package config is the read-only config screen (#104): what `hoist config show` and
// `hoist config path` print, on the TUI. It takes the already-marshalled, already-redacted
// YAML and the path it came from as plain strings — never internal/config itself — so the
// screen can show nothing the CLI would not, and cmd/hoist stays the one place that knows
// both the loader and the screen (AGENTS.md §4.8, the buildResolveFunc rule). Nothing here
// is editable: the file is the operator's, and a screen that wrote it would be a second
// loader to keep in step with the first.
package config

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/ui"
)

// BackMsg is emitted on esc; the root pops the screen (internal/app/app.go).
type BackMsg struct{}

// keyMap is this screen's key vocabulary on top of the root's global quit keys and the
// viewport's own paging bindings (newViewport).
type keyMap struct {
	Back, Top, Bottom key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Back:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Top:    key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
		Bottom: key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "end")),
	}
}

// Model is the screen. Value-typed: every method returns the updated copy.
type Model struct {
	path  string
	found bool
	text  string

	body          viewport.Model
	keys          keyMap
	styles        ui.Styles
	width, height int
}

// New builds the screen. path is where the config was read from, or looked for; found says
// which; text is the effective config as the CLI prints it (defaults filled, op refs
// redacted — the caller's job, since this package must not import the loader).
func New(path string, found bool, text string) Model {
	return Model{path: path, found: found, text: text, body: newViewport(), keys: defaultKeyMap()}
}

// newViewport binds only the paging keys — the same set the tag picker's commit body uses
// (#120): pgup/pgdn, ctrl+u/ctrl+d; g/G are handled by Update since the viewport's own
// defaults for them are "home"/"end" names this screen does not otherwise use.
func newViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
		Up:           key.NewBinding(key.WithKeys("up", "k")),
	}
	v.MouseWheelEnabled = false
	return v
}

// Init has nothing to fetch: everything the screen shows arrived in New.
func (m Model) Init() tea.Cmd { return nil }

// Update handles esc (BackMsg) and g/G, and forwards everything else to the viewport.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch {
	case key.Matches(kp, m.keys.Back):
		return m, func() tea.Msg { return BackMsg{} }
	case key.Matches(kp, m.keys.Top):
		m = m.layout()
		m.body.GotoTop()
		return m, nil
	case key.Matches(kp, m.keys.Bottom):
		m = m.layout()
		m.body.GotoBottom()
		return m, nil
	}
	// Laid out on this copy first — View lays out its own, so the retained viewport would
	// otherwise still be the zero-sized one New built (flight's log, Copilot #124).
	m = m.layout()
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(kp)
	return m, cmd
}

// SetSize records the terminal size and sizes the viewport to what the frame leaves.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	return m.layout()
}

// SetStyles applies the palette.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	return m
}

// CapturesText is false: no key on this screen is text.
func (m Model) CapturesText() bool { return false }

// layout sizes the body to the frame's one section and loads the text into it.
func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	m.body.SetWidth(max(m.width-2, 1))
	m.body.SetHeight(ui.BodyHeight(m.height, 1))
	m.body.SetContent(m.text)
	return m
}

// Title is the frame's title: the file's path when it exists, otherwise the CLI's own
// "no file at …; showing defaults" sentence (`hoist config show`), so both faces name the
// flags-only case in the same words.
func (m Model) Title() string {
	if m.found {
		return m.path
	}
	return "no file at " + m.path + "; showing defaults"
}

// View renders the frame: the title (Title), the YAML in its viewport, and the footer.
func (m Model) View() string {
	m = m.layout()
	left := m.styles.Status.Render("read-only · op refs redacted")
	right := m.styles.Hint.Render("↑/↓ pgup/pgdn ctrl+u/d g/G scroll · esc back")
	return ui.Frame{
		Title:    m.Title(),
		Sections: []string{m.body.View()},
		Footer:   ui.StatusBar(m.width, left, right),
	}.Render(m.styles, m.width, m.height)
}
