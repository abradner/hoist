package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/activity"
	appconfig "github.com/abradner/hoist/internal/app/config"
	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
)

// Promotion groups everything New needs to actually drive a confirmed plan and act on the
// flight screen's own requests, beyond what plan.Func already covers. Poll/OpenURL/OpenPRMode
// are unrelated to the Service seam below and stay here: OpenURL is nil when no browser opener
// is wired in (flight.OpenPRMsg then falls back to the pre-wiring "not wired yet" notice rather
// than panicking on a nil call). OpenPRMode is one of "launch", "display" or "both" (cmd/hoist
// owns reading config.PreferencesConfig.OpenPR and resolving it to this plain string, per
// AGENTS.md §4.8 — this package only ever compares against string literals, never importing
// internal/config's own constants for it, matching Poll's own already-translated-from-config
// shape); empty behaves like "launch", so a caller that never sets it (a test, in particular)
// gets today's original launch-only behavior rather than a silently different one.
//
// Starting and driving a promotion itself goes through session.Controller (New's own m.sess,
// built from svc and Poll — Train 2 design), not through this struct.
type Promotion struct {
	Poll       flight.PollDurations
	OpenURL    func(url string) error
	OpenPRMode string
	// Now and After are session.Controller's own clock/scheduler seam (session.Config), threaded
	// through here rather than reached for directly (AGENTS.md §4.8: this package builds
	// session.Config, but only cmd/hoist decides what a real run's clock is) so a test can drive
	// a real Start -> Step(landed) -> refresh sequence deterministically — a fixed Now and an
	// After that fires on request rather than actually sleeping — the same seam session.Config
	// already exposes to its own package's tests, now reachable from app.New (P3 #10,
	// t2-review.md). Both nil (every real caller, and every test that never needs to drive a
	// tick chain) keeps session.Config's own defaults: time.Now and tea.Tick.
	Now   func() time.Time
	After func(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd
}

// openURLResultMsg is the browser launcher's answer for one URL, delivered by the command
// flight.OpenPRMsg's handler issues (#56: the launch used to run inside Update).
type openURLResultMsg struct {
	url string
	err error
}

// Model is the root tea.Model: a stack of screens, the window size, and the theme, plus
// what a screen needs to open the plan screen (internal/app/plan) or the tag picker
// (internal/app/tags) without app.New having to be called again — the repo, the promotable
// prefixes, the envs config (pairs, production), the digest-resolution adaptor (nil in
// "digest sources: none" mode) and the tag-picker's own registry/forge adaptor (nil runs the
// picker with no data source at all, reported as its own error state rather than a panic).
type Model struct {
	stack         []Screen
	styles        ui.Styles
	width, height int

	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	planFn     plan.Func
	tagsFn     tags.BuildFunc
	// restartFn is everything the restart screen needs from the cluster, supplied by the root's
	// own caller (cmd/hoist) so this package opens no connection of its own (AGENTS.md §4.8).
	// cmd/hoist's buildRestartFuncs (Train 2 design PR 7) asks the cluster fresh on every call,
	// so a boot-time failure is retried the next time R is pressed rather than wedged for the
	// session; zero here means the caller chose not to wire a cluster at all, and R says so.
	restartFn apprestart.Funcs
	// watchFn builds the watch screen's read function for one family in one env (WithWatch;
	// cmd/hoist's buildWatchFunc). Never nil once wired: buildWatchFunc itself retries the
	// cluster per call (PR 7) and reports the real error from whatever failed, rather than this
	// package ever collapsing it to a generic "none configured". nil means the caller chose not
	// to wire a cluster at all, and w says so.
	watchFn watch.BuildFunc
	// history is what the tag picker and both confirm screens call for commit history and the
	// migration delta (M10). Set through WithHistory rather than New so the screens that
	// consume it can land one at a time; zero means "no history available" and each screen
	// degrades to a named gap.
	history history.Funcs

	// sess is the one place a build, resume, step, abandon or listing actually happens
	// (internal/app/session, Train 2 design). It is a value: every state change happens inside
	// Update or one of its own methods, each returning a new Controller (AGENTS.md §4.8's
	// value-model convention, D1 of the design). Every background command it issues reaches the
	// root as a session.Event, handled by exactly one case in Update below (D2); a flightScreen
	// only ever mirrors a Snapshot from it and drives nothing itself (D3, internal/app/flight's
	// own package doc).
	sess session.Controller

	// lastList is the most recent full listing session.ChangeListed carried (state files on
	// disk, each re-observed) — kept so a ChangeStepped change (Train 2 design PR 4) can
	// re-merge the controller's own freshest live snapshots into the matrix's in-flight pane
	// (mergeInFlight's own shape) without waiting for the next listing tick and without a forge
	// call of its own: the listed-but-not-live entries in the last full listing are still good,
	// only the live ones need refreshing.
	lastList []service.Listed

	// poll, openURL and openPRMode are Promotion's remaining fields, unpacked here — see
	// Promotion's own doc comment for what each one is and why a nil OpenURL degrades to a
	// notice rather than a panic.
	poll       flight.PollDurations
	openURL    func(url string) error
	openPRMode string

	// activity is the root's own record of what has happened this session (internal/app/activity,
	// Train 2 design PR9) — a real in-flight conflict, missing config, a promotion started,
	// landed, blocked or failed, an abandon, a browser-launch outcome. It replaces the old
	// transient "notice" string, which showed exactly one message and cleared unconditionally on
	// the operator's next keypress (#164's own shape, one layer further: a real refusal that
	// arrived a beat before an unrelated "j" was simply gone). View's own bottom row shows only
	// the latest entry, one line, plus a "l: activity (N)" hint — not cleared by a keypress
	// either, only ever replaced by a newer entry. Opening the log (l, the root's own generic
	// keyed handling below) does not dismiss it: View draws the row on every screen regardless of what is on
	// top of the stack (its own doc comment), so it keeps showing underneath the log screen too
	// — the full history is what the log adds, not a replacement for this row.
	activity activity.Log

	// configPath, configFound and configText are what the config screen shows (C, #104):
	// where the file was read from or looked for, whether it existed, and the effective
	// config already marshalled and redacted by cmd/hoist (WithConfigView) — this package
	// holds the strings and never re-marshals, so the screen can show nothing `hoist config
	// show` would not.
	configPath  string
	configFound bool
	configText  string

	// quitConfirming/quitConfirm/quitConfirmValue are q's own dialog, raised only when
	// session.Controller.AnyRunning is true (Train 2 design PR 3: leaving flight never cancels a
	// drive, so quitting the whole program is the one gesture that still needs a confirm before
	// every running drive is actually stopped watching at once) — the same keypress-then-
	// huh.Confirm shape every other destructive gesture in this package uses, read back with
	// GetValue, never confirmValue itself (AGENTS.md §9 entry 6). ctrl+c is deliberately NOT
	// gated by this: promotion state is durable (§4.1) and `hoist resume` recovers whatever
	// either quit path interrupts (the design doc's own open question #2, confirmed by the
	// operator).
	quitConfirming   bool
	quitConfirm      *huh.Confirm
	quitConfirmValue bool

	// kbd is what the terminal answered when View below asked for
	// KeyboardEnhancements.ReportAllKeysAsEscapeCodes (T3-01) — flag 8, the one Kitty-protocol
	// feature that can report a caps-lock letter as ModCapsLock distinct from ModShift
	// (internal/ui/keys.Binding.Matches is what actually uses that bit; this field only
	// records whether the terminal granted it). Its one use is a single line in T3-03's help
	// overlay ("caps lock ignored" vs "a capital counts as shift"), never passed down into any
	// value-typed screen (train3-design.md's own "needs your decision", resolved as: match
	// without tracking state per-screen, record once here for that one line instead).
	kbd tea.KeyboardEnhancementsMsg

	// helpOpen/helpScreen are the root's own "?" overlay (T3-03): opened only for a top screen
	// that implements keyed (KeyScreen), drawn with ui.Dialog over whatever is underneath.
	// helpScreen is fixed at the moment the overlay opens, not re-read from the stack on every
	// keypress, so the overlay's own content cannot change out from under the operator while it
	// is up (nothing on the stack changes while it is open anyway — every key but esc/?/enter is
	// swallowed).
	helpOpen   bool
	helpScreen keys.Screen

	// hint is the transient "q quits from the matrix · esc goes back" row (train3-design.md's
	// own T3-03 decision): unlike the activity row (Model.activity), it is not an entry in the
	// log — it is cleared unconditionally at the top of every keypress, so it survives exactly
	// one more key after the one that set it, then disappears whether or not that next key did
	// anything else. It shares the activity row's own screen-space mechanism (View, below): both
	// take their row OUT of the top screen via ui.NoticeLinesStyled, never appended past it
	// (§9 entry 10) — hint simply takes priority over the activity row while it is set, since
	// only one bottom row exists on the terminal's last line.
	hint string

	// matrixTick/matrixTicking are the in-flight pane's own countdown redraw chain
	// (armMatrixCountdown/matrixCountdownTick): a scope.ID allocated once at New, and whether a
	// scope.After for it is currently outstanding, so armMatrixCountdown starts exactly one
	// chain per waiting-to-not-waiting transition rather than one per Update call.
	matrixTick    scope.ID
	matrixTicking bool
}

// New returns the root model with the matrix screen on the stack. promotable lists the
// image repo prefixes that count as first-party (the same list hoist plan --promotable
// takes). envs is the selected repo's envs config (production, pairs), zero-valued when
// there is none. planFn is svc.Plan (internal/service): what the plan screen calls to build a
// promotion's gitops.Plan, and what openDeploy below calls directly to build a deploy's. svc is
// the root's one seam for starting, listing, resuming and abandoning a promotion, and for
// refreshing the repo — see Service's own doc comment (internal/app/service.go); nil degrades
// every gesture it would serve to a notice, exactly as the old per-func nil checks did (now
// via session.Controller's own ErrNoBackend, session.New(svc, ...) below — Service's method
// set is a strict superset of session.Backend's, so a nil svc converts to a nil Backend too).
// promo carries what's left for driving the flight screen (poll cadence, the browser opener) —
// see Promotion's own doc comment. tagsFn is what the tag-picker screen calls to list and fetch
// registry/forge data for one image repo; nil opens the picker with no data source (it reports
// the resulting error itself). The theme starts dark and is replaced when the terminal reports
// its background.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, planFn plan.Func, svc Service, promo Promotion, tagsFn tags.BuildFunc, restartFn apprestart.Funcs) Model {
	m := Model{
		styles:     ui.NewStyles(true),
		repo:       repo,
		promotable: promotable,
		envs:       envs,
		planFn:     planFn,
		sess: session.New(svc, session.Config{
			Deadline: promo.Poll.Deadline, ListEvery: promo.Poll.Approval,
			Now: promo.Now, After: promo.After,
		}),
		poll:       promo.Poll,
		openURL:    promo.OpenURL,
		openPRMode: promo.OpenPRMode,
		tagsFn:     tagsFn,
		restartFn:  restartFn,
		matrixTick: scope.New(),
	}
	// The matrix starts without a cluster question; WithDrift supplies one. Deriving it from
	// planFn (as before #122) collapsed a partial rollout to the one digest a plan picks.
	return m.push(matrixScreen{matrix.New(repo, promotable, envs, nil)})
}

