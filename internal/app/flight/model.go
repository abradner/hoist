package flight

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/redact"
)

// PollDurations is the plain-value slice of internal/config.PollConfig this screen actually
// needs, in place of importing internal/config itself. AGENTS.md §4.8: a screen never imports
// config/registry policy, only the plain values or function types cmd/hoist (the one place
// allowed to know both sides) translates for it. The root maps these same values into
// session.Config (Train 2 design, D3) for the controller that actually drives; this screen keeps
// its own copy only for anything it still renders directly (today: nothing computed from it, but
// NewAttached's signature keeps the type so a later PR — the visible-wait countdown, #106's
// design doc PR 8 — has somewhere to put a poll-driven display timer without a signature change).
type PollDurations struct {
	CI, Approval, Argo, Rollout, Deadline time.Duration
}

// keyMap is this screen's own key vocabulary, on top of the root's global quit keys.
type keyMap struct {
	Open, Reobserve, Abandon, Log, Back, Override key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Open:      key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		Reobserve: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "re-observe")),
		// Capital: a write this destructive gets the shift key out of reach of a mistyped
		// letter, mirroring the matrix screen's own R/r convention (AGENTS.md §4.8). Lower-case
		// x used to mean "stop watching" (read-only, nothing engine-side) but is retired (Train
		// 2 design PR 3): esc already does that, and does it without pretending the drive itself
		// stopped — a promotion this screen was merely watching keeps running whether or not
		// anything is watching it, so a second key for the identical no-op taught the wrong
		// mental model.
		Abandon:  key.NewBinding(key.WithKeys("X"), key.WithHelp("X", "abandon")),
		Log:      key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "log")),
		Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Override: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "treat no checks as green")),
	}
}

// OpenPRMsg asks whatever composes screens to open s.PR's URL in the operator's browser —
// the actual open mechanism (exec.Command("open", …) or equivalent) is out of scope for
// this screen (AGENTS.md §4.8: a screen requests navigation by emitting its own concrete
// type; mirrors matrix.OpenPlanMsg and plan.BackMsg).
type OpenPRMsg struct{ URL string }

// AbandonMsg asks whatever composes screens to retire promotion ID for good: `hoist abandon`'s
// own write — release the state file, and close the PR / delete the branch if it opened either.
// A distinct type from BackMsg on purpose: BackMsg's own doc comment and parity row describe
// purely-local, non-destructive semantics (leaving the branch, PR and state file exactly as they
// are, the drive itself still running) — giving that identical, harmless meaning a destructive
// second sense would silently invalidate the description without anything able to catch the
// drift. Emitted only after the X gesture's own huh.Confirm answers yes (AGENTS.md invariant 5's
// keypress-then-confirm shape), and never for a promotion this screen itself believes has already
// landed — the root's real handler re-observes and refuses authoritatively regardless (this
// screen's own guard is UI politeness only, the same relation OverrideCINoneMsg has to
// CIGreenStep's own re-check).
type AbandonMsg struct{ ID string }

// BackMsg asks whatever composes screens to close every screen above the matrix at once — esc,
// even when a plan or deploy confirm screen sits underneath this one (the screen that started the
// drive: m.start's own doc comment, app.go). It never pops back to that confirm screen instead:
// doing so used to leave it still ticked and ready, so Enter there started the very drive esc just
// left watching (audit UX-H6/FB-H2, the operator's own decision, a follow-up to Train 2 design PR
// 3's original "leaving flight never cancels anything"). The drive keeps running exactly as it
// was; the root only stops mirroring it onto a screen. enter/r on the matrix's in-flight pane
// re-attaches a fresh flight screen to the same running entry later (session.Controller.Running's
// own dedup, already in Start/Resume, means that never starts a second one).
type BackMsg struct{}

// OverrideCINoneMsg is the operator's confirmed answer to the one Blocked reason that has an
// in-band override: CIGreenStep's "no checks reported after the grace period; ci.none=prompt"
// (engine.IsCINonePromptBlock). It asks whatever composes screens to re-drive promotion ID
// with engine.PromotionState.CINoneOverride set — the TUI's `hoist resume <id>
// --override-ci-none` (#103). Emitted only after `c` and a huh.Confirm answered yes, never on
// the keypress alone, and only for the promotion this screen is showing: the override is a
// per-promotion, one-shot operator instruction, never a launch default and never config
// (AGENTS.md §4.5 — a default may not weaken a gate). The root answers it by calling
// session.Controller.OverrideCINone for this promotion's own entry.
type OverrideCINoneMsg struct{ ID string }

