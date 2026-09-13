package tags

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/registry"
)

// RegTagsFunc lists ImageRepo's registry tags alone — the fast half of what used to be one
// combined, blocking call (#PR8): the picker renders as soon as this answers, no longer
// blocked on GitTagsFunc's own slow crawl below. cmd/hoist builds this, wrapping whichever
// registry client this run already built — this package never opens a registry connection
// itself (AGENTS.md §4.8).
type RegTagsFunc func(ctx context.Context) (regTags []string, err error)

// GitTagsFunc returns the mapped app repo's own git tags (commit dates, for invariant 3's
// ordering) — the N+1 half (pkg/forge.Forge.Tags' own doc comment: one call to list tags,
// one more per tag for its commit date), split out from RegTagsFunc so it loads
// progressively after the row list has already rendered, rather than gating it (#PR8). mapped
// is this call's own observed answer: it supersedes BuildFunc's static guess below, since the
// config can say an image repo is mapped while the forge lookup this call makes still fails
// at runtime — DeriveRows/BackfillGitDates use THIS return, never BuildFunc's, once a result
// arrives. GitTags is always nil/empty when mapped is false, whichever reason made it false.
type GitTagsFunc func(ctx context.Context) (gitTags []forge.GitTag, mapped bool, err error)

// MetaFunc fetches one tag's registry metadata, lazily, only for the row the picker currently
// needs it for (New's own doc comment on laziness, AGENTS.md invariant 4).
type MetaFunc func(ctx context.Context, tag string) (registry.ImageMeta, error)

// BuildFunc is what internal/app.Model calls once per OpenTagsMsg to get imageRepo's own
// RegTagsFunc/GitTagsFunc/MetaFunc and whether RepoConfig.Apps maps it to an app git repo —
// RepoConfig's own static, config-known fact, used only as New's initial guess before either
// list has loaded. cmd/hoist builds this, wrapping whichever registry client (and, when
// mapped, pkg/forge/github.Client) the run already built — this package never opens either
// connection itself (AGENTS.md §4.8, mirroring plan.ResolveFunc's own wiring).
type BuildFunc func(imageRepo string) (mapped bool, regTagsFn RegTagsFunc, gitTagsFn GitTagsFunc, metaFn MetaFunc)

// Options is everything New takes beyond the image repo and the target env (M10: nine
// positional parameters had become a call nobody could read). Mapped is RepoConfig.Apps'
// static answer (see BuildFunc); Production gates the D key (the UI-side half of AGENTS.md
// §4.5 — internal/engine.DirectCommitGateStep is the half that matters); StagingEnv,
// StagingTags and HasStagingMismatch are StagingMismatch's result, computed by the root from
// data already discovered; RegTags, GitTags and Meta are nil-safe (a nil RegTags reports an
// error state rather than hanging; a nil GitTags just never backfills git-tag dates). History
// is the commit-history bundle (nil funcs degrade to a named gap); Declared is what the
// target env declares today (nil for a first deploy); Now is the clock relative dates are
// worded against, time.Now when nil.
type Options struct {
	Mapped, Production bool
	StagingEnv         string
	StagingTags        []string
	HasStagingMismatch bool
	RegTags            RegTagsFunc
	GitTags            GitTagsFunc
	Meta               MetaFunc
	History            history.Funcs
	Declared           *Declared
	Now                func() time.Time
}

type state int

const (
	stateLoading state = iota // ListFunc running
	stateReady
)

// focus is which pane the arrow keys move: the tag table or the commit list.
type focus int

const (
	focusTags focus = iota
	focusCommits
)

// BackMsg pops back to whatever's under the tags screen (AGENTS.md §4.8 pattern:
// matrix.OpenPlanMsg/plan.BackMsg's shape, reused here).
type BackMsg struct{}

// SelectedMsg is emitted on space ("review the change"), once the chosen row's metadata has
// loaded: the operator picked tag via the normal PR-mode path. This package only reports the
// choice — AGENTS.md §4.8 ("a screen names the transition, the root decides what it means").
// Delta carries the commit history already loaded for the tag, when there is one, so the
// confirm screen leads with it without a second fetch; Declared is what the env declares now.
type SelectedMsg struct {
	ImageRepo, Tag, Digest string
	// Target is the env this picker was opened for. Carried on the message rather than
	// left for the root to remember: the root would have to hold per-screen state to
	// recover it, and a message that does not say what it is about is how the wrong env
	// gets written.
	Target   string
	Delta    *migrate.Delta
	Declared *Declared
	// DeclaredSince is when the declared occurrence's manifest line last changed (zero when
	// unknown); HistoryNote is the sentence the pane showed when Delta is nil — the confirm
	// screen says the same thing rather than "no history" without a reason.
	DeclaredSince time.Time
	HistoryNote   string
}

// DirectRequestedMsg is emitted only once the operator has completed the keypress + huh.
// Confirm gesture AGENTS.md invariant 5 requires — never on the keypress alone, and never for
// a production target (the 'D' key is not offered at all when Production is true — see
// keyMap/onKey). This message is UI-side politeness only, exactly like plan.Model's own
// modeLabel/skipNotice: the actual, unbypassable enforcement lives in
// internal/engine.DirectCommitGateStep, which independently refuses a production env even if
// this screen (or any future caller) got this message wrong (invariant 5's "not UI-only
// gating").
type DirectRequestedMsg struct {
	ImageRepo, Tag, Digest string
	// Target, as on SelectedMsg.
	Target        string
	Delta         *migrate.Delta
	Declared      *Declared
	DeclaredSince time.Time
	HistoryNote   string
}

// nextGeneration hands out this process's next tag-picker generation id. Package-level and
// monotonically increasing (never reused, never reset) so that every Model instance New ever
// constructs — for the same image repo or a different one — gets a value no other instance,
// past or future, ever holds. See generation's own doc comment on Model for why imageRepo alone
// cannot serve this purpose.
var nextGeneration atomic.Int64

// regTagsLoadedMsg, gitTagsLoadedMsg, metaLoadedMsg, historyMsg and ageMsg all carry gen, the picker instance that
// requested them: each command closes over m.generation at the moment it is created, and the
// handlers discard a result whose gen doesn't match this model's own current one.
// internal/app's root routes a message to whatever screen is currently on top of its stack by
// type alone, not by which Model instance produced the tea.Cmd that resolves to it — if an
// operator leaves this picker while its own commands are still in flight and opens a picker
// for a different image repo, a stale result landing in the new picker would otherwise
// silently populate it with another repo's rows/metadata (common tag names like "latest" make
// this look plausible rather than obviously wrong).
//
// gen, not imageRepo, is what actually discriminates instances: imageRepo scopes only by
// REPOSITORY, and two picker instances for the SAME repo are common — closing this picker while
// its commands are still in flight, then immediately reopening a new picker for the identical
// repo, gives the new instance the identical imageRepo value. gen is assigned fresh by New for
// every instance regardless of repo, so it discriminates same-repo reopens exactly as it
// discriminates cross-repo ones; imageRepo is kept on these messages for context/debugging
// only, not as part of the discard decision.
// regTagsLoadedMsg carries RegTagsFunc's own answer — the fast half of the load (#PR8).
type regTagsLoadedMsg struct {
	imageRepo string
	gen       int64
	regTags   []string
	err       error
}

// gitTagsLoadedMsg carries GitTagsFunc's own answer — the slow half, arriving independently
// (#PR8): onRegTagsLoaded and onGitTagsLoaded each handle whichever order the two actually
// land in.
type gitTagsLoadedMsg struct {
	imageRepo string
	gen       int64
	gitTags   []forge.GitTag
	mapped    bool // this call's own observed answer — see GitTagsFunc's doc comment.
	err       error
}

type metaLoadedMsg struct {
	imageRepo string
	gen       int64
	tag       string
	meta      registry.ImageMeta
	err       error
}

// historyMsg is one tag's delta against the declared reference.
type historyMsg struct {
	gen   int64
	tag   string
	delta migrate.Delta
	err   error
}