// WithHistory supplies the commit-history and migration-delta functions (cmd/hoist's
// buildHistoryFuncs). See Model.history.
func (m Model) WithHistory(h history.Funcs) Model {
	m.history = h
	return m
}

// History returns what WithHistory set — for the screen constructors in later M10 PRs and
// for cmd/hoist's own wiring test.
func (m Model) History() history.Funcs { return m.history }

// WithWatch supplies the watch screen's builder — cmd/hoist's buildWatchFunc, the read-only
// Get/Deployment/JobLike adapter `hoist watch` also uses. See Model.watchFn.
func (m Model) WithWatch(b watch.BuildFunc) Model {
	m.watchFn = b
	return m
}

// WithDrift hands the matrix its cluster question — cmd/hoist's buildDriftFunc, the raw
// pod observations per env (every running build kept, #122), never the planning resolver,
// which picks one digest per repo and would collapse a partial rollout. nil leaves the
// matrix claiming nothing about the cluster.
func (m Model) WithDrift(drift matrix.DriftFunc) Model {
	return m.withMatrix(func(ms matrix.Model) matrix.Model { return ms.WithDrift(drift) })
}

// WithRefreshRepo hands the matrix cmd/hoist's own re-read-origin function (#PR7) — F5 asks
// the cluster (WithDrift's own DriftFunc) and re-reads the repo alike.
func (m Model) WithRefreshRepo(refresh matrix.RefreshRepoFunc) Model {
	return m.withMatrix(func(ms matrix.Model) matrix.Model { return ms.WithRefreshRepo(refresh) })
}

// WithRun tells the matrix which base branch and kube context this session runs against
// (the launch's --base/--kube-context, #105), so its title can say so.
func (m Model) WithRun(base, kubeContext string) Model {
	return m.withMatrix(func(ms matrix.Model) matrix.Model { return ms.WithRun(base, kubeContext) })
}

// WithConfigView supplies what C shows: the config file's path, whether it existed, and the
// effective config as text — cmd/hoist's cfg.Redacted().Marshal(), the same bytes `hoist
// config show` prints. Unset, C says there is nothing to show.
func (m Model) WithConfigView(path string, found bool, text string) Model {
	m.configPath, m.configFound, m.configText = path, found, text
	return m
}

// matrixOnTop reports whether the matrix is the screen being shown — the only time a
// listing is worth the forge calls, since only the matrix draws the pane.
func (m Model) matrixOnTop() bool {
	if len(m.stack) == 0 {
		return false
	}
	_, ok := m.stack[len(m.stack)-1].(matrixScreen)
	return ok
}

// Init asks the terminal for its background colour so the palette can follow it, starts
// whatever the top (only, at boot) screen's own Init needs, and starts the controller's own
// listing loop (session.Controller.Init: an immediate listing plus its recurring tick chain).
func (m Model) Init() tea.Cmd {
	var screenCmd tea.Cmd
	if len(m.stack) > 0 {
		screenCmd = m.stack[len(m.stack)-1].Init()
	}
	return tea.Batch(tea.RequestBackgroundColor, screenCmd, m.sess.Init())
}

// start is plan.StartMsg and deploy.StartMsg's one shared dispatch (Train 2 design PR 2: the
// two near-identical StartMsg blocks the pre-session-controller app.go carried collapse into
// this): ask session.Controller to Start req for (source, target), push a flightScreen already
// mirroring whatever Snapshot exists the instant it agrees to track it (Building, with nothing
// real yet — NewAttached's own doc comment), and let the resulting session.Event stream carry
// it the rest of the way. A refusal (no backend wired, or another promotion already targets this
// env) shows as a notice and pushes nothing, mirroring every other nil-adaptor convention in this
// package.
func (m Model) start(req service.StartRequest, source, target string) (Model, tea.Cmd) {
	sess, build, cmd, err := m.sess.Start(req, source, target)
	m.sess = sess
	if err != nil {
		m = m.noteErr(startErrorNotice(err))
		// A refusal this immediate (ErrNoBackend, ErrTargetBusy) never gets as far as a
		// flightScreen for popBuildFailed to pop and reset — the plan/deploy screen that just
		// set its own one-shot Enter guard (#PR8/FB-L4) is still the top of the stack right now,
		// so it is reset here instead, or Enter would stay silently dead on it.
		if top := len(m.stack) - 1; top >= 0 {
			if sr, ok := m.stack[top].(startResetter); ok {
				m.stack[top] = sr.ResetStarting()
			}
		}
		return m, nil
	}
	m = m.popIfBuilding()
	snap, _ := m.sess.BuildSnapshot(build)
	fs := flightScreen{flight.NewAttached(snap, m.poll)}
	m = m.push(fs)
	return m, tea.Batch(fs.Init(), cmd)
}

// startErrorNotice turns a session.Controller.Start/Resume refusal into the operator-facing
// notice — ErrNoBackend keeps the exact wording every other unwired-adaptor gesture in this
// package already used ("not wired up"), so existing callers/tests keep reading the same words;
// anything else (today, only ErrTargetBusy — a promotion already in flight for this target env,
// AGENTS.md invariant 5) is shown verbatim.
func startErrorNotice(err error) string {
	if errors.Is(err, session.ErrNoBackend) {
		return "starting a promotion is not wired up"
	}
	return fmt.Sprintf("could not start promotion: %v", err)
}

// note appends one activity.Entry — every former `m.notice = ...` site in this file now calls
// this instead (Model.activity's own doc comment). detail and url are optional; either may be
// empty.
func (m Model) note(kind activity.Kind, text, detail, url string) Model {
	m.activity = m.activity.Add(activity.Entry{At: time.Now(), Kind: kind, Text: text, Detail: detail, URL: url})
	return m
}

// noteErr is note's shorthand for the common Err case with no detail/URL.
func (m Model) noteErr(text string) Model { return m.note(activity.Err, text, "", "") }

// errText is a nil-safe error-to-string conversion for the note call sites above, which have to
// treat a *Change.Err whose type is a pointer that can itself be nil (Blocked's own
// *engine.BlockedError) the same way a plain nil error would be.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// noteStarted adds the "started" activity entry for a freshly built or resumed drive
// (session.ChangeBuilt) — its own PR URL when Controller already has one (almost never true this
// early, but a resumed drive can already carry it).
func (m Model) noteStarted(build session.BuildID, id string) Model {
	snap, ok := m.sess.BuildSnapshot(build)
	text := fmt.Sprintf("started %s → %s", snap.Source, snap.Target)
	if id != "" {
		text = fmt.Sprintf("started %s (%s → %s)", id, snap.Source, snap.Target)
	}
	url := ""
	if ok && snap.State.PR != nil {
		url = snap.State.PR.URL
	}
	return m.note(activity.Info, text, "", url)
}