// ReobserveMsg is R's own request: re-observe promotion ID now, rather than waiting for the
// next scheduled poll. Train 2 design, D3: the flight screen no longer drives anything itself —
// it only asks, and the root answers by calling session.Controller.Poke, whose own busy/not-found
// refusal is authoritative regardless of what this screen's own guard already believed.
type ReobserveMsg struct{ ID string }

// Model is the flight screen: a mirror of one session.Controller entry, never a driver of its
// own (Train 2 design, D3). It keeps no ctx, no Driver, no tick chain — session.Controller owns
// every background command; this package's own doc comment on Update covers why. Update handles
// only this screen's own keys, its spinner, and the two confirm dialogs (c/X); the state it
// renders is set exclusively by NewAttached and Mirror.
type Model struct {
	// id/build identify which session.Controller entry this screen mirrors — Attached() hands
	// these back to the root so apply() can route a Change to the right screen. id is empty
	// during Building (the entry's own real promotion id isn't known yet); build never changes
	// across the whole life of one attachment.
	id    string
	build session.BuildID

	state    engine.PromotionState
	order    []engine.StepName
	rows     []Row
	done     bool
	building bool
	// stopped mirrors session.Stopped: a Blocked step or a terminal (non-retryable) error —
	// R (ReobserveMsg) still lets the operator retry by hand.
	stopped bool
	// busy mirrors the entry's own Busy: a Step call (or the initial Start/Resume) is currently
	// outstanding, so the spinner animates and R/X/c are refused until it clears.
	busy bool
	// deadlineAt mirrors the entry's own DeadlineAt — the one absolute instant this drive's
	// whole budget names, owned and renewed by session.Controller now (Poke/OverrideCINone's own
	// rearm), never recomputed here. Zero when the controller was configured with no deadline.
	deadlineAt time.Time

	spinner spinner.Model
	showLog bool
	notice  string
	// errNotice is the last Snapshot.Err (redacted), shown until a later Mirror clears it.
	errNotice string
	// buildLog is the Snapshot's own progress log — one entry per line Hooks.Progress reported,
	// oldest first (session.LogLine, owned by the controller; this screen never accumulates its
	// own copy — Train 2 design, D3's own note on session.LogLine).
	buildLog []session.LogLine

	styles        ui.Styles
	keys          keyMap
	width, height int
	// now is the clock the header's elapsed/deadline are worded against; a test pins it.
	now func() time.Time
	// log is the History scrollback when l toggles it on.
	log viewport.Model

	// confirming is true while the `c` gesture's dialog is up; confirmOverride is the widget.
	// confirmValue is huh's write target only and is never read to decide anything — Value
	// binds a pointer into the copy of this value-typed model that built the widget, so the
	// answer is read back through the widget's GetValue (confirmAgreed), the shape the tag
	// picker's D gesture settled on after shipping broken twice (AGENTS.md §9 entry 6).
	confirming      bool
	confirmOverride *huh.Confirm
	confirmValue    bool

	// confirmingAbandon/confirmAbandon/confirmAbandonValue are the X gesture's own instance of
	// the identical pattern — a second, independent dialog, never sharing confirming/
	// confirmOverride with the c gesture above.
	confirmingAbandon   bool
	confirmAbandon      *huh.Confirm
	confirmAbandonValue bool
}

// WithNow fixes the clock (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// NewAttached builds the flight screen already attached to one session.Controller entry — the
// TUI's only way to construct this screen (Train 2 design, D3): the root calls it once, right
// after session.Controller.Start/Resume hands back a BuildID, and every later change reaches this
// same instance through Mirror rather than a fresh construction. It replaces New/NewBuilding/
// AdoptBuilt: whichever phase s names (Building included — s.State is zero then, and s.Source/
// Target/Direct carry what the confirmed plan already knew, mirrored the same way the old
// NewBuilding's own stub state did) is rendered directly, with nothing left to "adopt" later —
// mirroring a fresher snapshot onto the same instance already does that.
func NewAttached(s session.Snapshot, poll PollDurations) Model {
	m := Model{
		spinner: spinner.New(spinner.WithSpinner(spinner.Line)),
		keys:    defaultKeyMap(),
		now:     time.Now,
		log:     viewport.New(),
		styles:  ui.NewStyles(true),
		// Visible by default (§4.8 proposal from the M10 train): "what is hoist actually doing"
		// is the operator's question every time a promotion runs, not a fact to go looking for
		// behind a key. l still hides it for an operator who wants the room back.
		showLog: true,
	}
	_ = poll // kept for signature stability (see PollDurations' own doc comment)
	return m.Mirror(s)
}

