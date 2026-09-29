package matrix

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/flight"
	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui/keys"
)

// menu.go is the action menu enter opens for the cell under the cursor (T3-04, v2·02 mockup):
// what MenuFor lists is pure — a family, an env, the repo's pairs config and what is in
// flight, no terminal dependency — the same split cells.go and grid.go already keep between
// "what to say" and "how to draw it".

// MenuItem is one row of the action menu: a Binding when the item has its own letter (so a
// keypress can run it directly without moving the cursor first), the label shown beside it,
// the message running it emits, and whether the cursor's own cell actually supports it right
// now (a disabled item is still listed — v2·02 shows disabled writes on a production column
// with an "(asks)"-style constraint stated once, never a hidden action the operator wonders
// where it went, AGENTS.md principle 5's own "warn, don't block").
type MenuItem struct {
	B       keys.Binding
	Label   string
	Msg     tea.Msg
	Enabled bool
}

// MenuFor lists what enter can do for the cell (fam, env): promote into env, deploy a tag,
// watch, restart, plus one row per in-flight promotion already targeting env — matched on
// Target alone, since flight.Summary carries no family (a promotion targets an env, not one
// row of it), which is this function's own known imprecision when two families are promoting
// into the same env at once (ticketed in the T3-04/05 report rather than chased here).
func MenuFor(t Table, fam, env string, envs config.EnvsConfig, inflight []flight.Summary) []MenuItem {
	present := cellPresent(t, fam, env)
	items := []MenuItem{promoteItem(env, envs), tagItem(fam, env, present), watchItem(fam, env, present), restartItem(fam, env, present)}
	for _, s := range inflight {
		if s.Target != env || s.Done {
			continue
		}
		items = append(items, MenuItem{
			Label:   fmt.Sprintf("resume in flight %s (%s)", paneID(s), s.Verdict()),
			Msg:     resumeMsgFor(s),
			Enabled: true, // P2-9 (T3 review): a bare MenuItem{} left this false, so runMenuItem always returned nil
		})
	}
	return items
}

func cellPresent(t Table, fam, env string) bool {
	col := -1
	for i, e := range t.Envs {
		if e == env {
			col = i
		}
	}
	if col < 0 {
		return false
	}
	for _, r := range t.Rows {
		if r.Family == fam {
			return col < len(r.Cells) && r.Cells[col].Present
		}
	}
	return false
}

func promoteItem(env string, envs config.EnvsConfig) MenuItem {
	srcs := envs.SourcesOf(env)
	label := fmt.Sprintf("promote into %s from…", env)
	if len(srcs) == 1 {
		label = fmt.Sprintf("promote into %s from %s", env, srcs[0])
	}
	source := ""
	if len(srcs) == 1 {
		source = srcs[0]
	}
	return MenuItem{B: keys.Promote, Label: label, Msg: OpenPlanMsg{Source: source, Target: env}, Enabled: env != ""}
}

func tagItem(fam, env string, present bool) MenuItem {
	return MenuItem{B: keys.Tag, Label: fmt.Sprintf("deploy a tag to %s", env), Msg: openTagsMenuMsg{fam, env}, Enabled: present}
}

func watchItem(fam, env string, present bool) MenuItem {
	return MenuItem{B: keys.Watch, Label: "watch the rollout", Msg: OpenWatchMsg{Family: fam, Target: env}, Enabled: present}
}

func restartItem(fam, env string, present bool) MenuItem {
	return MenuItem{B: keys.Restart, Label: fmt.Sprintf("restart %s in %s", fam, env), Msg: OpenRestartMsg{Family: fam, Target: env}, Enabled: present}
}

// openTagsMenuMsg is the menu's own placeholder for "deploy a tag": the real OpenTagsMsg needs
// an image repo, which may take a chooser first when the cell runs more than one — the menu
// hands off to the same openChooser/OpenTagsMsg path the retired t-on-the-grid gesture used,
// rather than duplicating that branch here.
type openTagsMenuMsg struct{ Family, Env string }

// menuView renders the action menu's own body (v2·02): each item's key (blank for one with
// none), its label, a cursor marker on the highlighted row, and — for a production target — a
// closing sentence naming why every write there asks first (AGENTS.md §4.5).
func (m Model) menuView() string {
	var lines []string
	for i, item := range m.menuItems {
		marker := "  "
		if i == m.menuCursor {
			marker = selectedMarker
		}
		key := ""
		if item.B.Name != "" {
			key = item.B.Show
		}
		line := fmt.Sprintf("%s%-8s %s", marker, key, item.Label)
		if !item.Enabled {
			line = m.styles.Dim.Render(line)
		} else if i == m.menuCursor {
			line = m.styles.Accent.Render(line)
		}
		lines = append(lines, line)
	}
	if m.IsProduction(m.menuEnv) {
		// P2-13 (T3 review): the longer wording truncated at 80 columns ("`hoist appro…"),
		// hiding the command name a reviewer would actually type.
		lines = append(lines, "", m.styles.Production.Render("⚠ production: every write opens a PR, gated on `hoist approve`"))
	}
	return strings.Join(lines, "\n")
}

func resumeMsgFor(s flight.Summary) tea.Msg {
	if s.ID == "" {
		return ResumeMsg{Build: s.Build}
	}
	return ResumeMsg{ID: s.ID}
}