// matrixCountdownTick is the in-flight pane's own 1s redraw wake-up (commit 1 of T3-06's own
// train, matrix's counterpart to flight.Model's countdownTick): it carries no data, and exists
// purely so a message stamped with m.matrixTick reaches this root's Update once a second while
// matrixWantsCountdown() holds, making the pane's "next check in Ns" text advance instead of
// looking the same whether the next poll is one second away or wedged. scope.Result[T]'s own
// Stamped implementation and scope.Foreign are what let a stale chain from before a screen
// change get silently dropped rather than firing a second, redundant redraw loop.
type matrixCountdownTick struct{}

// matrixWantsCountdown reports whether the pane has something worth counting down: the matrix is
// the only screen on the stack (armMatrixCountdown never lets a chain outlive the matrix leaving
// the top, though nothing bad happens if it did — a stray redraw of a screen not on top costs
// nothing) and at least one in-flight entry has a real NextPoll (session.Snapshot's own Waiting
// signal — flight.Model's own waiting() reasons about the identical field).
func (m Model) matrixWantsCountdown() bool {
	if !m.matrixOnTop() {
		return false
	}
	ms, ok := m.stack[0].(matrixScreen)
	if !ok {
		return false
	}
	for _, s := range ms.InFlight() {
		if !s.NextPoll.IsZero() {
			return true
		}
	}
	return false
}

// armMatrixCountdown starts the countdown chain the moment matrixWantsCountdown becomes true,
// and does nothing while it already is running (m.matrixTicking) — the chain reschedules itself
// from its own delivery (the scope.Result[matrixCountdownTick] case below), exactly like
// flight.Model's countdownTick does. Called from the handful of places the in-flight list itself
// can change (apply, below, and Init) rather than from every Update return: batching a tick onto
// every single Update result — including one whose caller reads its returned tea.Cmd directly,
// invokes it once, and expects a specific concrete message back (several tests do exactly this
// for flight.AbandonMsg/ReobserveMsg/OverrideCINoneMsg) — silently turned that cmd into a
// tea.BatchMsg instead, which broke every one of them (caught by TestFlightAbandonMsg... going
// red the moment a first draft armed this from a blanket Update wrapper).
func (m Model) armMatrixCountdown() (Model, tea.Cmd) {
	if m.matrixTicking || !m.matrixWantsCountdown() {
		return m, nil
	}
	m.matrixTicking = true
	return m, scope.After(m.matrixTick, time.Second, matrixCountdownTick{})
}

