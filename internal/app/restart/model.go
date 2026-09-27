// Package restart is the matrix's R key: the screen that shows what a restart would roll, takes
// the confirmation, and follows the rollout.
//
// It is deliberately not the deploy confirm screen with a different noun. That screen's whole
// content is a diff, and a restart has none — nothing in the manifest changes. What an operator
// needs to see here instead is live: which Deployments, how many replicas each, what it last
// restarted at, and every reason this particular restart will not be seamless.
package restart

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

// ReadFunc reads what a restart of these Deployments would touch. Supplied by the root rather
// than reached for here: this package never opens a cluster connection of its own (AGENTS.md
// §4.8).
type ReadFunc func(ctx context.Context, env string, names []string) (restart.Plan, error)

// DoFunc performs the restart and returns the names it got through, in order, so a failure can
// say what had already rolled. It takes no env: restart.Plan already carries one, and two
// sources for the same fact is how they come to disagree.
type DoFunc func(ctx context.Context, p restart.Plan, at time.Time) ([]string, error)

// ObserveFunc reads how far the rollout has got.
type ObserveFunc func(ctx context.Context, env string, names []string, at time.Time) ([]restart.Progress, error)

// Funcs is the whole of this screen's access to the world.
type Funcs struct {
	Read    ReadFunc
	Do      DoFunc
	Observe ObserveFunc
	// Interval is how often the rollout is re-read once it is running.
	Interval time.Duration
	// ReadTimeout/DoTimeout/ObserveTimeout bound the Read/Do/Observe call respectively; zero
	// means scope.RestartRead/RestartDo/RestartObserve (AGENTS.md §4.8, "every command has a
	// deadline"). Only ever set away from zero by a test proving a call is actually bounded
	// (AGENTS.md §8: "prove a new test can fail") — cmd/hoist's own wiring never sets these.
	ReadTimeout, DoTimeout, ObserveTimeout time.Duration
}

func (f Funcs) readTimeout() time.Duration {
	if f.ReadTimeout > 0 {
		return f.ReadTimeout
	}
	return scope.RestartRead
}

func (f Funcs) doTimeout() time.Duration {
	if f.DoTimeout > 0 {
		return f.DoTimeout
	}
	return scope.RestartDo
}

func (f Funcs) observeTimeout() time.Duration {
	if f.ObserveTimeout > 0 {
		return f.ObserveTimeout
	}
	return scope.RestartObserve
}

type state int

const (
	stateReading state = iota
	stateConfirm
	// stateStarting is between the operator confirming and the DoFunc call actually returning
	// — startedMsg is only ever acted on while in this state (see Update's own case), so a
	// startedMsg from an earlier instance of this screen that raced a new one is refused even
	// on top of the scope.Foreign guard already dropping it by id (AGENTS.md §8, layered
	// checks: this is politeness, not the enforcement).
	stateStarting
	stateRolling
	stateDone
	stateFailed
)

// BackMsg asks whatever composes screens to pop this one. RollingContinues is set when esc was
// pressed while a restart the operator just confirmed was still starting or rolling (FB-L3): the
// screen is leaving, but the restart itself is not — it is a live cluster operation, not a
// drive this package can cancel — so the root adds the activity entry naming that instead of the
// screen silently vanishing mid-roll with nothing left to say so.
type BackMsg struct{ RollingContinues bool }

// Every message below is returned wrapped as scope.Result[T] (Init/start/tick/observe), so a
// result an earlier instance of this screen asked for — a different family's plan, an already
// superseded restart — cannot land on this one (AGENTS.md §4.8).
type planMsg struct {
	plan restart.Plan
	err  error
}
type startedMsg struct {
	at   time.Time
	done []string
	err  error
}
type progressMsg struct {
	progress []restart.Progress
	err      error
}
type tickMsg struct{}

