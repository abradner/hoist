package matrix

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/pkg/gitops"
)

// TestColumnCursor exercises Left/Right moving CurrentEnv over fixture()'s three envs
// (a, b, c — sorted), clamped at both ends.
func TestColumnCursor(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"})
	if got := m.CurrentEnv(); got != "a" {
		t.Fatalf("initial CurrentEnv() = %q, want a", got)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.CurrentEnv(); got != "a" {
		t.Errorf("Left at the first column: CurrentEnv() = %q, want a (clamped)", got)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := m.CurrentEnv(); got != "b" {
		t.Errorf("after Right: CurrentEnv() = %q, want b", got)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := m.CurrentEnv(); got != "c" {
		t.Errorf("after Right, Right: CurrentEnv() = %q, want c", got)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got := m.CurrentEnv(); got != "c" {
		t.Errorf("Right past the last column: CurrentEnv() = %q, want c (clamped)", got)
	}
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.CurrentEnv(); got != "b" {
		t.Errorf("after Left: CurrentEnv() = %q, want b", got)
	}
}

// TestPromoteOpensPlanForCurrentEnv: p emits OpenPlanMsg naming CurrentEnv, unforced; P
// (PromoteAs) emits the same with Force set — the matrix screen's half of "p opens the plan
// screen for the configured pair, P always prompts" (issue #2's second half).
func TestPromoteOpensPlanForCurrentEnv(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // CurrentEnv now "b"

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p produced no command")
	}
	msg, ok := cmd().(OpenPlanMsg)
	if !ok {
		t.Fatalf("p's command yields %T, want OpenPlanMsg", cmd())
	}
	if msg.Source != "b" || msg.Force {
		t.Errorf("p: OpenPlanMsg = %+v, want {Source: b, Force: false}", msg)
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
	if cmd == nil {
		t.Fatal("P produced no command")
	}
	msg, ok = cmd().(OpenPlanMsg)
	if !ok {
		t.Fatalf("P's command yields %T, want OpenPlanMsg", cmd())
	}
	if msg.Source != "b" || !msg.Force {
		t.Errorf("P: OpenPlanMsg = %+v, want {Source: b, Force: true}", msg)
	}
}

// TestDeployNewOpensTagsForCurrentCell: d (deploy new image) emits OpenTagsMsg naming the
// current cell's first-party image repo and CurrentEnv (M6's tag picker).
func TestDeployNewOpensTagsForCurrentCell(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"})
	// Default cursor: row 0 (families sort to "absent" first), col 0 (envs sort to "a" first).
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd == nil {
		t.Fatal("d produced no command")
	}
	msg, ok := cmd().(OpenTagsMsg)
	if !ok {
		t.Fatalf("d's command yields %T, want OpenTagsMsg", cmd())
	}
	if msg.Target != "a" || msg.ImageRepo != "ghcr.io/x/app" {
		t.Fatalf("OpenTagsMsg = %+v, want {ImageRepo: ghcr.io/x/app, Target: a}", msg)
	}
}

// TestDeployNewWithNoFirstPartyImageShowsNotice: the "thirdparty" family has no first-party
// image at all — d must not emit OpenTagsMsg{ImageRepo: ""} (which the root would otherwise
// hand to the tag picker as a real, empty image repo).
func TestDeployNewWithNoFirstPartyImageShowsNotice(t *testing.T) {
	m := New(fixture(), []string{"ghcr.io/"})
	m = m.SetSize(80, 24)
	// Move down to "thirdparty": absent, drift, empty, mixedtags, multi, pinned, sidecar,
	// thirdparty — 7 rows down from "absent".
	for i := 0; i < 7; i++ {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	if cmd != nil {
		t.Fatalf("d on a third-party-only cell produced a command: %v", cmd())
	}
	if !strings.Contains(m2.View(), "no first-party image") {
		t.Errorf("d with no first-party image: View() lacks the notice:\n%s", m2.View())
	}
}

// TestCurrentEnvEmptyRepo: a repo with no envs at all must not panic or index out of range.
func TestCurrentEnvEmptyRepo(t *testing.T) {
	m := New(&gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{}}, []string{"ghcr.io/"})
	if got := m.CurrentEnv(); got != "" {
		t.Errorf("CurrentEnv() on an empty repo = %q, want empty", got)
	}
}

// F8 regression: with zero envs, CurrentEnv() is "" — p/P must not emit OpenPlanMsg{Source:
// ""}, which the root would otherwise hand to the plan screen as a real (empty) source env.
// Both keys should instead surface a status-bar notice and produce no command.
func TestPromoteWithNoEnvsShowsNoticeInsteadOfOpeningPlan(t *testing.T) {
	m := New(&gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{}}, []string{"ghcr.io/"})
	m = m.SetSize(80, 24)

	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd != nil {
		t.Errorf("p with no envs produced a command: %v", cmd())
	}
	if !strings.Contains(m2.View(), "no environments discovered") {
		t.Errorf("p with no envs: View() lacks the notice:\n%s", m2.View())
	}

	m3, cmd := m.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
	if cmd != nil {
		t.Errorf("P with no envs produced a command: %v", cmd())
	}
	if !strings.Contains(m3.View(), "no environments discovered") {
		t.Errorf("P with no envs: View() lacks the notice:\n%s", m3.View())
	}
}