// Update handles window size, theme and the global keys, and forwards everything else to
// the top screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case scope.Result[matrixCountdownTick]:
		if scope.Foreign(m.matrixTick, msg) {
			return m, nil
		}
		if !m.matrixWantsCountdown() {
			m.matrixTicking = false
			return m, nil
		}
		return m, scope.After(m.matrixTick, time.Second, matrixCountdownTick{})
	case tea.KeyboardEnhancementsMsg:
		// Skipped by internal/parity's parser (tea.* messages, not a pkg.XMsg case), so this
		// needs no registry row (train3-design.md's own acceptance check). m.kbd's only
		// consumer is T3-03's help overlay.
		m.kbd = msg
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.quitConfirm != nil {
			m.quitConfirm.WithWidth(m.dialogWidth())
		}
		return m.each(func(s Screen) Screen { return s.SetSize(msg.Width, msg.Height) }), nil
	case tea.BackgroundColorMsg:
		m.styles = ui.NewStyles(msg.IsDark())
		if m.quitConfirm != nil {
			m.quitConfirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
		}
		return m.each(func(s Screen) Screen { return s.SetStyles(m.styles) }), nil
	case tea.KeyPressMsg:
		if m.quitConfirming {
			// Every key while the dialog is up goes to it, including q and ctrl+c's own letter
			// forms — an operator deciding whether to stop every running drive cannot quit past
			// the question by mistake (mirrors flight.Model's own CapturesText-gated confirms
			// one layer down, TestQuitKeyWhileFlightOverrideDialogIsOpenDoesNotQuit's own shape).
			return m.updateQuitConfirm(msg)
		}
		if m.helpOpen {
			return m.updateHelp(msg)
		}
		// The transient q hint (Model.hint's own doc comment) clears at the top of every
		// keypress, before this one is otherwise processed — "clears on the next key" means
		// exactly that, including a repeated q, which simply clears the old hint and sets an
		// identical new one in the same call.
		m.hint = ""
		// Unlike the old transient notice, the activity row is never cleared by a keypress here
		// — only a newer entry replaces it, or opening the log (l) dismisses it (Model.activity's
		// own doc comment, TestNoticeSurvivesKeypress).
		switch msg.String() {
		case "ctrl+c":
			// Immediate, no confirm, regardless of what is running: promotion state is durable
			// (AGENTS.md §4.1) and `hoist resume` recovers whatever this interrupts (Train 2
			// design's own operator decision). cmd/hoist's own main.go prints every id still in
			// flight and its `hoist resume <id>` line once the program actually exits (keymap
			// rule 4) — nothing more happens here than asking to quit.
			return m, tea.Quit
		case "q":
			// Audit keymap rule 4: q quits only from the matrix, and only when it is the only
			// screen on the stack (matrix always sits at stack index 0 — screen.go's own
			// invariant — so "top is the matrix" and "the stack holds only the matrix" are the
			// same condition). Anywhere else q is unbound: it shows the transient hint rather
			// than doing anything, unless the top screen is mid-text-entry, in which case the
			// letter falls through to it untouched (round 5, finding 3's own guard, unchanged).
			if !m.capturesText() {
				if _, onMatrix := m.top().(matrixScreen); onMatrix {
					if m.sess.AnyRunning() {
						return m.openQuitConfirm()
					}
					return m, tea.Quit
				}
				m.hint = "q quits from the matrix · esc goes back"
				return m, nil
			}
		case "?":
			// The help overlay (T3-03): only for a top screen with a stated row in
			// internal/ui/keys' registry (keyed), and never while it's mid-text-entry — a filter
			// query typing "?" is not asking for help.
			if !m.capturesText() {
				if s, ok := m.top().(keyed); ok {
					m.helpOpen = true
					m.helpScreen = s.KeyScreen()
					return m, nil
				}
			}
		case "l":
			// The activity log, from any screen that has opted into the registry (keyed) —
			// generic root-level handling, not a per-screen message (train3-design.md's own
			// "the root intercepts ?, l and q as keys, not as *Msgs"). Since T3-04, matrixScreen
			// implements keyed too, so this now covers the matrix itself as well — its own
			// former l handling (matrix.OpenActivityMsg) is retired.
			if !m.capturesText() {
				if _, ok := m.top().(keyed); ok {
					if _, already := m.top().(activityScreen); !already {
						as := activityScreen{activity.New(m.activity, time.Now)}
						m = m.push(as)
						return m, as.Init()
					}
				}
			}
		}
	case matrix.DriftMsg:
		// The matrix's own async answer, routed to it wherever it sits: the default below
		// forwards to the top screen only, and an env whose answer landed while the plan or
		// picker was open stayed "resolving…" for good (Copilot, #110).
		return m.withMatrix(func(ms matrix.Model) matrix.Model { ms, _ = ms.Update(msg); return ms }), nil
	case matrix.RepoRefreshedMsg:
		// Same #110-shaped routing as DriftMsg just above — F5's fetch can land after the
		// operator has already navigated onto the plan screen or the tag picker. Round-2
		// review (PR #182) found a second gap this case also closes: m.repo (this struct's own
		// field, below — what plan.New/tags/restart/deploy all read) was never updated by an
		// F5 refresh at all, only the matrix screen's OWN internal copy was, so a plan opened
		// after F5 silently kept building from the boot-time snapshot even while the table
		// itself had moved on. matrixRepo() reads back whatever the matrix screen's own
		// RepoRefreshedMsg handling just decided (WithRepo's nil-repo and stale-generation
		// guards live there, once, not duplicated here) and adopts it as the root's own.
		//
		// A failed refresh used to become the matrix's own clear-on-next-key notice — exactly
		// the #164 shape the activity log exists to end (P2 #7, t2-review.md): F5 is async, so
		// the operator can easily have pressed another key by the time this lands, and the
		// refusal disappeared before it was ever read. TakeRefreshError hands back that error
		// (and clears it on the matrix) so it goes to the activity log instead, which survives.
		var refreshErr string
		m = m.withMatrix(func(ms matrix.Model) matrix.Model {
			ms, _ = ms.Update(msg)
			refreshErr, ms = ms.TakeRefreshError()
			return ms
		})
		if r := m.matrixRepo(); r != nil {
			m.repo = r
		}
		if refreshErr != "" {
			m = m.noteErr(refreshErr)
		}
		return m, nil
	case session.Event:
		// D2 of the design: every controller-issued command reaches the root through exactly
		// this one case, and everything it means for the screens on the stack is decided in
		// apply(). session.Event is deliberately not named *Msg (internal/app/session's own
		// doc comment) so internal/parity's own parser — which only ever collects `case
		// pkg.XMsg` from this file — never mistakes this internal plumbing for a navigation
		// message the operator can trigger directly.
		var sessCmd tea.Cmd
		var changes []session.Change
		m.sess, sessCmd, changes = m.sess.Update(msg)
		m, applyCmd := m.apply(changes)
		return m, tea.Batch(sessCmd, applyCmd)
	case matrix.ResumeMsg:
		if msg.ID == "" {
			// A still-Building pane entry (P1 #2): it has no promotion id for Resume(id) to look
			// up yet — Resume("") used to reach Backend.Resume(ctx, "") and fail every time,
			// popping the freshly-pushed flight screen right back off with "could not start
			// promotion". The build is already tracked (session.Controller.Start put it there);
			// re-attaching is just mirroring its current Snapshot onto a fresh flight screen, the
			// same as m.start does the instant Start succeeds — no new command, nothing to wait
			// on. A build that has since finished/failed and dropped out of Controller entirely
			// is reported rather than silently pushing a stale screen.
			snap, ok := m.sess.BuildSnapshot(msg.Build)
			if !ok {
				m = m.noteErr("that promotion is no longer tracked")
				return m, nil
			}
			m = m.popIfBuilding()
			fs := flightScreen{flight.NewAttached(snap, m.poll)}
			m = m.push(fs)
			return m, fs.Init()
		}
		sess, build, cmd, err := m.sess.Resume(msg.ID)
		m.sess = sess
		if err != nil {
			m = m.noteErr("resuming a promotion is not wired up")
			return m, nil
		}
		m = m.popIfBuilding()
		snap, _ := m.sess.BuildSnapshot(build)
		fs := flightScreen{flight.NewAttached(snap, m.poll)}
		m = m.push(fs)
		return m, tea.Batch(fs.Init(), cmd)
	case matrix.OpenPlanMsg:
		// T3-04: p always names the Target (the cursor's column); Source is the one reverse
		// pair when exactly one exists, else "" — the plan screen itself then asks "promote
		// into <target> from…" (plan.New's own stateSelectEnv branch below).
		ps := planScreen{plan.New(m.repo, m.promotable, m.envs, msg.Source, msg.Target, m.planFn, m.history)}
		m = m.push(ps)
		return m, ps.Init()
	case deploy.BackMsg:
		// The deploy screen's Esc, handled exactly like the plan screen's below: without a case
		// here the message was forwarded to the top screen — the deploy screen itself — which
		// fed it to its own viewport, so Esc did nothing and the screen could not be left
		// (Copilot, PR #72). T3-08: openDeploy no longer pops the tags picker before pushing the
		// deploy screen, so this pop lands back on that same tags.Model instance — cursor,
		// filter and loaded rows all intact — rather than on the matrix. popAndRelist still is
		// the right call: it only re-lists when the pop actually lands on the matrix
		// (matrixOnTop), which it no longer does on this path, but a no-op relist costs nothing
		// and keeps this case identical to plan.BackMsg/flight.BackMsg below rather than growing
		// its own special case.
		return m.popAndRelist()
	case plan.BackMsg:
		return m.popAndRelist()
	case plan.StartMsg:
		p := filterTicked(msg.Plan, msg.Ticked)
		direct := msg.Mode == plan.ModeDirect
		view := msg.View
		req := service.StartRequest{
			Plan: p,
			Mode: service.Mode{Direct: direct, Confirmed: direct},
			View: &view,
		}
		return m.start(req, p.SourceEnv, p.TargetEnv)
	case flight.OverrideCINoneMsg:
		// The TUI's `hoist resume <id> --override-ci-none` (#103): the flight screen showing
		// promotion msg.ID asked, behind its own huh.Confirm, to treat a PR with no reported
		// checks as green under ci.none: prompt. session.Controller.OverrideCINone decides what
		// that means and re-drives it; the resulting session.Event stream mirrors the outcome
		// back onto whichever screen is attached to it (AGENTS.md §4.5 — the operator's own,
		// per-promotion decision, never a default).
		sess, cmd, err := m.sess.OverrideCINone(msg.ID)
		if err != nil {
			m = m.noteErr(fmt.Sprintf("override ignored for %s: %v", msg.ID, err))
			return m, nil
		}
		m.sess = sess
		return m, cmd
	case flight.ReobserveMsg:
		// R: the flight screen only asks (session.ReobserveMsg's own doc comment); Poke is the
		// one place that decides whether it can actually happen (busy, not found, or —
		// AGENTS.md invariant 5's own no-races-with-Abandon rule — abandoning). The refusal
		// reaches the operator as a notice, mirroring OverrideCINoneMsg's own convention just
		// above, rather than a silent no-op: this reaches the root even with no flight screen
		// left on the stack to show a notice of its own (matrix.ResumeMsg re-attaches to it
		// still Abandoning, otherwise, with nothing on screen ever explaining why).
		sess, cmd, err := m.sess.Poke(msg.ID)
		if err != nil {
			m = m.noteErr(fmt.Sprintf("cannot re-observe %s: %v", msg.ID, err))
			return m, nil
		}
		m.sess = sess
		return m, cmd
	case flight.BackMsg:
		// esc: stop watching this promotion from the TUI only — the drive itself keeps running,
		// Building included (Train 2 design PR 3, the operator's own decision: leaving flight
		// never cancels anything). Nothing here touches session.Controller at all any more;
		// enter/r on the matrix's in-flight pane (matrix.ResumeMsg below) re-attaches a fresh
		// flightScreen to the exact same BuildID/id later, and session.Controller.Start/Resume's
		// own dedup (already in place before this PR) is what keeps that from ever starting a
		// second driver for it.
		//
		// This always returns all the way to the matrix, closing every screen above it — not just
		// the flight screen itself. A flight screen is pushed directly on top of whatever screen
		// asked for it (the plan or deploy confirm screen, m.start's own doc comment), so a single
		// pop used to land back on that confirm screen with the exact same plan still ticked and
		// ready — Enter there would start the very drive esc just left watching (audit UX-H6/FB-H2,
		// the operator's own decision, follow-up to PR 3). truncateToMatrix's own doc comment has
		// the mechanics.
		return m.truncateToMatrix()
	case flight.OpenPRMsg:
		// openPRMode's three shapes (see Promotion.OpenPRMode's own doc comment): "display"
		// never attempts a launch at all — nothing here needs m.openURL, so a headless/SSH
		// session with no browser opener wired at all is never even in the nil-check branch
		// below for this mode. "both" attempts the launch (same as "launch") but always shows
		// the URL as text afterward too, regardless of outcome, so a copy/paste fallback
		// exists even when the launch itself succeeds. Anything else (including the empty
		// string) behaves exactly like "launch" always did: silent on success, a notice only
		// on failure — preserving the original pre-preferences behavior for any caller that
		// never set this field.
		if m.openPRMode == "display" {
			m = m.note(activity.Info, msg.URL, "", msg.URL)
			return m, nil
		}
		if m.openURL == nil {
			// Mirrors startPromotion's own nil convention above: a caller that hasn't
			// wired a browser opener in gets a clear notice instead of a nil-pointer
			// panic (documented follow-up work, per PR #39's own report).
			m = m.note(activity.Err, fmt.Sprintf("open PR not wired yet: %s", msg.URL), "", msg.URL)
			return m, nil
		}
		// The launcher can block for up to browser_launch_timeout; it runs as a command and
		// reports back through openURLResultMsg, so Update never waits on it (#56).
		open, url := m.openURL, msg.URL
		return m, func() tea.Msg { return openURLResultMsg{url: url, err: open(url)} }
	case openURLResultMsg:
		// Every branch that shows anything carries msg.url as the entry's own URL — "openURLResultMsg
		// entries carry the URL" (Train 2 design PR9) — so the activity screen and the bottom row
		// alike can show it; the quiet-success "launch" case adds no entry at all, unchanged from
		// the old notice convention's own silence there.
		switch {
		case m.openPRMode == "both" && msg.err != nil:
			m = m.note(activity.Err, fmt.Sprintf("%s (could not open automatically: %v)", msg.url, msg.err), "", msg.url)
		case m.openPRMode == "both":
			m = m.note(activity.Info, msg.url, "", msg.url)
		case msg.err != nil:
			m = m.note(activity.Err, fmt.Sprintf("could not open %s: %v", msg.url, msg.err), "", msg.url)
		}
		return m, nil
	case flight.WatchMsg:
		// w on the flight screen (T3-06): the same openWatch the matrix's own w/OpenWatchMsg
		// already uses — flight has already resolved which family (families(), its own doc
		// comment, raising its own chooser when there was more than one), so this is exactly
		// the plain (family, target) pair openWatch takes, pushed on top of the flight screen
		// rather than replacing it (esc off the watch screen returns here, still mirroring the
		// same running drive).
		return m.openWatch(msg.Family, msg.Target)
	case flight.AbandonMsg:
		// The flight screen's own shift+x gesture already confirmed the operator wants this
		// (flight.AbandonMsg's own doc comment) — pop back to the matrix immediately and let
		// session.Controller.Abandon do the real work: cancel, wait for a busy Step to actually
		// stop, then the real Backend.Abandon call.
		m = m.truncate(0)
		sess, cmd := m.sess.Abandon(msg.ID)
		m.sess = sess
		return m, cmd
	case matrix.OpenConfigMsg:
		if m.configText == "" {
			m = m.noteErr("no config to show: the launcher supplied none")
			return m, nil
		}
		cs := configScreen{appconfig.New(m.configPath, m.configFound, m.configText)}
		m = m.push(cs)
		return m, cs.Init()
	case appconfig.BackMsg:
		return m.pop(), nil
	case activity.BackMsg:
		return m.pop(), nil
	case matrix.OpenRestartMsg:
		return m.openRestart(msg.Family, msg.Target)
	case apprestart.BackMsg:
		// FB-L3: esc while a confirmed restart is still starting or rolling leaves the screen,
		// but not the restart itself — it is a live cluster operation this package cannot cancel
		// — so the activity log gets a line naming that instead of the screen just vanishing
		// mid-roll with nothing left to say so.
		m = m.pop()
		if msg.RollingContinues {
			m = m.note(activity.Info, "rollout continues", "", "")
		}
		return m, nil
	case matrix.OpenWatchMsg:
		return m.openWatch(msg.Family, msg.Target)
	case watch.BackMsg:
		return m.pop(), nil
	case matrix.OpenTagsMsg:
		var mapped bool
		var regTagsFn tags.RegTagsFunc
		var gitTagsFn tags.GitTagsFunc
		var metaFn tags.MetaFunc
		if m.tagsFn != nil {
			mapped, regTagsFn, gitTagsFn, metaFn = m.tagsFn(msg.ImageRepo)
		}
		production := m.envs.IsProduction(msg.Target)
		stagingEnv, stagingTags, hasMismatch := tags.StagingMismatch(m.repo, msg.ImageRepo, msg.Target, m.envs)
		opts := tags.Options{
			Mapped: mapped, Production: production,
			StagingEnv: stagingEnv, StagingTags: stagingTags, HasStagingMismatch: hasMismatch,
			RegTags: regTagsFn, GitTags: gitTagsFn, Meta: metaFn,
			History: m.history,
		}
		if d, ok := tags.DeclaredIn(m.repo, msg.ImageRepo, msg.Target); ok {
			opts.Declared = &d
		}
		ts := tagsScreen{tags.New(msg.ImageRepo, msg.Target, opts)}
		m = m.push(ts)
		return m, ts.Init()
	case tags.BackMsg:
		return m.pop(), nil
	case tags.SelectedMsg:
		// T3-07/T3-08: the picker's own direct-commit gesture retired (tags.DirectRequestedMsg
		// is gone); the deploy confirm screen offers shift+d itself now, once the diff is
		// already on screen, so there is only ever one path in here.
		return m.openDeploy(msg.ImageRepo, msg.Tag, msg.Digest, msg.Target, deployHistory(msg.Delta, msg.Declared, msg.DeclaredSince, msg.HistoryNote))
	case deploy.StartMsg:
		direct := msg.Mode == deploy.ModeDirect
		view := msg.View
		req := service.StartRequest{
			Plan: msg.Plan,
			Mode: service.Mode{Direct: direct, Confirmed: msg.Confirmed},
			View: &view,
		}
		return m.start(req, msg.Plan.SourceEnv, msg.Plan.TargetEnv)
	}
	if len(m.stack) == 0 {
		return m, nil
	}
	top := len(m.stack) - 1
	s, cmd := m.stack[top].Update(msg)
	stack := append([]Screen(nil), m.stack...)
	stack[top] = s
	m.stack = stack
	return m, cmd
}