// ageMsg is the declared occurrence's live age (when its manifest line last changed).
type ageMsg struct {
	gen int64
	age migrate.LineAge
	err error
}

type keyMap struct {
	Up, Down, Filter, Direct, Review, Read, Pane, Back key.Binding
	// Top and Bottom jump the commit-detail body to its ends (reading mode only).
	Top, Bottom key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "move")),
		Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Direct: key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "direct commit")),
		// Space reviews the change (opens the confirm screen); enter reads the commit under
		// the cursor. Inspecting is the cheap default and moving toward a write takes a
		// different, deliberate key (docs/tui/mockups.html, screen 02).
		Review: key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "review the change")),
		Read:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "read commit")),
		Pane:   key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "commits")),
		Back:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Top:    key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top of the body")),
		Bottom: key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "end of the body")),
	}
}

// Model is the tag picker screen for one image repo. It is a value: Update, SetSize and
// SetStyles return the updated model, matching internal/app/plan's convention.
type Model struct {
	imageRepo  string
	target     string
	mapped     bool // RepoConfig.Apps has an entry for imageRepo
	production bool

	// generation uniquely identifies this Model instance, assigned once by New from
	// nextGeneration. Two instances for the SAME imageRepo (closing this picker while its
	// commands are still in flight, then immediately reopening a new picker for the identical
	// repo) get different generations — imageRepo alone cannot tell them apart, since a
	// reopened picker's imageRepo is, by definition, identical to the one it replaced. See
	// regTagsLoadedMsg/metaLoadedMsg's own doc comment.
	generation int64

	// ctx/cancel scope every listFn/metaFn/history call this instance ever makes (the
	// commands close over ctx, never context.Background()). cancel is called once the operator
	// leaves this picker for good — Esc (onKey's Back handling, both in and out of the confirm
	// dialog), a review selection, or a confirmed direct-mode request (round-N finding, Codex
	// P2, "cancel tag loads when leaving the picker"): without it, a load already in flight
	// when the picker closes keeps running to completion in the background even though its
	// eventual result is already discarded by the generation guard above — for a mapped repo,
	// ListFunc can walk Forge.Tags through up to 301 sequential GitHub requests, so repeatedly
	// opening and closing pickers could pile up obsolete crawls consuming the API rate limit,
	// or leave one hanging behind a slow request, for no operator-visible reason.
	ctx    context.Context
	cancel context.CancelFunc

	stagingEnv         string
	stagingTags        []string // rows.StagingMismatch's own doc comment: 1+ distinct tags, sorted
	hasStagingMismatch bool

	regTagsFn RegTagsFunc
	gitTagsFn GitTagsFunc
	metaFn    MetaFunc

	// regTags/gitTags/gitTagsLoaded (#PR8) persist each half's own answer independently of
	// arrival order: onRegTagsLoaded and onGitTagsLoaded are two separate commands (Init's own
	// tea.Batch), and either can land first. gitTagsLoaded — not len(gitTags) > 0 — is the
	// "has this call returned yet" flag, since a real, successful answer for an unmapped repo
	// is legitimately empty.
	regTags       []string
	regTagsLoaded bool
	gitTags       []forge.GitTag
	gitTagsLoaded bool

	// history (M10): what the target env declares, the commit delta per tag against it, and
	// the declared line's live age. deltas is keyed by tag; a tag absent from it has not been
	// asked for yet, one present with Loaded false is in flight.
	histFn   history.Funcs
	declared *Declared
	deltas   map[string]history.State
	age      migrate.LineAge
	ageErr   error
	ageKnown bool
	now      func() time.Time

	state state
	err   error

	rows        []Row
	selectedTag string
	// cursorMoved is false until the operator's first real cursor movement (moveCursor).
	// onGitTagsLoaded's own backfill uses it to decide whether the still-default initial
	// selection should track the newly-confirmed top row (#PR8: before this, the initial
	// selection could freeze on whatever the picker happened to render first — the
	// registry's own arbitrary order, or a provisional Created-based guess — and never catch
	// up once the real git-date order landed). Once the operator has moved the cursor
	// themselves, their selection is never overridden by a later reorder, mirroring
	// Reorder's own promise for a row loading mid-fetch.
	cursorMoved bool

	// focus is which list the arrow keys move; commitIdx the cursor in the commit pane;
	// reading is the commit-detail view (mockup 08), a mode of this screen rather than a
	// screen of its own, so the picker's context and its loads survive a look at one commit.
	focus     focus
	commitIdx int
	reading   bool
	// body is the commit-detail view's scrolling section (the commit message and its
	// migration files) — a viewport, since a body can be longer than the terminal (#120).
	// Laid out by layoutReading on whichever copy needs it, the flight screen's log pattern.
	body viewport.Model

	filtering   bool
	filterInput textinput.Model
	filterQuery string

	confirming    bool
	confirmDirect *huh.Confirm
	// confirmValue is huh's write target only. It is NOT read to decide anything: Value takes
	// the address of a field in whichever Model copy built the widget, and every Update since
	// has returned a new copy, so this field on the current model stays at whatever it was
	// initialised to no matter what the operator typed. confirmAgreed asks the widget instead.
	confirmValue bool

	spinner spinner.Model
	notice  string

	styles        ui.Styles
	keys          keyMap
	width, height int
}

// New builds the tag picker for imageRepo, choosing a tag for target. See Options.
func New(imageRepo, target string, o Options) Model {
	ctx, cancel := context.WithCancel(context.Background())
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return Model{
		imageRepo:          imageRepo,
		target:             target,
		mapped:             o.Mapped,
		production:         o.Production,
		generation:         nextGeneration.Add(1),
		ctx:                ctx,
		cancel:             cancel,
		stagingEnv:         o.StagingEnv,
		stagingTags:        o.StagingTags,
		hasStagingMismatch: o.HasStagingMismatch,
		regTagsFn:          o.RegTags,
		gitTagsFn:          o.GitTags,
		metaFn:             o.Meta,
		histFn:             o.History,
		declared:           o.Declared,
		deltas:             map[string]history.State{},
		now:                now,
		state:              stateLoading,
		keys:               defaultKeyMap(),
		spinner:            spinner.New(spinner.WithSpinner(spinner.Line)),
		filterInput:        textinput.New(),
		body:               newBodyViewport(),
		styles:             ui.NewStyles(true),
	}
}

// Init starts the spinner, the async tag/git-tag list load and, when the env declares this
// image already, the blame that dates that declaration. Listing talks to a registry (and,
// when mapped, a forge), so it is a tea.Cmd here, never run inside Update (AGENTS.md §4.3).
// Init fires regTagsCmd and gitTagsCmd as two independent commands (#PR8), not one after the
// other: the picker renders as soon as regTagsCmd answers, and gitTagsCmd's own slow N+1
// crawl (a mapped repo's cost) backfills the ordering whenever it lands, in either order —
// onRegTagsLoaded/onGitTagsLoaded each handle both.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.regTagsCmd(), m.gitTagsCmd(), m.ageCmd())
}

func (m Model) regTagsCmd() tea.Cmd {
	regTagsFn := m.regTagsFn
	imageRepo := m.imageRepo
	gen := m.generation
	ctx := m.ctx
	return func() tea.Msg {
		if regTagsFn == nil {
			// Findings 4/5 (round N): this used to say "no registry configured for this
			// repo" — a hardcoded placeholder naming neither the actual image repo nor how
			// to fix it. Name imageRepo and point at the config knob that supplies
			// regTagsFn (cmd/hoist wires this from the matching registries[] entry —
			// BuildFunc's own doc comment).
			return regTagsLoadedMsg{imageRepo: imageRepo, gen: gen, err: fmt.Errorf("no registry configured for %s; add a matching entry under registries[] in the config file", imageRepo)}
		}
		regTags, err := regTagsFn(ctx)
		return regTagsLoadedMsg{imageRepo: imageRepo, gen: gen, regTags: regTags, err: err}
	}
}

