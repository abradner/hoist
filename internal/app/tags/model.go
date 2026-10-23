package tags

import (
	"context"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/migrate"
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

// keyMap is the tag picker's own key set, sourced from the approved keymap (docs/audit's
// "Screen × key" table, internal/ui/keys' registry) rather than invented locally. enter
// is the screen's one primary action (review the change → SelectedMsg); → opens the commit
// reader, ← (Left, reading mode only) and tab move between the list and the commits; space is
// no longer bound at all (rule 2: enter is always the primary), and the D/direct-commit gesture
// retires from this screen entirely — direct mode is now a deploy-confirm-screen concern
// (internal/app/deploy), reached only after a tag has been reviewed.
type keyMap struct {
	Up, Down, Left, Right, Filter, Review, Pane, Back, Reload key.Binding
	// Home and End jump the commit-detail body to its ends (reading mode only) — replacing the
	// old g/G (rule 7: home/end are the shared page-jump keys).
	Home, End key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:     keys.Up.Bubbles(),
		Down:   keys.Down.Bubbles(),
		Left:   keys.Left.Bubbles(),
		Right:  keys.Right.Bubbles(),
		Filter: keys.Filter.Bubbles(),
		// Review is the screen's primary action (enter), matching rule 2. Reading a commit
		// moves to → (Right) — inspecting is spatial, not the write-adjacent primary.
		Review: keys.Enter.Bubbles(),
		Pane:   keys.Tab.Bubbles(),
		Back:   keys.Esc.Bubbles(),
		Reload: keys.Refresh.Bubbles(),
		Home:   keys.Home.Bubbles(),
		End:    keys.End.Bubbles(),
	}
}

// Model is the tag picker screen for one image repo. It is a value: Update, SetSize and
// SetStyles return the updated model, matching internal/app/plan's convention.
type Model struct {
	imageRepo  string
	target     string
	mapped     bool // RepoConfig.Apps has an entry for imageRepo
	production bool

	// scope owns the ctx every listFn/metaFn/history call this instance ever makes (the
	// commands close over scope.Ctx(), never context.Background() directly). scope.Close is
	// called both explicitly — once the operator leaves this picker for good (Esc, a review
	// selection, a confirmed direct-mode request — the fix for a stale tag load outliving the
	// picker) — and again by the root's own pop when this screen is actually
	// removed from the stack (AGENTS.md §4.8; safe to call more than once). Without it, a load
	// already in flight when the picker closes keeps running to completion in the background
	// even though its eventual result is already discarded by the scope guard in Update — for
	// a mapped repo, ListFunc can walk Forge.Tags through up to 301 sequential GitHub requests,
	// so repeatedly opening and closing pickers could pile up obsolete crawls consuming the API
	// rate limit, or leave one hanging behind a slow request, for no operator-visible reason.
	scope scope.Scope

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

	spinner spinner.Model
	notice  string

	styles        ui.Styles
	keys          keyMap
	width, height int
}

// New builds the tag picker for imageRepo, choosing a tag for target. See Options.
func New(imageRepo, target string, o Options) Model {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return Model{
		imageRepo:          imageRepo,
		target:             target,
		mapped:             o.Mapped,
		production:         o.Production,
		scope:              scope.Open(),
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

// Close cancels this instance's outstanding list/meta/history calls — the same effect as this
// screen's own explicit leave-point cancels (onKey's Back handling, a review selection, a
// confirmed direct-mode request), called instead by the root's own pop the moment this screen is
// actually removed from the stack (AGENTS.md §4.8). Safe alongside those explicit calls:
// scope.Scope.Close is idempotent.
func (m Model) Close() { m.scope.Close() }

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

// Update handles the async loads, the spinner tick, and the screen's own keys.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if scope.Foreign(m.scope.ID, msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case regTagsLoadedMsg:
		return m.onRegTagsLoaded(msg)
	case gitTagsLoadedMsg:
		return m.onGitTagsLoaded(msg)
	case metaLoadedMsg:
		return m.onMetaLoaded(msg)
	case historyMsg:
		m.deltas[msg.tag] = history.State{Loaded: true, Delta: msg.delta, Err: msg.err}
		return m, nil
	case ageMsg:
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

// SetSize lays the screen out to width × height.
func (m Model) SetSize(width, height int) Model {
	m.width, m.height = width, height
	m.filterInput.SetWidth(max(width-14, 10))
	return m
}

// SetStyles applies the palette.
func (m Model) SetStyles(s ui.Styles) Model {
	m.styles = s
	return m
}
