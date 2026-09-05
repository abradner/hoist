package deploy

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

func fixture(t *testing.T, envs config.EnvsConfig) Model {
	t.Helper()
	// The fixture repo's app-production is the only env with a web occurrence to deploy into;
	// named here rather than passed so the call sites read as what they vary — the env config.
	const target = "app-production"
	r, err := gitops.Discover("../../../testdata/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	ref := image.Ref{
		Repo:   "ghcr.io/example/web",
		Tag:    "v9",
		Digest: "sha256:" + strings.Repeat("d", 64),
	}
	pl, err := gitops.BuildDeployPlan(r, target, ref, []string{"ghcr.io/example/"})
	if err != nil {
		t.Fatal(err)
	}
	m := New(pl, r.Root, ref.String(), envs, ui.NewStyles(true))
	return m.SetSize(120, 40)
}

// The screen's whole reason to exist: the operator sees the bytes before anything is written.
func TestViewShowsTheDecisionAndTheDiff(t *testing.T) {
	v := fixture(t, config.EnvsConfig{}).View()
	for _, want := range []string{"ghcr.io/example/web:v9", "app-production", "mode: PR", "occurrence", "image:"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
}

// enter confirms, and says what it confirmed.
func TestEnterEmitsStartMsgInPRModeByDefault(t *testing.T) {
	m := fixture(t, config.EnvsConfig{})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	msg, ok := cmd().(StartMsg)
	if !ok {
		t.Fatalf("enter produced %T, want StartMsg", cmd())
	}
	if msg.Mode != ModePR {
		t.Errorf("Mode = %q, want the PR default", msg.Mode)
	}
	if msg.Confirmed {
		t.Error("Confirmed must be false for a PR-mode deploy: nothing gated it")
	}
	if msg.Target != "app-production" || !msg.Plan.IsDeploy() {
		t.Errorf("StartMsg should carry the deploy plan and its target: %+v", msg)
	}
}

// §4.5: production always opens a PR. The key refuses with the reason rather than doing
// nothing, which reads as a broken keybinding.
func TestModeKeyRefusesDirectOnAProductionTarget(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"app-production"}}
	m := fixture(t, envs)
	m, _ = m.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	if m.mode != ModePR {
		t.Errorf("mode = %q, want it held at PR for a production target", m.mode)
	}
	if m.confirm != nil {
		t.Error("production must not even open the direct-mode confirmation")
	}
	if !strings.Contains(m.View(), "always open a PR") {
		t.Errorf("view should say why m did nothing:\n%s", m.View())
	}
}

// WithDirectMode is the picker's D path, whose gesture already happened. It must not bypass
// the production rule.
func TestWithDirectModeHonoursTheProductionRule(t *testing.T) {
	nonProd := fixture(t, config.EnvsConfig{}).WithDirectMode()
	if nonProd.mode != ModeDirect {
		t.Errorf("mode = %q, want direct: the picker's gesture already ran", nonProd.mode)
	}
	prod := fixture(t, config.EnvsConfig{Production: []string{"app-production"}}).WithDirectMode()
	if prod.mode != ModePR {
		t.Errorf("mode = %q, want PR: production has no direct path (§4.5)", prod.mode)
	}
	if !strings.Contains(prod.View(), "always open a PR") {
		t.Errorf("view should say why direct mode was dropped:\n%s", prod.View())
	}
}

// A direct-mode confirm carries Confirmed, which is what engine.DirectCommitGateStep reads as
// the record of the operator's gesture.
func TestDirectModeConfirmCarriesConfirmed(t *testing.T) {
	m := fixture(t, config.EnvsConfig{}).WithDirectMode()
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	msg := cmd().(StartMsg)
	if msg.Mode != ModeDirect || !msg.Confirmed {
		t.Errorf("Mode=%q Confirmed=%v, want direct and confirmed", msg.Mode, msg.Confirmed)
	}
}

func TestEscGoesBack(t *testing.T) {
	m := fixture(t, config.EnvsConfig{})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Errorf("esc produced %T, want BackMsg", cmd())
	}
}

// TestEnterRefusesWhenTheDiffCouldNotBeRendered is the screen's own promise, enforced: the
// deploy confirm exists so the bytes are visible before anything is written, and enter used to
// emit StartMsg regardless of whether RenderDiff had failed — confirming a write against
// something the operator was never shown (Copilot, PR #72).
func TestEnterRefusesWhenTheDiffCouldNotBeRendered(t *testing.T) {
	m := fixture(t, config.EnvsConfig{})
	m.diffErr = errors.New("reading cluster/apps/app-production/web/deployment.yaml: permission denied")

	m2, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter produced a command despite an unrendered diff: %v", cmd())
	}
	v := m2.View()
	if !strings.Contains(v, "cannot confirm") {
		t.Errorf("the refusal should say why:\n%s", v)
	}
	// And the same key still works once there is a diff to look at, so this is a gate, not a
	// screen that never confirms.
	if _, c := fixture(t, config.EnvsConfig{}).Update(tea.KeyPressMsg{Code: tea.KeyEnter}); c == nil {
		t.Error("enter on a rendered diff must still confirm")
	}
}
