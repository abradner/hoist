// This file is the screen's two dialogs: the direct-mode confirmation and the digest-override prompt.
// Moved out of model.go unchanged; see there for the Model itself.

package plan

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/confirm"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

// toggleDirect is shift+d (was m): behind a confirmation only when turning direct mode
// ON — turning it back off is silent, mirroring deploy.Model's own toggleDirect and the
// approved keymap's own "confirm when turning on" wording (the retired m gesture had confirmed
// both directions, which the keymap never asked for). Never offered at all for a production
// target: hidden from the footer/help (hints' own registry row) and, pressed anyway, a silent
// no-op here too — politeness only (rule 5's note); the real, unbypassable enforcement is
// engine.DirectCommitGateStep, which refuses a production env regardless of what this screen
// believed.
func (m Model) toggleDirect() (Model, tea.Cmd) {
	if m.envs.IsProduction(m.target) {
		return m, nil
	}
	if m.mode == ModeDirect {
		m.mode = ModePR
		return m, nil
	}
	m.confirming = true
	m.buildConfirm()
	return m, tea.Batch(m.confirmDirect.Init(), m.confirmDirect.Focus())
}

// buildConfirm builds the direct-mode dialog, only ever reached (via toggleDirect above) while
// turning direct mode on, so the verb is fixed. WithKeyMap is not decoration: huh.NewConfirm
// leaves its keymap zero-valued, and a zero key.Binding matches nothing, so a standalone
// Confirm ignored y, n and the arrows — this screen's m gesture could not be completed by a
// real operator until M10, and its test had set the bound bool directly (#85's named trap;
// internal/app/tags and internal/app/deploy had already been fixed the same way).
func (m *Model) buildConfirm() {
	m.confirmDirect = confirm.New("Switch to direct mode (commit straight to the default branch, no PR)?", m.width)
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		switch kmsg.String() {
		case "esc":
			m.confirming = false
			return m, nil
		case "enter":
			m.confirming = false
			// Read from the widget, never from a bound bool: Value(&field) captures a
			// field on the Model copy that built the widget, which every Update since has
			// superseded.
			if m.confirmAgreed() {
				m.mode = ModeDirect
			}
			return m, nil
		}
	}
	f, cmd := m.confirmDirect.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirmDirect = c
	}
	return m, cmd
}

// buildOverride builds the o dialog for repo: a huh.Input pre-filled with "<repo>=" so the
// operator types only the reference, in the exact form `--digest` takes. WithKeyMap for the
// same reason buildConfirm gives (§9 entry 6); the answer is read back with GetValue.
func (m *Model) buildOverride(repo string) {
	m.overrideRepo = repo
	m.overrideErr = ""
	m.overrideValue = repo + "="
	if ov, ok := m.overrides[repo]; ok {
		m.overrideValue = repo + "=" + ov.String()
	}
	m.overrideInput = huh.NewInput().
		Title("override the digest for " + repo).
		Description("repo=repo:tag@sha256:<digest> — wins over every digest source, as --digest does").
		Value(&m.overrideValue)
	m.overrideInput.WithKeyMap(keys.HuhKeyMap())
	m.overrideInput.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	m.overrideInput.WithWidth(ui.DialogWidth(m.width))
}

// updateOverride drives the o dialog: esc closes it with nothing changed; enter validates
// the text through ValidateOverride and, when it holds, records the override and rebuilds
// the plan through resolveFn with it in place — the same load path the screen opened with —
// or, when it does not, shows the refusal in the dialog and keeps it open. Enter is handled
// here rather than left to huh's own Submit: a standalone Input's Submit only emits
// NextField, which nothing on this screen listens for.
func (m Model) updateOverride(msg tea.Msg) (Model, tea.Cmd) {
	if kmsg, ok := msg.(tea.KeyPressMsg); ok {
		m.overrideErr = ""
		switch kmsg.String() {
		case "esc":
			m.overriding = false
			return m, nil
		case "enter":
			text, _ := m.overrideInput.GetValue().(string)
			ref, err := ValidateOverride(m.overrideRepo, text)
			if err != nil {
				// Redacted here as well as at View's boundary: the operator may paste a
				// credential-bearing string by accident, and the refusal echoes what was typed.
				m.overrideErr = redact.Strings(err.Error())
				return m, nil
			}
			m.overriding = false
			m.overrides[ref.Repo] = ref
			m.keepTicked = true
			// Only the ID changes: a load/delta still outstanding for the pre-override plan
			// must not land on the new rows, but this is one screen instance reloading itself
			// in place, not a new one — its ctx (and so its Close-on-pop lifetime, owned by
			// the root, AGENTS.md §4.8) stays exactly what it was for this screen's whole life.
			m.scope.ID = scope.New()
			m.state = stateLoading
			m.status = fmt.Sprintf("re-resolving digests from %s pods with the override…", m.source)
			m.showYAML = false
			return m, tea.Batch(m.spinner.Tick, m.loadCmd())
		}
	}
	f, cmd := m.overrideInput.Update(msg)
	if in, ok := f.(*huh.Input); ok {
		m.overrideInput = in
	}
	return m, cmd
}

// overrideBody is the dialog's content: the input, and under it the last refusal.
func (m Model) overrideBody() string {
	body := m.overrideInput.View()
	if m.overrideErr != "" {
		body += "\n" + m.styles.Bad.Render(ansi.Wordwrap(m.overrideErr, ui.DialogWidth(m.width), ""))
	}
	return body
}

func (m Model) confirmAgreed() bool {
	if m.confirmDirect == nil {
		return false
	}
	return confirm.Agreed(m.confirmDirect)
}
