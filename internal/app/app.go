package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	appconfig "github.com/abradner/hoist/internal/app/config"
	"github.com/abradner/hoist/internal/app/deploy"
	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	apprestart "github.com/abradner/hoist/internal/app/restart"
	"github.com/abradner/hoist/internal/app/tags"
	"github.com/abradner/hoist/internal/app/watch"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
)

// StartPromotionFunc builds a real engine.PromotionState and flight.DriveFunc for a plan the
// operator just confirmed (plan.StartMsg) — the id/branch/worktree derivation, the
// claim-then-rescan one-in-flight check, and the prior-state merge-in that
// cmd/hoist/promote.go's buildPromotionForConfirm already does for the CLI path (AGENTS.md
// §4.8's "cmd/hoist owns the adapter" rule: this package only ever sees the plain function
// type, never pkg/git, pkg/forge or internal/config themselves). It is called from inside a
// tea.Cmd (see the plan.StartMsg case below), never directly from Update, since it can talk to
// a real git remote and forge (AGENTS.md §4.3) — exactly like plan.ResolveFunc.
//
// A non-nil error means the plan cannot start right now (a real in-flight conflict, missing
// github config, a claim failure, or every ticked edit already being a no-op — see
// cmd/hoist/wiring.go's own anyRealEdit guard) and is shown as a notice on whichever screen
// popped up plan.StartMsg, rather than pushing the flight screen at all. p is expected to
// already be filtered to the operator's ticked selection (see filterTicked below) — this type
// itself carries no notion of "ticked", only whatever Plan the caller hands it.
//
// progress, when non-nil, is called with one short line per preflight stage as the
// implementation reaches it (claiming the target env, checking for a conflicting promotion,
// fetching and comparing the checkout against origin, direct mode's fresh-base check, saving
// the initial state) — never blocking, never required: a nil progress is exactly as valid as
// a nil onWaiting already is throughout internal/engine, and this package's own caller (the
// flight screen pushed by plan.StartMsg/deploy.StartMsg, via flight.NewBuilding) is what
// turns these lines into something the operator sees while the preflight work — a real git
// fetch, a real forge round trip — runs off the Update call stack. cmd/hoist's own
// implementation reuses the same callback for engine.Drive's onWaiting and per-step save
// hooks once driving starts, so preflight and the drive that follows it read as one
// continuous log, not two.
type StartPromotionFunc func(ctx context.Context, p gitops.Plan, opts StartOpts, progress func(string)) (engine.PromotionState, flight.DriveFunc, error)

// StartOpts is how a screen says which shape of promotion it confirmed. It is a struct rather
// than a bool so that adding a future mode does not change every call site's meaning silently.
//
// Direct selects engine.AllDirectSteps over engine.AllSteps: commit straight to the base branch
// with no PR. Confirmed must be true only in direct response to the operator's own
// keypress-then-confirm gesture — engine.DirectCommitGateStep trusts it as the record of that
// gesture and refuses production regardless of it (internal/engine/direct.go), so a screen that
// sets it without one is not bypassing the gate, only lying to it.
type StartOpts struct {
	Direct    bool
	Confirmed bool
}

// Promotion groups everything New needs to actually drive a confirmed plan and act on the
// flight screen's own requests, beyond what ResolveFunc already covers — the wiring PR #39
// left as a stub (see plan.StartMsg's and flight.OpenPRMsg's cases below). Start is nil in a
// context with nothing to drive (mirrors ResolveFunc's own nil convention): the plan screen's
// Enter key then shows a notice instead of pushing a read-only flight screen. OpenURL is nil
// the same way: flight.OpenPRMsg then falls back to the pre-wiring "not wired yet" notice
// rather than panicking on a nil call. OpenPRMode is one of "launch", "display" or "both"
// (cmd/hoist owns reading config.PreferencesConfig.OpenPR and resolving it to this plain
// string, per AGENTS.md §4.8 — this package only ever compares against string literals, never
// importing internal/config's own constants for it, matching Poll's own already-translated-
// from-config shape); empty behaves like "launch", so a caller that never sets it (a test, in
// particular) gets today's original launch-only behavior rather than a silently different one.
type Promotion struct {
	Start      StartPromotionFunc
	Poll       flight.PollDurations
	OpenURL    func(url string) error
	OpenPRMode string
}

