// This file is repo and env selection: the env/source selects, the repo multi-select, and the cursor over it.
// Moved out of model.go unchanged; see there for the Model itself.

package plan

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/ui/keys"
)

func (m *Model) buildEnvSelect() {
	candidates := TargetsFor(m.repo, m.source)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote %s to…", m.source)).Value(&m.target)
	// Wires Down/Up/"/" filtering the same way a huh.Form/Group would, without adopting either
	// (AGENTS.md §4.7 — this is component wiring, not layout). See CapturesText's own doc
	// comment for why this call has to happen here rather than being left to a Form/Group.
	sel.WithKeyMap(keys.HuhKeyMap())
	if len(candidates) > 0 {
		opts := make([]huh.Option[string], 0, len(candidates))
		for _, e := range candidates {
			opts = append(opts, huh.NewOption(e, e))
		}
		sel = sel.Options(opts...)
	} else {
		// (UX-M18's own convention, matrix's emptyView): name what to check, not just
		// what failed — hoist discovers envs from Argo CD Application wrappers under the apps
		// root, so a repo with only one is either genuinely single-env or looking in the wrong
		// place.
		m.err = fmt.Errorf("no other env to promote %s to — check repos[].envs.pairs and apps_root in the config, or --apps-root/--repo if this looked in the wrong place", m.source)
	}
	m.envSelect = sel
}

// buildSourceSelect is buildEnvSelect's mirror for the screen's own "promote into <target>
// from…": every other discovered env is a candidate source, bound to m.source instead of
// m.target.
func (m *Model) buildSourceSelect() {
	candidates := SourcesFor(m.repo, m.target)
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("promote into %s from…", m.target)).Value(&m.source)
	sel.WithKeyMap(keys.HuhKeyMap())
	if len(candidates) > 0 {
		opts := make([]huh.Option[string], 0, len(candidates))
		for _, e := range candidates {
			opts = append(opts, huh.NewOption(e, e))
		}
		sel = sel.Options(opts...)
	} else {
		m.err = fmt.Errorf("no other env to promote into %s from — check repos[].envs.pairs and apps_root in the config, or --apps-root/--repo if this looked in the wrong place", m.target)
	}
	m.envSelect = sel
}

// labelWidth is what a left-pane option may take before huh's own "> ✓ " marker.
func (m Model) labelWidth() int {
	if m.leftWidth <= 0 {
		return 34
	}
	return max(m.leftWidth-6, 12)
}

func (m *Model) buildMultiSelect() {
	sel := Selectable(m.rows)
	m.ticked = make([]string, 0, len(sel))
	for _, r := range sel {
		m.ticked = append(m.ticked, r.Repo)
	}
	m.rebuildMultiSelect()
}

// rebuildMultiSelect builds the left pane's field for the current width, keeping the ticked
// set (bound through Value) and the cursor — called at load and again when the pane's width
// changes, since the labels are truncated to it and huh would otherwise wrap them.
//
// The cursor has to be put back by hand (#121): huh v2.0.3's MultiSelect exposes the cursor
// only through Hovered(), and re-setting Options on the existing widget is no better than a
// rebuild — Options calls selectOptions, which parks the cursor on the first ticked option.
// So the hovered index is read before the rebuild and walked back to with Up/Down presses
// through the widget's own Update (GotoTop is disabled in huh's default keymap, so there is
// no shorter route) — the same keys an operator would press, never a private field.
func (m *Model) rebuildMultiSelect() {
	hovered := m.hoveredIndex()
	sel := Selectable(m.rows)
	labels := Labels(sel, m.prefix, m.labelWidth())
	opts := make([]huh.Option[string], 0, len(sel))
	ticked := map[string]bool{}
	for _, t := range m.ticked {
		ticked[t] = true
	}
	for i, r := range sel {
		opts = append(opts, huh.NewOption(labels[i], r.Repo).Selected(ticked[r.Repo]))
	}
	ms := huh.NewMultiSelect[string]().Value(&m.ticked)
	// Same wiring as buildEnvSelect's own WithKeyMap call — see CapturesText's doc comment.
	// This is what makes Down/space("x" retired)/"/" actually reach the field's Update.
	ms.WithKeyMap(keys.HuhKeyMap())
	if len(opts) > 0 {
		ms = ms.Options(opts...)
	}
	ms.Focus()
	m.multiSelect = ms
	if hovered >= 0 {
		m.moveCursorTo(hovered)
	}
}

// withNewlySelectable is ticked narrowed to the rows still selectable, plus every row that is
// selectable now and was not before: an explicit untick survives, a newly offered row is ticked.
// In row order.
func withNewlySelectable(ticked []string, before, now []Row) []string {
	was := make(map[string]bool, len(before))
	for _, r := range before {
		was[r.Repo] = true
	}
	set := make(map[string]bool, len(ticked))
	for _, t := range ticked {
		set[t] = true
	}
	out := make([]string, 0, len(now))
	for _, r := range now {
		if set[r.Repo] || !was[r.Repo] {
			out = append(out, r.Repo)
		}
	}
	return out
}

