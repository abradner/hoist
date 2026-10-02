package flight

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

// PollDurations is the plain-value slice of internal/config.PollConfig this screen actually
// needs, in place of importing internal/config itself. AGENTS.md §4.8: a screen never imports
// config/registry policy, only the plain values or function types cmd/hoist (the one place
// allowed to know both sides) translates for it. The root maps these same values into
// session.Config for the controller that actually drives; this screen keeps
// its own copy only for anything it still renders directly (today: nothing computed from it, but
// NewAttached's signature keeps the type so a poll-driven display timer has somewhere to
// live without a signature change).
type PollDurations struct {
	CI, Approval, Argo, Rollout, Deadline time.Duration
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
// drift. Emitted only after the shift+x gesture's own huh.Confirm answers yes (AGENTS.md invariant 5's
// keypress-then-confirm shape), and never for a promotion this screen itself believes has already
// landed — the root's real handler re-observes and refuses authoritatively regardless (this
// screen's own guard is UI politeness only, the same relation OverrideCINoneMsg has to
// CIGreenStep's own re-check).
type AbandonMsg struct{ ID string }

// BackMsg asks whatever composes screens to close every screen above the matrix at once — esc,
// even when a plan or deploy confirm screen sits underneath this one (the screen that started the
// drive: m.start's own doc comment, app.go). It never pops back to that confirm screen instead:
// doing so used to leave it still ticked and ready, so Enter there started the very drive esc just
// left watching (audit UX-H6/FB-H2, the operator's own decision, a follow-up to the original
// "leaving flight never cancels anything"). The drive keeps running exactly as it
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

// ReobserveMsg is r's own request: re-observe promotion ID now, rather than waiting for the
// next scheduled poll. The flight screen no longer drives anything itself —
// it only asks, and the root answers by calling session.Controller.Poke, whose own busy/not-found
// refusal is authoritative regardless of what this screen's own guard already believed. Plain
// "r" (keys.Refresh, Verb class) rather than a Write binding (the v2 keymap): asking to
// re-observe now writes nothing itself, unlike shift+x/shift+c below.
type ReobserveMsg struct{ ID string }

// WatchMsg asks whatever composes screens to open the read-only watch screen for Family in
// Target — w's own request, the same navigation-by-message-of-its-own-type convention
// every other screen transition uses (AGENTS.md §4.8: matrix.OpenPlanMsg, plan.BackMsg).
// Family is resolved by this screen from its own PromotionState.Edits (families, below) before
// this is ever emitted — never left for the root to guess, and never the root's job to derive
// from a promotion (the root's openWatch, app.go, already takes a plain (family, target) pair
// for exactly this reason: the matrix's identical w gesture resolves its own family from the
// cursor, not from what it asks the root to open).
type WatchMsg struct{ Family, Target string }

// Model is the flight screen: a mirror of one session.Controller entry, never a driver of its
// own. It keeps no ctx, no Driver, no tick chain — session.Controller owns
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
	// abandoning mirrors session.Abandoning: an Abandon has already been asked for and is either
	// waiting on a busy Step to notice its cancelled ctx or has its Backend.Abandon call already
	// outstanding. X is refused while this is true — the operator has already confirmed once, and
	// a second confirm-and-fire would race a second Backend.Abandon against the first rather than
	// just being redundant.
	abandoning bool
	// busy mirrors the entry's own Busy: a Step call (or the initial Start/Resume) is currently
	// outstanding, so the spinner animates and R/X/c are refused until it clears.
	busy bool
	// deadlineAt mirrors the entry's own DeadlineAt — the one absolute instant this drive's
	// whole budget names, owned and renewed by session.Controller now (Poke/OverrideCINone's own
	// rearm), never recomputed here. Zero when the controller was configured with no deadline.
	deadlineAt time.Time
	// nextPoll mirrors the entry's own Snapshot.NextPoll — when session.Controller will next
	// re-observe this promotion on its own, absent an r. Zero while building, busy, done or
	// stopped (session.Controller only ever sets it for a Waiting entry); actionSection's own
	// countdown is what makes that wait visible rather than a screen that
	// looks the same whether it is about to check again in one second or is quietly wedged.
	nextPoll time.Time
	// tickID stamps this instance's own 1s countdown-redraw ticks (scope.After), distinct from
	// any other flight instance's — never a Scope/ctx, since this screen drives nothing and owns
	// no cancellable call of its own: the tick carries no work, it only wakes Update once a
	// second so the countdown's rendered text advances while this screen is the one on top (a
	// tick delivered while some other screen is on top reaches THAT screen's Update instead,
	// which does not recognize it and drops it — which is exactly what stops the chain once this
	// screen is no longer visible, with nothing further to clean up).
	tickID scope.ID

	spinner spinner.Model
	notice  string
	// errNotice is the last Snapshot.Err (redacted), shown until a later Mirror clears it.
	errNotice string
	// buildLog is the Snapshot's own progress log — one entry per line Hooks.Progress reported,
	// oldest first (session.LogLine, owned by the controller; this screen never accumulates its
	// own copy).
	buildLog []session.LogLine

	styles        ui.Styles
	width, height int
	// now is the clock the header's elapsed/deadline are worded against; a test pins it.
	now func() time.Time
	// log is the History scrollback, always shown: M10's "visible by
	// default" already made it the common case; l no longer toggles it — the root's generic
	// keyed handling (KeyScreen, below) now sends l to the shared activity screen instead, so
	// this screen has nothing left for a toggle to hide, and every mockup (v2·04a/b) draws the
	// history section unconditionally).
	log viewport.Model

	// confirming is true while the `shift+c` gesture's dialog is up; confirmOverride is the widget.
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

	// choosingFamily/familyChooser are w's own dialog when this promotion touches more than one
	// family (families, below) — a plain huh.Select over the choices, the identical
	// keys.HuhKeyMap()-then-GetValue() shape the matrix's own image/resume choosers already use
	// (internal/app/matrix's openChooser). A single family never raises this: w emits WatchMsg
	// directly.
	choosingFamily bool
	familyChooser  *huh.Select[string]
}