// Model is the screen.
type Model struct {
	styles ui.Styles
	funcs  Funcs

	env        string
	family     string
	names      []string
	production bool

	// scope is this instance's owned context plus its ID: a planMsg/startedMsg/progressMsg/
	// tickMsg stamped by any other value is Foreign and dropped at the top of Update (AGENTS.md
	// §4.8), and Close (called by the root's pop when this screen is removed) cancels
	// whatever Read/Do/Observe call is outstanding at that moment.
	scope scope.Scope

	state  state
	plan   restart.Plan
	at     time.Time
	notice string
	// done is which Deployments have finished rolling, kept on the model rather than anywhere
	// package-level: two of these screens can exist at once (two envs), and shared mutable
	// state between them would be both a race and a lie.
	done map[string]bool

	// confirm is the production gate: §4.5 cannot apply to something that commits nothing, so
	// what stands in its place is a second, deliberate gesture. Non-production envs do not get
	// one — the target list and its warnings are already on screen, and enter is the answer.
	confirm  *huh.Confirm
	confirmV bool

	body          viewport.Model
	width, height int
}

// busyMarker renders alongside the state word while busy() is true — reading, starting or
// rolling (#PR8/FB-M7's own list): before this, none of those three states carried any visible
// sign that they were doing anything rather than having quietly wedged (the same "is this still
// alive" question flight.Model's own spinner already answers for a promotion in flight). A
// bubbles/v2 spinner.Model was tried here first and never actually animated (P3 #9,
// t2-review.md): Init/start/the Rolling transition below are relied on elsewhere (attach-style
// app-level tests, sessionBuildCmd's own single-cmd shape) to return their real work as ONE
// unbatched command, and nothing in this package ever issued the spinner's own tea.Tick to
// advance it past its first frame — so the "spinner" was a permanently frozen glyph, which reads
// as wedged rather than busy, the opposite of the point. A plain static marker says exactly what
// it is: something is happening, with no claim of motion this screen never delivers.
const busyMarker = "…"

// busy reports whether the header should show busyMarker beside the state word: an outstanding
// Read/Do/Observe call, or a rollout still in progress. Never stateConfirm, stateDone or
// stateFailed — nothing is happening for the operator to wait on in any of those.
func (m Model) busy() bool {
	return m.state == stateReading || m.state == stateStarting || m.state == stateRolling
}

// New builds the screen for one family in one env. names are the Deployments the repo says that
// family declares; nothing is read from the cluster until Init runs.
//
// New refuses a Funcs with Do set but Observe left nil (FB-L3): without Observe, onProgress's
// own tick chain has nothing to call once a restart is under way (observe's own `obs == nil`
// guard returns a nil cmd, silently breaking the chain), so the screen would strand in
// "rolling" forever with no way to tell the operator the rollout finished, or even that it
// started successfully. That is a wiring mistake, not a runtime condition to render around, so
// it fails at construction with a named reason instead of reproducing FB-L3 live.
func New(env, family string, names []string, production bool, funcs Funcs, styles ui.Styles) Model {
	m := Model{
		styles: styles, funcs: funcs,
		env: env, family: family, names: names, production: production,
		scope: scope.Open(),
		state: stateReading,
		body:  viewport.New(),
	}
	if funcs.Do != nil && funcs.Observe == nil {
		m.state = stateFailed
		m.notice = "restart screen misconfigured: Do is set without Observe, so a restart here would have no way to tell whether it finished"
	}
	return m
}

// Close cancels this instance's outstanding Read/Do/Observe call. Called by the root's own pop
// when this screen is actually removed from the stack (AGENTS.md §4.8).
func (m Model) Close() { m.scope.Close() }

