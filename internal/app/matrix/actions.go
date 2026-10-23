package matrix

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/ui/confirm"
	"github.com/abradner/hoist/internal/ui/keys"
)

// chooserKind names what the dialog under m.chooser is choosing between.
type chooserKind int

const (
	chooserImage  chooserKind = iota // t on a cell with several first-party images
	chooserOpenPR                    // o with several in flight that have PRs
)

// doPromote is p: promote into the cursor column. Exactly one reverse pair uses it directly;
// otherwise the plan screen itself asks which source (Source left empty).
func (m Model) doPromote() (Model, tea.Cmd) {
	target := m.CurrentEnv()
	if target == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	srcs := m.envs.SourcesOf(target)
	source := ""
	if len(srcs) == 1 {
		source = srcs[0]
	}
	return m, func() tea.Msg { return OpenPlanMsg{Source: source, Target: target} }
}

func (m Model) doRestart() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	family := m.CurrentFamily()
	if family == "" {
		m.notice = "no family under the cursor"
		return m, nil
	}
	return m, func() tea.Msg { return OpenRestartMsg{Family: family, Target: env} }
}

func (m Model) doWatch() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	family := m.CurrentFamily()
	if family == "" {
		m.notice = "no family under the cursor"
		return m, nil
	}
	return m, func() tea.Msg { return OpenWatchMsg{Family: family, Target: env} }
}

func (m Model) doOpenPR() (Model, tea.Cmd) {
	withPR := m.withPR()
	switch len(withPR) {
	case 0:
		m.notice = "nothing in flight has a PR to open"
		return m, nil
	case 1:
		url := withPR[0].PR.URL
		return m, func() tea.Msg { return flight.OpenPRMsg{URL: url} }
	default:
		return m.openOpenPRChooser(withPR)
	}
}

// doResumeFromPane is enter while focus is on the in-flight pane: resume/re-attach to the
// promotion under the pane's own cursor.
func (m Model) doResumeFromPane() (Model, tea.Cmd) {
	if m.paneCursor < 0 || m.paneCursor >= len(m.inflight) {
		return m, nil
	}
	s := m.inflight[m.paneCursor]
	return m, func() tea.Msg { return resumeMsgFor(s) }
}

// doAbandonFromPane is shift+x on the pane: abandon the promotion under the pane's cursor,
// behind a huh.Confirm — an abandon is destructive (it closes the PR and deletes the branch),
// so it never fires on the bare keypress.
func (m Model) doAbandonFromPane() (Model, tea.Cmd) {
	if m.focus != FocusPane || m.paneCursor < 0 || m.paneCursor >= len(m.inflight) {
		return m, nil
	}
	s := m.inflight[m.paneCursor]
	cf := confirm.New(fmt.Sprintf("abandon %s (%s → %s)?", paneID(s), s.Source, s.Target), 0).Affirmative("abandon").Negative("cancel")
	m.confirmAbandon = cf
	m.confirmAbandonID = s.ID
	m.confirmAbandonBuild = s
	return m, tea.Batch(cf.Init(), cf.Focus())
}

func (m Model) updateConfirmAbandon(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	_, cmd := m.confirmAbandon.Update(msg)
	if msg.String() == "esc" {
		m.confirmAbandon = nil
		return m, nil
	}
	if msg.String() != "enter" {
		return m, cmd
	}
	yes, _ := m.confirmAbandon.GetValue().(bool) // GetValue, never a captured field (§9 entry 6)
	s := m.confirmAbandonBuild
	m.confirmAbandon = nil
	if !yes {
		return m, nil
	}
	return m, func() tea.Msg { return flight.AbandonMsg{ID: s.ID} }
}

// doTag is t: deploy a new tag into the cursor cell. Mirrors the retired d gesture exactly.
func (m Model) doTag() (Model, tea.Cmd) {
	env := m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	repos := m.currentImageRepos(env)
	switch len(repos) {
	case 0:
		m.notice = "no first-party image in this cell"
		return m, nil
	case 1:
		repo := repos[0]
		return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: repo, Target: env} }
	default:
		return m.openChooser(env, repos)
	}
}

// openMenu opens the action menu for the cell under the cursor (enter).
func (m Model) openMenu() (Model, tea.Cmd) {
	fam, env := m.CurrentFamily(), m.CurrentEnv()
	if env == "" {
		m.notice = "no environments discovered"
		return m, nil
	}
	m.menuItems = MenuFor(m.matrix, fam, env, m.envs, m.inflight)
	m.menuCursor = 0
	m.menuFamily, m.menuEnv = fam, env
	m.menuOpen = true
	return m, nil
}

