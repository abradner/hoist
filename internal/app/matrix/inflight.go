package matrix

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
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
// enter on the pane) — the TUI's `hoist resume <id>`. Build carries the session.Controller
// BuildID for an entry still Building (P1 #2): ID is empty until a real promotion id exists, so
// re-attaching to it before then has nothing else to key by — the root re-attaches by Build
// directly (session.Controller.BuildSnapshot) rather than calling Resume with an empty id, which
// would fail to find anything and, worse, would have started a second drive had Resume("")
// happened to succeed.
type ResumeMsg struct {
	ID    string
	Build session.BuildID
}

// SetInFlight replaces what the pane shows. err is the listing's own failure (the state
// directory unreadable), shown in the pane's place; a per-promotion re-observation failure
// travels inside its Summary.
func (m Model) SetInFlight(list []flight.Summary, err error) Model {
	// A finished promotion is not in flight: `hoist promotions` lists every state file, the
	// pane lists what is still moving, or stuck — and r never offers to resume a done one.
	m.inflight = m.inflight[:0:0]
	for _, s := range list {
		if !s.Done {
			m.inflight = append(m.inflight, s)
		}
	}
	m.inflightErr = ""
	if err != nil {
		m.inflightErr = redact.Strings(err.Error())
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

// inflightPane renders the pane's own content for the rows the frame can give it, or "" when
// there is nothing to show or no room even for the compact form (then inflightLine goes into
// notes). T3-05: this is plain content now, drawn INSIDE the matrix's own frame as one more
// Section (View, below) rather than a separately-bordered ui.Box stacked under it — the
// mockups' own "in flight · N" header line replaces the old box title, so what used to be the
// box's own top/bottom border rows are two fewer rows this needs.
func (m Model) inflightPane(rows int) string {
	n := len(m.inflight)
	if n == 0 && m.inflightErr == "" {
		return ""
	}
	if m.inflightErr != "" {
		if rows < 2 {
			return ""
		}
		return "in flight\n" + m.styles.Warn.Render(ansi.Wordwrap("cannot list promotions: "+m.inflightErr, max(m.width-2, 1), ""))
	}
	title := m.paneTitle(n)
	// Decided by measuring, not by counting: the step strip wraps at narrow widths, so the
	// expanded form's height depends on the terminal.
	if expanded := title + "\n" + strings.Join(m.expandedSections(), "\n"); lipgloss.Height(expanded) <= rows {
		return expanded
	}
	lines := make([]string, 0, n)
	for i, s := range m.inflight {
		lines = append(lines, m.compactLine(i, s))
	}
	if compact := title + "\n" + strings.Join(lines, "\n"); lipgloss.Height(compact) <= rows {
		return compact
	}
	return ""
}

// paneTitle is the pane's own header row: "in flight · N" (v2·01a/b), left-aligned so a
// caller that wants to add a right-aligned companion (a poll countdown, ticketed — see the
// T3-05 report's own "uncomputable" list) has somewhere to put it without reflowing this.
func (m Model) paneTitle(n int) string {
	return m.styles.Title.Render(fmt.Sprintf("in flight · %d", n))
}

// paneRows is how many rows inflightPane will take for a given budget — the same decision,
// so layout can subtract it from the table.
func (m Model) paneRows(rows int) int {
	if p := m.inflightPane(rows); p != "" {
		return strings.Count(p, "\n") + 1
	}
	return 0
}

// paneID is the identity a pane line shows: the real promotion id once one exists, or a
// placeholder for a still-Building live entry (flight.Summary.ID is empty until then, P3 #1/P1
// #2 — an empty string rendered bare read as a layout bug, not "no id yet").
func paneID(s flight.Summary) string {
	if s.ID == "" {
		return "(starting)"
	}
	return s.ID
}

// paneAge is compactLine/inflightLine's "how long has this been going" text: a Building entry's
// StartedAt is the zero time (flight.StartedAt reads it off engine.PromotionState.History, which
// a Building entry has none of yet), so subtracting it from now() would read as an absurd
// multi-decade duration rather than "just started".
func paneAge(m Model, s flight.Summary) string {
	if s.StartedAt.IsZero() {
		return "starting"
	}
	return ui.Span(m.now().Sub(s.StartedAt))
}

// expandedSections is one section per promotion: two header lines, a rule, then the action.
func (m Model) expandedSections() []string {
	out := make([]string, 0, len(m.inflight)*2)
	for i, s := range m.inflight {
		started := "starting…"
		if !s.StartedAt.IsZero() {
			started = "started " + ui.Ago(m.now(), s.StartedAt)
		}
		head := m.styles.Accent.Render(paneID(s)) + "   " + m.styles.Title.Render(pair(s)) + m.styles.Dim.Render("   "+started)
		if s.Live {
			// This session is driving it right now — never true for a listing-only entry
			// (Train 2 design PR 3): distinguish that from "re-observed, last seen here".
			head += "  " + m.styles.Warn.Render("driving")
		}
		head = m.paneMarker(i) + head
		strip := m.styleStrip(s)
		text, command := approvalCopy(s)
		var action string
		switch {
		case command != "" && ansi.StringWidth(text)+4+ansi.StringWidth(command) > m.width-2:
			// The command is the executable part: when the two do not share the line, it
			// gets its own rather than losing its tail to truncation.
			action = m.styles.Warn.Render(text) + "\n    " + m.styles.Accent.Render(command)
		case command != "":
			action = m.styles.Warn.Render(text) + "    " + m.styles.Accent.Render(command)
		case text != "":
			action = m.styles.Warn.Render(text)
		default:
			action = m.styles.Dim.Render(s.Verdict())
		}
		if cd := m.countdownText(s); cd != "" {
			if action != "" {
				action += "  " + m.styles.Dim.Render(cd)
			} else {
				action = m.styles.Dim.Render(cd)
			}
		}
		out = append(out, head+"\n"+strip, action)
	}
	return out
}

// paneMarker is the pane-row cursor (commit 1 of the T3-06 train): "▸ " styled through
// st.Cursor for the entry under m.paneCursor while focus is on the pane (Focus, grid.go's own
// FocusPane), or two plain spaces otherwise — the same width either way, so a row's own text
// never shifts depending on whether it happens to be selected. The marker is text, not only
// colour, for the same reason the grid's own row cursor is (dataRow's own doc comment): goldens
// are ANSI-stripped, so a cursor that only ever changed a background colour would be invisible
// to every test that isn't a style assertion.
func (m Model) paneMarker(i int) string {
	if m.focus == FocusPane && i == m.paneCursor {
		return m.styles.Cursor.Render(selectedMarker)
	}
	return "  "
}

// countdownText is "next check in Ns" for an entry session.Controller has told this session it
// will re-observe on its own (Summary.NextPoll, non-zero exactly when Waiting) — "" for anything
// else, so a caller can append it unconditionally without an extra empty check of its own.
// Rounded to the second so it does not repaint on sub-second jitter, mirroring flight.Model's
// own actionSection countdown, the pane's counterpart once T3-06 lands.
func (m Model) countdownText(s flight.Summary) string {
	if s.NextPoll.IsZero() {
		return ""
	}
	remaining := s.NextPoll.Sub(m.now())
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("next check in %s", remaining.Round(time.Second))
}

// approvalCopy was this pane's own local copy of the wording fix (T3-05, UX-H10) for a
// promotion blocked on approval; T3-06 moved it into flight.ApprovalCopy (exported) once that
// package had its own PR to land the identical fix in, so the pane and flight.Model's own
// actionSection render the same sentence from one place rather than two copies kept in step by
// hand. Kept as a thin local alias rather than rewriting every call site in this file.
func approvalCopy(s flight.Summary) (text, command string) { return flight.ApprovalCopy(s) }

// styleStrip renders the step strip with the shared ui.Step glyph set (T3-05: "one glyph set",
// retiring this pane's own ●/◍/✗/○ in favour of the same ✓/◐/·/✗/⏸ set flight.Model itself
// will move to in T3-06) — derived from each Row's own Glyph rather than parsing
// Summary.StepStrip()'s pre-rendered string, since the mapping needs the state, not the glyph
// character. When the whole pipeline does not fit the width, it breaks after the merge (or the
// direct push) so the post-merge steps sit on their own line, as the mockup draws them, rather
// than being truncated away.
func (m Model) styleStrip(s flight.Summary) string {
	labels := make([]string, len(s.Rows))
	plain := make([]string, len(s.Rows))
	styled := make([]string, len(s.Rows))
	width := 0
	for i, r := range s.Rows {
		labels[i] = flight.Label(r.Step)
		if r.Step == engine.StepPROpened && s.PR != nil && s.PR.Number > 0 {
			labels[i] = fmt.Sprintf("PR #%d", s.PR.Number)
		}
		state := stepState(r)
		plain[i] = ui.StepGlyph(state) + " " + labels[i]
		styled[i] = m.styles.Step(state, labels[i])
		width += ansi.StringWidth(plain[i]) + 2
	}
	if width > m.width-2 {
		for i := range plain {
			if strings.HasSuffix(plain[i], " merge") || strings.HasSuffix(plain[i], " push to base") {
				return strings.Join(styled[:i+1], "  ") + "\n" + strings.Join(styled[i+1:], "  ")
			}
		}
	}
	return strings.Join(styled, "  ")
}

// stepState maps a flight.Row's own Glyph string to the shared ui.StepState enum — the one
// place that translation happens, so the matrix's pane and (once T3-06 lands) the flight
// screen itself read the same five states off the same Row data.
func stepState(r flight.Row) ui.StepState {
	switch r.Glyph {
	case flight.GlyphDone:
		return ui.StepDone
	case flight.GlyphActive:
		return ui.StepActive
	case flight.GlyphWaiting:
		return ui.StepWaiting
	case flight.GlyphBlocked:
		return ui.StepFailed
	default:
		return ui.StepPending
	}
}

// compactLine is the narrow form: "⟳ 5pr6sd333t → app-production   blocked on approval · 12m".
func (m Model) compactLine(i int, s flight.Summary) string {
	// The target can be a 63-character namespace; the verdict is what must survive, so the
	// target is the part that gives way.
	target := ansi.Truncate(s.Target, 24, "…")
	verdict := s.Verdict()
	if s.Live {
		verdict += " · driving"
	}
	line := m.paneMarker(i) + m.styles.Accent.Render("⟳ "+paneID(s)+" → "+target) + "   " + m.styles.Warn.Render(verdict) + m.styles.Dim.Render(" · "+paneAge(m, s))
	if cd := m.countdownText(s); cd != "" {
		line += m.styles.Dim.Render(" · " + cd)
	}
	return line
}

// inflightLine is the one-line fold for the notes section when no pane fits at all.
func (m Model) inflightLine() string {
	if m.inflightErr != "" {
		return m.styles.Warn.Render("⟳ cannot list promotions: " + m.inflightErr)
	}
	if len(m.inflight) == 0 {
		return ""
	}
	verdicts := make([]string, 0, len(m.inflight))
	for _, s := range m.inflight {
		verdicts = append(verdicts, paneID(s)+" "+s.Verdict())
	}
	return m.styles.Accent.Render(fmt.Sprintf("⟳ %d in flight: ", len(m.inflight))) + strings.Join(verdicts, "; ")
}

func pair(s flight.Summary) string {
	if s.Source == "" {
		return "deploy → " + s.Target
	}
	return s.Source + " → " + s.Target
}