// gitTagsCmd is a no-op command (nil) when this repo was never mapped at all (m.mapped false
// from BuildFunc's own static guess, GitTagsFn nil) — an unmapped repo has no app repo to ask,
// so there is nothing for this to eventually answer; the picker's own unmapped-ordering
// fallback (Reorder) is already in effect from the moment regTagsCmd's own rows render.
func (m Model) gitTagsCmd() tea.Cmd {
	if m.gitTagsFn == nil {
		return nil
	}
	gitTagsFn := m.gitTagsFn
	imageRepo := m.imageRepo
	gen := m.generation
	ctx := m.ctx
	return func() tea.Msg {
		gitTags, mapped, err := gitTagsFn(ctx)
		return gitTagsLoadedMsg{imageRepo: imageRepo, gen: gen, gitTags: gitTags, mapped: mapped, err: err}
	}
}

func (m Model) fetchCmd(tag string) tea.Cmd {
	metaFn := m.metaFn
	imageRepo := m.imageRepo
	gen := m.generation
	ctx := m.ctx
	return func() tea.Msg {
		if metaFn == nil {
			// Same class of fix as loadCmd's listFn==nil case above: name the image repo and
			// tag this call was for, and point at the same registries[] config knob, rather
			// than a bare "no registry configured" the operator can't act on.
			return metaLoadedMsg{imageRepo: imageRepo, gen: gen, tag: tag, err: fmt.Errorf("no registry configured for %s:%s; add a matching entry under registries[] in the config file", imageRepo, tag)}
		}
		meta, err := metaFn(ctx, tag)
		return metaLoadedMsg{imageRepo: imageRepo, gen: gen, tag: tag, meta: meta, err: err}
	}
}

// historyCmd asks for tag's delta against the declared reference, or nil when there is no
// way to (no history wired, no declared reference, or the tag already asked for).
func (m Model) historyCmd(tag string) tea.Cmd {
	if m.histFn.Delta == nil || m.declared == nil || tag == "" {
		return nil
	}
	if _, asked := m.deltas[tag]; asked {
		return nil
	}
	m.deltas[tag] = history.State{}
	delta, gen, ctx := m.histFn.Delta, m.generation, m.ctx
	from := m.declared.Ref
	to := image.Ref{Repo: m.imageRepo, Tag: tag}
	return func() tea.Msg {
		d, err := delta(ctx, from, to)
		return historyMsg{gen: gen, tag: tag, delta: d, err: err}
	}
}

// ageCmd dates the declared occurrence's manifest line, once.
func (m Model) ageCmd() tea.Cmd {
	if m.histFn.LiveAge == nil || m.declared == nil {
		return nil
	}
	liveAge, gen, ctx, occ := m.histFn.LiveAge, m.generation, m.ctx, m.declared.Occurrence
	return func() tea.Msg {
		age, err := liveAge(ctx, occ)
		return ageMsg{gen: gen, age: age, err: err}
	}
}

// Update handles the async loads, the spinner tick, and the screen's own keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case regTagsLoadedMsg:
		return m.onRegTagsLoaded(msg)
	case gitTagsLoadedMsg:
		return m.onGitTagsLoaded(msg)
	case metaLoadedMsg:
		return m.onMetaLoaded(msg)
	case historyMsg:
		if msg.gen != m.generation {
			return m, nil
		}
		m.deltas[msg.tag] = history.State{Loaded: true, Delta: msg.delta, Err: msg.err}
		return m, nil
	case ageMsg:
		if msg.gen != m.generation {
			return m, nil
		}
		m.age, m.ageErr, m.ageKnown = msg.age, msg.err, true
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return m, nil
}

// onRegTagsLoaded (#PR8) renders rows the instant the registry's own tag list answers,
// without waiting for gitTagsCmd's own slower crawl: if gitTags has ALREADY landed by this
// point (onGitTagsLoaded ran first — a genuine possibility, not just a hypothetical, when a
// large registry's own tag list is slower than a small app repo's git-tag crawl), this builds
// the fully-ordered result directly via DeriveRows; otherwise it builds the unmapped-shaped
// rows DeriveRows already produces for that case, and onGitTagsLoaded backfills them once it
// lands.
func (m Model) onRegTagsLoaded(msg regTagsLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.generation {
		// A stale result from a picker instance this model is not (a closed-and-reopened
		// picker for the same repo, or a different repo entirely) — discard it without
		// touching any of this model's own state (see this message's own doc comment; gen,
		// not imageRepo, is what actually discriminates instances here).
		return m, nil
	}
	m.state = stateReady
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.regTags = msg.regTags
	m.regTagsLoaded = true
	m.rows = DeriveRows(msg.regTags, m.gitTags, m.gitTagsLoaded && m.mapped)
	if len(m.rows) > 0 {
		m.selectedTag = m.rows[0].Tag
	}
	var fetch tea.Cmd
	m, fetch = m.fetchVisible()
	return m, tea.Batch(fetch, m.historyCmd(m.selectedTag))
}

// onGitTagsLoaded (#PR8) backfills git-tag dates onto rows that already rendered from
// onRegTagsLoaded — or, if this lands FIRST, just records the answer for onRegTagsLoaded to
// use directly once it runs. Never blocks the picker's own opening: gitTagsCmd's own slow N+1
// crawl (a mapped repo's cost) is exactly what this exists to keep off the critical path.
func (m Model) onGitTagsLoaded(msg gitTagsLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.generation {
		return m, nil
	}
	m.gitTagsLoaded = true
	if msg.err != nil {
		// Degrade, not fail (AGENTS.md principle 5): the registry tags alone are still a
		// usable picker, just without invariant 3's own preferred ordering — the same
		// degrade-not-fail shape cmd/hoist's own GitTagsFunc implementation already applies
		// when the forge call itself errors. Folded into the msg.mapped==false handling below
		// (GitTagsFunc's own doc comment: "GitTags is always nil/empty when mapped is false,
		// whichever reason made it false") rather than returning early here unchanged — this
		// call's own error is latent in real wiring today (cmd/hoist's own GitTagsFunc already
		// converts a forge error to mapped=false, nil err), but a second forge adaptor
		// shouldn't have to rediscover the fallback gap below by hitting it directly.
		msg.mapped = false
	}
	// This call's own observed answer (GitTagsFunc's doc comment) supersedes New's
	// constructor-time guess: the config can say this image repo is mapped while the forge
	// lookup this call made still comes back false at runtime (finding 3, round 2, carried
	// over from the pre-split design) — m.mapped must reflect that unconditionally, on
	// success, whether or not msg.mapped itself is true, or a stale true guess would leave
	// onRegTagsLoaded (if it runs later) treating an unmapped answer as mapped.
	m.mapped = msg.mapped
	if !m.mapped {
		// A regression this split introduced: onMetaLoaded's own Reorder call already reads
		// m.mapped, but real bubbletea dispatch (and uitest.Drain's own depth-first traversal
		// — regTagsCmd's whole subtree, every meta fetch included, finishes before
		// gitTagsCmd's single command even starts) commonly finishes every meta fetch for the
		// visible window before this msg ever arrives. Without this, a runtime forge failure
		// discovered only after every visible row already loaded left rows in the registry's
		// own arbitrary order forever — no later onMetaLoaded call remains to re-trigger the
		// Created-based fallback (invariant 3) once mapped is known to be false.
		if m.regTagsLoaded {
			m.rows = Reorder(m.rows, false)
		}
		return m, nil
	}
	m.gitTags = msg.gitTags
	if !m.regTagsLoaded {
		return m, nil
	}
	m.rows = BackfillGitDates(m.rows, msg.gitTags)
	if m.cursorMoved || len(m.rows) == 0 {
		return m, nil
	}
	// Same reasoning as m.selectedTag's own assignment in onRegTagsLoaded: the initial
	// selection is a default cursor position, not an operator choice, so once the real
	// (git-date) order is known it should track the new top row — but only while the
	// operator hasn't moved the cursor themselves yet (cursorMoved's own doc comment). And,
	// found by an adversarial review of this commit: a selection change means nothing renders
	// for it without the same two commands onRegTagsLoaded/moveCursor already fire on every
	// OTHER selection change — without these, the reordered top row's commit delta was never
	// requested (the stale one from the old top row stuck around instead) and, in the common
	// timing where every visible-window meta fetch already finished before this message
	// landed, its registry metadata (digest, created) never loaded either.
	m.selectedTag = m.rows[0].Tag
	var fetch tea.Cmd
	m, fetch = m.fetchVisible()
	return m, tea.Batch(fetch, m.historyCmd(m.selectedTag))
}