// apply reacts to every session.Change one Update(session.Event) call produced (D2/D3 of the
// Train 2 design): most changes mean "mirror the fresher Snapshot onto whichever flightScreen is
// attached to this Build" — the default case, handled once by mirrorAttached below — with three
// exceptions this root itself has to act on: a failed build has no Snapshot left to mirror onto
// (the entry was never fully created) and instead pops the preflight screen with a notice; a
// fresh listing goes to the matrix's in-flight pane, merged with whatever this session's own
// controller still has live (a Building/Stepping entry with no state file on disk yet, which the
// listing alone would never show); and an abandon's own outcome adds a notice and, on success,
// asks for a fresh listing right away rather than waiting for the next tick.
func (m Model) apply(changes []session.Change) (Model, tea.Cmd) {
	// needsRefresh/needsRelist are decided across the WHOLE batch, and acted on at most once
	// each, after the loop — never per change. A single Driver.Step can land and finish in the
	// same tick (session.Controller.Update's own doc comment: "a stepMsg that both lands and
	// finishes" produces ChangeLanded AND ChangeDone together), and calling
	// requestMatrixRefresh/Relist once per change would refresh twice for what the operator
	// experiences as one event — matrix's own refreshingRepo/refreshAgain coalescing would stop
	// that from corrupting anything, but it would still cost a second, pointless fetch
	// (TestLandedRefreshesOnce's own point).
	var cmds []tea.Cmd
	var needsRefresh, needsRelist bool
	for _, ch := range changes {
		switch ch.Kind {
		case session.ChangeBuildFailed:
			m = m.popBuildFailed(ch.Build, ch.Err)
		case session.ChangeListed:
			m.lastList = ch.List
			m = m.remergeInFlight()
		case session.ChangeStepped:
			// Train 2 design PR 4, FB-M8 for a drive running here: the pane reflects this
			// session's own freshest live snapshot the instant a Step lands, rather than
			// waiting for the next listing tick (session.Config.ListEvery) — no forge call, just
			// a re-merge of what this session already knows against the last full listing.
			m = m.mirrorAttached(ch.Build, ch.Snap)
			m = m.remergeInFlight()
		case session.ChangeBuilt:
			// PR9's own "started" entry — the PR URL is almost never known this early (the
			// preflight that produces this Change runs before anything is pushed), but a resumed
			// drive can already have one, and noteStarted names it when so.
			m = m.mirrorAttached(ch.Build, ch.Snap)
			m = m.noteStarted(ch.Build, ch.ID)
		case session.ChangeLanded:
			// A landed drive can have moved exactly what the matrix's own drift column and repo
			// read describe (a merge, a direct push) — refresh both the way F5 does
			// (matrix.Model.RequestRefresh, its own repoGen guard and refreshAgain coalescing
			// unchanged) so the operator sees the new tag without pressing F5 themselves, and
			// relist right away so the pane doesn't wait for the next tick to show it landed.
			m = m.mirrorAttached(ch.Build, ch.Snap)
			m = m.note(activity.OK, ch.ID+" landed", "", "")
			needsRefresh = true
			needsRelist = true
		case session.ChangeDone:
			// Silent here (already reported once, at ChangeLanded, on the same or an earlier
			// batch — session.Controller.Update's own doc comment on "a stepMsg that both lands
			// and finishes"): a direct promotion with no separate PR/landed step still needs the
			// refresh and relist, just not a second activity entry for the same event.
			m = m.mirrorAttached(ch.Build, ch.Snap)
			needsRefresh = true
			needsRelist = true
		case session.ChangeBlocked:
			m = m.mirrorAttached(ch.Build, ch.Snap)
			m = m.note(activity.Err, fmt.Sprintf("%s blocked: %s", ch.ID, errText(ch.Err)), "", "")
			needsRelist = true
		case session.ChangeFailed:
			// The short Text is what the bottom row can show on one line; the full error goes in
			// Detail, read in full only on the activity screen (l) — PR9's own "failed with full
			// error" (TestLongErrorFullInActivityView is this case's own regression test).
			m = m.mirrorAttached(ch.Build, ch.Snap)
			m = m.note(activity.Err, ch.ID+" failed", errText(ch.Err), "")
			needsRelist = true
		case session.ChangeRefused:
			// A whole-listing failure (the forge/state directory unreachable) — left for a
			// later train PR to surface; the pane simply keeps its last good listing.
		case session.ChangeAbandoned:
			m = m.note(activity.OK, "abandoned "+ch.ID, "", "")
			m = m.mirrorAttached(ch.Build, ch.Snap)
			needsRelist = true
		case session.ChangeAbandonFailed:
			m = m.note(activity.Err, fmt.Sprintf("abandon %s failed: %v", ch.ID, ch.Err), "", "")
			m = m.mirrorAttached(ch.Build, ch.Snap)
		default:
			m = m.mirrorAttached(ch.Build, ch.Snap)
		}
	}
	if needsRefresh {
		var refreshCmd tea.Cmd
		m, refreshCmd = m.requestMatrixRefresh()
		cmds = append(cmds, refreshCmd)
	}
	if needsRelist {
		var relistCmd tea.Cmd
		m.sess, relistCmd = m.sess.Relist()
		cmds = append(cmds, relistCmd)
	}
	// Every case above can change what the matrix's in-flight pane shows (a fresh listing, a
	// live entry's own Step landing with a new NextPoll) — apply already batches its own cmds,
	// so this is a safe place to also arm the countdown redraw, unlike Update's own many other
	// single-cmd return points (armMatrixCountdown's own doc comment).
	var tickCmd tea.Cmd
	m, tickCmd = m.armMatrixCountdown()
	cmds = append(cmds, tickCmd)
	return m, tea.Batch(cmds...)
}