// ID is the promotion this screen shows — "" during Building, before session.Controller's own
// backend has produced a real deterministic id.
func (m Model) ID() string { return m.id }

// Attached reports which session.Controller entry this screen mirrors — the root's apply() uses
// Build (stable across the whole attachment, including the id-less Building window) to decide
// which on-stack flightScreen a Change belongs to; id is handed back too since some callers (a
// notice naming "this promotion") want it directly.
func (m Model) Attached() (id string, build session.BuildID) { return m.id, m.build }

// Mirror replaces this screen's displayed state with s — the root calls it after every
// session.Controller change whose Build matches Attached()'s own (D3). A snapshot for a
// different build is a caller bug (mirrorAttached in app.go only ever mirrors a matching one) and
// is applied as-is rather than defended against here a second time (AGENTS.md §8, the deletion
// test: the one real guard belongs where the routing decision is made).
func (m Model) Mirror(s session.Snapshot) Model {
	m.id = s.ID
	m.build = s.Build
	m.building = s.Phase == session.Building
	m.busy = s.Busy
	m.done = s.Done
	m.stopped = s.Phase == session.Stopped
	m.deadlineAt = s.DeadlineAt

	state := s.State
	if m.building {
		// Nothing real exists yet — render what the confirmed plan already told the controller
		// (NewBuilding's old shape), so the header/order are correct from the very first paint.
		state = engine.PromotionState{SourceEnv: s.Source, TargetEnv: s.Target, Direct: s.Direct}
	}
	m.state = state
	m.order = OrderFor(state)
	if m.building {
		m.rows = DeriveRows(m.order, false, nil)
	} else {
		m.rows = DeriveRows(m.order, s.Done, s.Statuses)
	}

	m.buildLog = s.Log
	if s.Err != nil {
		m.errNotice = redact.Strings(s.Err.Error())
	} else {
		m.errNotice = ""
	}
	return m
}

// Init starts the spinner's tick chain whenever there is something to animate (Building, or a
// Step outstanding) — a mirrored screen with nothing in flight has nothing to animate, so this
// returns nil rather than a permanent, invisible tick loop (PR #39 review finding #5, still true
// here: the loop this guards is the spinner's own reschedule in Update, not a poll this screen no
// longer drives).
func (m Model) Init() tea.Cmd {
	if m.building || m.busy {
		return m.spinner.Tick
	}
	return nil
}