func (m Model) updateMenu(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case keys.Esc.Matches(msg):
		m.menuOpen = false
		return m, nil
	case keys.Up.Matches(msg):
		m.menuCursor = clamp(m.menuCursor-1, max(len(m.menuItems)-1, 0))
		return m, nil
	case keys.Down.Matches(msg):
		m.menuCursor = clamp(m.menuCursor+1, max(len(m.menuItems)-1, 0))
		return m, nil
	case keys.Enter.Matches(msg):
		if m.menuCursor < 0 || m.menuCursor >= len(m.menuItems) {
			m.menuOpen = false
			return m, nil
		}
		return m.runMenuItem(m.menuItems[m.menuCursor])
	}
	// A letter that matches one of the listed items runs it directly, without moving the
	// cursor there first (v2·02: "the letter runs it directly").
	for _, item := range m.menuItems {
		if item.B.Name != "" && item.B.Matches(msg) {
			return m.runMenuItem(item)
		}
	}
	return m, nil
}

func (m Model) runMenuItem(item MenuItem) (Model, tea.Cmd) {
	m.menuOpen = false
	if !item.Enabled {
		return m, nil
	}
	if openMsg, ok := item.Msg.(openTagsMenuMsg); ok {
		repos := m.currentImageReposFor(openMsg.Family, openMsg.Env)
		switch len(repos) {
		case 0:
			m.notice = "no first-party image in this cell"
			return m, nil
		case 1:
			repo := repos[0]
			return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: repo, Target: openMsg.Env} }
		default:
			return m.openChooser(openMsg.Env, repos)
		}
	}
	msg := item.Msg
	return m, func() tea.Msg { return msg }
}

// openChooser asks which of several first-party images t meant, as a dialog over the matrix.
func (m Model) openChooser(env string, repos []string) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(repos))
	for _, r := range repos {
		opts = append(opts, huh.NewOption(r, r))
	}
	sel := huh.NewSelect[string]().Title(fmt.Sprintf("deploy which image in %s?", env)).Options(opts...)
	sel.WithKeyMap(keys.HuhKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = env
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func chooserKey(s flight.Summary) string {
	if s.ID != "" {
		return s.ID
	}
	return fmt.Sprintf("build:%d", s.Build)
}

// openOpenPRChooser asks which of several in-flight promotions o meant. It is the only caller
// of the in-flight chooser, so there is no other kind for it to be parameterised over.
func (m Model) openOpenPRChooser(from []flight.Summary) (Model, tea.Cmd) {
	opts := make([]huh.Option[string], 0, len(from))
	for _, s := range from {
		opts = append(opts, huh.NewOption(paneID(s)+"  "+pair(s)+"  "+s.Verdict(), chooserKey(s)))
	}
	sel := huh.NewSelect[string]().Title("open which promotion's PR?").Options(opts...)
	sel.WithKeyMap(keys.HuhKeyMap())
	sel.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.chooser = sel
	m.chooserTarget = ""
	m.chooserKind = chooserOpenPR
	return m, tea.Batch(sel.Init(), sel.Focus())
}

func (m Model) withPR() []flight.Summary {
	var out []flight.Summary
	for _, s := range m.inflight {
		if s.PR != nil && s.PR.URL != "" {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) updateChooser(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.chooser, m.chooserKind = nil, chooserImage
		return m, nil
	case "enter":
		if m.chooser.GetFiltering() {
			break
		}
		choice, _ := m.chooser.GetValue().(string)
		target, kind := m.chooserTarget, m.chooserKind
		m.chooser, m.chooserKind = nil, chooserImage
		if choice == "" {
			return m, nil
		}
		if kind == chooserOpenPR {
			for _, s := range m.withPR() {
				if s.ID == choice {
					url := s.PR.URL
					return m, func() tea.Msg { return flight.OpenPRMsg{URL: url} }
				}
			}
			m.notice = "that promotion no longer has a PR to open"
			return m, nil
		}
		return m, func() tea.Msg { return OpenTagsMsg{ImageRepo: choice, Target: target} }
	}
	_, cmd := m.chooser.Update(msg)
	return m, cmd
}