// mirrorAttached finds the flightScreen (wherever it sits in the stack) attached to build and
// calls its Mirror with the freshest Snapshot the controller still has — or, when the entry has
// already been removed (Done/Abandoned, whose own Change carries fallback in ch.Snap — see
// session.Change.Snap's own doc comment), that fallback instead.
func (m Model) mirrorAttached(build session.BuildID, fallback session.Snapshot) Model {
	for i, s := range m.stack {
		fs, ok := s.(flightScreen)
		if !ok {
			continue
		}
		if _, b := fs.Attached(); b != build {
			continue
		}
		snap, ok := m.sess.BuildSnapshot(build)
		if !ok {
			snap = fallback
		}
		stack := append([]Screen(nil), m.stack...)
		stack[i] = flightScreen{fs.Mirror(snap)}
		m.stack = stack
		return m
	}
	return m
}

// remergeInFlight re-merges this session's own live snapshots (session.Controller.Live) into the
// matrix's in-flight pane against the last full listing this session saw (m.lastList) — the exact
// merge session.ChangeListed already does, replayed with fresher live data and no forge call
// (Train 2 design PR 4). Used both for a fresh listing itself and, ChangeStepped's own case
// above, for the pane to reflect a live entry's progress in between listing ticks.
func (m Model) remergeInFlight() Model {
	live := m.sess.Live()
	list := m.lastList
	return m.withMatrix(func(ms matrix.Model) matrix.Model {
		return ms.SetInFlight(mergeInFlight(list, live), nil)
	})
}

// requestMatrixRefresh triggers the matrix's own completion-triggered refresh (matrix.Model.
// RequestRefresh: refresh() for drift, askRepoRefresh() for the repo, through the existing
// repoGen guard and its refreshAgain coalescing — Train 2 design PR 4). The matrix always sits
// at stack index 0 (push never inserts below it, pop and truncateToMatrix both refuse to remove
// it), so this indexes directly rather than searching the whole stack the way withMatrix does
// for a message that can land while some other screen is on top.
func (m Model) requestMatrixRefresh() (Model, tea.Cmd) {
	if len(m.stack) == 0 {
		return m, nil
	}
	ms, ok := m.stack[0].(matrixScreen)
	if !ok {
		return m, nil
	}
	nm, cmd := ms.RequestRefresh()
	m.stack = append([]Screen(nil), m.stack...)
	m.stack[0] = matrixScreen{nm}
	return m, cmd
}

// startResetter is implemented by a screen adapter that sets its own one-shot Enter guard
// before emitting a StartMsg (planScreen, deployScreen — #PR8/FB-L4, plan.Model.starting's own
// doc comment): popBuildFailed below clears it on whichever screen a failed build pops back
// onto, since nothing else would — that screen's own guard has no way to know the build it
// started has since failed.
type startResetter interface{ ResetStarting() Screen }

// popBuildFailed removes the preflight flightScreen a Start/Resume call's own failure leaves
// with nothing to mirror onto, and shows why.
func (m Model) popBuildFailed(build session.BuildID, err error) Model {
	if top := len(m.stack) - 1; top >= 0 {
		if fs, ok := m.stack[top].(flightScreen); ok {
			if _, b := fs.Attached(); b == build {
				m = m.pop()
			}
		}
	}
	if top := len(m.stack) - 1; top >= 0 {
		if sr, ok := m.stack[top].(startResetter); ok {
			m.stack[top] = sr.ResetStarting()
		}
	}
	m = m.noteErr(fmt.Sprintf("could not start promotion: %v", err))
	return m
}

// mergeInFlight combines a fresh listing (state files on disk, each re-observed) with this
// session's own live entries (session.Controller.Live) — a promotion still in its Building or
// early Stepping window has no state file yet and so never appears in list, and the freshest
// data for one that does is the controller's own in-memory copy, not whatever the listing found
// a moment earlier. Live entries win by id; a listed entry with no live counterpart is kept as
// re-observed. An id-less (still Building) live entry has nothing to collide with and is always
// kept.
func mergeInFlight(list []service.Listed, live []session.Snapshot) []flight.Summary {
	out := make([]flight.Summary, 0, len(list)+len(live))
	seen := make(map[string]bool, len(live))
	for _, s := range live {
		if s.ID != "" {
			seen[s.ID] = true
		}
		out = append(out, summaryForSnapshot(s))
	}
	for _, l := range list {
		if seen[l.State.ID] {
			continue
		}
		out = append(out, summaryFor(l))
	}
	return out
}

// summaryFor turns one service.Listed into the flight.Summary the in-flight pane renders —
// Unconfigured and Err both become a Summary whose own Err names why re-observation could not
// happen at all (never dropped), exactly as cmd/hoist's old observeForList wording did.
func summaryFor(l service.Listed) flight.Summary {
	switch {
	case l.Unconfigured:
		return flight.Summarize(l.State, false, nil, fmt.Errorf("repo %s is not in the config file", l.State.RepoFullName))
	case l.Err != nil:
		return flight.Summarize(l.State, false, nil, l.Err)
	default:
		return flight.Summarize(l.State, l.Done, l.Statuses, nil)
	}
}

// summaryForSnapshot is summaryFor's twin for a session.Controller entry still tracked in
// memory — the same conversion NewAttached/Mirror use to render a flight screen, reused here so
// the pane and the screen never disagree about what one entry looks like.
func summaryForSnapshot(s session.Snapshot) flight.Summary {
	state := s.State
	if s.Phase == session.Building {
		state = engine.PromotionState{SourceEnv: s.Source, TargetEnv: s.Target, Direct: s.Direct}
	}
	sum := flight.Summarize(state, s.Done, s.Statuses, s.Err)
	// This entry is being driven by THIS session, right now — never merely re-observed from a
	// listing (summaryFor's own path never sets this) — so the pane can say so (Train 2 design
	// PR 3, matrix.compactLine/expandedSections).
	sum.Live = true
	// Carried through even once ID is known, so a caller never has to branch on phase to decide
	// which handle to use — matrix.ResumeMsg always has this session's own BuildID available for
	// a still-Building entry, which has no promotion id yet for Resume(id) to look up (P1 #2:
	// enter on a Building pane entry used to call Backend.Resume("") and fail).
	sum.Build = s.Build
	// s.NextPoll is only ever set for a Waiting live entry (Mirror's own doc comment in
	// internal/app/flight/model.go) — carried straight through so the matrix's in-flight pane
	// can word its own countdown off the identical field flight.Model's waiting() already reads,
	// rather than re-deriving "is this entry waiting" a second way.
	sum.NextPoll = s.NextPoll
	return sum
}