// Update handles the screen's own keys and the spinner's tick chain. Every async result this
// screen used to process directly (a drive's own Tick, a progress line) now arrives only as a
// fresher Mirror call from the root — this package issues no tea.Cmd that talks to a Driver or a
// channel at all (Train 2 design, D3; TestFlightNeverCallsDriver pins it).
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		// Keep the chain alive only while something is actually animating; stop it the moment
		// nothing is, rather than rescheduling unconditionally (PR #39 review finding #5).
		if !m.building && !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	if m.confirming {
		// Esc leaves the dialog without answering it (the tag picker's round-3 finding:
		// huh's own Update swallows Esc, trapping the operator); everything else is the
		// widget's, until enter reads its answer.
		if key.Matches(msg, m.keys.Back) {
			m.confirming = false
			return m, nil
		}
		return m.updateConfirm(msg)
	}
	if m.confirmingAbandon {
		if key.Matches(msg, m.keys.Back) {
			m.confirmingAbandon = false
			return m, nil
		}
		return m.updateConfirmAbandon(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Override):
		if !m.offersCINoneOverride() {
			// Any other block (a failed check, ci.none=block, a branch conflict) has no
			// in-band override; the key does nothing rather than open a dialog whose yes
			// would change nothing (AGENTS.md §2 principle 1: an offer that cannot deliver
			// is a bug).
			return m, nil
		}
		return m.openConfirm()
	case key.Matches(msg, m.keys.Open):
		if url, ok := PRURL(m.state); ok {
			return m, func() tea.Msg { return OpenPRMsg{URL: url} }
		}
		m.notice = "no PR to open yet"
		return m, nil
	case key.Matches(msg, m.keys.Reobserve):
		if m.id == "" {
			m.notice = "nothing to re-observe yet — still starting"
			return m, nil
		}
		if m.busy || m.done {
			return m, nil
		}
		id := m.id
		return m, func() tea.Msg { return ReobserveMsg{ID: id} }
	case key.Matches(msg, m.keys.Abandon):
		// Nothing to abandon when the promotion has no real ID yet (Building) — a UI politeness
		// check only, since the root's real handler re-observes and refuses authoritatively
		// regardless (AbandonMsg's own doc comment).
		if m.id == "" {
			m.notice = "nothing to abandon — this promotion isn't being driven yet"
			return m, nil
		}
		if m.done {
			m.notice = "this promotion has already landed — abandoning is not a rollback"
			return m, nil
		}
		return m.openConfirmAbandon()
	case key.Matches(msg, m.keys.Log):
		m.showLog = !m.showLog
		return m, nil
	case key.Matches(msg, m.keys.Back):
		return m, func() tea.Msg { return BackMsg{} }
	}
	if m.showLog {
		// The log is a viewport: unmatched keys (↑/↓, PageUp/PageDown, g/G) scroll it. Without
		// this, l showed the first page of a long history and nothing moved it. Laid out on
		// this copy first — View lays out its own copy, so the retained viewport would
		// otherwise be the zero-sized one NewAttached built (Copilot, #124).
		m = m.layout()
		var cmd tea.Cmd
		m.log, cmd = m.log.Update(msg)
		return m, cmd
	}
	return m, nil
}

// offersCINoneOverride is true exactly when `c` has something to do: the drive stopped on a
// Blocked CI step whose reason is the ci.none=prompt one (engine.IsCINonePromptBlock) — the
// single Blocked reason with an override. A failed or skipped check, ci.none=block, or any
// other step's block never qualifies, so the offer, the hint and the key all agree.
func (m Model) offersCINoneOverride() bool {
	if m.done || !m.stopped {
		return false
	}
	for _, r := range m.rows {
		if r.Glyph == GlyphBlocked {
			return r.Step == engine.StepCIGreen && engine.IsCINonePromptBlock(r.Detail)
		}
	}
	return false
}

// openConfirm raises the `c` gesture's dialog — the tag picker's D shape: keypress, then a
// huh.Confirm, and only a yes emits anything.
func (m Model) openConfirm() (Model, tea.Cmd) {
	m.confirming = true
	m.confirmValue = false
	title := fmt.Sprintf("Treat this PR's missing checks as green and let %s merge on approval alone? ci.none is prompt; this applies to promotion %s only.", m.state.TargetEnv, m.id)
	m.confirmOverride = huh.NewConfirm().Title(title).Value(&m.confirmValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6).
	m.confirmOverride.WithKeyMap(huh.NewDefaultKeyMap())
	m.confirmOverride.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmOverride.WithWidth(m.dialogWidth())
	return m, tea.Batch(m.confirmOverride.Init(), m.confirmOverride.Focus())
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirming = false
		if !m.confirmAgreed() {
			return m, nil
		}
		id := m.id
		return m, func() tea.Msg { return OverrideCINoneMsg{ID: id} }
	}
	f, cmd := m.confirmOverride.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmOverride = c
	}
	return m, cmd
}

// confirmAgreed reads the operator's answer from the widget, never from confirmValue — see
// the field's own comment.
func (m Model) confirmAgreed() bool {
	if m.confirmOverride == nil {
		return false
	}
	v, _ := m.confirmOverride.GetValue().(bool)
	return v
}

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

// openConfirmAbandon raises the X gesture's own dialog — the identical keypress-then-confirm
// shape as openConfirm/the tag picker's D, on its own independent widget.
func (m Model) openConfirmAbandon() (Model, tea.Cmd) {
	m.confirmingAbandon = true
	m.confirmAbandonValue = false
	title := fmt.Sprintf("Abandon promotion %s? This retires its state and, if it opened a PR, closes it and deletes the branch. This is not a rollback.", m.id)
	m.confirmAbandon = huh.NewConfirm().Title(title).Value(&m.confirmAbandonValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6).
	m.confirmAbandon.WithKeyMap(huh.NewDefaultKeyMap())
	m.confirmAbandon.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmAbandon.WithWidth(m.dialogWidth())
	return m, tea.Batch(m.confirmAbandon.Init(), m.confirmAbandon.Focus())
}