// Init reads the cluster — or, for a screen New already refused (see New's own doc comment),
// does nothing at all: there is nothing left to read from a misconfiguration a real cluster
// call could not fix.
func (m Model) Init() tea.Cmd {
	if m.state == stateFailed {
		return nil
	}
	read, env, names, sc := m.funcs.Read, m.env, m.names, m.scope
	if read == nil {
		return scope.DoCtx(sc, m.funcs.readTimeout(), func(context.Context) planMsg { return planMsg{err: fmt.Errorf("restarting is not wired up")} })
	}
	return scope.DoCtx(sc, m.funcs.readTimeout(), func(ctx context.Context) planMsg {
		p, err := read(ctx, env, names)
		return planMsg{plan: p, err: err}
	})
}

// Update implements the screen contract.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if scope.Foreign(m.scope.ID, msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case scope.Result[planMsg]:
		pm := msg.V
		if pm.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(pm.err.Error())
			return m, nil
		}
		m.plan = pm.plan
		m.state = stateConfirm
		if len(m.plan.Targets) == 0 {
			m.state, m.notice = stateFailed, fmt.Sprintf("none of %s's Deployments exist in the cluster", m.env)
		}
		return m.render(), nil
	case scope.Result[startedMsg]:
		if m.state != stateStarting {
			return m, nil // superseded — see stateStarting's own doc comment
		}
		sm := msg.V
		m.at = sm.at
		if sm.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(sm.err.Error())
			if len(sm.done) > 0 {
				m.notice += fmt.Sprintf(" — already restarted: %s", strings.Join(sm.done, ", "))
			}
			return m.render(), nil
		}
		m.state = stateRolling
		return m.render(), m.tick()
	case scope.Result[progressMsg]:
		pm := msg.V
		if pm.err != nil {
			m.state, m.notice = stateFailed, redact.Strings(pm.err.Error())
			return m.render(), nil
		}
		return m.onProgress(pm.progress)
	case scope.Result[tickMsg]:
		return m, m.observe()
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return m, cmd
}

func (m Model) onProgress(progress []restart.Progress) (Model, tea.Cmd) {
	var pending []string
	for _, pr := range progress {
		switch {
		case pr.Superseded:
			m.state = stateFailed
			m.notice = fmt.Sprintf("%s was restarted by something else while this one was in flight; the pods are rolling, but not for this restart", pr.Name)
			return m.render(), nil
		case pr.Blocked != "":
			m.state, m.notice = stateFailed, fmt.Sprintf("%s: %s", pr.Name, pr.Blocked)
			return m.render(), nil
		case !pr.Done:
			pending = append(pending, pr.Name)
		}
	}
	if m.done == nil {
		m.done = map[string]bool{}
	}
	for _, pr := range progress {
		if pr.Done {
			m.done[pr.Name] = true
		}
	}
	if len(pending) == 0 {
		m.state = stateDone
		return m.render(), nil
	}
	return m.render(), m.tick()
}

func (m Model) onKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}
	switch msg.String() {
	case "esc":
		continuing := m.state == stateStarting || m.state == stateRolling
		return m, func() tea.Msg { return BackMsg{RollingContinues: continuing} }
	case "enter":
		if m.state != stateConfirm {
			return m, nil
		}
		if m.production {
			// The one extra gesture production gets. Nothing is committed, so there is no PR to
			// review and no approval comment to wait for; what stands in its place is having to
			// say yes on purpose.
			m.confirmV = false
			m.confirm = huh.NewConfirm().
				Title(fmt.Sprintf("Restart %s in %s? It is a production env.", ui.Plural(len(m.plan.Targets), "Deployment"), m.env))
			// huh.NewConfirm leaves keymap zero-valued, and a zero key.Binding matches nothing:
			// a Confirm used standalone rather than inside a huh.Form ignores every keypress.
			m.confirm.WithKeyMap(huh.NewDefaultKeyMap())
			m.confirm.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
			m.confirm.WithWidth(m.dialogWidth())
			return m, tea.Batch(m.confirm.Init(), m.confirm.Focus())
		}
		return m.start()
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return m, cmd
}