// bottomLine is the root's own activity row (Model.activity's own doc comment, replacing the old
// transient notice): the latest entry's Text, truncated to leave room for a fixed "· l: activity
// (N)" suffix so the row is always exactly one line, never the old notice's up-to-NoticeMaxLines
// wrap — the full text (and Detail, and URL) is still there in full on the activity screen (l);
// this row only ever has to name enough of it to be worth reading, and how many more there are.
// Empty when nothing has happened yet.
func (m Model) bottomLine() string {
	e, ok := m.activity.Latest()
	if !ok {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	suffix := fmt.Sprintf(" · l: activity (%d)", m.activity.Len())
	room := width - ansi.StringWidth(suffix)
	text := redact.Strings(e.Text)
	if room > 0 {
		text = ansi.Truncate(text, room, "…")
	} else {
		text = ""
	}
	return text + suffix
}

// activityStyle colours the root's activity row by the latest entry's own Kind (T3-02): Info
// for a plain report, Good for a landed or completed outcome, Bad for a refusal or failure —
// replacing the uniform amber every entry used to render in regardless of what happened, the
// same colour a bare warning or a genuine failure got. Info is also the default for the empty
// case (no entry yet), though bottomLine returns "" then and NoticeLinesStyled renders nothing.
func (m Model) activityStyle() lipgloss.Style {
	e, ok := m.activity.Latest()
	if !ok {
		return m.styles.Info
	}
	switch e.Kind {
	case activity.OK:
		return m.styles.Good
	case activity.Err:
		return m.styles.Bad
	default:
		return m.styles.Info
	}
}

// bottomText and bottomStyle pick which one row the terminal's last line shows (see Model.hint's
// own doc comment): the transient q hint takes priority over the activity row while it is set,
// since only one such row exists on screen at a time — never both at once, and never the hint
// added as its own activity.Entry (T3-03's own decision: it is not a record of anything that
// happened, only a momentary correction).
func (m Model) bottomText() string {
	if m.hint != "" {
		return m.hint
	}
	return m.bottomLine()
}

func (m Model) bottomStyle() lipgloss.Style {
	if m.hint != "" {
		return m.styles.Warn
	}
	return m.activityStyle()
}

// View renders the top screen in the alternate screen buffer, with the root's own bottom row
// (bottomText/bottomStyle, above — the transient q hint, or else the activity row) on the
// terminal's last row when there is one to show.
//
// The row's own line is taken OUT of the screen above rather than appended after it. Every
// screen draws through ui.Frame.Render, which emits exactly `height` lines, so a row merely
// appended to that landed on row height+1 and the alternate screen buffer never showed it:
// pressing enter on the deploy confirm screen and having the promotion refused — an in-flight
// conflict, a missing repos[].github, a claim conflict — was indistinguishable from a dead key,
// and the only way to read the reason was to re-run the equivalent command on the CLI (#164).
// Re-sizing the top screen here, on every render, rather than once when the entry was added,
// keeps this a pure render concern: nothing in the stack is mutated, and unlike the old notice
// this row is NOT cleared on the next keypress (Model.activity's own doc comment,
// TestNoticeSurvivesKeypress) — it only goes away once the log itself is empty, which never
// happens once the first entry lands.
func (m Model) View() tea.View {
	notice := ui.NoticeLinesStyled(m.bottomStyle(), m.bottomText(), m.width)
	content := ""
	if n := len(m.stack); n > 0 {
		top := m.stack[n-1]
		// Never shrink the screen to nothing: a terminal too short to hold both keeps the
		// screen at full height and the row is the thing that goes missing, which is no
		// worse than today and leaves the screen legible.
		if h := m.height - len(notice); len(notice) > 0 && h > 0 {
			top = top.SetSize(m.width, h)
		}
		content = top.View()
	}
	if len(notice) > 0 {
		content += "\n" + strings.Join(notice, "\n")
	}
	if m.helpOpen {
		content = ui.Dialog(m.styles, content, keys.HelpTitle(m.helpScreen), keys.HelpView(m.helpScreen, m.kbd), m.width, m.height)
	}
	if m.quitConfirming && m.quitConfirm != nil {
		content = ui.Dialog(m.styles, content, "quit hoist?", m.quitConfirm.View(), m.width, m.height)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	// T3-01: ask every run for the one Kitty-protocol feature that can tell a caps-lock letter
	// apart from a real shift (internal/ui/keys.Binding.Matches, point 3) — a terminal that
	// doesn't support it, or hasn't opted in, just never sends a KeyboardEnhancementsMsg back,
	// and the legacy path (point 4 of Matches) is what every terminal runs today regardless
	// (train3-design.md's own "Modifier" note). Requesting it costs nothing on a terminal that
	// ignores it (tmux, Terminal.app default sessions verified in that note).
	v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	return v
}

// openQuitConfirm raises q's own dialog, reached only when session.Controller.AnyRunning is true
// — the tag picker's D shape, one layer up: keypress, then a huh.Confirm, and only a yes actually
// stops anything.
func (m Model) openQuitConfirm() (Model, tea.Cmd) {
	m.quitConfirming = true
	m.quitConfirmValue = false
	title := "Quit hoist? Every drive still running here will stop being watched — nothing is rolled back, and `hoist resume` picks each one back up later."
	m.quitConfirm = huh.NewConfirm().Title(title).Value(&m.quitConfirmValue)
	// Not decoration: huh.NewConfirm ships a zero keymap, so without this y/n/enter do nothing
	// (AGENTS.md §9 entry 6).
	m.quitConfirm.WithKeyMap(huh.NewDefaultKeyMap())
	m.quitConfirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.quitConfirm.WithWidth(m.dialogWidth())
	return m, tea.Batch(m.quitConfirm.Init(), m.quitConfirm.Focus())
}

func (m Model) updateQuitConfirm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		switch kmsg.String() {
		case "esc":
			// Leave the dialog without answering it — huh's own Update swallows Esc, the same
			// trap the tag picker's round-3 finding caught one layer down.
			m.quitConfirming = false
			return m, nil
		case "enter":
			m.quitConfirming = false
			if !m.quitConfirmAgreed() {
				return m, nil
			}
			// StopAll cancels every tracked entry's ctx and drops it from this process — the
			// real branch/PR/state file are untouched, and `hoist resume` re-observes each one
			// from the top exactly as if the operator had quit before ever starting the TUI
			// (AGENTS.md §4.1).
			m.sess = m.sess.StopAll()
			return m, tea.Quit
		}
	}
	f, cmd := m.quitConfirm.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.quitConfirm = c
	}
	return m, cmd
}

// quitConfirmAgreed reads the operator's answer from the widget, never from quitConfirmValue —
// Value binds a pointer into the copy of this value-typed model that built the widget, so a later
// Update can never see a write through it (AGENTS.md §9 entry 6).
func (m Model) quitConfirmAgreed() bool {
	if m.quitConfirm == nil {
		return false
	}
	v, _ := m.quitConfirm.GetValue().(bool)
	return v
}

// dialogWidth sizes a root-level huh dialog the same way flight.Model and tags.Model already
// size theirs.
func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

// openRestart pushes the restart screen for one family in one env. The Deployment names come
// from the repo here, in the root, for the same reason openDeploy builds its plan here: the
// matrix names a choice, and working out what that choice would touch is the root's job
// (AGENTS.md §4.8).
//
// The matrix is NOT popped, unlike the deploy path: a restart is small and repeatable, and
// backing out of the confirmation should land on the cell it started from.
func (m Model) openRestart(family, target string) (tea.Model, tea.Cmd) {
	if m.restartFn.Read == nil {
		return m.noteErr("restarting needs a cluster connection, and none is configured"), nil
	}
	names, err := restart.Targets(m.repo, target, []string{family})
	if err != nil {
		return m.noteErr(fmt.Sprintf("cannot restart %s in %s: %v", family, target, err)), nil
	}
	rs := restartScreen{apprestart.New(target, family, names, m.envs.IsProduction(target), m.restartFn, m.styles)}
	m = m.push(rs)
	return m, rs.Init()
}

// openWatch pushes the watch screen for one family in one env. Resolving the family to its
// Application and workload names is the builder's job (cmd/hoist), the same split
// openRestart makes: the matrix names a choice, the root asks what it means.
func (m Model) openWatch(family, target string) (tea.Model, tea.Cmd) {
	if m.watchFn == nil {
		return m.noteErr("watching needs a cluster connection, and none is configured"), nil
	}
	funcs, err := m.watchFn(family, target)
	if err != nil {
		return m.noteErr(fmt.Sprintf("cannot watch %s in %s: %v", family, target, err)), nil
	}
	ws := watchScreen{watch.New(family, target, funcs, m.styles)}
	m = m.push(ws)
	return m, ws.Init()
}

// openDeploy builds the plan for one image into one env and pushes the confirm screen for it.
// The plan is built here, in the root, rather than in the picker: internal/app/tags names a
// choice, and deciding what that choice would write is the root's job (AGENTS.md §4.8).
//
// A build failure is a notice on the matrix rather than a screen: the operator picked a tag
// that cannot be written (an unpinned ref, a repo with no occurrence in the env), and the
// useful response is the reason, not an empty confirm screen.
//
// planFn (svc.Plan) is called directly here, off the Update call stack notwithstanding —
// AGENTS.md §4.3's own reasoning is about a call that can talk to a cluster or registry; a
// deploy plan never resolves a digest (the reference is caller-supplied), so Plan's own Deploy
// branch is exactly as pure as gitops.BuildDeployPlan was. Plan also attaches
// WarnDeployIntoProduction itself now, so the confirm screen and the PR body it later renders
// agree with the CLI's dry run by construction (service:Plan, PR B) rather than by both callers
// remembering to attach it.
func (m Model) openDeploy(imageRepo, tag, digest, target string, h deploy.History) (tea.Model, tea.Cmd) {
	ref := image.Ref{Repo: imageRepo, Tag: tag, Digest: digest}
	ctx, cancel := scope.Timeout(scope.Resolve)
	defer cancel()
	pc, err := m.planFn(ctx, service.PlanRequest{Repo: m.repo, Target: target, Deploy: &ref})
	if err != nil {
		// T3-08: the picker stays on the stack (below) — a build failure is a notice on IT,
		// not a pop back past it to the matrix, since the operator's next move is most likely
		// picking a different tag from the very list they were just looking at.
		return m.noteErr(fmt.Sprintf("cannot deploy %s to %s: %v", ref, target, err)), nil
	}
	pl := pc.Plan
	ds := deployScreen{deploy.New(pl, m.repo.Root, ref.String(), m.envs, m.styles).WithHistory(h).WithView(pc.View)}
	// T3-08: the picker stays on the stack underneath, unlike before this train — deploy.BackMsg
	// (esc) pops back onto that same tags.Model instance, cursor/filter/loaded rows intact,
	// rather than all the way to the matrix. Direct mode is the deploy screen's own shift+d
	// gesture now (Model.onKey/toggleDirect), never set here.
	m = m.push(ds)
	return m, ds.Init()
}

// deployHistory repacks what the picker learned into the confirm screen's own plain shape:
// the two packages share no type for it, so neither imports the other.
func deployHistory(delta *migrate.Delta, declared *tags.Declared, since time.Time, note string) deploy.History {
	h := deploy.History{Delta: delta, Note: note, Since: since}
	if declared != nil {
		h.Declared = declared.Ref
		h.DeclaredRefs = declared.Refs
	}
	return h
}

// push adds a screen on top, sized and themed like the rest. The slice is copied so the
// returned model shares nothing with the receiver.
func (m Model) push(s Screen) Model {
	s = s.SetStyles(m.styles)
	if m.width > 0 && m.height > 0 {
		s = s.SetSize(m.width, m.height)
	}
	m.stack = append(append([]Screen(nil), m.stack...), s)
	return m
}