// promotionBuiltMsg is delivered once the tea.Cmd wrapping a StartPromotionFunc call finishes
// (see the plan.StartMsg case below) — an app.go-private message, never exported, since
// nothing outside the root ever needs to construct or match it. gen is stamped with the
// issuing Model's own m.buildGen at the moment the request was launched, so Update can drop a
// stale result the same way flight.Model.onDriveResult already drops a stale driveResultMsg —
// see Model.buildGen's own doc comment for what "stale" means at this layer.
type promotionBuiltMsg struct {
	gen     uint64
	state   engine.PromotionState
	driveFn flight.DriveFunc
	err     error
	// deadlineAt is the one absolute instant m.poll.Deadline named at plan.StartMsg time — zero
	// when there is no configured deadline at all. Carried through so the flight screen's own
	// budget (Model.deadlineAt, driveCmd's own bound) shares this SAME instant rather than
	// starting a fresh poll.Deadline-length window of its own once the build finishes: without
	// this, the time this build step itself took (a real git/forge round trip, AGENTS.md §4.3)
	// went uncounted against the operator's configured deadline, so the TUI's total wait could
	// exceed poll.deadline even though the CLI path wraps build+drive under one ctx timeout
	// (Copilot review).
	deadlineAt time.Time
}

// InFlight is how the root lists what is promoting right now for the matrix's pane, and
// re-drives one of them on the flight screen (M10: the TUI's `hoist promotions` and `hoist
// resume`). List re-observes every state file against the forge and the cluster — AGENTS.md
// §4.1, never the recorded phase — so it is called off the Update stack, at boot and then
// every Poll.Approval while the matrix is the top screen. Resume builds the same state and
// DriveFunc `hoist resume <id>` would. Both nil means the feature is not wired (a flags-only
// run, a test): the pane stays absent and r says so.
type InFlight struct {
	List   func(ctx context.Context) ([]flight.Summary, error)
	Resume func(ctx context.Context, id string) (engine.PromotionState, flight.DriveFunc, error)
}

// openURLResultMsg is the browser launcher's answer for one URL, delivered by the command
// flight.OpenPRMsg's handler issues (#56: the launch used to run inside Update).
type openURLResultMsg struct {
	url string
	err error
}

// inFlightMsg carries one listing back; gen drops a listing from before the matrix was
// replaced (it never is today, but the guard costs nothing) or one that raced a newer one.
type inFlightMsg struct {
	gen  uint64
	list []flight.Summary
	err  error
}

// inFlightTickMsg is the poll: re-list, but only when the matrix is on top.
type inFlightTickMsg struct{}

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
	resolveFn  plan.ResolveFunc
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
	// inFlight is InFlight's pair (WithInFlight); listGen stamps each listing.
	inFlight InFlight
	listGen  uint64

	// startPromotion, poll, openURL and openPRMode are Promotion's fields, unpacked here —
	// see Promotion's own doc comment for what each one is and why a nil Start/OpenURL
	// degrades to a notice rather than a panic.
	startPromotion StartPromotionFunc
	poll           flight.PollDurations
	openURL        func(url string) error
	openPRMode     string

	// notice is a transient, root-level message shown below the top screen — used for
	// plan.StartMsg's own construction failure (a real in-flight conflict, missing config) and
	// for flight.OpenPRMsg/AbortMsg when no real handler is wired in (nil Start/OpenURL).
	// Cleared on the next keypress, mirroring every screen's own per-keypress notice
	// convention (matrix.Model, plan.Model, flight.Model all clear theirs the same way).
	notice string

	// buildGen is the generation of the current (or most recently abandoned)
	// startPromotion request. The plan screen stays fully interactive while its StartMsg's
	// startPromotion call runs in the background (buildStartPromotion can take a real round
	// trip to git/the forge) — so before that call's promotionBuiltMsg ever arrives, the
	// operator can press Esc (abandoning it, plan.BackMsg below) or Enter again (a second,
	// overlapping StartMsg for the same or a different plan). Without this guard, whichever
	// promotionBuiltMsg happened to arrive later was adopted unconditionally regardless of
	// which request — or none at all — the operator still cared about: a plan already backed
	// out of could still get its flight screen pushed and start driving (creating a real
	// branch/PR) the moment its result landed, and two staggered confirmations could each push
	// their own flight screen for the same promotion, driving it from two independent
	// goroutines at once (Codex review, PR #50 round 4). Every StartMsg increments buildGen and
	// stamps the new value into the promotionBuiltMsg its own command will eventually produce;
	// popping the plan screen away before that arrives (plan.BackMsg) increments it again with
	// nothing to stamp, so any request still outstanding is orphaned. The promotionBuiltMsg
	// case checks msg.gen against the current value before acting on the result at all — the
	// same shape as flight.Model.gen/onDriveResult, one layer up the stack, guarding the
	// analogous build step instead of the drive step.
	buildGen uint64
	// buildCancel cancels whichever startPromotion call buildGen currently names, or nil when
	// none is outstanding. buildGen alone (above) only ever stops an abandoned/superseded
	// build's RESULT from being acted on once it eventually arrives — it does nothing to the
	// goroutine itself, which was bounded only by poll.Deadline (often hours) and so kept
	// running its real network work (the claim-then-rescan one-in-flight scan in particular)
	// to completion regardless of whether the operator had already moved on. Backing out
	// (plan.BackMsg) or superseding (a second plan.StartMsg before the first's result lands)
	// now calls this to interrupt that work promptly instead of leaving it to run unwatched
	// (Copilot review, PR #50). It cannot undo a claim/state-save that had already completed
	// before cancellation reached it — that narrow residual case leaves a real, resumable
	// state file behind, the same as any other process-killed-mid-flight promotion already
	// recovers from via `hoist resume`.
	buildCancel context.CancelFunc

	// configPath, configFound and configText are what the config screen shows (C, #104):
	// where the file was read from or looked for, whether it existed, and the effective
	// config already marshalled and redacted by cmd/hoist (WithConfigView) — this package
	// holds the strings and never re-marshals, so the screen can show nothing `hoist config
	// show` would not.
	configPath  string
	configFound bool
	configText  string
}

