package tags

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/migrate"
)

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	// filtering keeps its own Esc handling (clears the filter only, stays on this screen) —
	// checked first, ahead of every other mode.
	if m.filtering {
		return m.updateFilter(msg)
	}
	if m.reading {
		return m.updateReading(msg)
	}
	if key.Matches(msg, m.keys.Back) {
		m.scope.Close() // leaving the picker for good — see the scope field's own doc comment.
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
	case key.Matches(msg, m.keys.Reload):
		return m.reload()
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
	case key.Matches(msg, m.keys.Right):
		if len(m.currentCommits()) == 0 {
			m.notice = "no commit to read here — enter reviews the change"
			return m, nil
		}
		m.reading = true
		m.body.GotoTop()
		return m, nil
	case key.Matches(msg, m.keys.Review):
		return m.selectCurrent()
	case key.Matches(msg, m.keys.Home):
		// the registry has always listed Home/End on this list ("top"/
		// "bottom"), but only updateReading's own commit-detail body handled them — the list
		// itself had no case at all. moveCursor's own clamp (idx<0 -> 0) does the actual "jump
		// to top" work; a delta at least as negative as the current index always lands there.
		if m.focus == focusCommits {
			m.commitIdx = 0
			return m, nil
		}
		return m.moveCursor(-len(m.filtered()))
	case key.Matches(msg, m.keys.End):
		if m.focus == focusCommits {
			m.commitIdx = max(len(m.currentCommits())-1, 0)
			return m, nil
		}
		return m.moveCursor(len(m.filtered()))
	}
	return m, nil
}

// newBodyViewport is the commit-detail viewport with only the paging keys bound: ↑/↓ (and
// j/k) stay the picker's own "next commit", and enter (not space — space is unbound on this
// screen since #120's v2 redesign) stays "review the change" from inside the reader, so the
// viewport's defaults for those (line scroll, page down) are unbound rather than fought over.
func newBodyViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = viewport.KeyMap{
		PageDown:     keys.PgDn.Bubbles(),
		PageUp:       keys.PgUp.Bubbles(),
		HalfPageDown: keys.HalfPageDown.Bubbles(),
		HalfPageUp:   keys.HalfPageUp.Bubbles(),
	}
	v.MouseWheelEnabled = false
	return v
}

// updateReading handles the commit-detail view: ↑/↓ walk the commits (each one read from its
// top), ← (and esc, its equivalent — rule 3: esc is always back) return to the list, and every
// other key scrolls the body — PageUp/PageDown, ctrl+u/ctrl+d for half a page, home/end for the
// ends (replacing g/G, #120).
func (m Model) updateReading(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back), key.Matches(msg, m.keys.Left):
		m.reading = false
	case key.Matches(msg, m.keys.Up):
		m.commitIdx = max(m.commitIdx-1, 0)
		m.body.GotoTop()
	case key.Matches(msg, m.keys.Down):
		m.commitIdx = min(m.commitIdx+1, max(len(m.currentCommits())-1, 0))
		m.body.GotoTop()
	case key.Matches(msg, m.keys.Review):
		m.reading = false
		return m.selectCurrent()
	case key.Matches(msg, m.keys.Home):
		m = m.layoutReading()
		m.body.GotoTop()
	case key.Matches(msg, m.keys.End):
		m = m.layoutReading()
		m.body.GotoBottom()
	default:
		// Laid out on this copy first — View lays out its own, so the retained viewport
		// would otherwise still be the zero-sized one New built (the same gotcha as flight's log).
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
// The root queries this before treating "q" as its own global quit key:
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

// selectCurrent emits SelectedMsg for the cursor row — this screen's one primary action (rule
// 2), reached only by enter. retires the D/direct-commit gesture from this screen
// entirely: reviewing a tag always goes to the deploy confirm screen now, and direct mode is
// offered there instead (internal/app/deploy), after the diff is already on screen.
// SelectedMsg is never emitted until the current row's metadata (digest) has actually loaded —
// a selection without a resolved digest would promote a tag hoist cannot yet pin, which
// AGENTS.md principle 3 refuses at the manifest-write layer anyway; refusing it here just gives
// an earlier, clearer notice.
func (m Model) selectCurrent() (Model, tea.Cmd) {
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
		// unselectable with no operator-visible explanation. Only the absence
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
	m.scope.Close() // leaving the picker for good — see the scope field's own doc comment.
	tag, digest, delta, declared := r.Tag, r.Meta.Digest, m.currentDelta(), m.declared
	since, note := m.declaredSince(), m.historyNote()
	return m, func() tea.Msg {
		return SelectedMsg{ImageRepo: m.imageRepo, Tag: tag, Digest: digest, Target: m.target, Delta: delta, Declared: declared, DeclaredSince: since, HistoryNote: note}
	}
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
			m.commitIdx = 0 // a new tag's commits, so the cursor starts over
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.fetchVisible()
		cmd = tea.Batch(cmd, fetchCmd, m.historyCmd(m.selectedTag))
	}
	return m, cmd
}