// pop drops the top screen, when there is more than one — the matrix, at the bottom, is
// never popped. doc.go's "the stack has push only today" note is what this ends: a screen
// asks to be popped with a message the root recognizes by concrete type (matrix.OpenPlanMsg
// pushes, plan.BackMsg pops), never by calling back into app itself.
func (m Model) pop() Model {
	if len(m.stack) <= 1 {
		return m
	}
	return m.truncate(len(m.stack) - 2)
}

// closer is implemented by a screen adapter whose underlying Model owns a scope.Scope
// (plan, watch, restart, tags today) — promoted automatically, since every adapter embeds its
// package's Model by value (internal/app/screen.go). truncate calls Close on every screen it
// removes, so an outstanding DoCtx call for a screen that no longer exists on the stack is
// cancelled the moment it stops existing, rather than left to run until its own per-call timeout
// (AGENTS.md §4.8).
type closer interface{ Close() }

// truncate drops every screen above index n, closing each one removed. It is the general form
// both pop (n = len(stack)-2) and truncateToMatrix (n = 0) reduce to, used directly wherever more
// than one screen must go at once (flight.AbandonMsg's own immediate pop-to-matrix). n outside
// [0, len(stack)-1) is a no-op — never a slice panic, since nothing this package builds computes
// n from anything but a stack length it already holds.
func (m Model) truncate(n int) Model {
	if n < 0 || n >= len(m.stack)-1 {
		return m
	}
	for _, s := range m.stack[n+1:] {
		if c, ok := s.(closer); ok {
			c.Close()
		}
	}
	m.stack = append([]Screen(nil), m.stack[:n+1]...)
	return m
}

// popIfBuilding pops the top screen only when it is a flight screen still attached to a
// Building entry (flight.Model's own building field, mirrored from session.Building) — a
// superseding start()/matrix.ResumeMsg call uses it to undo the preflight screen a previous one
// left, reverting to whatever was underneath (the confirm screen, #164-tested to show the
// notice correctly) exactly as if that screen had never been pushed.
func (m Model) popIfBuilding() Model {
	if top := len(m.stack) - 1; top >= 0 {
		if fs, ok := m.stack[top].(flightScreen); ok {
			if id, _ := fs.Attached(); id == "" {
				return m.pop()
			}
		}
	}
	return m
}

// popAndRelist pops and, when that lands on the matrix, re-lists what is in flight at once
// rather than waiting for the next tick: the operator just came back from a screen that may
// have started or finished a promotion.
func (m Model) popAndRelist() (Model, tea.Cmd) {
	m = m.pop()
	if m.matrixOnTop() {
		var cmd tea.Cmd
		m.sess, cmd = m.sess.Relist()
		return m, cmd
	}
	return m, nil
}

// truncateToMatrix drops every screen above the matrix at once, unlike pop (and popAndRelist),
// which remove exactly one. It is for esc gestures the operator expects to close out of
// entirely, regardless of how many screens happen to be stacked above the matrix right now —
// flight.BackMsg's own case is the motivating one: a flight screen is pushed directly on top of
// the plan or deploy confirm screen that started it (m.start's doc comment), so popping only the
// flight screen used to land back on that confirm screen, still ticked and ready to start the
// very same drive again on Enter (audit UX-H6/FB-H2). The matrix always sits at stack index 0
// (push never inserts below it, pop refuses to remove it — pop's own doc comment), so slicing to
// it is always safe, even when the stack is already just the matrix. Landing on the matrix always
// re-lists, mirroring popAndRelist's own reason for doing so.
func (m Model) truncateToMatrix() (Model, tea.Cmd) {
	m = m.truncate(0)
	var cmd tea.Cmd
	m.sess, cmd = m.sess.Relist()
	return m, cmd
}

// withMatrix applies f to the matrix screen wherever it sits in the stack — today always the
// bottom, and (after tags.SelectedMsg/DirectRequestedMsg's own pop) always the new top too,
// since matrix.OpenTagsMsg is the only thing that ever pushes a tags screen, always directly
// onto the matrix.
func (m Model) withMatrix(f func(matrix.Model) matrix.Model) Model {
	for i, s := range m.stack {
		ms, ok := s.(matrixScreen)
		if !ok {
			continue
		}
		stack := append([]Screen(nil), m.stack...)
		stack[i] = matrixScreen{f(ms.Model)}
		m.stack = stack
		return m
	}
	return m
}

// matrixRepo reads the matrix screen's own current *gitops.Repo back out, wherever it actually
// sits in the stack — nil only if no matrix screen exists at all, which withMatrix's own
// no-op-if-absent contract means never happens in practice (the bottom of the stack always is
// one). Used by the RepoRefreshedMsg case above so the root's own repo snapshot follows F5
// without re-deriving matrix.Model's staleness/nil guards a second time here.
func (m Model) matrixRepo() *gitops.Repo {
	for _, s := range m.stack {
		if ms, ok := s.(matrixScreen); ok {
			return ms.Repo()
		}
	}
	return nil
}

// top returns the screen currently on top of the stack, or nil when the stack is empty (never
// true in practice once New has run — the matrix always sits at index 0 — but every caller here
// treats a nil result the same as "nothing to defer to", matching capturesText's own convention).
func (m Model) top() Screen {
	if len(m.stack) == 0 {
		return nil
	}
	return m.stack[len(m.stack)-1]
}

// updateHelp handles every key while the help overlay is open: esc, ? and enter all close it
// (train3-design.md's T3-03 scope); ctrl+c still quits immediately, exactly as it does
// everywhere else; every other key is swallowed rather than reaching the screen underneath,
// so the overlay behaves like a real modal rather than a transparent one that happens to also
// draw a box.
func (m Model) updateHelp(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "?", "enter":
		m.helpOpen = false
		return m, nil
	}
	return m, nil
}

// InFlightIDs lists every promotion id this session still tracks, sorted — cmd/hoist's own
// main.go prints these with their `hoist resume <id>` line once the program has actually quit
// (keymap rule 4: "on the way out it prints the ids still in flight"), so an operator who quit
// past a running drive (q's own confirm, or ctrl+c's immediate exit) is told exactly how to pick
// each one back up rather than having to remember or reconstruct it.
func (m Model) InFlightIDs() []string {
	var ids []string
	for _, snap := range m.sess.Live() {
		if snap.ID != "" {
			ids = append(ids, snap.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// capturesText reports whether the top screen is currently mid-text-entry (see Screen.
// CapturesText's own doc comment) — false when the stack is empty, matching how View/Update
// already treat an empty stack as "nothing to defer to".
func (m Model) capturesText() bool {
	if len(m.stack) == 0 {
		return false
	}
	return m.stack[len(m.stack)-1].CapturesText()
}

// filterTicked narrows p's Edits down to only the repos the operator actually ticked before
// confirming — the same set plan.Model.recomputeDiff already filters the confirm screen's own
// diff by (internal/app/plan/rows.go's RenderDiff: "if !ticked[e.Ref.Repo] ... continue").
// Without this, plan.StartMsg's Plan field carries every edit BuildPlan produced regardless of
// what the operator unticked (plan.Model never mutates m.plan itself — only the rendered diff
// and m.ticked track the selection), so a real promotion would commit every repo in the plan,
// including ones the confirm screen's own diff never showed as changing (Codex review, PR
// #50). Edits for a repo not in ticked are dropped entirely — never downgraded to a NoOp or
// moved into Untouched — mirroring RenderDiff's own treatment of the identical set, so the
// commit message/PR body engine.RenderCommitMessage/RenderPRBody render from p.Edits (both key
// off Edit.New.Repo, cmd/hoist's internal/engine/template.go) describe exactly what the
// operator saw and confirmed, nothing more.
//
// Warnings gets the same treatment, for the same reason: engine.RenderPRBody also renders
// p.Warnings verbatim, and without this a PR body could carry a warning about a repo the
// operator explicitly unticked — one that never appears in p.Edits and so never appears
// anywhere else in the PR — describing a repo not part of the promotion at all (Copilot, PR
// #50 round 4). plan.WarningRepo names the repo each warning is about (every Warning built by
// pkg/gitops or pkg/resolve carries Occurrences for exactly one repo); a warning naming no
// repo at all (WarningRepo returns "", which none of today's constructors produce, but nothing
// forbids a future one that isn't per-repo) is kept unconditionally rather than dropped, since
// there is no ticked/unticked repo to test it against.
func filterTicked(p gitops.Plan, ticked []string) gitops.Plan {
	keep := make(map[string]bool, len(ticked))
	for _, r := range ticked {
		keep[r] = true
	}
	edits := make([]gitops.Edit, 0, len(p.Edits))
	for _, e := range p.Edits {
		if keep[e.Ref.Repo] {
			edits = append(edits, e)
		}
	}
	p.Edits = edits
	warnings := make([]gitops.Warning, 0, len(p.Warnings))
	for _, w := range p.Warnings {
		if repo := plan.WarningRepo(w); repo == "" || keep[repo] {
			warnings = append(warnings, w)
		}
	}
	p.Warnings = warnings
	return p
}

// each applies f to every screen, copying the stack.
func (m Model) each(f func(Screen) Screen) Model {
	stack := make([]Screen, len(m.stack))
	for i, s := range m.stack {
		stack[i] = f(s)
	}
	m.stack = stack
	return m
}