// New returns the root model with the matrix screen on the stack. promotable lists the
// image repo prefixes that count as first-party (the same list hoist plan --promotable
// takes). envs is the selected repo's envs config (production, pairs), zero-valued when
// there is none. resolveFn is what the plan screen calls to resolve digests; nil runs it in
// "digest sources: none" mode throughout. promo is what confirming a plan and driving the
// flight screen need — see Promotion's own doc comment. tagsFn is what the tag-picker screen
// calls to list and fetch registry/forge data for one image repo; nil opens the picker with no
// data source (it reports the resulting error itself, same as a resolveFn failure does for
// plan). The theme starts dark and is replaced when the terminal reports its background.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, resolveFn plan.ResolveFunc, promo Promotion, tagsFn tags.BuildFunc, restartFn apprestart.Funcs) Model {
	m := Model{
		styles:         ui.NewStyles(true),
		repo:           repo,
		promotable:     promotable,
		envs:           envs,
		resolveFn:      resolveFn,
		startPromotion: promo.Start,
		poll:           promo.Poll,
		openURL:        promo.OpenURL,
		openPRMode:     promo.OpenPRMode,
		tagsFn:         tagsFn,
		restartFn:      restartFn,
	}
	// The matrix starts without a cluster question; WithDrift supplies one. Deriving it from
	// resolveFn (as before #122) collapsed a partial rollout to the one digest a plan picks.
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

// WithInFlight supplies the in-flight listing and resume functions (cmd/hoist's
// buildInFlightFuncs). See InFlight.
func (m Model) WithInFlight(f InFlight) Model {
	m.inFlight = f
	return m
}

// listInFlight issues one listing for the current generation, or nil when List is not wired.
func (m Model) listInFlight() (Model, tea.Cmd) {
	if m.inFlight.List == nil {
		return m, nil
	}
	// Each listing gets a new generation, so a slow earlier one that lands after a faster
	// later one is dropped rather than painting an older snapshot over a newer pane.
	m.listGen++
	return m, m.listInFlightAt(m.listGen)
}

// listInFlightAt is the listing command for one generation; Init uses the current one, since
// its model copy is discarded and the root must still recognise the answer.
func (m Model) listInFlightAt(gen uint64) tea.Cmd {
	if m.inFlight.List == nil {
		return nil // the in-flight adaptor is optional (WithInFlight); nothing to list
	}
	list, deadline := m.inFlight.List, m.poll.Deadline
	return func() tea.Msg {
		ctx := context.Background()
		if deadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, deadline)
			defer cancel()
		}
		summaries, err := list(ctx)
		return inFlightMsg{gen: gen, list: summaries, err: err}
	}
}

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