func (m Model) onMetaLoaded(msg metaLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.generation {
		// Same stale-result guard as onRegTagsLoaded — see regTagsLoadedMsg's own doc comment.
		return m, nil
	}
	for i, r := range m.rows {
		if r.Tag == msg.tag {
			m.rows[i].MetaLoading = false
			m.rows[i].MetaLoaded = msg.err == nil
			m.rows[i].MetaErr = msg.err
			if msg.err == nil {
				m.rows[i].Meta = msg.meta
			}
			break
		}
	}
	// Reorder must not trust a bare m.mapped here (#PR8): before gitTagsCmd's own answer has
	// landed, m.mapped is still New's constructor-time guess, not this instance's own
	// observed fact — passing it unconditionally left a mapped-but-not-yet-loaded repo's rows
	// un-reordered even once every visible row's metadata was in (onGitTagsLoaded's own doc
	// comment). Reorder only when mapped is both true AND confirmed by gitTagsCmd's own
	// return; otherwise (unmapped, or mapped-but-still-unconfirmed) it's safe to apply the
	// Created-based fallback now — BackfillGitDates fully re-sorts from scratch if gitTags
	// does later confirm mapped=true, so an early Created-order here is never load-bearing
	// wrong, only provisional.
	m.rows = Reorder(m.rows, m.mapped && m.gitTagsLoaded)
	return m.fetchVisible()
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// filtering keeps its own Esc handling (clears the filter, stays on this screen) — checked
	// first, unaffected by the confirm fix below.
	if m.filtering {
		return m.updateFilter(msg)
	}
	// confirming previously fell straight into updateConfirm, which only ever handled Enter —
	// Esc was silently swallowed by huh's own widget update, trapping the operator in the
	// confirm dialog despite the status bar's own "esc back" hint (round-3 finding). Checked
	// here, before updateConfirm, so Esc actually leaves — matching how a key press this
	// screen doesn't otherwise special-case already falls through to Back below.
	if m.confirming {
		if key.Matches(msg, m.keys.Back) {
			m.confirming = false
			m.cancel() // leaving the picker for good — see the ctx/cancel field's own doc comment.
			return m, func() tea.Msg { return BackMsg{} }
		}
		return m.updateConfirm(msg)
	}
	if m.reading {
		return m.updateReading(msg)
	}
	if key.Matches(msg, m.keys.Back) {
		m.cancel() // leaving the picker for good — see the ctx/cancel field's own doc comment.
		return m, func() tea.Msg { return BackMsg{} }
	}
	if m.state != stateReady {
		return m, nil
	}
	m.notice = ""
	switch {
	case key.Matches(msg, m.keys.Filter):
		m.filtering = true
		m.focus = focusTags
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.CursorEnd()
		return m, m.filterInput.Focus()
	case key.Matches(msg, m.keys.Pane):
		if m.focus == focusTags && len(m.currentCommits()) > 0 {
			m.focus = focusCommits
		} else {
			m.focus = focusTags
		}
		return m, nil
	case key.Matches(msg, m.keys.Up):
		if m.focus == focusCommits {
			m.commitIdx = max(m.commitIdx-1, 0)
			return m, nil
		}
		return m.moveCursor(-1)
	case key.Matches(msg, m.keys.Down):
		if m.focus == focusCommits {
			m.commitIdx = min(m.commitIdx+1, max(len(m.currentCommits())-1, 0))
			return m, nil
		}
		return m.moveCursor(1)
	case key.Matches(msg, m.keys.Read):
		if len(m.currentCommits()) == 0 {
			m.notice = "no commit to read here — space reviews the change"
			return m, nil
		}
		m.reading = true
		m.body.GotoTop()
		return m, nil
	case key.Matches(msg, m.keys.Review):
		return m.selectCurrent(false)
	case key.Matches(msg, m.keys.Direct):
		if m.production {
			m.notice = fmt.Sprintf("direct mode is not offered for %s: it is a production env, so every change goes through a PR", m.target)
			return m, nil
		}
		return m.selectCurrent(true)
	}
	return m, nil
}

// newBodyViewport is the commit-detail viewport with only the paging keys bound: ↑/↓ (and
// j/k) stay the picker's own "next commit", and space stays "review the change", so the
// viewport's defaults for those (line scroll, page down) are unbound rather than fought over.
func newBodyViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
	}
	v.MouseWheelEnabled = false
	return v
}

// updateReading handles the commit-detail view: ↑/↓ walk the commits (each one read from
// its top), esc returns to the list, and every other key scrolls the body — PageUp/PageDown,
// ctrl+u/ctrl+d for half a page, g/G for the ends (#120).
func (m Model) updateReading(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		m.reading = false
	case key.Matches(msg, m.keys.Up):
		m.commitIdx = max(m.commitIdx-1, 0)
		m.body.GotoTop()
	case key.Matches(msg, m.keys.Down):
		m.commitIdx = min(m.commitIdx+1, max(len(m.currentCommits())-1, 0))
		m.body.GotoTop()
	case key.Matches(msg, m.keys.Review):
		m.reading = false
		return m.selectCurrent(false)
	case key.Matches(msg, m.keys.Top):
		m = m.layoutReading()
		m.body.GotoTop()
	case key.Matches(msg, m.keys.Bottom):
		m = m.layoutReading()
		m.body.GotoBottom()
	default:
		// Laid out on this copy first — View lays out its own, so the retained viewport
		// would otherwise still be the zero-sized one New built (flight's log, Copilot #124).
		m = m.layoutReading()
		var cmd tea.Cmd
		m.body, cmd = m.body.Update(msg)
		return m, cmd
	}
	return m, nil
}

// historyPending reports whether tag's delta was asked for and has not answered yet — the one
// state in which reviewing the change would throw the history away.
func (m Model) historyPending(tag string) bool {
	if m.histFn.Delta == nil || m.declared == nil {
		return false
	}
	st, asked := m.deltas[tag]
	return asked && !st.Loaded
}

// currentCommits is the loaded delta's commits for the cursor tag, nil when none.
func (m Model) currentCommits() []migrate.Commit {
	st, ok := m.deltas[m.selectedTag]
	if !ok || !st.Loaded || st.Err != nil {
		return nil
	}
	return st.Delta.Commits
}

// declaredSince is the declared line's age when the blame answered, zero otherwise.
func (m Model) declaredSince() time.Time {
	if m.ageKnown && m.ageErr == nil {
		return m.age.Since
	}
	return time.Time{}
}

// historyNote is the gap sentence the pane shows for the cursor tag when there is no delta
// to carry — "" when there is a delta.
func (m Model) historyNote() string {
	if m.currentDelta() != nil {
		return ""
	}
	if m.selectedTag == "" {
		return ""
	}
	if m.declared == nil {
		return fmt.Sprintf("no commit history — %s does not declare %s yet, so there is nothing to compare with", m.target, m.imageRepo)
	}
	if m.histFn.Delta == nil {
		return fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", m.imageRepo)
	}
	lines := history.Lines(m.deltas[m.selectedTag], m.selectedTag, "", m.target, m.imageRepo, m.histFn.Mapped == nil || m.histFn.Mapped(m.imageRepo), 1, -1)
	if len(lines) > 0 && lines[0].Role != "head" {
		return lines[0].Text
	}
	return ""
}

// currentDelta is the loaded delta for the cursor tag, for the selection messages.
func (m Model) currentDelta() *migrate.Delta {
	st, ok := m.deltas[m.selectedTag]
	if !ok || !st.Loaded || st.Err != nil {
		return nil
	}
	d := st.Delta
	return &d
}

