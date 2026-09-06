package matrix

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/ui"
)

// The in-flight pane (M10, #85 screen 05/06): what is promoting right now, on the entry
// screen, sized to the terminal. "1 in flight" is a number an operator cannot act on; the
// pane answers what it is doing, what it is waiting for, and what to type.
//
// The pane is a guest on the matrix: it takes rows only when the table has them to spare.
// Expanded (id, envs, the step strip, the blocked reason and the command) when
// expandedRows fit under the table; compact (one line per promotion: id, target, verdict,
// age) when compactRows do; folded into the notes section as one line when not even that
// fits. Absent when nothing is in flight.

// ResumeMsg asks the root to re-drive one in-flight promotion on the flight screen (r, or
// enter on the pane) — the TUI's `hoist resume <id>`.
type ResumeMsg struct {
	ID string
}

// SetInFlight replaces what the pane shows. err is the listing's own failure (the state
// directory unreadable), shown in the pane's place; a per-promotion re-observation failure
// travels inside its Summary.
func (m Model) SetInFlight(list []flight.Summary, err error) Model {
	m.inflight = append([]flight.Summary(nil), list...)
	m.inflightErr = ""
	if err != nil {
		m.inflightErr = err.Error()
	}
	return m
}

// InFlight is what the pane currently shows.
func (m Model) InFlight() []flight.Summary { return m.inflight }

// WithNow fixes the clock the pane ages promotions with (tests); the default is time.Now.
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// minTableRows is the least the table keeps (header, two families, one spare) before the
// pane folds into the notes section instead of taking rows from it.
const minTableRows = 4

// inflightPane renders the pane for the rows the frame can give it, or "" when there is
// nothing to show or no room even for the compact form (then inflightLine goes into notes).
func (m Model) inflightPane(rows int) string {
	n := len(m.inflight)
	if n == 0 && m.inflightErr == "" {
		return ""
	}
	if m.inflightErr != "" {
		if rows < 3 {
			return ""
		}
		return ui.Box(m.styles, "in flight", []string{m.styles.Warn.Render(ansi.Wordwrap("cannot list promotions: "+m.inflightErr, max(m.width-2, 1), ""))}, m.width)
	}
	// Decided by measuring, not by counting: the step strip wraps at narrow widths, so the
	// expanded form's height depends on the terminal.
	title := fmt.Sprintf("in flight (%d)", n)
	if expanded := ui.Box(m.styles, title, m.expandedSections(), m.width); lipgloss.Height(expanded) <= rows {
		return expanded
	}
	lines := make([]string, 0, n)
	for _, s := range m.inflight {
		lines = append(lines, m.compactLine(s))
	}
	if compact := ui.Box(m.styles, title, []string{strings.Join(lines, "\n")}, m.width); lipgloss.Height(compact) <= rows {
		return compact
	}
	return ""
}

// paneRows is how many rows inflightPane will take for a given budget — the same decision,
// so layout can subtract it from the table.
func (m Model) paneRows(rows int) int {
	if p := m.inflightPane(rows); p != "" {
		return strings.Count(p, "\n") + 1
	}
	return 0
}

// expandedSections is one section per promotion: two header lines, a rule, then the action.
func (m Model) expandedSections() []string {
	out := make([]string, 0, len(m.inflight)*2)
	for _, s := range m.inflight {
		head := m.styles.Accent.Render(s.ID) + "   " + m.styles.Title.Render(pair(s)) + m.styles.Dim.Render("   started "+ui.Ago(m.now(), s.StartedAt))
		strip := m.styleStrip(s)
		text, command := s.Action()
		var action string
		switch {
		case command != "":
			action = m.styles.Warn.Render(text) + "    " + m.styles.Accent.Render(command)
		case text != "":
			action = m.styles.Warn.Render(text)
		default:
			action = m.styles.Dim.Render(s.Verdict())
		}
		out = append(out, head+"\n"+strip, action)
	}
	return out
}

// styleStrip colours the step strip — done good, active warn, blocked bad, unreached dim —
// and, when the whole pipeline does not fit the width, breaks it after the merge (or the
// direct push) so the post-merge steps sit on their own line, as the mockup draws them,
// rather than being truncated away.
func (m Model) styleStrip(s flight.Summary) string {
	parts := strings.Split(s.StepStrip(), "  ")
	if ansi.StringWidth(s.StepStrip()) > m.width-2 {
		for i, p := range parts {
			if strings.HasSuffix(p, " merge") || strings.HasSuffix(p, " push to base") {
				head := m.styleParts(parts[:i+1])
				tail := m.styleParts(parts[i+1:])
				return strings.Join(head, "  ") + "\n" + strings.Join(tail, "  ")
			}
		}
	}
	return strings.Join(m.styleParts(parts), "  ")
}

func (m Model) styleParts(parts []string) []string {
	parts = append([]string(nil), parts...)
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, flight.StripDone):
			parts[i] = m.styles.Good.Render(p)
		case strings.HasPrefix(p, flight.StripActive):
			parts[i] = m.styles.Warn.Render(p)
		case strings.HasPrefix(p, flight.StripBlocked):
			parts[i] = m.styles.Bad.Render(p)
		default:
			parts[i] = m.styles.Dim.Render(p)
		}
	}
	return parts
}

// compactLine is the narrow form: "⟳ 5pr6sd333t → app-production   blocked on approval · 12m".
func (m Model) compactLine(s flight.Summary) string {
	return m.styles.Accent.Render("⟳ "+s.ID+" → "+s.Target) + "   " + m.styles.Warn.Render(s.Verdict()) + m.styles.Dim.Render(" · "+ui.Span(m.now().Sub(s.StartedAt)))
}

// inflightLine is the one-line fold for the notes section when no pane fits at all.
func (m Model) inflightLine() string {
	if len(m.inflight) == 0 {
		return ""
	}
	verdicts := make([]string, 0, len(m.inflight))
	for _, s := range m.inflight {
		verdicts = append(verdicts, s.ID+" "+s.Verdict())
	}
	return m.styles.Accent.Render(fmt.Sprintf("⟳ %d in flight: ", len(m.inflight))) + strings.Join(verdicts, "; ")
}

func pair(s flight.Summary) string {
	if s.Source == "" {
		return "deploy → " + s.Target
	}
	return s.Source + " → " + s.Target
}