// WithNow fixes the clock (tests).
func (m Model) WithNow(now func() time.Time) Model {
	m.now = now
	return m
}

// newViewport is the log pane's own scrolling body, bound to keys.ViewportKeyMap
// rather than left on viewport.New()'s bubbles-library default, which binds bare "d"
// to half-page down — this screen has no diff key, but the registry lists "d" as unbound here,
// and the bubbles default paged the log out from under an operator who pressed it expecting
// nothing to happen.
func newViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = keys.ViewportKeyMap()
	return v
}

// NewAttached builds the flight screen already attached to one session.Controller entry — the
// TUI's only way to construct this screen: the root calls it once, right
// after session.Controller.Start/Resume hands back a BuildID, and every later change reaches this
// same instance through Mirror rather than a fresh construction. It replaces New/NewBuilding/
// AdoptBuilt: whichever phase s names (Building included — s.State is zero then, and s.Source/
// Target/Direct carry what the confirmed plan already knew, mirrored the same way the old
// NewBuilding's own stub state did) is rendered directly, with nothing left to "adopt" later —
// mirroring a fresher snapshot onto the same instance already does that.
func NewAttached(s session.Snapshot, poll PollDurations) Model {
	m := Model{
		spinner: spinner.New(spinner.WithSpinner(spinner.Line)),
		now:     time.Now,
		log:     newViewport(),
		styles:  ui.NewStyles(true),
		tickID:  scope.New(),
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
// session.Controller change whose Build matches Attached()'s own. A snapshot for a
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
	m.abandoning = s.Phase == session.Abandoning
	m.deadlineAt = s.DeadlineAt
	m.nextPoll = s.NextPoll

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

// families is the distinct set of families this promotion's own Edits touch, sorted, for w's own
// gesture — a family is named by its path's last element (AGENTS.md's own glossary,
// `<env>/<family>/*.yaml`), so this needs nothing beyond what gitops.Edit.File already carries
// (path.Base(path.Dir(...))): no apps-root, no config import (AGENTS.md §4.8 — this package
// takes plain values only). Empty before any Edits exist yet (Building, or a state this screen
// was mirrored onto before the plan's own edits were computed) — w says so rather than opening a
// chooser over nothing.
func (m Model) families() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range m.state.Edits {
		f := path.Base(path.Dir(e.File))
		if f == "" || f == "." || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// waiting is true exactly when actionSection's own countdown applies: an ordinary in-progress
// promotion, not building, not mid-Step, not done, not stopped, with session.Controller having
// told this screen when it will next check on its own (Mirror's own nextPoll assignment).
func (m Model) waiting() bool {
	return !m.building && !m.busy && !m.done && !m.stopped && !m.nextPoll.IsZero()
}

// countdownTick is the 1s redraw wake-up actionSection's countdown needs while m.waiting() — it
// carries no data of its own; scope.Result[countdownTick] exists purely to be delivered back to
// THIS instance (tickID's own doc comment) and dropped by any other screen's Update.
type countdownTick struct{}

// Init starts whichever tick chain this screen needs to animate on its own: the spinner's while
// Building or a Step is outstanding (unchanged), or the 1s countdown redraw while m.waiting() —
// a mirrored screen with nothing in flight and nothing counting down has nothing to animate, so
// this returns nil rather than a permanent, invisible tick loop (the loop this guards is
// Update's own reschedule, not a poll this screen drives).
func (m Model) Init() tea.Cmd {
	if m.building || m.busy {
		return m.spinner.Tick
	}
	if m.waiting() {
		return scope.After(m.tickID, time.Second, countdownTick{})
	}
	return nil
}

// Update handles the screen's own keys and its two tick chains. Every async result this screen
// used to process directly (a drive's own Tick, a progress line) now arrives only as a fresher
// Mirror call from the root — this package issues no tea.Cmd that talks to a Driver or a channel
// at all (TestFlightNeverCallsDriver pins it). The spinner and the countdown
// chains hand off to each other at whichever end delivers next (their own doc comments): a
// mirrorAttached call from the root updates this screen's state directly, with no Init and no
// message through here, so each chain's own delivery is what notices the OTHER condition has
// become true meanwhile and starts that chain instead — the only gap is a screen sitting fully
// idle (neither chain alive) when a background Mirror alone moves it back into m.waiting(), which
// self-heals on the next keypress or Mirror-carried spinner/countdown delivery regardless.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if !m.building && !m.busy {
			// The step that was outstanding when this tick was scheduled has since resolved.
			// Hand off to the countdown chain if there is now something to count down to,
			// rather than just stopping (never
			// reschedule unconditionally).
			if m.waiting() {
				return m, scope.After(m.tickID, time.Second, countdownTick{})
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case scope.Result[countdownTick]:
		if scope.Foreign(m.tickID, msg) {
			return m, nil
		}
		if m.building || m.busy {
			// A poll fired while this chain was ticking. Hand off to the spinner chain rather
			// than reschedule a countdown that no longer applies.
			return m, m.spinner.Tick
		}
		if m.waiting() {
			return m, scope.After(m.tickID, time.Second, countdownTick{})
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is this screen's own gestures — everything in its keys.ScrFlight registry row
// except ?/l/q, which the root intercepts generically before a keypress ever reaches here
// (KeyScreen, below, and AGENTS.md §4.8's own "the root intercepts ?, l and q as keys" bullet).
// enter is deliberately unbound (FB-L4): a flight screen shows an already-running drive, and
// there is nothing left to confirm here the way plan/deploy's own enter starts one.
func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	m.notice = ""
	if m.confirming {
		// Esc leaves the dialog without answering it (the tag picker's finding:
		// huh's own Update swallows Esc, trapping the operator); everything else is the
		// widget's, until enter reads its answer.
		if keys.Esc.Matches(msg) {
			m.confirming = false
			return m, nil
		}
		return m.updateConfirm(msg)
	}
	if m.confirmingAbandon {
		if keys.Esc.Matches(msg) {
			m.confirmingAbandon = false
			return m, nil
		}
		return m.updateConfirmAbandon(msg)
	}
	if m.choosingFamily {
		// esc while the chooser's own "/" filter is open must close only the
		// filter — huh's own Update handles that — not the whole chooser; checked before the
		// unconditional close below, the mirror of updateFamilyChooser's own enter-while-
		// filtering guard just below it.
		if keys.Esc.Matches(msg) && (m.familyChooser == nil || !m.familyChooser.GetFiltering()) {
			m.choosingFamily = false
			return m, nil
		}
		return m.updateFamilyChooser(msg)
	}
	switch {
	case keys.CINone.Matches(msg):
		if !m.offersCINoneOverride() {
			// Any other block (a failed check, ci.none=block, a branch conflict) has no
			// in-band override; the key does nothing rather than open a dialog whose yes
			// would change nothing (AGENTS.md §2 principle 1: an offer that cannot deliver
			// is a bug).
			return m, nil
		}
		return m.openConfirm()
	case keys.Open.Matches(msg):
		if url, ok := PRURL(m.state); ok {
			return m, func() tea.Msg { return OpenPRMsg{URL: url} }
		}
		m.notice = "no PR to open yet"
		return m, nil
	case keys.Watch.Matches(msg):
		return m.startWatch()
	case keys.Refresh.Matches(msg):
		if m.id == "" {
			m.notice = "nothing to re-observe yet — still starting"
			return m, nil
		}
		if m.busy || m.done {
			return m, nil
		}
		id := m.id
		return m, func() tea.Msg { return ReobserveMsg{ID: id} }
	case keys.Abandon.Matches(msg):
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
		if m.abandoning {
			m.notice = "abandon already in progress"
			return m, nil
		}
		return m.openConfirmAbandon()
	case keys.Home.Matches(msg):
		m = m.layout()
		m.log.GotoTop()
		return m, nil
	case keys.End.Matches(msg):
		m = m.layout()
		m.log.GotoBottom()
		return m, nil
	case keys.Esc.Matches(msg):
		return m, func() tea.Msg { return BackMsg{} }
	}
	// The log is always shown now (l no longer toggles it — see the log field's own
	// doc comment), so any key this switch didn't recognize is the viewport's: ↑/↓,
	// PageUp/PageDown scroll it. Laid out on this copy first — View lays out its own copy, so
	// the retained viewport would otherwise be the zero-sized one NewAttached built (the same
	// gotcha as config.Model's and activity.Model's own).
	m = m.layout()
	var cmd tea.Cmd
	m.log, cmd = m.log.Update(msg)
	return m, cmd
}

// startWatch is w's own gesture: a single family goes straight to WatchMsg, several raise the
// chooser (openFamilyChooser), and none — nothing computed from this promotion's own Edits yet,
// e.g. still Building — says so rather than opening a chooser over nothing or guessing.
func (m Model) startWatch() (Model, tea.Cmd) {
	fams := m.families()
	switch len(fams) {
	case 0:
		m.notice = "nothing to watch yet — no family known for this promotion"
		return m, nil
	case 1:
		fam, target := fams[0], m.state.TargetEnv
		return m, func() tea.Msg { return WatchMsg{Family: fam, Target: target} }
	default:
		return m.openFamilyChooser(fams)
	}
}

// openFamilyChooser raises w's own dialog over several families — the matrix's identical
// openChooser shape (internal/app/matrix/model.go): keys.HuhKeyMap() so the widget's default
// keys actually work (AGENTS.md §9 entry 6), the answer read back through GetValue rather than
// a bound pointer (the same entry).
func (m Model) openFamilyChooser(fams []string) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(fams))
	for _, f := range fams {
		opts = append(opts, huh.NewOption(f, f))
	}
	sel := huh.NewSelect[string]().Title("watch which family?").Options(opts...)
	sel.WithKeyMap(keys.HuhKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	sel.WithWidth(m.dialogWidth())
	m.choosingFamily = true
	m.familyChooser = sel
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func (m Model) updateFamilyChooser(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		if m.familyChooser.GetFiltering() {
			f, cmd := m.familyChooser.Update(msg)
			if c, ok := f.(*huh.Select[string]); ok {
				m.familyChooser = c
			}
			return m, cmd
		}
		choice, _ := m.familyChooser.GetValue().(string)
		target := m.state.TargetEnv
		m.choosingFamily = false
		if choice == "" {
			return m, nil
		}
		return m, func() tea.Msg { return WatchMsg{Family: choice, Target: target} }
	}
	f, cmd := m.familyChooser.Update(msg)
	if c, ok := f.(*huh.Select[string]); ok {
		m.familyChooser = c
	}
	return m, cmd
}

// offersCINoneOverride is true exactly when `shift+c` has something to do: the drive stopped on a
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

// openConfirm raises the `shift+c` gesture's dialog — the tag picker's D shape: keypress, then a
// huh.Confirm, and only a yes emits anything.
func (m Model) openConfirm() (Model, tea.Cmd) {
	m.confirming = true
	m.confirmValue = false
	title := fmt.Sprintf("Treat this PR's missing checks as green and let %s merge on approval alone? ci.none is prompt; this applies to promotion %s only.", m.state.TargetEnv, m.id)
	m.confirmOverride = huh.NewConfirm().Title(title).Value(&m.confirmValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6). keys.HuhKeyMap rather than huh.NewDefaultKeyMap directly:
	// the one shared keymap every standalone huh field in this app now uses.
	m.confirmOverride.WithKeyMap(keys.HuhKeyMap())
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

// openConfirmAbandon raises the shift+x gesture's own dialog — the identical keypress-then-confirm
// shape as openConfirm/the tag picker's D, on its own independent widget.
func (m Model) openConfirmAbandon() (Model, tea.Cmd) {
	m.confirmingAbandon = true
	m.confirmAbandonValue = false
	title := fmt.Sprintf("Abandon promotion %s? This retires its state and, if it opened a PR, closes it and deletes the branch. This is not a rollback.", m.id)
	m.confirmAbandon = huh.NewConfirm().Title(title).Value(&m.confirmAbandonValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6). keys.HuhKeyMap rather than huh.NewDefaultKeyMap directly.
	m.confirmAbandon.WithKeyMap(keys.HuhKeyMap())
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
	if m.familyChooser != nil {
		m.familyChooser.WithWidth(m.dialogWidth())
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
	// The log is always shown now (see the log field's own doc comment), so its own
	// section and the "history" label above it are unconditional too.
	sections++
	fixed++ // the "history" label above the log
	m.log.SetWidth(m.width - 2)
	m.log.SetHeight(max(ui.BodyHeight(m.height, sections)-fixed, 3))
	m.log.SetContent(m.logView())
	return m
}

// CapturesText reports whether the shift+c/shift+x/w gesture's dialog is up: while it is, the
// root must hand every key to this screen rather than treat q as quit, or an operator deciding
// whether to treat no checks as green (or to abandon, or which family to watch) can quit the
// program mid-decision.
func (m Model) CapturesText() bool {
	return m.confirming || m.confirmingAbandon || m.choosingFamily
}

// SetStyles applies the palette (and re-themes whichever dialog is up).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	if m.confirmOverride != nil {
		m.confirmOverride.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	if m.confirmAbandon != nil {
		m.confirmAbandon.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	if m.familyChooser != nil {
		m.familyChooser.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	return m
}

// View renders the frame: the header (what and how long), the step list, what it is waiting
// for and what to type, the history, notices, and the footer. The whole assembled string
// passes through redact.Strings once here at the final boundary, matching plan.Model's own
// belt-and-suspenders convention.
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
	sections = append(sections, m.styles.Dim.Render("history")+"\n"+m.log.View())
	out := ui.Frame{Title: m.title(), Sections: sections, Footer: m.footer()}.Render(m.styles, m.width, m.height)
	if m.confirming && m.confirmOverride != nil {
		out = ui.Dialog(m.styles, out, "treat no checks as green", m.confirmOverride.View(), m.width, m.height)
	}
	if m.confirmingAbandon && m.confirmAbandon != nil {
		out = ui.Dialog(m.styles, out, "abandon this promotion", m.confirmAbandon.View(), m.width, m.height)
	}
	if m.choosingFamily && m.familyChooser != nil {
		out = ui.Dialog(m.styles, out, "watch which family?", m.familyChooser.View(), m.width, m.height)
	}
	return redact.Strings(out)
}

// title follows the app-wide `hoist · <noun> · <state>` pattern, naming the state word
// stateWord derives from the same fields the header/action sections already read — never a
// separate "in flight" catch-all, so the title and the body never disagree about what is
// happening.
func (m Model) title() string {
	noun := "promotion"
	if m.state.SourceEnv == "" {
		noun = "deploy"
	}
	return fmt.Sprintf("hoist · %s · %s", noun, m.stateWord())
}

// stateWord is the one word (or short phrase) title/statusLeft both name this promotion's
// current state with: starting, waiting for approval, blocked, stopped, in flight, or done.
func (m Model) stateWord() string {
	switch {
	case m.building:
		return "starting"
	case m.done:
		return "done"
	case m.stopped:
		if _, blocked := BlockedStep(m.rows); blocked {
			return "blocked"
		}
		return "stopped"
	}
	for _, r := range m.rows {
		if r.Active && r.Glyph == GlyphWaiting && r.Step == engine.StepApproved {
			return "waiting for approval"
		}
	}
	return "in flight"
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
	if start := m.startedAt(); !start.IsZero() {
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
			b.WriteString(indentWrap(r.Detail, "    ", m.width-6) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// indentWrap wraps text to width and prefixes indent on every resulting line, not only the
// first (UX-M3): ansi.Wrap itself only wraps, so a naive "indent + Wrap(...)" left every
// continuation line flush against the frame's own left edge, reading as a new, unrelated line
// rather than the rest of one sentence.
func indentWrap(text, indent string, width int) string {
	wrapped := ansi.Wrap(text, max(width-len(indent), 20), "")
	lines := strings.Split(wrapped, "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
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
	// ApprovalCopy, not sum.Action() directly: the one wording fix v2·04a settled on (UX-H10) —
	// "waiting for an approver to comment `hoist approve <id>` on PR #N" as ONE sentence, not
	// Action()'s own "blocked on you — comment ... to release it:" plus a separate command line.
	// Exported from this package now rather than kept as the matrix pane's own local
	// wrapper, so the screen and the pane render the identical sentence from one place.
	// command is always "" after ApprovalCopy: Action()'s only non-empty command was the
	// approval-wait one, and ApprovalCopy folds it into text as one sentence (its own doc
	// comment) — nothing left needs the separate accent-styled command line the earlier
	// rendering had here.
	text, _ := ApprovalCopy(sum)
	// approvalWaiting is true exactly when text is the ApprovalCopy sentence rather than some
	// other active row's plain detail — the one case that takes priority over the generic
	// countdown below, matching the earlier precedence (Action's own non-empty command
	// implied the identical priority, before ApprovalCopy folded it into text and left command
	// always "").
	approvalStep, hasActive := ActiveStep(m.rows)
	approvalWaiting := hasActive && approvalStep == engine.StepApproved && !m.stopped && text != ""
	switch {
	case m.done:
		return m.styles.Good.Render("done")
	case m.stopped:
		if _, blocked := BlockedStep(m.rows); blocked {
			// The reason itself is never repeated here (TestReasonOnceNamesShiftC): it is
			// already the blocked row's own indented detail line, right above, in the step
			// list — this section only ever says what to do about it, once.
			if m.offersCINoneOverride() {
				// The one block with an in-band answer (#103's own TUI equivalent).
				return m.styles.Bad.Render("blocked") + "\n" + m.styles.Accent.Render("press shift+c to treat no checks as green for this promotion") + "\n" + m.styles.Dim.Render("r to re-observe once resolved")
			}
			return m.styles.Bad.Render("blocked") + "\n" + m.styles.Dim.Render("r to re-observe once resolved")
		}
		return m.styles.Bad.Render("stopped — see the error below; r retries")
	case approvalWaiting:
		full := text
		if _, ok := PRURL(m.state); ok {
			full += " · o opens it"
		}
		// Wrapped, like every other multi-word section here (notes, blocked reasons) — a bare
		// Render truncated this mid-word at 80 columns once the PR number pushed it past the
		// frame's width (v2·04a's own two-line rendering of the identical sentence).
		return m.styles.Warn.Render(ansi.Wrap(full, max(m.width-2, 20), ""))
	case m.waiting():
		// #PR8: the ordinary "nothing wrong, just waiting for the next scheduled check" case
		// used to say nothing at all here (the step list's own Active row already names what
		// it's waiting on) — indistinguishable from a hang for however long the poll interval
		// is. Round to the second so the countdown does not repaint on sub-second jitter.
		remaining := m.nextPoll.Sub(m.now())
		if remaining < 0 {
			remaining = 0
		}
		return m.styles.Dim.Render(fmt.Sprintf("next check in %s · r now", remaining.Round(time.Second)))
	}
	return ""
}

// logView renders ONE chronological list, each event once. Two sources feed it, disjoint by
// construction: state.History (what the engine recorded — one entry per CHANGE of a step's
// state, see engine.appendHistory) and buildLog (the controller's Hooks.Progress lines, which
// are preflight and signing-wait lines only — internal/service never echoes a History entry
// into Progress). They are merged by time, stably, so a progress line that ties a history entry
// reads first, and every row uses the step's plain Label rather than the raw engine name.
func (m Model) logView() string {
	if len(m.buildLog) == 0 && len(m.state.History) == 0 {
		return m.styles.Dim.Render("(no history yet)")
	}
	type row struct {
		at   time.Time
		text string
	}
	rows := make([]row, 0, len(m.buildLog)+len(m.state.History))
	for _, line := range m.buildLog {
		rows = append(rows, row{line.At, line.Text})
	}
	for _, h := range m.state.History {
		rows = append(rows, row{h.At, fmt.Sprintf("%-12s  %s", Label(h.Step), h.Detail)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	var b strings.Builder
	// Relative times through ui.Ago — "12m ago  approval  no approval comment yet", never an
	// RFC3339 timestamp (UX-M4).
	for _, r := range rows {
		fmt.Fprintf(&b, "%s  %s\n", ui.Ago(m.now(), r.at), r.text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// startedAt is when this promotion began from the operator's point of view: the earlier of the
// first History entry and the first progress line, so the header's "started" never claims a
// later moment than the oldest line in the log below it (preflight runs before any History
// exists).
func (m Model) startedAt() time.Time {
	start := StartedAt(m.state)
	for _, l := range m.buildLog {
		if !l.At.IsZero() && (start.IsZero() || l.At.Before(start)) {
			start = l.At
		}
	}
	return start
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
				return "blocked: no checks reported; shift+c treats them as green"
			}
			return "blocked: resolve the conflict, then press r to re-observe"
		}
		return "stopped: see error below"
	}
	return ""
}

// footer renders through keys.Footer, matching this screen's own row in
// internal/ui/keys' registry — v2·04a/b's own order (esc, o, w, l), with the write bindings and
// r after the always-shown set so a narrow terminal drops them first (Hint.Pri's own doc
// comment: a lower Pri survives longest). esc's own wording is the one explicit ask from the
// design: "keeps running" while the drive is still live, dropped once it's done.
func (m Model) footer() string {
	escLong := "esc back"
	if !m.done {
		escLong = "esc back (keeps running)"
	}
	hints := []keys.Hint{{B: keys.Esc, Long: escLong, Short: "esc back", Pri: 0}}
	if _, ok := PRURL(m.state); ok {
		hints = append(hints, keys.Hint{B: keys.Open, Long: "o open PR", Pri: 2})
	}
	hints = append(hints, keys.Hint{B: keys.Watch, Long: "w watch", Pri: 3})
	hints = append(hints, keys.Hint{B: keys.Log, Long: "l log", Pri: 4})
	if m.offersCINoneOverride() {
		hints = append(hints, keys.Hint{B: keys.CINone, Long: "shift+c treat as green", Pri: 1})
	}
	if !m.done && !m.abandoning {
		// Offering shift+x on a finished promotion just refuses (handleKey's own guard:
		// "abandoning is not a rollback"). Same for one already Abandoning: the operator has
		// already confirmed once, and re-offering the key invites the exact second
		// confirm-and-fire this fix closes off at the controller too.
		hints = append(hints, keys.Hint{B: keys.Abandon, Long: "shift+x abandon", Pri: 6})
	}
	if !m.building && !m.done {
		hints = append(hints, keys.Hint{B: keys.Refresh, Long: "r re-observe", Pri: 5})
	}
	return keys.Footer(m.styles, m.width, m.statusLeft(), hints, true)
}

// KeyScreen implements the root's keyed interface (internal/app/screen.go): the root's generic
// ?/l handling now covers this screen, rather than flight owning its own help text or
// its own l-toggles-the-log gesture.
func (m Model) KeyScreen() keys.Screen { return keys.ScrFlight }