// CapturesText implements app.Screen (via tagsScreen's thin delegate in internal/app/screen.go).
// The root queries this before treating "q" as its own global quit key (round 5, finding 3):
// only the filter's own text-entry mode counts — the confirm dialog is a huh.Confirm (y/n/enter/
// esc only; "q" was never meant to be typed there, so leaving it to the global quit key is fine).
func (m Model) CapturesText() bool { return m.filtering }

func (m Model) filtered() []Row { return Filter(m.rows, m.filterQuery) }

func (m Model) moveCursor(delta int) (Model, tea.Cmd) {
	rows := m.filtered()
	if len(rows) == 0 {
		return m, nil
	}
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		idx = 0
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	m.selectedTag = rows[idx].Tag
	m.cursorMoved = true
	m.commitIdx = 0
	var fetch tea.Cmd
	m, fetch = m.fetchVisible()
	return m, tea.Batch(fetch, m.historyCmd(m.selectedTag))
}

// selectCurrent emits SelectedMsg or, once the operator has confirmed, opens the huh.Confirm
// gesture direct mode requires (invariant 5's keypress-then-confirm shape) before a
// DirectRequestedMsg is ever emitted. Neither message is emitted until the current row's
// metadata (digest) has actually loaded — a selection without a resolved digest would promote
// a tag hoist cannot yet pin, which AGENTS.md principle 3 refuses at the manifest-write layer
// anyway; refusing it here just gives an earlier, clearer notice.
func (m Model) selectCurrent(direct bool) (Model, tea.Cmd) {
	rows := m.filtered()
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		return m, nil
	}
	r := rows[idx]
	if !r.MetaLoaded {
		// MetaErr set means fetchVisible already tried this row and settled it as failed
		// (its own doc comment: a failed row is never rescheduled) — "still loading" would be
		// permanently wrong for it and would never self-correct, leaving the row silently
		// unselectable with no operator-visible explanation (round-N finding). Only the absence
		// of both MetaLoaded and MetaErr means a fetch is genuinely still in flight.
		if r.MetaErr != nil {
			m.notice = fmt.Sprintf("metadata for %s failed to load and will not be retried — hoist never writes an image it has no digest for", r.Tag)
		} else {
			m.notice = fmt.Sprintf("still loading metadata for %s — try again in a moment", r.Tag)
		}
		return m, nil
	}
	if m.historyPending(r.Tag) {
		// The confirm screen leads with this history and never refetches it; leaving now
		// would cancel the load and show the no-history form for a mapped image.
		m.notice = fmt.Sprintf("still reading %s's commits — try again in a moment", r.Tag)
		return m, nil
	}
	if !direct {
		m.cancel() // leaving the picker for good — see the ctx/cancel field's own doc comment.
		tag, digest, delta, declared := r.Tag, r.Meta.Digest, m.currentDelta(), m.declared
		since, note := m.declaredSince(), m.historyNote()
		return m, func() tea.Msg {
			return SelectedMsg{ImageRepo: m.imageRepo, Tag: tag, Digest: digest, Target: m.target, Delta: delta, Declared: declared, DeclaredSince: since, HistoryNote: note}
		}
	}
	m.confirming = true
	m.confirmValue = false
	verb := fmt.Sprintf("Commit %s directly to %s's base branch — no PR, no review? This is only offered for non-production envs.", r.Tag, m.target)
	m.confirmDirect = huh.NewConfirm().Title(verb).Value(&m.confirmValue)
	// WithKeyMap is not optional decoration: huh.NewConfirm leaves keymap zero-valued, and a
	// zero key.Binding matches nothing, so a Confirm used standalone (rather than inside a
	// huh.Form, which installs the keymap itself) ignores every keypress. Without it y/n/←/→
	// all did nothing and this gesture could not be completed at all by a real operator —
	// only by a test reaching past the widget to set the bool (Copilot, PR #72).
	m.confirmDirect.WithKeyMap(huh.NewDefaultKeyMap())
	m.confirmDirect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.confirmDirect.WithWidth(m.dialogWidth())
	return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		m.confirming = false
		if !m.confirmAgreed() {
			return m, nil
		}
		rows := m.filtered()
		idx := IndexOf(rows, m.selectedTag)
		if idx < 0 {
			return m, nil
		}
		r := rows[idx]
		m.cancel() // leaving the picker for good — see the ctx/cancel field's own doc comment.
		tag, digest, delta, declared := r.Tag, r.Meta.Digest, m.currentDelta(), m.declared
		since, note := m.declaredSince(), m.historyNote()
		return m, func() tea.Msg {
			return DirectRequestedMsg{ImageRepo: m.imageRepo, Tag: tag, Digest: digest, Target: m.target, Delta: delta, Declared: declared, DeclaredSince: since, HistoryNote: note}
		}
	}
	f, cmd := m.confirmDirect.Update(msg)
	// Keep whatever the widget returned: huh's own Update is where the operator's y/n lands,
	// and its accessor is the only honest reading of it (see confirmValue's own comment).
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmDirect = c
	}
	return m, cmd
}

// confirmAgreed is the operator's actual answer, read from the widget rather than from the
// bool huh was pointed at. The pointer form (Value(&m.confirmValue)) captures a field in a
// Model copy that Update immediately supersedes, so reading the field made the D gesture
// unreachable through real input: y then enter emitted nothing at all, and the tests that
// covered it assigned the field directly and so could never have caught it (Copilot, PR #72).
func (m Model) confirmAgreed() bool {
	if m.confirmDirect == nil {
		return false
	}
	v, _ := m.confirmDirect.GetValue().(bool)
	return v
}

