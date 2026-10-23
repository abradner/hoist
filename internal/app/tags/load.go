package tags

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

// regTagsLoadedMsg, gitTagsLoadedMsg, metaLoadedMsg, historyMsg and ageMsg all carry id, the scope ID of the picker
// instance that requested them: each command closes over m.scope.ID at the moment it is created, and
// Update drops (scope.Foreign) a result whose id isn't this model's own current one.
// internal/app's root routes a message to whatever screen is currently on top of its stack by
// type alone, not by which Model instance produced the tea.Cmd that resolves to it — if an
// operator leaves this picker while its own commands are still in flight and opens a picker
// for a different image repo, a stale result landing in the new picker would otherwise
// silently populate it with another repo's rows/metadata (common tag names like "latest" make
// this look plausible rather than obviously wrong).
//
// id, not imageRepo, is what actually discriminates instances: imageRepo scopes only by
// REPOSITORY, and two picker instances for the SAME repo are common — closing this picker while
// its commands are still in flight, then immediately reopening a new picker for the identical
// repo, gives the new instance the identical imageRepo value. id is assigned fresh by scope.Open for
// every instance regardless of repo, so it discriminates same-repo reopens exactly as it
// discriminates cross-repo ones; imageRepo is kept on these messages for context/debugging
// only, not as part of the discard decision.
// regTagsLoadedMsg carries RegTagsFunc's own answer — the fast half of the load (#PR8).
type regTagsLoadedMsg struct {
	imageRepo string
	id        scope.ID
	regTags   []string
	err       error
}

// gitTagsLoadedMsg carries GitTagsFunc's own answer — the slow half, arriving independently
// (#PR8): onRegTagsLoaded and onGitTagsLoaded each handle whichever order the two actually
// land in.
type gitTagsLoadedMsg struct {
	imageRepo string
	id        scope.ID
	gitTags   []forge.GitTag
	mapped    bool // this call's own observed answer — see GitTagsFunc's doc comment.
	err       error
}

type metaLoadedMsg struct {
	imageRepo string
	id        scope.ID
	tag       string
	meta      registry.ImageMeta
	err       error
}

// historyMsg is one tag's delta against the declared reference.
type historyMsg struct {
	id    scope.ID
	tag   string
	delta migrate.Delta
	err   error
}

// ageMsg is the declared occurrence's live age (when its manifest line last changed).
type ageMsg struct {
	id  scope.ID
	age migrate.LineAge
	err error
}

// StampedBy implements scope.Stamped for all five async results: Update drops one whose id is
// not this instance's current scope ID (see Update's first line).
func (m regTagsLoadedMsg) StampedBy() scope.ID { return m.id }
func (m gitTagsLoadedMsg) StampedBy() scope.ID { return m.id }
func (m metaLoadedMsg) StampedBy() scope.ID    { return m.id }
func (m historyMsg) StampedBy() scope.ID       { return m.id }
func (m ageMsg) StampedBy() scope.ID           { return m.id }