// hoveredIndex is the position of the hovered row among the selectable rows, -1 when there
// is no field or no row under the cursor.
func (m Model) hoveredIndex() int {
	if m.multiSelect == nil {
		return -1
	}
	repo, ok := m.multiSelect.Hovered()
	if !ok {
		return -1
	}
	for i, r := range Selectable(m.rows) {
		if r.Repo == repo {
			return i
		}
	}
	return -1
}

// moveCursorTo walks the field's cursor to index i with Up/Down presses; bounded by the row
// count so a stale index can never loop.
func (m *Model) moveCursorTo(i int) {
	n := len(Selectable(m.rows))
	for step := 0; step < n; step++ {
		at := m.hoveredIndex()
		if at < 0 || at == i {
			return
		}
		code := tea.KeyDown
		if at > i {
			code = tea.KeyUp
		}
		m.multiSelect.Update(tea.KeyPressMsg{Code: code})
	}
}

// filtering reports whether the currently-active huh field has its own filter box open (the
// operator typed "/"), for the field whichever state the screen is in owns. While it is true,
// every key — esc included — belongs to the widget, never to this screen's own key handling:
// a probe found "/" then "r" firing plan.RefreshMsg, "/" then enter
// firing plan.StartMsg (starting the promotion), and esc popping the whole screen instead of
// just closing the filter.
func (m Model) filtering() bool {
	switch m.state {
	case stateSelectEnv:
		return m.envSelect != nil && m.envSelect.GetFiltering()
	case stateReady:
		return m.multiSelect != nil && m.multiSelect.GetFiltering()
	default:
		return false
	}
}

func (m Model) updateSelectEnv(msg tea.Msg) (Model, tea.Cmd) {
	// While the select's own filter is open (the operator typed "/"), every key —
	// including enter — belongs to the widget's filter box, never to this screen's own
	// enter-confirms-the-choice handling below. Without this check, enter while filtering
	// could confirm the still-highlighted pre-filter option instead of accepting the filter
	// text, and esc would fall through to huh's own Update, which on a Select swallows esc
	// silently rather than closing the screen — so forwarding it here is also what keeps a
	// dialog-level esc from being mistaken for "no-op" instead of "closes the filter".
	if m.envSelect.GetFiltering() {
		_, cmd := m.envSelect.Update(msg)
		if v, ok := m.envSelect.GetValue().(string); ok {
			if m.selectingSource {
				m.source = v
			} else {
				m.target = v
			}
		}
		return m, cmd
	}
	if kmsg, ok := msg.(tea.KeyPressMsg); ok && kmsg.String() == "enter" {
		if m.selectingSource {
			if m.source == "" {
				return m, nil
			}
		} else if m.target == "" {
			return m, nil
		}
		m.state = stateLoading
		m.status = fmt.Sprintf("resolving digests from %s pods…", m.source)
		return m, m.Init()
	}
	_, cmd := m.envSelect.Update(msg)
	// Re-read the field this select is bound to from the widget itself rather than trusting
	// buildEnvSelect's/buildSourceSelect's own Value(&m.target)/Value(&m.source) binding to
	// have kept it current: this Model is a value passed by copy through every Update in the
	// chain, so the pointer that binding captured addresses a Model snapshot that stopped
	// being "the" model the instant New returned. Without this resync, Down could move the
	// highlighted option while the field silently stayed pinned to whichever option
	// construction time happened to default to.
	if v, ok := m.envSelect.GetValue().(string); ok {
		if m.selectingSource {
			m.source = v
		} else {
			m.target = v
		}
	}
	return m, cmd
}

// updateMultiSelect forwards msg to the multi-select field unconditionally and resyncs
// m.ticked/the diff from it — the tail of updateReady's own handling, factored out so the
// filtering short-circuit above can reach it directly without falling through the switch that
// owns r/d/e/enter/tab when the filter isn't open.
func (m Model) updateMultiSelect(msg tea.Msg) (Model, tea.Cmd) {
	if m.multiSelect == nil {
		return m, nil
	}
	before := append([]string(nil), m.ticked...)
	hoveredBefore, _ := m.multiSelect.Hovered()
	_, cmd := m.multiSelect.Update(msg)
	// Re-read m.ticked from the field itself — see updateSelectEnv's own resync comment for
	// why buildMultiSelect's Value(&m.ticked) binding alone can't keep this Model's own copy
	// current across Toggle presses (space/x), only View()'s rendering (which reads the
	// field's own internal option state directly, not m.ticked).
	if v, ok := m.multiSelect.GetValue().([]string); ok {
		m.ticked = v
	}
	if !equalSets(before, m.ticked) {
		m = m.recomputeDiff()
	}
	if hovered, _ := m.multiSelect.Hovered(); hovered != hoveredBefore || !equalSets(before, m.ticked) {
		m = m.refreshRight()
	}
	return m, cmd
}
