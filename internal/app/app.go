package app

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/plan"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
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
type StartPromotionFunc func(ctx context.Context, p gitops.Plan) (engine.PromotionState, flight.DriveFunc, error)

// Promotion groups everything New needs to actually drive a confirmed plan and act on the
// flight screen's own requests, beyond what ResolveFunc already covers — the wiring PR #39
// left as a stub (see plan.StartMsg's and flight.OpenPRMsg's cases below). Start is nil in a
// context with nothing to drive (mirrors ResolveFunc's own nil convention): the plan screen's
// Enter key then shows a notice instead of pushing a read-only flight screen. OpenURL is nil
// the same way: flight.OpenPRMsg then falls back to the pre-wiring "not wired yet" notice
// rather than panicking on a nil call.
type Promotion struct {
	Start   StartPromotionFunc
	Poll    flight.PollDurations
	OpenURL func(url string) error
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

// Model is the root tea.Model: a stack of screens, the window size, and the theme, plus
// what a screen needs to open the plan screen (internal/app/plan) without app.New having
// to be called again — the repo, the promotable prefixes, the envs config (pairs,
// production) and the digest-resolution adaptor (nil in "digest sources: none" mode).
type Model struct {
	stack         []Screen
	styles        ui.Styles
	width, height int

	repo       *gitops.Repo
	promotable []string
	envs       config.EnvsConfig
	resolveFn  plan.ResolveFunc

	// startPromotion, poll and openURL are Promotion's three fields, unpacked here — see
	// Promotion's own doc comment for what each one is and why a nil Start/OpenURL degrades
	// to a notice rather than a panic.
	startPromotion StartPromotionFunc
	poll           flight.PollDurations
	openURL        func(url string) error

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
}

// New returns the root model with the matrix screen on the stack. promotable lists the
// image repo prefixes that count as first-party (the same list hoist plan --promotable
// takes). envs is the selected repo's envs config (production, pairs), zero-valued when
// there is none. resolveFn is what the plan screen calls to resolve digests; nil runs it in
// "digest sources: none" mode throughout. promo is what confirming a plan and driving the
// flight screen need — see Promotion's own doc comment. The theme starts dark and is
// replaced when the terminal reports its background.
func New(repo *gitops.Repo, promotable []string, envs config.EnvsConfig, resolveFn plan.ResolveFunc, promo Promotion) Model {
	m := Model{
		styles:         ui.NewStyles(true),
		repo:           repo,
		promotable:     promotable,
		envs:           envs,
		resolveFn:      resolveFn,
		startPromotion: promo.Start,
		poll:           promo.Poll,
		openURL:        promo.OpenURL,
	}
	return m.push(matrixScreen{matrix.New(repo, promotable)})
}

// Init asks the terminal for its background colour so the palette can follow it, and starts
// whatever the top (only, at boot) screen's own Init needs.
func (m Model) Init() tea.Cmd {
	var screenCmd tea.Cmd
	if len(m.stack) > 0 {
		screenCmd = m.stack[len(m.stack)-1].Init()
	}
	return tea.Batch(tea.RequestBackgroundColor, screenCmd)
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
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	case matrix.OpenPlanMsg:
		target := ""
		if m.envs.Pairs != nil {
			target = m.envs.Pairs[msg.Source]
		}
		ps := planScreen{plan.New(m.repo, m.promotable, m.envs, msg.Source, target, msg.Force, m.resolveFn)}
		m = m.push(ps)
		return m, ps.Init()
	case plan.BackMsg:
		// Abandon any startPromotion request this plan screen has outstanding — see
		// Model.buildGen's own doc comment. Bumping unconditionally (whether or not a request
		// is actually in flight) costs nothing: it only ever prevents an already-resolved or
		// never-issued gen from matching again, never a live one.
		m.buildGen++
		return m.pop(), nil
	case plan.StartMsg:
		if msg.Mode == plan.ModeDirect {
			// Direct mode's real step-selection machinery (an engine.Step set that commits
			// straight to the target env's base branch, no PR — M6/PR #43, not merged into
			// this branch) does not exist anywhere in this codebase yet: buildStartPromotion
			// (cmd/hoist/wiring.go) always builds engine.AllSteps regardless of what the
			// operator chose, and StartPromotionFunc's own signature has no Mode parameter to
			// carry the choice through even if it did. Silently driving PR mode here would
			// mean the confirm screen told the operator "commit straight to the default
			// branch, no PR" and then opened one anyway — a lie the operator has no way to
			// notice until the PR shows up. Until M6 lands, refuse honestly instead: a clear
			// notice, no call to startPromotion at all, so the plan screen (still on top)
			// lets the operator switch back to PR mode (m) and confirm that instead.
			m.notice = fmt.Sprintf("direct mode isn't wired up in the TUI yet (M6 not merged): confirming would open a PR for %s -> %s instead, not commit straight to the branch — press m to switch back to PR mode", msg.Source, msg.Target)
			return m, nil
		}
		if m.startPromotion == nil {
			// Mirrors ResolveFunc's own nil convention: a caller that hasn't wired
			// cmd/hoist's adaptor in gets a clear notice instead of a nil-pointer panic,
			// and the plan screen stays on top so the operator can see it.
			m.notice = "starting a promotion is not wired up"
			return m, nil
		}
		start, p, deadline := m.startPromotion, filterTicked(msg.Plan, msg.Ticked), m.poll.Deadline
		m.buildGen++
		gen := m.buildGen
		// deadlineAt is the one absolute instant this whole promotion's budget names, stamped
		// here — before the build even starts — so the flight screen constructed below (once
		// this build succeeds) can share it rather than starting a fresh poll.Deadline-length
		// window of its own once the build finishes (promotionBuiltMsg's own doc comment).
		var deadlineAt time.Time
		if deadline > 0 {
			deadlineAt = time.Now().Add(deadline)
		}
		return m, func() tea.Msg {
			// buildPromotionForConfirm (cmd/hoist/promote.go) can talk to a real git
			// remote and forge — the claim-then-rescan one-in-flight check re-observes
			// any conflicting promotion for this target env — so this runs off the
			// Update call stack (AGENTS.md §4.3), exactly like plan.ResolveFunc's own
			// loadCmd.
			// Bounded by deadlineAt, not left on context.Background() — the same reasoning
			// as flight.Model.driveCmd's own bound: a single hung network call must not
			// stall the plan screen forever with no way to cancel.
			ctx := context.Background()
			if !deadlineAt.IsZero() {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadlineAt)
				defer cancel()
			}
			state, driveFn, err := start(ctx, p)
			return promotionBuiltMsg{gen: gen, state: state, driveFn: driveFn, err: err, deadlineAt: deadlineAt}
		}
	case promotionBuiltMsg:
		if msg.gen != m.buildGen {
			// Stale: superseded by a later StartMsg (a second confirmation before this
			// one's result arrived), or the plan screen that issued it has since been
			// popped away (plan.BackMsg above). Dropped outright, before either the
			// error or the success branch below ever runs — see Model.buildGen's own
			// doc comment.
			return m, nil
		}
		if msg.err != nil {
			// A real in-flight conflict, missing github config, or a claim failure —
			// shown as a notice on whatever screen is still on top (matrix or plan,
			// whichever popped up plan.StartMsg) rather than crashing or silently
			// pushing a broken flight screen.
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
			m.notice = "promotion built with no error but no way to drive it (internal bug) — refusing to open a read-only flight screen"
			return m, nil
		}
		// pollForFlight shares msg.deadlineAt's own absolute instant rather than handing
		// flight.New the raw m.poll.Deadline it would otherwise recompute a fresh window
		// from (time.Now() at THIS point, after the build already spent some of the
		// budget) — Deadline becomes "however much of that instant is left", which is
		// what actually keeps build+drive under one shared deadline, the same guarantee
		// the CLI path gets for free from a single ctx.WithTimeout wrapping both.
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
	case flight.BackMsg:
		return m.pop(), nil
	case flight.OpenPRMsg:
		if m.openURL == nil {
			// Mirrors startPromotion's own nil convention above: a caller that hasn't
			// wired a browser opener in gets a clear notice instead of a nil-pointer
			// panic (documented follow-up work, per PR #39's own report).
			m.notice = fmt.Sprintf("open PR not wired yet: %s", msg.URL)
			return m, nil
		}
		if err := m.openURL(msg.URL); err != nil {
			m.notice = fmt.Sprintf("could not open %s: %v", msg.URL, err)
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
		// msg.ID is always a real id, but this handler does not even need it. A
		// driveCmd already in flight for the popped screen may still deliver one more
		// (harmless, unmatched) message to whatever screen is now on top once it
		// completes or its own poll.Deadline elapses.
		if len(m.stack) > 1 {
			m.stack = append([]Screen(nil), m.stack[:1]...)
		}
		return m, nil
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
// notice (see Model.notice) appended below it when one is set.
func (m Model) View() tea.View {
	content := ""
	if n := len(m.stack); n > 0 {
		content = m.stack[n-1].View()
	}
	if m.notice != "" {
		// Every notice set above (a start failure whose error can embed a git/forge
		// transport message, an open-URL failure) passes through redact.Strings here,
		// once, at the render boundary — the same convention plan.Model.View and
		// flight.Model.View already use, rather than wrapping each setter individually.
		content += "\n" + m.styles.Notice.Render(redact.Strings(m.notice))
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
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