func (m Model) updateConfirm(msg tea.Msg) (Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "esc" {
		m.confirm = nil
		return m.render(), nil
	}
	f, cmd := m.confirm.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		m.confirm = c
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "enter" {
		// Read from the widget, not the bool it was pointed at: Value takes the address of a
		// field in whichever Model copy built it, and every Update since has returned a new one.
		agreed, _ := m.confirm.GetValue().(bool)
		m.confirm = nil
		if !agreed {
			m.notice = "not restarted"
			return m.render(), nil
		}
		return m.start()
	}
	return m, cmd
}

func (m Model) start() (Model, tea.Cmd) {
	do, pl, sc := m.funcs.Do, m.plan, m.scope
	if do == nil {
		m.state, m.notice = stateFailed, "restarting is not wired up"
		return m.render(), nil
	}
	at := time.Now().UTC()
	m.state = stateStarting
	m.notice = ""
	return m.render(), scope.DoCtx(sc, m.funcs.doTimeout(), func(ctx context.Context) startedMsg {
		done, err := do(ctx, pl, at)
		return startedMsg{at: at, done: done, err: err}
	})
}

func (m Model) tick() tea.Cmd {
	d := m.funcs.Interval
	if d <= 0 {
		d = 3 * time.Second
	}
	return scope.After(m.scope.ID, d, tickMsg{})
}

func (m Model) observe() tea.Cmd {
	obs, env, at, sc := m.funcs.Observe, m.env, m.at, m.scope
	names := m.pending()
	if obs == nil || len(names) == 0 {
		return nil
	}
	return scope.DoCtx(sc, m.funcs.observeTimeout(), func(ctx context.Context) progressMsg {
		pr, err := obs(ctx, env, names, at)
		return progressMsg{progress: pr, err: err}
	})
}

func (m Model) pending() []string {
	var out []string
	for _, t := range m.plan.Targets {
		if !m.done[t.Name] {
			out = append(out, t.Name)
		}
	}
	return out
}

// render refreshes the body for the current state. Called wherever the state changes, so View
// itself stays a pure read.
func (m Model) render() Model {
	var b strings.Builder
	switch m.state {
	case stateReading:
		b.WriteString(m.styles.Dim.Render("reading " + m.env + "…"))
	default:
		fmt.Fprintf(&b, "%s\n", m.styles.Title.Render(fmt.Sprintf("%s in %s", ui.Plural(len(m.plan.Targets), "Deployment"), m.env)))
		for _, st := range m.plan.Targets {
			mark := " "
			switch {
			case m.done[st.Name]:
				mark = m.styles.Good.Render("✔")
			case m.state == stateRolling:
				mark = m.styles.Warn.Render("…")
			}
			was := st.RestartedAt
			if was == "" {
				was = "never restarted this way"
			}
			fmt.Fprintf(&b, "\n %s %s  %s\n", mark, m.styles.Accent.Render(st.Name), m.styles.Dim.Render(fmt.Sprintf("%d replica(s) · %s · last restart: %s", st.Replicas, st.Strategy, was)))
			for _, c := range st.GracefulRestartConcerns() {
				// Wrapped to what the frame and the indent leave, every continuation line
				// indented under the "!": a concern is one sentence and reads as one.
				const indent = "     "
				wrapped := ansi.Wrap("! "+c, max(m.width-2-len(indent)-2, 20), "")
				for i, line := range strings.Split(wrapped, "\n") {
					pad := indent
					if i > 0 {
						pad = indent + "  "
					}
					fmt.Fprintf(&b, "%s%s\n", pad, m.styles.Warn.Render(line))
				}
			}
		}
		for _, name := range m.plan.Absent {
			fmt.Fprintf(&b, "\n   %s\n     %s\n", m.styles.Dim.Render(name), m.styles.Warn.Render("! declared in the repo but not in the cluster — not restarted"))
		}
	}
	m.body.SetContent(strings.TrimRight(b.String(), "\n"))
	return m
}