func (m Model) updateFilter(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		switch kmsg.String() {
		case "enter":
			m.filtering = false
			m.filterInput.Blur()
			return m, nil
		case "esc":
			m.filtering = false
			m.filterQuery = ""
			m.filterInput.SetValue("")
			m.filterInput.Blur()
			return m.fetchVisible()
		}
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	if m.filterInput.Value() != m.filterQuery {
		m.filterQuery = m.filterInput.Value()
		rows := m.filtered()
		if IndexOf(rows, m.selectedTag) < 0 && len(rows) > 0 {
			m.selectedTag = rows[0].Tag
			m.commitIdx = 0 // a new tag's commits, so the cursor starts over (Copilot, #112)
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.fetchVisible()
		cmd = tea.Batch(cmd, fetchCmd, m.historyCmd(m.selectedTag))
	}
	return m, cmd
}

// Fixed rows the frame spends outside the tag table: the meta section's lines and the
// commit pane's, so pageSize and the render agree on what is visible.
const (
	paneRowsWide   = 8 // head + up to 7 commit lines, when the terminal is tall
	paneRowsNarrow = 4
	minPageSize    = 3
)

// paneRows is how many lines the commit pane gets: none when there is no way to have one,
// one for a gap sentence, else a share of the height.
func (m Model) paneRows() int {
	if m.histFn.Delta == nil || m.declared == nil {
		return 1
	}
	if m.height >= 30 {
		return paneRowsWide
	}
	return paneRowsNarrow
}

// metaLines is the meta section's own line count: the repo→target line, the declared line
// when there is one, the staging note when there is one, the filter line when active.
func (m Model) metaLines() int {
	n := 1
	if m.declared != nil {
		n += lipgloss.Height(m.declaredLine()) // a split env's line may wrap
	}
	if m.hasStagingMismatch {
		n += lipgloss.Height(m.wrap(m.stagingNote()))
	}
	if m.filtering || m.filterQuery != "" {
		n++
	}
	return n
}

// pageSize is how many tag rows the table shows and fetchVisible keeps warm around the
// cursor: what the frame has left after its chrome, the meta section and the commit pane —
// with a floor, and a fixed default before the screen has a size at all (a snapshot test).
func (m Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	body := ui.BodyHeight(m.height, 3) - m.metaLines() - m.paneRows() - 1 // header row
	if m.notice != "" {
		body--
	}
	return max(body-m.dividerRows(), minPageSize)
}

// dividerRows is how many table rows the dividers can take from the page: one per group
// present beyond the release group (#91), and one for the "unordered" rule when a mapped
// repo has a release tag with no git date. Counted over the whole filtered list rather than
// the window — the window depends on the page size — so a divider outside the window
// over-reserves a row rather than pushing the cursor's row off the bottom of the frame.
func (m Model) dividerRows() int {
	classes := map[Class]bool{}
	unordered := false
	for _, r := range m.filtered() {
		classes[r.Class] = true
		if m.mapped && r.Class == ClassRelease && !r.HasGitDate {
			unordered = true
		}
	}
	n := 0
	for c := range classes {
		if c != ClassRelease {
			n++
		}
	}
	if unordered {
		n++
	}
	return n
}

// visibleWindow computes the half-open [start,end) slice of rows this screen keeps warm and
// draws, centered on (or otherwise including) the cursor — the single source of truth both
// fetchVisible (what gets a MetaFunc call) and the render share, so the two can never drift
// apart (AGENTS.md §8, layered checks: one definition of "visible", not two that can disagree
// about what's on screen). rows is the caller's own m.filtered() result, passed in rather
// than recomputed here so a caller that already has it doesn't pay for it twice.
func (m Model) visibleWindow(rows []Row) (start, end int) {
	if len(rows) == 0 {
		return 0, 0
	}
	idx := IndexOf(rows, m.selectedTag)
	if idx < 0 {
		idx = 0
	}
	page := m.pageSize()
	start = idx - page/2
	if start < 0 {
		start = 0
	}
	end = start + page
	if end > len(rows) {
		end = len(rows)
		start = max(0, end-page)
	}
	return start, end
}

// fetchVisible fires MetaFunc for every row within the current visibleWindow that isn't already
// loaded, loading, or settled with an error (finding 4: a prior MetaErr means this row's
// Config call already failed once — never rescheduled without an explicit re-arm this round
// chose not to add — see this function's own loop comment) — AGENTS.md invariant 4's "fetch on
// demand as rows become visible/selected", never every tag up front. Called after the list
// loads, on cursor movement, on a filter change, and after a metadata load reorders the list
// (which can shift what's near the cursor for the unmapped/Created-sorted case).
func (m Model) fetchVisible() (Model, tea.Cmd) {
	rows := m.filtered()
	if len(rows) == 0 {
		return m, nil
	}
	start, end := m.visibleWindow(rows)

	byTag := make(map[string]int, len(m.rows))
	for i, r := range m.rows {
		byTag[r.Tag] = i
	}
	var cmds []tea.Cmd
	for _, r := range rows[start:end] {
		i := byTag[r.Tag]
		// A row with MetaErr already set is settled, exactly like MetaLoaded — finding 4: a
		// row whose Config call failed (a missing manifest, an unsupported platform, a
		// temporarily unavailable registry) must not be rescheduled every time fetchVisible
		// runs again (the cursor moving, a filter change, another row's own metadata landing
		// and reordering the list), or it retries unboundedly for as long as the picker stays
		// open and the row remains visible. No explicit retry action exists yet this round
		// (the finding allows either shape) — a failed row simply stays settled, rendered via
		// builtCell/digestCell's own "load failed"/"—" cases, until this Model is torn down
		// and a fresh one (a new generation) is opened.
		if m.rows[i].MetaLoaded || m.rows[i].MetaLoading || m.rows[i].MetaErr != nil {
			continue
		}
		m.rows[i].MetaLoading = true
		cmds = append(cmds, m.fetchCmd(r.Tag))
	}
	return m, tea.Batch(cmds...)
}

// SetSize lays the screen out to width × height.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	if m.confirmDirect != nil {
		m.confirmDirect.WithWidth(m.dialogWidth())
	}
	m.filterInput.SetWidth(max(width-14, 10))
	return m
}

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

// SetStyles applies the palette (and its dark/light flag to huh's own Charm theme —
// AGENTS.md §4.7: a component's own theming is not a layout library).
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	if m.confirmDirect != nil {
		m.confirmDirect.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	}
	return m
}

// View renders the current state in the M10 frame. Every rendered string passes through
// redact.Strings once more here (AGENTS.md §4.4/R-002), matching plan.Model.View's own
// final-boundary pattern.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		// A screen the root has not sized yet (a test calling View directly) still renders
		// something readable rather than nothing.
		m.width, m.height = 80, 24
	}
	var out string
	switch {
	case m.err != nil:
		out = m.viewErr()
	case m.state == stateLoading:
		out = m.viewLoading()
	case m.reading:
		out = m.viewReading()
	default:
		out = m.viewReady()
	}
	if m.confirming && m.confirmDirect != nil {
		out = ui.Dialog(m.styles, out, "direct commit", m.confirmDirect.View(), m.width, m.height)
	}
	return redact.Strings(out)
}

func (m Model) title() string { return "hoist · deploy" }