func (m Model) updateConfirmAbandon(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirmingAbandon = false
		if !m.confirmAbandonAgreed() {
			return m, nil
		}
		id := m.id
		return m, func() tea.Msg { return AbandonMsg{ID: id} }
	}
	f, cmd := m.confirmAbandon.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmAbandon = c
	}
	return m, cmd
}

// confirmAbandonAgreed reads the operator's answer from the widget, never from
// confirmAbandonValue — see confirmAgreed's own comment; the identical reasoning.
func (m Model) confirmAbandonAgreed() bool {
	if m.confirmAbandon == nil {
		return false
	}
	v, _ := m.confirmAbandon.GetValue().(bool)
	return v
}

// SetSize records the terminal size. The log is a viewport sized to what the frame leaves
// (layout) and scrolls with the unmatched keys handleKey forwards; the step list has no
// scrolling and degrades to the one-line strip on a short terminal (stepsSection).
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	if m.confirmOverride != nil {
		m.confirmOverride.WithWidth(m.dialogWidth())
	}
	if m.confirmAbandon != nil {
		m.confirmAbandon.WithWidth(m.dialogWidth())
	}
	return m.layout()
}

// layout sizes the log viewport to what the frame leaves after the fixed sections.
func (m Model) layout() Model {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	fixed := lipgloss.Height(m.headerSection()) + lipgloss.Height(m.stepsSection())
	sections := 2
	if a := m.actionSection(); a != "" {
		fixed += lipgloss.Height(a)
		sections++
	}
	if n := m.notes(); n != "" {
		fixed += lipgloss.Height(n)
		sections++
	}
	if m.showLog {
		sections++
		fixed++ // the "history" label above the log
	}
	m.log.SetWidth(m.width - 2)
	m.log.SetHeight(max(ui.BodyHeight(m.height, sections)-fixed, 3))
	m.log.SetContent(m.logView())
	return m
}

// CapturesText reports whether the c/X gesture's dialog is up: while it is, the root must
// hand every key to this screen rather than treat q as quit, or an operator deciding
// whether to treat no checks as green (or to abandon) can quit the program mid-decision.
func (m Model) CapturesText() bool { return m.confirming || m.confirmingAbandon }

// SetStyles applies the palette (and re-themes whichever dialog is up).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	if m.confirmOverride != nil {
		m.confirmOverride.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	if m.confirmAbandon != nil {
		m.confirmAbandon.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	return m
}

// View renders the frame: the header (what and how long), the step list, what it is waiting
// for and what to type, the log when toggled, notices, and the footer. The whole assembled
// string passes through redact.Strings once here at the final boundary, matching
// plan.Model's own belt-and-suspenders convention.
func (m Model) View() string {
	m = m.layout()
	sections := []string{m.headerSection(), m.stepsSection()}
	if a := m.actionSection(); a != "" {
		sections = append(sections, a)
	}
	// notes (the transient notice, the last plumbing error) comes BEFORE the log, not after —
	// ui.Frame.Render crops from the bottom when a short terminal can't hold everything
	// (AGENTS.md §9 entry 10).
	if n := m.notes(); n != "" {
		sections = append(sections, n)
	}
	if m.showLog {
		sections = append(sections, m.styles.Dim.Render("history")+"\n"+m.log.View())
	}
	out := ui.Frame{Title: m.title(), Sections: sections, Footer: ui.StatusBar(m.width, m.styles.Status.Render(m.statusLeft()), m.styles.Hint.Render(m.hint()))}.Render(m.styles, m.width, m.height)
	if m.confirming && m.confirmOverride != nil {
		out = ui.Dialog(m.styles, out, "treat no checks as green", m.confirmOverride.View(), m.width, m.height)
	}
	if m.confirmingAbandon && m.confirmAbandon != nil {
		out = ui.Dialog(m.styles, out, "abandon this promotion", m.confirmAbandon.View(), m.width, m.height)
	}
	return redact.Strings(out)
}