// View renders the screen in the frame: the header, the target list, a notes section for
// the notice, and the footer; the production confirmation is a dialog over it. Every
// rendered string passes through redact.Strings at this one boundary, the same convention
// the other screens use.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	m = m.layout()
	sections := []string{m.headerSection(), m.body.View()}
	if m.notice != "" {
		sections = append(sections, m.styles.Notice.Render(ansi.Wrap(m.notice, max(m.width-2, 20), "")))
	}
	title := fmt.Sprintf("hoist · restart · %s/%s", m.family, m.env)
	view := ui.Frame{Title: title, Sections: sections, Footer: m.footer()}.Render(m.styles, m.width, m.height)
	if m.confirm != nil {
		view = ui.Dialog(m.styles, view, "production", m.confirm.View(), m.width, m.height)
	}
	return redact.Strings(view)
}

// footer renders through keys.Footer (T3-03), matching the screen's own row in
// internal/ui/keys' registry rather than a hand-built hint string.
func (m Model) footer() string {
	hints := []keys.Hint{{B: keys.Log, Long: "l activity", Pri: 2}}
	if m.state == stateConfirm {
		hints = append(hints, keys.Hint{B: keys.Enter, Long: "enter restart", Pri: 1})
	}
	hints = append(hints, keys.Hint{B: keys.Esc, Long: "esc back", Pri: 0})
	return keys.Footer(m.styles, m.width, "", hints, true)
}

// KeyScreen implements the root's keyed interface (internal/app/screen.go).
func (m Model) KeyScreen() keys.Screen { return keys.ScrRestart }

func (m Model) headerSection() string {
	left := m.styles.Title.Render(m.env) + " / " + m.styles.Title.Render(m.family)
	word := m.stateWord()
	if m.busy() {
		// #PR8/FB-M7: reading/starting/rolling are the three states an operator has no way to
		// tell apart from a wedged screen without this — the same question flight.Model's own
		// spinner answers for a promotion in flight.
		word = busyMarker + " " + word
	}
	right := m.styles.Dim.Render(word)
	if m.production {
		right = m.styles.Production.Render("production") + "   " + right
	}
	return ui.StatusBar(max(m.width-2, 1), left, right)
}

func (m Model) stateWord() string {
	switch m.state {
	case stateReading:
		return "reading"
	case stateConfirm:
		return "not yet restarted"
	case stateStarting:
		return "starting"
	case stateRolling:
		return "rolling"
	case stateDone:
		return "all rolled"
	default:
		return "failed"
	}
}

func (m Model) dialogWidth() int { return max(min(m.width-8, 72), 20) }

// layout sizes the body viewport to what the frame leaves.
func (m Model) layout() Model {
	sections := 2
	fixed := 1
	if m.notice != "" {
		sections++
		fixed += strings.Count(ansi.Wrap(m.notice, max(m.width-2, 20), ""), "\n") + 1
	}
	m.body.SetWidth(m.width - 2)
	m.body.SetHeight(max(ui.BodyHeight(m.height, sections)-fixed, 3))
	if m.confirm != nil {
		m.confirm.WithWidth(m.dialogWidth())
	}
	return m
}

// SetSize implements the screen contract. The body's lines are wrapped for a width, so a
// width change re-renders them — a viewport only clips what it was handed, and a body
// rendered for 120 columns shown at 80 lost the end of every concern (the 80×24 golden
// pinned exactly that until this was added).
func (m Model) SetSize(width, height int) Model {
	changed := width != m.width
	m.width, m.height = width, height
	if changed {
		m = m.render()
	}
	return m.layout()
}

// SetStyles implements the screen contract.
func (m Model) SetStyles(s ui.Styles) Model { m.styles = s; return m.render() }

// CapturesText reports whether a keypress belongs to this screen's own input — true only while
// the production confirmation is open.
func (m Model) CapturesText() bool { return m.confirm != nil }