func (m Model) regTagsCmd() tea.Cmd {
	regTagsFn := m.regTagsFn
	imageRepo := m.imageRepo
	id := m.scope.ID
	ctx := m.scope.Ctx()
	return func() tea.Msg {
		if regTagsFn == nil {
			// Name imageRepo (not a generic "no registry" message) and point at the config knob that supplies
			// regTagsFn (cmd/hoist wires this from the matching registries[] entry —
			// BuildFunc's own doc comment).
			return regTagsLoadedMsg{imageRepo: imageRepo, id: id, err: fmt.Errorf("no registry configured for %s; add a matching entry under registries[] in the config file", imageRepo)}
		}
		regTags, err := regTagsFn(ctx)
		return regTagsLoadedMsg{imageRepo: imageRepo, id: id, regTags: regTags, err: err}
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
	id := m.scope.ID
	ctx := m.scope.Ctx()
	return func() tea.Msg {
		gitTags, mapped, err := gitTagsFn(ctx)
		return gitTagsLoadedMsg{imageRepo: imageRepo, id: id, gitTags: gitTags, mapped: mapped, err: err}
	}
}

func (m Model) fetchCmd(tag string) tea.Cmd {
	metaFn := m.metaFn
	imageRepo := m.imageRepo
	id := m.scope.ID
	ctx := m.scope.Ctx()
	return func() tea.Msg {
		if metaFn == nil {
			// Same class of fix as loadCmd's listFn==nil case above: name the image repo and
			// tag this call was for, and point at the same registries[] config knob, rather
			// than a bare "no registry configured" the operator can't act on.
			return metaLoadedMsg{imageRepo: imageRepo, id: id, tag: tag, err: fmt.Errorf("no registry configured for %s:%s; add a matching entry under registries[] in the config file", imageRepo, tag)}
		}
		meta, err := metaFn(ctx, tag)
		return metaLoadedMsg{imageRepo: imageRepo, id: id, tag: tag, meta: meta, err: err}
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
	delta, id, ctx := m.histFn.Delta, m.scope.ID, m.scope.Ctx()
	from := m.declared.Ref
	to := image.Ref{Repo: m.imageRepo, Tag: tag}
	return func() tea.Msg {
		d, err := delta(ctx, from, to)
		return historyMsg{id: id, tag: tag, delta: d, err: err}
	}
}

// ageCmd dates the declared occurrence's manifest line, once.
func (m Model) ageCmd() tea.Cmd {
	if m.histFn.LiveAge == nil || m.declared == nil {
		return nil
	}
	liveAge, id, ctx, occ := m.histFn.LiveAge, m.scope.ID, m.scope.Ctx(), m.declared.Occurrence
	return func() tea.Msg {
		age, err := liveAge(ctx, occ)
		return ageMsg{id: id, age: age, err: err}
	}
}

// onRegTagsLoaded (#PR8) renders rows the instant the registry's own tag list answers,
// without waiting for gitTagsCmd's own slower crawl: if gitTags has ALREADY landed by this
// point (onGitTagsLoaded ran first — a genuine possibility, not just a hypothetical, when a
// large registry's own tag list is slower than a small app repo's git-tag crawl), this builds
// the fully-ordered result directly via DeriveRows; otherwise it builds the unmapped-shaped
// rows DeriveRows already produces for that case, and onGitTagsLoaded backfills them once it
// lands.
func (m Model) onRegTagsLoaded(msg regTagsLoadedMsg) (Model, tea.Cmd) {
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
	// lookup this call made still comes back false at runtime — m.mapped must reflect that
	// unconditionally, on
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
	// operator hasn't moved the cursor themselves yet (cursorMoved's own doc comment). And
	// a selection change means nothing renders
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

// reload discards this instance's loaded tags, deltas and age, and re-issues Init's own load
// commands (its r/F5/ctrl+r "reload tags" row). The scope ID is replaced exactly as a fresh
// New would assign one, so any regTagsLoadedMsg/gitTagsLoadedMsg/metaLoadedMsg/historyMsg/ageMsg
// still in flight from before the reload is discarded by the same scope.Foreign guard that
// already protects a closed-and-reopened picker (regTagsLoadedMsg's own doc comment) — reload is
// not a new Model instance, but it needs the identical discrimination for the commands it
// discards without cancelling (scope.Scope has no per-command cancel, only Close for the whole
// instance).
func (m Model) reload() (Model, tea.Cmd) {
	m.scope.ID = scope.New()
	m.state = stateLoading
	m.err = nil
	m.regTags, m.regTagsLoaded = nil, false
	m.gitTags, m.gitTagsLoaded = nil, false
	m.rows = nil
	m.selectedTag = ""
	m.cursorMoved = false
	m.deltas = map[string]history.State{}
	m.age, m.ageErr, m.ageKnown = migrate.LineAge{}, nil, false
	m.notice = ""
	return m, m.Init()
}

// fetchVisible fires MetaFunc for every row within the current visibleWindow that isn't already
// loaded, loading, or settled with an error (a prior MetaErr means this row's
// Config call already failed once — never rescheduled without an explicit re-arm, which does
// not exist — see this function's own loop comment) — AGENTS.md invariant 4's "fetch on
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
		// A row with MetaErr already set is settled, exactly like MetaLoaded: a
		// row whose Config call failed (a missing manifest, an unsupported platform, a
		// temporarily unavailable registry) must not be rescheduled every time fetchVisible
		// runs again (the cursor moving, a filter change, another row's own metadata landing
		// and reordering the list), or it retries unboundedly for as long as the picker stays
		// open and the row remains visible. No explicit retry action exists yet this round
		// (the finding allows either shape) — a failed row simply stays settled, rendered via
		// builtCell/digestCell's own "load failed"/"—" cases, until this Model is torn down
		// and a fresh one (a new scope ID) is opened.
		if m.rows[i].MetaLoaded || m.rows[i].MetaLoading || m.rows[i].MetaErr != nil {
			continue
		}
		m.rows[i].MetaLoading = true
		cmds = append(cmds, m.fetchCmd(r.Tag))
	}
	return m, tea.Batch(cmds...)
}
