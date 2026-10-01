package activity

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
)

// BackMsg is emitted on esc; the root pops the screen (internal/app/app.go — l, handled
// generically for any keyed screen, is what pushes it: the same New-on-open,
// adapter-in-app.go shape every other read-only screen in this package uses, config.Model in
// particular).
type BackMsg struct{}

// keyMap is this screen's own key vocabulary on top of the viewport's own paging bindings
// (newViewport). Home/End (P2-8, T3 review) jump the log to its ends — the design's own row
// for this screen, missing until now.
type keyMap struct {
	Back      key.Binding
	Home, End key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Back: keys.Esc.Bubbles(),
		Home: keys.Home.Bubbles(),
		End:  keys.End.Bubbles(),
	}
}

// Model is the activity log screen (l on the matrix and, per the proposed keymap, every other
// screen — docs/audit/2026-09-ux-arch-audit.md "Proposed keymap"). It shows every Entry the
// root's own activity.Log has recorded, oldest first, full text, redacted and wrapped but never
// truncated (Lines' own doc comment) — the operator's way to read a result or an error in full
// once the bottom row's own one-line summary is all that is left on screen. Value-typed like
// every other screen (AGENTS.md §4.8).
//
// New takes a snapshot of the log as it stood when the operator pressed l, not a live feed: like
// config.Model's own already-marshalled text, what this screen shows is exactly what the root
// held at the moment it opened, never re-fetched or updated in place while it's open. An entry
// added while the log is open next shows the next time l is pressed.
type Model struct {
	log Log
	now func() time.Time

	body          viewport.Model
	keys          keyMap
	styles        ui.Styles
	width, height int
}

// New builds the screen over log. now ages every entry (ui.Ago); nil defaults to time.Now, and
// every real caller (app.go) supplies one, mirroring every other relative-time screen's own
// convention (AGENTS.md §4.8).
func New(log Log, now func() time.Time) Model {
	if now == nil {
		now = time.Now
	}
	return Model{log: log, now: now, body: newViewport(), keys: defaultKeyMap()}
}

// newViewport binds only the paging keys, the same set config.Model's own body uses.
func newViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = keys.ViewportKeyMap()
	v.MouseWheelEnabled = false
	return v
}

// Init has nothing to fetch: everything the screen shows arrived in New.
func (m Model) Init() tea.Cmd { return nil }

// Update handles esc (BackMsg) and forwards everything else to the viewport.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	if key.Matches(kp, m.keys.Back) {
		return m, func() tea.Msg { return BackMsg{} }
	}
	// Laid out on this copy first — View lays out its own, so the retained viewport would
	// otherwise still be the zero-sized one New built (config.Model's own comment notes the
	// same gotcha).
	m = m.layout()
	if key.Matches(kp, m.keys.Home) {
		m.body.GotoTop()
		return m, nil
	}
	if key.Matches(kp, m.keys.End) {
		m.body.GotoBottom()
		return m, nil
	}
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

// layout sizes the body to the frame's one section and loads the rendered lines into it.
func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	w := max(m.width-2, 1)
	m.body.SetWidth(w)
	m.body.SetHeight(ui.BodyHeight(m.height, 1))
	m.body.SetContent(strings.Join(Lines(m.log, m.now, w), "\n"))
	return m
}

// pluralEntries is ui.Plural's own "%ss" rule corrected for entry's irregular plural
// ("entries", never "entrys") — the one noun on this screen ui.Plural cannot be used for as-is
// (P2-13, T3 review: this always read "N entries", even "1 entries").
func pluralEntries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// View renders the frame: the title, the log in its viewport, and the footer.
func (m Model) View() string {
	m = m.layout()
	status := pluralEntries(m.log.Len())
	title := fmt.Sprintf("hoist · activity · %s", status)
	hints := []keys.Hint{{B: keys.Esc, Long: "esc back", Pri: 0}}
	return ui.Frame{
		Title:    title,
		Sections: []string{m.body.View()},
		Footer:   keys.Footer(m.styles, m.width, status, hints, true),
	}.Render(m.styles, m.width, m.height)
}

// KeyScreen implements the root's keyed interface (internal/app/screen.go).
func (m Model) KeyScreen() keys.Screen { return keys.ScrActivity }