func (m Model) title() string {
	if m.state.SourceEnv == "" {
		return "hoist · deploy · in flight"
	}
	return "hoist · promotion · in flight"
}

// headerSection is the id, the envs, how long it has run and how long it has left.
func (m Model) headerSection() string {
	// A deploy has no source env, so the promotion's "A → B" would render with a hole where
	// the source belongs — the state's own empty SourceEnv is what distinguishes them, the
	// same discriminator internal/engine/template.go and cmd/hoist's success line use.
	pair := m.styles.Title.Render("deploy → " + m.state.TargetEnv)
	if m.state.SourceEnv != "" {
		pair = m.styles.Title.Render(m.state.SourceEnv + " → " + m.state.TargetEnv)
	}
	id := m.id
	if id == "" {
		id = "…"
	}
	left := m.styles.Accent.Render(id) + "   " + pair
	if m.state.Direct {
		left += "   " + m.styles.Warn.Render("direct")
	}
	var right []string
	if start := StartedAt(m.state); !start.IsZero() {
		right = append(right, "started "+ui.Ago(m.now(), start))
	}
	if !m.deadlineAt.IsZero() && !m.done {
		right = append(right, "deadline "+ui.Until(m.now(), m.deadlineAt))
	}
	return ui.StatusBar(max(m.width-2, 1), left, m.styles.Dim.Render(strings.Join(right, " · ")))
}

