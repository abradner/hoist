package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	appconfig "github.com/abradner/hoist/internal/app/config"
	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/session"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/internal/ui"
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
	// Zero when no cluster is configured; R then says so rather than opening a screen that
	// cannot do anything.
	restartFn apprestart.Funcs
	// watchFn builds the watch screen's read function for one family in one env (WithWatch;
	// cmd/hoist's buildWatchFunc). nil means no cluster is configured, and w says so.
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

	// notice is a transient, root-level message shown below the top screen — used for
	// a real in-flight conflict, missing config, and for flight.OpenPRMsg when no real handler
	// is wired in (nil svc/OpenURL). Cleared on the next keypress, mirroring
	// every screen's own per-keypress notice convention (matrix.Model, plan.Model, flight.Model
	// all clear theirs the same way).
	notice string

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
		sess:       session.New(svc, session.Config{Deadline: promo.Poll.Deadline, ListEvery: promo.Poll.Approval}),
		poll:       promo.Poll,
		openURL:    promo.OpenURL,
		openPRMode: promo.OpenPRMode,
		tagsFn:     tagsFn,
		restartFn:  restartFn,
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
		m.notice = startErrorNotice(err)
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

// Update handles window size, theme and the global keys, and forwards everything else to
// the top screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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
		m.notice = ""
		switch msg.String() {
		case "ctrl+c":
			// Immediate, no confirm, regardless of what is running: promotion state is durable
			// (AGENTS.md §4.1) and `hoist resume` recovers whatever this interrupts (Train 2
			// design's own operator decision).
			return m, tea.Quit
		case "q":
			// Round 5, finding 3: this used to quit unconditionally, before the top screen's
			// own key handling ever saw the press — so typing "q" into the tag picker's filter
			// (or a huh field's own "/" filter, plan.Model.CapturesText) quit the whole program
			// instead of typing. Falls through to the normal forward-to-screen code below
			// whenever the top screen reports it's mid-text-entry; ctrl+c above is unaffected
			// and always quits.
			if !m.capturesText() {
				if m.sess.AnyRunning() {
					return m.openQuitConfirm()
				}
				return m, tea.Quit
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
		m = m.withMatrix(func(ms matrix.Model) matrix.Model { ms, _ = ms.Update(msg); return ms })
		if r := m.matrixRepo(); r != nil {
			m.repo = r
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
		sess, build, cmd, err := m.sess.Resume(msg.ID)
		m.sess = sess
		if err != nil {
			m.notice = "resuming a promotion is not wired up"
			return m, nil
		}
		m = m.popIfBuilding()
		snap, _ := m.sess.BuildSnapshot(build)
		fs := flightScreen{flight.NewAttached(snap, m.poll)}
		m = m.push(fs)
		return m, tea.Batch(fs.Init(), cmd)
	case matrix.OpenPlanMsg:
		target := ""
		if m.envs.Pairs != nil {
			target = m.envs.Pairs[msg.Source]
		}
		ps := planScreen{plan.New(m.repo, m.promotable, m.envs, msg.Source, target, msg.Force, m.planFn, m.history)}
		m = m.push(ps)
		return m, ps.Init()
	case deploy.BackMsg:
		// The deploy screen's Esc, handled exactly like the plan screen's below: without a case
		// here the message was forwarded to the top screen — the deploy screen itself — which
		// fed it to its own viewport, so Esc did nothing and the screen could not be left
		// (Copilot, PR #72). popAndRelist (Train 2 design PR 4): the operator backing out of a
		// deploy confirm may have just watched one land on the flight screen before backing out
		// further, so the pane re-lists at once rather than waiting for the next tick, exactly
		// like plan.BackMsg and flight.BackMsg below.
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
			m.notice = fmt.Sprintf("override ignored for %s: %v", msg.ID, err)
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
			m.notice = fmt.Sprintf("cannot re-observe %s: %v", msg.ID, err)
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
			m.notice = msg.URL
			return m, nil
		}
		if m.openURL == nil {
			// Mirrors startPromotion's own nil convention above: a caller that hasn't
			// wired a browser opener in gets a clear notice instead of a nil-pointer
			// panic (documented follow-up work, per PR #39's own report).
			m.notice = fmt.Sprintf("open PR not wired yet: %s", msg.URL)
			return m, nil
		}
		// The launcher can block for up to browser_launch_timeout; it runs as a command and
		// reports back through openURLResultMsg, so Update never waits on it (#56).
		open, url := m.openURL, msg.URL
		return m, func() tea.Msg { return openURLResultMsg{url: url, err: open(url)} }
	case openURLResultMsg:
		switch {
		case m.openPRMode == "both" && msg.err != nil:
			m.notice = fmt.Sprintf("%s (could not open automatically: %v)", msg.url, msg.err)
		case m.openPRMode == "both":
			m.notice = msg.url
		case msg.err != nil:
			m.notice = fmt.Sprintf("could not open %s: %v", msg.url, msg.err)
		}
		return m, nil
	case flight.AbandonMsg:
		// The flight screen's own X gesture already confirmed the operator wants this
		// (flight.AbandonMsg's own doc comment) — pop back to the matrix immediately and let
		// session.Controller.Abandon do the real work: cancel, wait for a busy Step to actually
		// stop, then the real Backend.Abandon call.
		if len(m.stack) > 1 {
			m.stack = append([]Screen(nil), m.stack[:1]...)
		}
		sess, cmd := m.sess.Abandon(msg.ID)
		m.sess = sess
		return m, cmd
	case matrix.OpenConfigMsg:
		if m.configText == "" {
			m.notice = "no config to show: the launcher supplied none"
			return m, nil
		}
		cs := configScreen{appconfig.New(m.configPath, m.configFound, m.configText)}
		m = m.push(cs)
		return m, cs.Init()
	case appconfig.BackMsg:
		return m.pop(), nil
	case matrix.OpenRestartMsg:
		return m.openRestart(msg.Family, msg.Target)
	case apprestart.BackMsg:
		return m.pop(), nil
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
		return m.openDeploy(msg.ImageRepo, msg.Tag, msg.Digest, msg.Target, false, deployHistory(msg.Delta, msg.Declared, msg.DeclaredSince, msg.HistoryNote))
	case tags.DirectRequestedMsg:
		// DirectRequestedMsg is only emitted after the picker's own keypress + huh.Confirm
		// gesture (tags.DirectRequestedMsg's doc comment), so the confirm screen opens already
		// in direct mode rather than making the operator repeat the gesture. It still shows
		// the diff first: the gesture chose a mode, not a change.
		return m.openDeploy(msg.ImageRepo, msg.Tag, msg.Digest, msg.Target, true, deployHistory(msg.Delta, msg.Declared, msg.DeclaredSince, msg.HistoryNote))
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
		case session.ChangeLanded, session.ChangeDone:
			// A landed or finished drive can have moved exactly what the matrix's own drift
			// column and repo read describe (a merge, a direct push) — refresh both the way F5
			// does (matrix.Model.RequestRefresh, its own repoGen guard and refreshAgain
			// coalescing unchanged) so the operator sees the new tag without pressing F5
			// themselves, and relist right away so the pane doesn't wait for the next tick to
			// drop this entry (Done) or show it landed (Landed).
			m = m.mirrorAttached(ch.Build, ch.Snap)
			needsRefresh = true
			needsRelist = true
		case session.ChangeBlocked, session.ChangeFailed:
			// Relist only: a blocked or failed drive hasn't landed anything new for drift or the
			// repo to reflect, but the pane's own phase has moved and should not wait either.
			m = m.mirrorAttached(ch.Build, ch.Snap)
			needsRelist = true
		case session.ChangeRefused:
			// A whole-listing failure (the forge/state directory unreachable) — left for a
			// later train PR to surface; the pane simply keeps its last good listing.
		case session.ChangeAbandoned:
			m.notice = "abandoned " + ch.ID
			m = m.mirrorAttached(ch.Build, ch.Snap)
			needsRelist = true
		case session.ChangeAbandonFailed:
			m.notice = fmt.Sprintf("abandon %s failed: %v", ch.ID, ch.Err)
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
	m.notice = fmt.Sprintf("could not start promotion: %v", err)
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
	return sum
}

// View renders the top screen in the alternate screen buffer, with the root's own transient
// notice (see Model.notice) on the terminal's last rows when one is set.
//
// The notice's rows are taken OUT of the screen above rather than appended after it. Every
// screen draws through ui.Frame.Render, which emits exactly `height` lines, so a notice
// merely appended to that landed on row height+1 and the alternate screen buffer never
// showed it: pressing enter on the deploy confirm screen and having the promotion refused —
// an in-flight conflict, a missing repos[].github, a claim conflict — was indistinguishable
// from a dead key, and the only way to read the reason was to re-run the equivalent command
// on the CLI (#164). Re-sizing the top screen here rather than on the Update that set the
// notice keeps this a pure render concern: nothing in the stack is mutated, and the screen
// returns to full height on the next keypress, which clears the notice.
func (m Model) View() tea.View {
	// Every notice set above (a start failure whose error can embed a git/forge transport
	// message, an open-URL failure) passes through redact.Strings here, once, at the render
	// boundary — the same convention plan.Model.View and flight.Model.View already use,
	// rather than wrapping each setter individually.
	notice := ui.NoticeLines(m.styles, redact.Strings(m.notice), m.width)
	content := ""
	if n := len(m.stack); n > 0 {
		top := m.stack[n-1]
		// Never shrink the screen to nothing: a terminal too short to hold both keeps the
		// screen at full height and the notice is the thing that goes missing, which is no
		// worse than today and leaves the screen legible.
		if h := m.height - len(notice); len(notice) > 0 && h > 0 {
			top = top.SetSize(m.width, h)
		}
		content = top.View()
	}
	if len(notice) > 0 {
		content += "\n" + strings.Join(notice, "\n")
	}
	if m.quitConfirming && m.quitConfirm != nil {
		content = ui.Dialog(m.styles, content, "quit hoist?", m.quitConfirm.View(), m.width, m.height)
	}
	v := tea.NewView(content)
	v.AltScreen = true
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
		return m.withMatrixNotice("restarting needs a cluster connection, and none is configured"), nil
	}
	names, err := restart.Targets(m.repo, target, []string{family})
	if err != nil {
		return m.withMatrixNotice(fmt.Sprintf("cannot restart %s in %s: %v", family, target, err)), nil
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
		return m.withMatrixNotice("watching needs a cluster connection, and none is configured"), nil
	}
	funcs, err := m.watchFn(family, target)
	if err != nil {
		return m.withMatrixNotice(fmt.Sprintf("cannot watch %s in %s: %v", family, target, err)), nil
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
func (m Model) openDeploy(imageRepo, tag, digest, target string, direct bool, h deploy.History) (tea.Model, tea.Cmd) {
	ref := image.Ref{Repo: imageRepo, Tag: tag, Digest: digest}
	pc, err := m.planFn(context.Background(), service.PlanRequest{Repo: m.repo, Target: target, Deploy: &ref})
	if err != nil {
		return m.pop().withMatrixNotice(fmt.Sprintf("cannot deploy %s to %s: %v", ref, target, err)), nil
	}
	pl := pc.Plan
	ds := deployScreen{deploy.New(pl, m.repo.Root, ref.String(), m.envs, m.styles).WithHistory(h).WithView(pc.View)}
	if direct {
		ds = deployScreen{ds.WithDirectMode()}
	}
	m = m.pop() // the picker has done its job
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
	m.stack = append([]Screen(nil), m.stack[:len(m.stack)-1]...)
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
	if len(m.stack) > 1 {
		m.stack = append([]Screen(nil), m.stack[0])
	}
	var cmd tea.Cmd
	m.sess, cmd = m.sess.Relist()
	return m, cmd
}

// withMatrix applies f to the matrix screen wherever it sits in the stack (see
// withMatrixNotice for why the whole stack is searched).
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

// withMatrixNotice sets notice on the matrix screen, wherever it actually sits in the stack —
// today always the bottom, and (after tags.SelectedMsg/DirectRequestedMsg's own pop above)
// always the new top too, since matrix.OpenTagsMsg is the only thing that ever pushes a tags
// screen, always directly onto the matrix. A no-op if the matrix isn't on the stack at all.
func (m Model) withMatrixNotice(notice string) Model {
	for i, s := range m.stack {
		ms, ok := s.(matrixScreen)
		if !ok {
			continue
		}
		stack := append([]Screen(nil), m.stack...)
		stack[i] = matrixScreen{ms.WithNotice(notice)}
		m.stack = stack
		return m
	}
	return m
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