// metaSection is the frame's first section: the repo and target, what the env declares
// today and for how long, the staging note, and the filter line.
func (m Model) metaSection() string {
	lines := []string{m.styles.Title.Render(m.imageRepo) + "  →  " + m.styles.Title.Render(m.target) + m.productionChip()}
	if m.declared != nil {
		lines = append(lines, m.declaredLine())
	}
	if m.hasStagingMismatch {
		lines = append(lines, m.styles.Notice.Render(m.wrap(m.stagingNote())))
	}
	switch {
	case m.filtering:
		lines = append(lines, "filter: "+m.filterInput.View())
	case m.filterQuery != "":
		lines = append(lines, m.styles.Dim.Render(fmt.Sprintf("filter: %q (esc in filter mode to clear)", m.filterQuery)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) productionChip() string {
	if !m.production {
		return ""
	}
	return "   " + m.styles.Production.Render("production")
}

// declaredLine words what the env declares: "app-production declares  v1 · 111111111111 ·
// since 34 days ago". "Declares", never "runs": this is the manifest, not the cluster. A
// split env (one image repo at several references, #119) names every reference and says
// which one the commit pane measures from — the first by file then line (Declared.Ref) —
// rather than claiming a single declared build; BuildDeployPlan rewrites every one of them.
func (m Model) declaredLine() string {
	ref := m.declared.Ref
	parts := []string{m.styles.Dim.Render(m.target + " declares"), "  " + m.styles.Accent.Render(tagOrDigest(ref))}
	if ref.Digest != "" {
		parts = append(parts, " · "+ShortDigest(ref.Digest))
	}
	if m.declared.Split() {
		for _, r := range m.declared.Refs[1:] {
			parts = append(parts, m.styles.Dim.Render(" and ")+m.styles.Accent.Render(tagOrDigest(r)))
			if r.Digest != "" {
				parts = append(parts, " · "+ShortDigest(r.Digest))
			}
		}
		parts = append(parts, m.styles.Warn.Render(" — split"), m.styles.Dim.Render("; commits are counted from "+tagOrDigest(ref)))
	}
	switch {
	case m.ageKnown && m.ageErr == nil:
		since := "since " + ui.Ago(m.now(), m.age.Since)
		if m.age.Approximate {
			since += " (approximately)"
		}
		parts = append(parts, m.styles.Dim.Render(" · "+since))
	case m.ageKnown:
		parts = append(parts, m.styles.Dim.Render(" · age unknown"))
	case m.histFn.LiveAge != nil:
		parts = append(parts, m.styles.Dim.Render(" · since …"))
	}
	return m.wrap(strings.Join(parts, ""))
}

func tagOrDigest(r image.Ref) string {
	if r.Tag != "" {
		return r.Tag
	}
	return ShortDigest(r.Digest)
}

// stagingNote renders the paired staging env's committed manifest tag(s) for this image repo:
// the single agreed tag in the common case, or an explicit disagreement (finding 3, round N)
// when StagingMismatch found more than one distinct tag across the staging env's own
// families/occurrences — gitops.BuildPlan's WarnSourceDisagrees is the same "warn, don't
// block" shape for the analogous disagreement among one env's SOURCE occurrences; this
// mirrors it rather than silently rendering an arbitrary one of them and hiding the rest.
// Only ever called when m.hasStagingMismatch is true, which StagingMismatch never reports
// alongside an empty m.stagingTags (see its own doc comment).
//
// Known gap, deliberately not closed here (issue #74): when the paired staging env has no
// occurrence of this image repo AT ALL, StagingMismatch reports ok=false and the note is
// suppressed entirely — so the strongest "this has never been through staging" case is the
// one case that gets no verdict. Closing it means StagingMismatch distinguishing "no pair
// configured" from "pair exists, nothing of this repo in it", which is the same return
// reshaping #74 already needs for digests. Until then nothing here, and nothing in
// docs/repo-map.md, may describe this note as covering that case.
func (m Model) stagingNote() string {
	// The env's own committed state comes first and is described in exactly the terms it is
	// read in — a manifest occurrence, never a live cluster read (this package has no cluster
	// connection wired in at all), and never collapsed to one tag when the env disagrees with
	// itself.
	base := fmt.Sprintf("note: %s (paired staging) disagrees with itself on this image's committed manifest tag: %s",
		m.stagingEnv, strings.Join(m.stagingTags, ", "))
	if len(m.stagingTags) == 1 {
		base = fmt.Sprintf("note: %s (paired staging)'s committed manifest tag is %s", m.stagingEnv, m.stagingTags[0])
	}
	// The comparison this note existed to enable but never made. StagingMismatch has always
	// fetched the paired staging env's committed tags; until now the screen printed them and
	// left the operator to check the tag under their own cursor against the list by eye —
	// which is the actual question ("has this build been anywhere first?") on the screen where
	// it is being answered. Appended rather than substituted so the honest description of what
	// was read survives the verdict.
	tag := m.cursorTag()
	switch {
	case tag == "":
		return base
	case m.stagingRuns(tag):
		// Named as a tag comparison, because that is all it is: StagingMismatch carries the
		// staging env's committed tag STRINGS, and a tag is mutable — staging may have
		// committed v1 when v1 meant one digest and the registry may point v1 at another now.
		// Saying "v1 is committed there" would let that read as "this build went through
		// staging", which this data cannot support (Copilot, PR #73; issue #74 for carrying
		// the digests through and comparing those).
		return base + fmt.Sprintf("; %s is the tag committed there — tags move, so this is not proof of the same build", tag)
	default:
		return base + fmt.Sprintf("; warning: %s (under the cursor) is not committed there — it has not been through %s",
			tag, m.stagingEnv)
	}
}

// cursorTag is the tag the operator is looking at, or "" when nothing is selected yet and
// there is nothing to compare.
func (m Model) cursorTag() string { return m.selectedTag }

// stagingRuns reports whether the paired staging env's committed manifest carries tag.
func (m Model) stagingRuns(tag string) bool {
	for _, t := range m.stagingTags {
		if t == tag {
			return true
		}
	}
	return false
}

func (m Model) wrap(s string) string { return ansi.Wordwrap(s, max(m.width-2, 20), "") }

func (m Model) viewErr() string {
	// Wrapped, not left as one long line. A registry failure is a list — one clause per
	// credential source — and printing it unwrapped means the terminal clips it after the first
	// clause, which is the one least likely to be the actionable one: an operator sees
	// "HOIST_GHCR_TOKEN is not set" and never reaches "cluster: not configured", which is the
	// clause that tells them what to fix.
	body := m.styles.Notice.Render(wrapError(m.err.Error(), max(m.width-2, 20)))
	return ui.Frame{Title: m.title(), Sections: []string{m.metaSection(), body}, Footer: ui.StatusBar(m.width, "", m.styles.Hint.Render("esc back"))}.Render(m.styles, m.width, m.height)
}

// wrapError breaks an error across lines at its own clause separators first, then at spaces,
// so a credential chain's "source: reason; source: reason" reads one source per line.
func wrapError(msg string, width int) string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for _, clause := range strings.Split(msg, "; ") {
		out = append(out, ansi.Wordwrap(clause, width, ""))
	}
	return strings.Join(out, "\n")
}

func (m Model) viewLoading() string {
	return ui.Frame{Title: m.title(), Sections: []string{m.metaSection(), m.spinner.View() + " listing tags…"}, Footer: ui.StatusBar(m.width, "", m.styles.Hint.Render("esc back"))}.Render(m.styles, m.width, m.height)
}

func (m Model) viewReady() string {
	sections := []string{m.metaSection(), m.tableSection(), m.paneSection()}
	return ui.Frame{Title: m.title(), Sections: sections, Footer: m.footer()}.Render(m.styles, m.width, m.height)
}

// tableSection is the header row and the visible window of tag rows, plus the notices that
// belong to the table.
func (m Model) tableSection() string {
	rows := m.filtered()
	widths := m.columnWidths(rows)
	var b strings.Builder
	b.WriteString(m.styles.Header.Render(m.tableRow(widths, "", "TAG", "BUILT", "DIGEST", "")))
	if len(rows) == 0 {
		b.WriteString("\n  (no matching tags)")
	}
	// Windowed to the same [start,end) fetchVisible uses (visibleWindow), so the cursor's row
	// is always among what's drawn — moving the cursor past one screen's worth of rows must
	// scroll the window with it, never leave the selected row off-screen (finding 5).
	start, end := m.visibleWindow(rows)
	// Groups (#91): a divider above the first drawn row of the digest and moving groups —
	// also when the window opens mid-group, so a scrolled page still says what it is
	// looking at. The release group is the default view and gets none; the "unordered"
	// divider (a mapped repo's releases with no matching git tag) lives inside it only,
	// since a digest or moving tag never matches a git tag and would always be "unordered".
	dividerShown := false
	prev := ClassRelease
	for i, r := range rows[start:end] {
		if r.Class != prev || (i == 0 && r.Class != ClassRelease) {
			b.WriteString("\n" + m.styles.Dim.Render(ansi.Truncate(r.Class.Divider(), max(m.width-2, 10), "…")))
		}
		prev = r.Class
		if m.mapped && r.Class == ClassRelease && !r.HasGitDate && !dividerShown {
			b.WriteString("\n" + m.styles.Dim.Render("── unordered (no matching git tag) ──"))
			dividerShown = true
		}
		b.WriteString("\n" + m.rowLine(widths, r))
	}
	// Finding 4 (round 5): for an unmapped repo, Created-based ordering (invariant 3's fallback)
	// is only actually established among rows fetchVisible has already loaded — AGENTS.md
	// invariant 4's deliberate laziness (New's own doc comment) means a row outside every window
	// the cursor has visited so far may never be fetched at all, so a genuinely newer tag sitting
	// there can never be sorted to the top. Rather than eagerly fetching every row up front
	// (which would defeat that laziness) or silently claiming a Created-sort this screen hasn't
	// actually established, count and name how many rows outside the current window are still
	// unevaluated so the operator can tell the sort is provisional, not complete.
	if !m.mapped {
		var pending int
		for i, r := range rows {
			if i >= start && i < end {
				continue
			}
			if !r.MetaLoaded {
				pending++
			}
		}
		if pending > 0 {
			b.WriteString("\n" + m.styles.Notice.Render(fmt.Sprintf(
				"%d tag(s) outside the visible window haven't been evaluated yet — Created order isn't fully established",
				pending,
			)))
		}
	}
	if m.notice != "" {
		b.WriteString("\n" + m.styles.Notice.Render(m.wrap(m.notice)))
	}
	return b.String()
}

// columnWidths sizes the four columns to the visible rows: the tag column to its widest
// tag (bounded), BUILT and DIGEST fixed, the provenance column with the rest.
func (m Model) columnWidths(rows []Row) [4]int {
	tag := len("TAG")
	for _, r := range rows {
		tag = max(tag, ansi.StringWidth(r.Tag))
	}
	tag = min(tag, max(m.width/3, 12))
	return [4]int{tag, 13, 12, 0}
}

func (m Model) tableRow(w [4]int, marker, tag, built, digest, prov string) string {
	return fmt.Sprintf("%-2s%-*s  %-*s  %-*s  %s", marker, w[0], ansi.Truncate(tag, w[0], "…"), w[1], built, w[2], digest, prov)
}

func (m Model) rowLine(w [4]int, r Row) string {
	marker := "  "
	if r.Tag == m.selectedTag {
		marker = "▸ "
	}
	line := m.tableRow(w, marker, r.Tag, m.builtCell(r), m.digestCell(r), m.provenance(r))
	if r.Tag == m.selectedTag && m.focus == focusTags {
		return m.styles.Selected.Render(line)
	}
	return line
}

// provenance is the fourth column: where else this tag is — "in app-staging" when the
// paired staging env's committed manifest carries it, "◂ declared here" for what the target
// env declares now (any of its references, when split). Both are tag comparisons, and the
// staging note says what that proves.
func (m Model) provenance(r Row) string {
	var parts []string
	if m.declared != nil && m.declaresTag(r.Tag) {
		parts = append(parts, m.styles.Accent.Render("◂ declared here"))
	}
	if m.hasStagingMismatch && m.stagingRuns(r.Tag) {
		parts = append(parts, m.styles.Good.Render("in "+m.stagingEnv))
	}
	return strings.Join(parts, "  ")
}

// declaresTag reports whether any of the target env's declared references carries tag.
func (m Model) declaresTag(tag string) bool {
	if m.declared == nil {
		return false
	}
	if len(m.declared.Refs) == 0 {
		return m.declared.Ref.Tag == tag // a Declared built by hand, without Refs
	}
	for _, r := range m.declared.Refs {
		if r.Tag == tag {
			return true
		}
	}
	return false
}

// builtCell is when the build was made, relative to now: the app repo's git tag date when
// the repo is mapped, else the registry's Created — the same source DeriveRows/Reorder order
// the rows by.
func (m Model) builtCell(r Row) string {
	switch {
	case r.HasGitDate:
		return ui.Ago(m.now(), r.GitDate)
	case r.MetaLoaded:
		return ui.Ago(m.now(), r.Meta.Created)
	case r.MetaErr != nil:
		return "load failed"
	case r.MetaLoading:
		return m.spinner.View()
	default:
		return "…"
	}
}

func (m Model) digestCell(r Row) string {
	switch {
	case r.MetaLoaded:
		return ShortDigest(r.Meta.Digest)
	case r.MetaErr != nil:
		return "—"
	case r.MetaLoading:
		return m.spinner.View()
	default:
		return "…"
	}
}

// paneSection is the commit pane for the cursor tag.
func (m Model) paneSection() string {
	if m.selectedTag == "" {
		return m.styles.Dim.Render("no tag under the cursor")
	}
	declared := "what " + m.target + " declares"
	if m.declared != nil {
		declared = tagOrDigest(m.declared.Ref)
	}
	if m.declared == nil {
		return m.styles.Dim.Render(fmt.Sprintf("no commit history — %s does not declare %s yet, so there is nothing to compare with", m.target, m.imageRepo))
	}
	if m.histFn.Delta == nil {
		return m.styles.Dim.Render(fmt.Sprintf("no commit history — %s has no app repo in repos[].apps", m.imageRepo))
	}
	at := -1
	if m.focus == focusCommits {
		at = m.commitIdx
	}
	lines := history.Lines(m.deltas[m.selectedTag], m.selectedTag, declared, m.target, m.imageRepo, m.histFn.Mapped == nil || m.histFn.Mapped(m.imageRepo), m.paneRows(), at)
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		text := m.wrap(l.Text)
		switch l.Role {
		case "head":
			if strings.Contains(l.Text, "migration") && !strings.Contains(l.Text, "not tracked") {
				head, rest, _ := strings.Cut(l.Text, " · ")
				text = m.styles.Title.Render(head) + " · " + m.styles.Warn.Render(rest)
			} else {
				text = m.styles.Title.Render(l.Text)
			}
		case "commit", "migration":
			marker := "  "
			if m.focus == focusCommits && l.Index == m.commitIdx {
				marker = "▸ "
			}
			text = marker + l.Text
			if l.Role == "migration" {
				text = marker + m.styles.Warn.Render(ansi.Truncate(l.Text, max(m.width-2-2-12, 10), "…")) + "  " + m.styles.Warn.Render("migration")
			}
			if marker == "▸ " {
				text = m.styles.Selected.Render(ansi.Strip(text))
			}
		case "more", "gap", "wait":
			text = m.styles.Dim.Render(text)
		}
		out = append(out, text)
	}
	return strings.Join(out, "\n")
}

// readingHead is the commit-detail view's fixed section: subject and position. The second
// line says how much of the body is on screen when it does not all fit, so a clipped body
// is never mistaken for the whole of it.
func (m Model) readingHead(c migrate.Commit, n int) string {
	st := m.deltas[m.selectedTag]
	// Forward: the commit is in the cursor tag and not in what the env declares. A rollback
	// lists the commits being removed, so the containment reads the other way round.
	in, notIn := m.selectedTag, tagOrDigest(st.Delta.From.Ref)
	if st.Delta.Direction == migrate.DirectionRollback {
		in, notIn = notIn, in
	}
	pos := fmt.Sprintf("%d of %d in %s · not in %s · %s · %s", m.commitIdx+1, n, in, notIn, c.Author, ui.Ago(m.now(), c.Date))
	if m.body.TotalLineCount() > m.body.Height() {
		pos += fmt.Sprintf(" · body %d%% (pgdn scrolls)", int(m.body.ScrollPercent()*100))
	}
	return m.styles.Title.Render(history.ShortSHA(c.SHA)+"   "+c.Subject) + "\n" + m.styles.Dim.Render(pos)
}

// readingBody is what the viewport scrolls: the commit message, wrapped, and the migration
// files the commit carries.
func (m Model) readingBody(c migrate.Commit) string {
	body := c.Body
	if strings.TrimSpace(body) == "" {
		body = m.styles.Dim.Render("(no body)")
	} else {
		body = m.wrap(body)
	}
	if len(c.Migrations) > 0 {
		body += "\n\n" + m.styles.Warn.Render("migrations in this commit:\n  "+strings.Join(c.Migrations, "\n  "))
	}
	return body
}

// layoutReading sizes the body viewport to what the frame leaves after the head, and loads
// the cursor commit into it — the offset is kept, so a scrolled body stays scrolled across
// a redraw. A no-op when there is no commit to read.
func (m Model) layoutReading() Model {
	commits := m.currentCommits()
	if len(commits) == 0 || m.commitIdx >= len(commits) {
		return m
	}
	c := commits[m.commitIdx]
	m.body.SetWidth(m.width - 2)
	m.body.SetHeight(max(ui.BodyHeight(m.height, 2)-2, 1)) // the head is two lines
	m.body.SetContent(m.readingBody(c))
	return m
}

// viewReading is the commit-detail view (mockup 08): subject and position, then the body and
// the commit's migration files in a viewport — PageUp/PageDown, ctrl+u/ctrl+d and g/G scroll
// it when the message is longer than the terminal (#120); ↑/↓ move to the next commit.
func (m Model) viewReading() string {
	commits := m.currentCommits()
	if len(commits) == 0 || m.commitIdx >= len(commits) {
		m.reading = false
		return m.viewReady()
	}
	m = m.layoutReading()
	sections := []string{m.readingHead(commits[m.commitIdx], len(commits)), m.body.View()}
	return ui.Frame{Title: "hoist · deploy · commit", Sections: sections, Footer: ui.StatusBar(m.width, "", m.styles.Hint.Render("↑/↓ next commit · pgup/pgdn ctrl+u/d g/G scroll · space review · esc back"))}.Render(m.styles, m.width, m.height)
}

func (m Model) footer() string {
	help := "↑/↓ move · / filter · space review the change"
	if len(m.currentCommits()) > 0 {
		help = "↑/↓ move · tab commits · enter read commit · space review the change"
	}
	if !m.production {
		help += " · D direct"
	}
	help += " · esc back"
	return ui.StatusBar(m.width, "", m.styles.Hint.Render(help))
}