// stepList renders one line per step (glyph + label) with a second, indented detail line
// for whichever row is Active — e.g. "CI 2/3 complete". A fully done promotion has no active
// row (DeriveRows never sets Active when done — every row is already Done), but its own last
// row still carries a real Detail (engine.Status's short-circuited final-step Observation,
// e.g. "merged as <sha>; branch deleted"), so that one row's detail is shown too.
func (m Model) stepList() string {
	var b strings.Builder
	last := len(m.rows) - 1
	for i, r := range m.rows {
		marker := r.Glyph
		if r.Active && m.busy {
			marker = m.spinner.View()
		}
		line := marker + " " + Label(r.Step)
		switch r.Glyph {
		case GlyphDone:
			line = m.styles.Good.Render(line)
		case GlyphBlocked:
			line = m.styles.Bad.Render(line)
		case GlyphActive, GlyphWaiting:
			line = m.styles.Warn.Render(line)
		default:
			line = m.styles.Dim.Render(line)
		}
		b.WriteString(line + "\n")
		if r.Detail != "" && (r.Active || (m.done && i == last)) {
			b.WriteString("    " + ansi.Wrap(r.Detail, max(m.width-6, 20), "") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepsSection is the step list when the terminal has the rows for it, else the one-line
// strip the matrix's in-flight pane draws — degrade by dropping evidence, never the verdict:
// on a short terminal the notice and the action still fit, and every step is still named.
func (m Model) stepsSection() string {
	list := m.stepList()
	other := lipgloss.Height(m.headerSection()) + lipgloss.Height(m.actionSection()) + lipgloss.Height(m.notes()) + 4
	if lipgloss.Height(list)+other <= m.height {
		return list
	}
	sum := Summary{ID: m.id, PR: m.state.PR, Rows: m.rows, Done: m.done}
	strip := sum.StepStrip()
	if ansi.StringWidth(strip) > m.width-2 {
		// Pack whole steps onto lines no wider than the frame's interior: a fixed break
		// after the merge still overflowed on terminals narrower than the first half.
		var lines []string
		line := ""
		for _, p := range strings.Split(strip, "  ") {
			switch {
			case line == "":
				line = p
			case ansi.StringWidth(line)+2+ansi.StringWidth(p) <= m.width-2:
				line += "  " + p
			default:
				lines = append(lines, line)
				line = p
			}
		}
		strip = strings.Join(append(lines, line), "\n")
	}
	if step, ok := ActiveStep(m.rows); ok {
		for _, r := range m.rows {
			if r.Step == step && r.Detail != "" {
				strip += "\n" + ansi.Truncate("    "+r.Detail, max(m.width-2, 20), "…")
			}
		}
	}
	return strip
}

// actionSection is what the promotion is waiting for and what the operator can do about it
// — the same words the matrix's in-flight pane uses (Summary), so the two never disagree.
// Empty when there is nothing to say beyond the step list itself.
func (m Model) actionSection() string {
	if m.building {
		// The one thing this section says during preflight: it's alive, and — via the same
		// spinner.View() the step list uses for an Active row — what it's doing right now,
		// the last line the log received. Before the first line arrives there is nothing
		// truer to say than "starting".
		label := "starting"
		if n := len(m.buildLog); n > 0 {
			label = m.buildLog[n-1].Text
		}
		return m.styles.Warn.Render(m.spinner.View() + " " + label)
	}
	sum := Summary{ID: m.id, Source: m.state.SourceEnv, Target: m.state.TargetEnv, Direct: m.state.Direct, PR: m.state.PR, Rows: m.rows, Done: m.done}
	text, command := sum.Action()
	switch {
	case m.done:
		return m.styles.Good.Render("done")
	case command != "":
		return m.styles.Warn.Render(text) + "\n\n    " + m.styles.Accent.Render(command)
	case m.stopped:
		if _, blocked := BlockedStep(m.rows); blocked {
			// The step's own reason first — a CI policy block, a failed check, a missing
			// Application — and the generic advice only where there is none.
			if text != "" {
				out := m.styles.Bad.Render(ansi.Wrap("blocked — "+text, max(m.width-2, 20), ""))
				if m.offersCINoneOverride() {
					// The one block with an in-band answer: the CLI's own re-run command is
					// already in the reason text; this is the TUI's equivalent (#103).
					return out + "\n" + m.styles.Accent.Render("press c to treat no checks as green for this promotion") + "\n" + m.styles.Dim.Render("R to re-observe once resolved")
				}
				return out + "\n" + m.styles.Dim.Render("R to re-observe once resolved")
			}
			return m.styles.Bad.Render("blocked — resolve the conflict, then R to re-observe")
		}
		return m.styles.Bad.Render("stopped — see the error below; R retries")
	}
	return ""
}

// logView renders state.History first, then buildLog — chronological order, oldest to
// newest: state.History is the settled record up to the last landed Tick (or, before the first
// one has landed at all, empty); buildLog is the controller's own progress log for this build —
// preflight lines before a PromotionState exists, or a step mid-Act during drive that hasn't
// reached its own appendHistory yet.
func (m Model) logView() string {
	if len(m.buildLog) == 0 && len(m.state.History) == 0 {
		return m.styles.Dim.Render("(no history yet)")
	}
	var b strings.Builder
	for _, h := range m.state.History {
		fmt.Fprintf(&b, "%s  %-12s  %s\n", h.At.Format(time.RFC3339), h.Step, h.Detail)
	}
	for _, line := range m.buildLog {
		fmt.Fprintf(&b, "%s  %s\n", line.At.Format(time.RFC3339), line.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// notes is the transient notice and the last plumbing error, word-wrapped.
func (m Model) notes() string {
	var lines []string
	if m.notice != "" {
		lines = append(lines, m.styles.Notice.Render(ansi.Wrap(m.notice, max(m.width-2, 20), "")))
	}
	if m.errNotice != "" {
		lines = append(lines, m.styles.Bad.Render(ansi.Wrap(m.errNotice, max(m.width-2, 20), "")))
	}
	return strings.Join(lines, "\n")
}

func (m Model) statusLeft() string {
	if m.done {
		return "promotion complete"
	}
	if m.stopped {
		// A Blocked step (BlockedStep's own doc comment) stops polling too, but never sets
		// m.errNotice — its own reason is already shown as the blocked row's Detail, in the
		// step list above, not as a separate notice below.
		if _, blocked := BlockedStep(m.rows); blocked {
			if m.offersCINoneOverride() {
				return "blocked: no checks reported; c treats them as green"
			}
			return "blocked: resolve the conflict, then press R to re-observe"
		}
		return "stopped: see error below"
	}
	return ""
}

func (m Model) hint() string {
	h := "R re-observe · l log · esc back"
	// Offering X on a finished promotion just refuses (handleKey's own guard: "abandoning is
	// not a rollback"), the same reasoning "o open PR" already applies below for a promotion
	// with no PR yet — a key that only ever leads to a notice is not worth advertising.
	if !m.done {
		h = "X abandon · " + h
	}
	if m.offersCINoneOverride() {
		h = "c treat as green · " + h
	}
	if _, ok := PRURL(m.state); ok {
		h = "o open PR · " + h
	}
	return h
}