// inFlightTick schedules the next listing at Poll.Approval — the cadence the engine itself
// re-observes an approval at — with a floor so a zero config never spins.
func (m Model) inFlightTick() tea.Cmd {
	if m.inFlight.List == nil {
		return nil
	}
	every := m.poll.Approval
	if every < 5*time.Second {
		every = 30 * time.Second
	}
	return tea.Tick(every, func(time.Time) tea.Msg { return inFlightTickMsg{} })
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

// Init asks the terminal for its background colour so the palette can follow it, and starts
// whatever the top (only, at boot) screen's own Init needs.
func (m Model) Init() tea.Cmd {
	var screenCmd tea.Cmd
	if len(m.stack) > 0 {
		screenCmd = m.stack[len(m.stack)-1].Init()
	}
	// Init's model copy is discarded, so the boot listing carries the current generation;
	// every later one (a tick, a pop) increments through listInFlight's returned model.
	return tea.Batch(tea.RequestBackgroundColor, screenCmd, m.listInFlightAt(m.listGen), m.inFlightTick())
}

// Update handles window size, theme and the global keys, and forwards everything else to
// the top screen.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m.each(func(s Screen) Screen { return s.SetSize(msg.Width, msg.Height) }), nil
	case tea.BackgroundColorMsg:
		m.styles = ui.NewStyles(msg.IsDark())
		return m.each(func(s Screen) Screen { return s.SetStyles(m.styles) }), nil
	case tea.KeyPressMsg:
		m.notice = ""
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "q":
			// Round 5, finding 3: this used to quit unconditionally, before the top screen's
			// own key handling ever saw the press — so typing "q" into the tag picker's filter
			// (or a huh field's own "/" filter, plan.Model.CapturesText) quit the whole program
			// instead of typing. Falls through to the normal forward-to-screen code below
			// whenever the top screen reports it's mid-text-entry; ctrl+c above is unaffected
			// and always quits.
			if !m.capturesText() {
				return m, tea.Quit
			}
		}
	case matrix.DriftMsg:
		// The matrix's own async answer, routed to it wherever it sits: the default below
		// forwards to the top screen only, and an env whose answer landed while the plan or
		// picker was open stayed "resolving…" for good (Copilot, #110).
		return m.withMatrix(func(ms matrix.Model) matrix.Model { ms, _ = ms.Update(msg); return ms }), nil
	case inFlightMsg:
		if msg.gen != m.listGen {
			return m, nil
		}
		return m.withMatrix(func(ms matrix.Model) matrix.Model { return ms.SetInFlight(msg.list, msg.err) }), nil
	case inFlightTickMsg:
		if !m.matrixOnTop() {
			return m, m.inFlightTick()
		}
		m, list := m.listInFlight()
		return m, tea.Batch(list, m.inFlightTick())
	case matrix.ResumeMsg:
		if m.inFlight.Resume == nil {
			m.notice = "resuming a promotion is not wired up"
			return m, nil
		}
		resume, deadline := m.inFlight.Resume, m.poll.Deadline
		m.buildGen++
		gen := m.buildGen
		if m.buildCancel != nil {
			m.buildCancel()
		}
		var deadlineAt time.Time
		if deadline > 0 {
			deadlineAt = time.Now().Add(deadline)
		}
		ctx := context.Background()
		var cancelDeadline context.CancelFunc
		if !deadlineAt.IsZero() {
			ctx, cancelDeadline = context.WithDeadline(ctx, deadlineAt)
		}
		ctx, cancel := context.WithCancel(ctx)
		m.buildCancel = cancel
		id := msg.ID
		return m, func() tea.Msg {
			defer cancel()
			if cancelDeadline != nil {
				defer cancelDeadline()
			}
			// The same promotionBuiltMsg path a confirmed plan takes: the flight screen is
			// pushed with the resumed state and drives it from wherever Observe finds it.
			state, driveFn, err := resume(ctx, id)
			return promotionBuiltMsg{gen: gen, state: state, driveFn: driveFn, err: err, deadlineAt: deadlineAt}
		}
	case matrix.OpenPlanMsg:
		target := ""
		if m.envs.Pairs != nil {
			target = m.envs.Pairs[msg.Source]
		}
		ps := planScreen{plan.New(m.repo, m.promotable, m.envs, msg.Source, target, msg.Force, m.resolveFn, m.history)}
		m = m.push(ps)
		return m, ps.Init()
	case deploy.BackMsg:
		// The deploy screen's Esc, handled exactly like the plan screen's below: without a case
		// here the message was forwarded to the top screen — the deploy screen itself — which
		// fed it to its own viewport, so Esc did nothing and the screen could not be left
		// (Copilot, PR #72). The same build-generation invalidation applies: a deploy confirm
		// can have a startPromotion request outstanding just as a plan confirm can.
		m.buildGen++
		if m.buildCancel != nil {
			m.buildCancel()
			m.buildCancel = nil
		}
		return m.pop(), nil
	case plan.BackMsg:
		// Abandon any startPromotion request this plan screen has outstanding — see
		// Model.buildGen's own doc comment. Bumping unconditionally (whether or not a request
		// is actually in flight) costs nothing: it only ever prevents an already-resolved or
		// never-issued gen from matching again, never a live one. buildCancel actually
		// interrupts it, rather than merely disowning its eventual result — see its own doc
		// comment.
		m.buildGen++
		if m.buildCancel != nil {
			m.buildCancel()
			m.buildCancel = nil
		}
		return m.popAndRelist()
	case plan.StartMsg:
		if m.startPromotion == nil {
			// Mirrors ResolveFunc's own nil convention: a caller that hasn't wired
			// cmd/hoist's adaptor in gets a clear notice instead of a nil-pointer panic,
			// and the plan screen stays on top so the operator can see it.
			m.notice = "starting a promotion is not wired up"
			return m, nil
		}
		start, p, deadline := m.startPromotion, filterTicked(msg.Plan, msg.Ticked), m.poll.Deadline
		direct := msg.Mode == plan.ModeDirect
		m.buildGen++
		gen := m.buildGen
		if m.buildCancel != nil {
			// Superseding a build still outstanding from an earlier StartMsg — see
			// Model.buildCancel's own doc comment: buildGen alone only disowns its eventual
			// result, this actually interrupts the work.
			m.buildCancel()
		}
		// deadlineAt is the one absolute instant this whole promotion's budget names, stamped
		// here — before the build even starts — so the flight screen constructed below (once
		// this build succeeds) can share it rather than starting a fresh poll.Deadline-length
		// window of its own once the build finishes (promotionBuiltMsg's own doc comment).
		var deadlineAt time.Time
		if deadline > 0 {
			deadlineAt = time.Now().Add(deadline)
		}
		// Bounded by deadlineAt, not left on context.Background() — the same reasoning as
		// flight.Model.driveCmd's own bound: a single hung network call must not stall the
		// plan screen forever with no way to cancel. Wrapped again with WithCancel so
		// plan.BackMsg/a superseding StartMsg (above) can also interrupt it promptly, rather
		// than leaving it to run unwatched for however long is left of poll.Deadline (Copilot
		// review, PR #50) — cancel is stored on the model, not just deferred below, precisely
		// so Update can reach it from a later message.
		ctx := context.Background()
		var cancelDeadline context.CancelFunc
		if !deadlineAt.IsZero() {
			ctx, cancelDeadline = context.WithDeadline(ctx, deadlineAt)
		}
		ctx, cancel := context.WithCancel(ctx)
		m.buildCancel = cancel
		// Pushed on the keypress, not once the build finishes: without this, nothing on
		// screen changed for however long the preflight work (a real git fetch, the
		// claim-then-rescan in-flight check, a state save) took, the plan screen stayed on
		// top and kept receiving keys, and a second enter — the natural response to a key
		// that looks dead — cancelled the first attempt and started it over (defect A). The
		// building screen renders with what's already known (source/target/direct) and a
		// spinner; AdoptBuilt below turns this SAME instance into a normal driving one once
		// the build actually returns, so there is no flicker and no second screen.
		//
		// popIfBuilding first: a superseding StartMsg (this same case, reached again before
		// the previous one resolved) must not stack a second building screen on top of the
		// first — the buildCancel() call above already orphans that earlier attempt's
		// eventual result (buildGen no longer matches it), but without also removing its
		// screen, the abandoned one stays buried in the stack forever: invisible, its own
		// progressCh already closed by its own cancelled goroutine, surfacing only if the
		// operator ever pops back far enough to reach it.
		m = m.popIfBuilding()
		progressCh := make(chan string, 32)
		m = m.push(flightScreen{flight.NewBuilding(p.SourceEnv, p.TargetEnv, direct, m.poll, progressCh)})
		fs := m.stack[len(m.stack)-1]
		buildCmd := func() tea.Msg {
			// buildPromotionForConfirm (cmd/hoist/promote.go) can talk to a real git
			// remote and forge — the claim-then-rescan one-in-flight check re-observes
			// any conflicting promotion for this target env — so this runs off the
			// Update call stack (AGENTS.md §4.3), exactly like plan.ResolveFunc's own
			// loadCmd.
			defer cancel()
			if cancelDeadline != nil {
				defer cancelDeadline()
			}
			// progressCh is deliberately never closed here. The same channel — captured by
			// this same progress closure — is reused for the whole drive that follows a
			// successful build (AdoptBuilt keeps listening; driveFuncFor's wrapped save and
			// onWaiting report through the identical callback), so this build call finishing
			// is not this channel's end of life; closing it here would panic the very next
			// send from engine.Drive's own history hook once driving actually starts — sending
			// on a closed channel panics unconditionally in Go, select/default only guards a
			// full buffer, never a closed one. An unclosed, undrained channel is harmless
			// (every send already goes through the same select/default below, so a channel
			// nobody is reading from just silently drops); the one cost is that listenCmd's
			// goroutine, if this screen is ever abandoned (backed out of, or the promotion
			// finishes) with no one left to close or read it, blocks forever rather than
			// exiting — one leaked goroutine per screen's whole lifetime, not per line, and
			// not per poll tick; accepted for now rather than adding a second signal whose own
			// lifecycle would have to be gotten right just as carefully as this one.
			// The plan screen's mode toggle (m) is gated on IsProduction and sits behind its own
			// huh.Confirm, so reaching ModeDirect here IS the keypress-then-confirm gesture
			// engine.DirectCommitGateStep asks Confirmed to attest — which the gate then
			// re-checks against envs.production independently anyway.
			state, driveFn, err := start(ctx, p, StartOpts{Direct: direct, Confirmed: direct}, func(line string) {
				// Never blocks the build goroutine on a slow-draining UI: the channel is
				// generously buffered for the handful of preflight lines this ever carries,
				// and a genuinely full buffer means dropping a line, not stalling a real
				// git/forge call on rendering.
				select {
				case progressCh <- line:
				default:
				}
			})
			return promotionBuiltMsg{gen: gen, state: state, driveFn: driveFn, err: err, deadlineAt: deadlineAt}
		}
		return m, tea.Batch(fs.Init(), buildCmd)
	case promotionBuiltMsg:
		if msg.gen != m.buildGen {
			// Stale: superseded by a later StartMsg (a second confirmation before this
			// one's result arrived), or the plan screen that issued it has since been
			// popped away (plan.BackMsg above). Dropped outright, before either the
			// error or the success branch below ever runs — see Model.buildGen's own
			// doc comment. m.buildCancel is left untouched: it belongs to whichever
			// newer build actually superseded this one (or is already nil if this was
			// abandoned via BackMsg instead), never to this stale result.
			return m, nil
		}
		// This result belongs to the build m.buildCancel was guarding — it has finished
		// (successfully or not), so there is nothing left for that cancel to interrupt.
		m.buildCancel = nil
		if msg.err != nil {
			// A real in-flight conflict, missing github config, or a claim failure. popIfBuilding
			// removes the preflight flight screen plan.StartMsg/deploy.StartMsg pushed on the
			// keypress (matrix.ResumeMsg never pushes one, so this is a no-op for that path,
			// unchanged from before this PR) — reverting to whatever was underneath restores
			// exactly the pre-existing, #164-tested behavior: the notice on the now-visible
			// confirm screen, never crashing or silently leaving a dead building screen up.
			m = m.popIfBuilding()
			m.notice = fmt.Sprintf("could not start promotion: %v", msg.err)
			return m, nil
		}
		if msg.driveFn == nil {
			// StartPromotionFunc's own doc comment says a non-nil error is the only signal
			// that a plan cannot start; a nil error together with a nil driveFn is a
			// contract violation by whatever built this msg (a bug in cmd/hoist's own
			// adaptor), not a state this screen should silently paper over by pushing a
			// read-only flight screen — that would reintroduce exactly the pre-wiring stub
			// behavior this PR exists to remove, with no visible sign anything is wrong.
			m = m.popIfBuilding()
			m.notice = "promotion built with no error but no way to drive it (internal bug) — refusing to open a read-only flight screen"
			return m, nil
		}
		// The common case: plan.StartMsg/deploy.StartMsg already pushed a building flight
		// screen on the keypress (flight.NewBuilding), and its own poll/deadlineAt were fixed
		// at that moment — the same instant msg.deadlineAt names, since both were computed
		// from m.poll.Deadline within the same Update call. AdoptBuilt turns this SAME
		// instance into a real, driving screen without recomputing either, which is what
		// keeps build+drive sharing one budget (its own doc comment) — no "remaining time"
		// recompute needed here at all once the screen already exists.
		if top := len(m.stack) - 1; top >= 0 {
			if fs, ok := m.stack[top].(flightScreen); ok && fs.Building() {
				adopted, cmd := fs.AdoptBuilt(msg.state, msg.driveFn)
				stack := append([]Screen(nil), m.stack...)
				stack[top] = flightScreen{adopted}
				m.stack = stack
				return m, cmd
			}
		}
		// Fallback: no building screen was pre-pushed — matrix.ResumeMsg's own path
		// (re-observing an already-existing promotion, not starting a fresh one, so there is
		// no preflight phase to show). pollForFlight recomputes the remaining budget exactly
		// as this whole handler always has, since flight.New is only now constructing this
		// screen's own deadlineAt, unlike the adopt path above.
		pollForFlight := m.poll
		if !msg.deadlineAt.IsZero() {
			if remaining := time.Until(msg.deadlineAt); remaining > 0 {
				pollForFlight.Deadline = remaining
			} else {
				// The build itself already consumed the whole budget (or overran it) —
				// flight.New treats poll.Deadline <= 0 as "no deadline at all" (its own
				// doc comment), which would be exactly backwards here: budget exhausted
				// must fail fast, not grant a fresh unbounded window. A minimal positive
				// duration keeps flight.New's own deadlineAt in the past (or effectively
				// now), so the very first poll reports context.DeadlineExceeded instead.
				pollForFlight.Deadline = time.Nanosecond
			}
		}
		fs := flightScreen{flight.New(msg.state, pollForFlight, msg.driveFn)}
		m = m.push(fs)
		return m, fs.Init()
	case flight.OverrideCINoneMsg:
		// The TUI's `hoist resume <id> --override-ci-none` (#103): the flight screen showing
		// promotion msg.ID asked, behind its own huh.Confirm, to treat a PR with no reported
		// checks as green under ci.none: prompt. The root decides what that means — set the
		// override on THAT promotion's state and re-drive it — and the screen's own state is
		// where the override lives (flight.Model.ApplyCINoneOverride): every driveCmd hands
		// that state to the DriveFunc, so cmd/hoist's engine.Drive sees CINoneOverride on the
		// next CIGreenStep.Observe and its save persists it. Nothing here is a default: a
		// promotion whose screen never confirmed keeps false, and the confirm path
		// (buildStartPromotion) never sets it (AGENTS.md §4.5).
		top := len(m.stack) - 1
		var fs flightScreen
		ok := top >= 0
		if ok {
			fs, ok = m.stack[top].(flightScreen)
		}
		if !ok || fs.ID() != msg.ID {
			m.notice = "override ignored: promotion " + msg.ID + " is not the flight screen on top"
			return m, nil
		}
		next, cmd := fs.ApplyCINoneOverride()
		m.stack[top] = flightScreen{next}
		return m, cmd
	case flight.BackMsg:
		// Cancel the flight screen's shared drive context before popping it — see
		// flight.Model.Cancel's own doc comment. Without this, a driveCmd already in flight
		// for the popped screen kept running to completion (committing, pushing, opening a
		// PR, merging) even though nothing was watching it anymore.
		if top := len(m.stack) - 1; top >= 0 {
			if fs, ok := m.stack[top].(flightScreen); ok {
				fs.Cancel()
				if fs.Building() {
					// fs.Cancel() is a no-op here — a building screen's own m.cancel is nil
					// until AdoptBuilt (driveCmd has nothing to run yet). The build this
					// screen is watching is m.buildCancel's, not this screen's own, so
					// backing out has to reach that instead: bump buildGen and cancel it the
					// same way a superseding StartMsg already does (plan.StartMsg's own
					// comment on buildGen). Without this, the build goroutine keeps running
					// after the operator has already backed out, and its eventual
					// promotionBuiltMsg — still carrying the gen this popped screen was
					// built under — would resurrect a screen the operator explicitly left:
					// the success branch would push (or, with a second building screen
					// already up from a fresh attempt, wrongly adopt into) a flight screen
					// nobody asked to see again.
					m.buildGen++
					if m.buildCancel != nil {
						m.buildCancel()
						m.buildCancel = nil
					}
				}
			}
		}
		return m.popAndRelist()
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
	case flight.AbortMsg:
		// Real abort semantics at the engine level (close the PR? delete the branch?) are
		// deliberately out of scope here: no milestone has ever defined what "abort" means
		// for a promotion, and inventing one now risks a rushed, unreviewed design in an
		// area that has already been hardened hard for safety (invariant 5, the
		// claim-then-rescan dance) elsewhere. The one narrow, safe interpretation
		// implemented instead: stop watching this promotion from the TUI and return to
		// the matrix, leaving the real branch/PR/state file exactly as they are — the
		// operator drives it further via `hoist resume <id>` or the forge directly, and a
		// re-opened plan screen can always confirm the same digests again (the same
		// deterministic id, per AGENTS.md §4.1) to pick the flight screen back up. No
		// engine call happens here at all: flight.Model itself now refuses to emit
		// AbortMsg for an empty/read-only promotion (PR #39 review finding #2), so
		// msg.ID is always a real id, but this handler does not even need it.
		//
		// Cancel the flight screen's shared drive context before popping it — see
		// flight.Model.Cancel's own doc comment. A driveCmd already in flight for the popped
		// screen is NOT harmless left running: it can keep committing, pushing, opening a PR
		// or merging after the operator has walked away, and — since the claim was already
		// released once the initial state saved — a later reconfirmation of the same
		// deterministic promotion id could start a second driver racing the first (Copilot
		// review, PR #50 round 11; this corrects an earlier version of this comment that
		// called the same lingering call harmless).
		if top := len(m.stack) - 1; top >= 0 {
			if fs, ok := m.stack[top].(flightScreen); ok {
				fs.Cancel()
			}
		}
		if len(m.stack) > 1 {
			m.stack = append([]Screen(nil), m.stack[:1]...)
		}
		return m.listInFlight()
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
		production := plan.IsProduction(msg.Target, m.envs)
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
		if m.startPromotion == nil {
			m.notice = "starting a deploy is not wired up"
			return m, nil
		}
		start, deadline := m.startPromotion, m.poll.Deadline
		direct := msg.Mode == deploy.ModeDirect
		m.buildGen++
		gen := m.buildGen
		if m.buildCancel != nil {
			m.buildCancel()
		}
		var deadlineAt time.Time
		if deadline > 0 {
			deadlineAt = time.Now().Add(deadline)
		}
		ctx := context.Background()
		var cancelDeadline context.CancelFunc
		if !deadlineAt.IsZero() {
			ctx, cancelDeadline = context.WithDeadline(ctx, deadlineAt)
		}
		ctx, cancel := context.WithCancel(ctx)
		m.buildCancel = cancel
		p := msg.Plan
		// Same preflight-phase push as plan.StartMsg above — see its own comment for why,
		// and its popIfBuilding comment for why a superseding StartMsg must not stack a
		// second building screen on top of the first.
		m = m.popIfBuilding()
		progressCh := make(chan string, 32)
		m = m.push(flightScreen{flight.NewBuilding(p.SourceEnv, p.TargetEnv, direct, m.poll, progressCh)})
		fs := m.stack[len(m.stack)-1]
		buildCmd := func() tea.Msg {
			defer cancel()
			if cancelDeadline != nil {
				defer cancelDeadline()
			}
			// progressCh is deliberately never closed here — see plan.StartMsg's own comment
			// on this same pattern above for why: this channel is reused for the whole drive
			// that follows, and closing it here would panic the first send engine.Drive's own
			// history hook makes once driving starts.
			state, driveFn, err := start(ctx, p, StartOpts{Direct: direct, Confirmed: msg.Confirmed}, func(line string) {
				select {
				case progressCh <- line:
				default:
				}
			})
			return promotionBuiltMsg{gen: gen, state: state, driveFn: driveFn, err: err, deadlineAt: deadlineAt}
		}
		return m, tea.Batch(fs.Init(), buildCmd)
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
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

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
	rs := restartScreen{apprestart.New(target, family, names, plan.IsProduction(target, m.envs), m.restartFn, m.styles)}
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
func (m Model) openDeploy(imageRepo, tag, digest, target string, direct bool, h deploy.History) (tea.Model, tea.Cmd) {
	ref := image.Ref{Repo: imageRepo, Tag: tag, Digest: digest}
	pl, err := gitops.BuildDeployPlan(m.repo, target, ref, m.promotable)
	if err != nil {
		return m.pop().withMatrixNotice(fmt.Sprintf("cannot deploy %s to %s: %v", ref, target, err)), nil
	}
	// The identical warning cmd/hoist's own `deploy` attaches, from the same helper, so the
	// confirm screen and the PR body it later renders agree with the CLI's dry run.
	plan.WarnDeployIntoProduction(&pl, m.envs)
	ds := deployScreen{deploy.New(pl, m.repo.Root, ref.String(), m.envs, m.styles).WithHistory(h)}
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

// popIfBuilding pops the top screen only when it is a flight screen still in its preflight
// phase (flight.NewBuilding, flight.Model.Building) — promotionBuiltMsg's own error branches
// use it to undo the screen plan.StartMsg/deploy.StartMsg pushed on the keypress, reverting to
// whatever was underneath (the confirm screen, #164-tested to show the notice correctly)
// exactly as if that screen had never been pushed. A no-op for any other top screen, in
// particular matrix.ResumeMsg's own path, which never pushes a building screen at all.
func (m Model) popIfBuilding() Model {
	if top := len(m.stack) - 1; top >= 0 {
		if fs, ok := m.stack[top].(flightScreen); ok && fs.Building() {
			return m.pop()
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
		return m.listInFlight()
	}
	return m, nil
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

// withMatrixNotice sets notice on the matrix screen, wherever it actually sits in the stack —
// today always the bottom, and (after tags.SelectedMsg/DirectRequestedMsg's own pop above)
// always the new top too, since matrix.OpenTagsMsg is the only thing that ever pushes a tags
// screen, always directly onto the matrix. A no-op if the matrix isn't on the stack at all.
//
// Round-N finding (Copilot): an earlier revision of this function only ever checked
// m.stack[top], so the doc comment's "wherever it sits in the stack" was true only by the
// current stack shape's own invariant (matrix is always at the position this checked), not
// because the code actually searched for it — a claim the mechanism didn't itself deliver
// (AGENTS.md principle 1). Searching the whole stack, rather than the top slot alone, makes
// the claim true unconditionally: it costs one more loop over a stack that is never more than
// a few screens deep, and it means a future screen shape (a third screen pushed between the
// matrix and a tags screen, say) degrades to "notice still lands on the matrix" instead of
// "notice silently vanishes".
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
