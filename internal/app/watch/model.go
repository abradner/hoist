// Package watch is the matrix's w key: one Argo Application's sync/health/revision and the
// rollout state of every workload its family declares, re-read on a cadence — the TUI face of
// `hoist watch --app <name>` (#101).
//
// It is read-only by construction, and the construction is the point: this package imports
// neither pkg/argo nor pkg/rollout, so it cannot name Argo.Refresh, let alone call it. The
// world reaches it as a Func returning plain Snapshot values, built in cmd/hoist
// (buildWatchFunc) from the same Get/Deployment/JobLike reads `hoist watch` makes — watching
// is not promoting (AGENTS.md §4.7), on either face. TestNeverImportsClusterPackages pins the
// import list so that guarantee cannot rot quietly.
package watch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
	"github.com/abradner/hoist/pkg/redact"
)

// Snapshot is one read of the Application and its workloads: plain, JSON-safe values with no
// cluster types in them, so the screen can be tested from literals and the adapter from fakes.
type Snapshot struct {
	// App is the Application's metadata.name; Namespace its destination namespace (the env).
	App, Namespace string
	// SyncStatus, HealthStatus, Revision and OperationPhase are Argo's own status words, ""
	// when Argo has not filled one in; ReconciledAt is zero when it never has.
	SyncStatus, HealthStatus, Revision, OperationPhase string
	ReconciledAt                                       time.Time
	// Workloads lists every Deployment, then every Job/CronJob, the family declares — the
	// same set and order `hoist watch` prints.
	Workloads []Workload
}

// Workload is one Deployment, Job or CronJob's current state.
type Workload struct {
	Kind, Name string
	// Replicas is a Deployment's spec.replicas; 0 for a Job/CronJob, which has none.
	Replicas int32
	// Complete and DeadlineExceeded are kubectl's own rollout verdict, Deployments only.
	Complete, DeadlineExceeded bool
	// Detail is the kubectl-style line: what is still pending, or that it rolled out.
	Detail string
	// Images are "container name=ref" pairs, initContainers marked, as `hoist watch` prints.
	Images []string
}

// Func reads one Snapshot. Every call re-observes the cluster (AGENTS.md §4.1); nothing is
// cached between polls.
type Func func(ctx context.Context) (Snapshot, error)

// Funcs is the whole of this screen's access to the world for one Application.
type Funcs struct {
	Read Func
	// Interval is the poll cadence: the tighter of poll.argo and poll.rollout, exactly as
	// `hoist watch` polls (cmd/hoist/watch.go's watchInterval). Zero falls back to 3s.
	Interval time.Duration
	// Now is the clock the "last polled" age is taken against; nil means time.Now. A test
	// pins it so the golden is stable (AGENTS.md §4.8).
	Now func() time.Time
}

// BuildFunc is how the root asks for one family-in-env's Funcs — cmd/hoist's buildWatchFunc,
// which resolves the family's Application and workload names from the discovered repo and
// closes over the Argo/rollout adaptors. An error means the cell cannot be watched (no
// cluster configured, a family with no Application) and is shown as a matrix notice.
type BuildFunc func(family, env string) (Funcs, error)

// BackMsg asks whatever composes screens to pop this one.
type BackMsg struct{}

// snapshotMsg is delivered once a poll finishes. Returned wrapped as scope.Result[snapshotMsg]
// (poll), so a read this screen instance is no longer the current one for — the operator backed
// out with w still outstanding, then opened w again for a different family — cannot land here
// (AGENTS.md §4.8).
type snapshotMsg struct {
	snap Snapshot
	err  error
	at   time.Time
}

// tickMsg carries the generation of the tick that scheduled it; a stale one (r, or a later
// snapshot, has scheduled a newer tick since) is ignored, so a manual poll never leaves a
// second timer chain running beside the cadence — the same guard as app.go's listGen. Also
// returned wrapped as scope.Result[tickMsg] (tick), since gen alone only dedups chains within
// one screen instance — it says nothing about a tick scheduled by an instance for a family this
// screen was never showing.
type tickMsg struct{ gen uint64 }

// Model is the screen.
type Model struct {
	styles ui.Styles
	funcs  Funcs

	family, env string

	// scope is this instance's owned context plus its ID: a snapshotMsg/tickMsg stamped by any
	// other value is Foreign and dropped at the top of Update (AGENTS.md §4.8), and Close
	// (called by the root's pop when this screen is actually removed from the stack) cancels
	// whatever read is outstanding at that moment rather than leaving it to run to completion.
	scope scope.Scope

	snap       Snapshot
	err        string
	lastPolled time.Time
	polls      int
	// polling is true while a read is outstanding, so r during one does not start a second.
	polling bool
	// tickGen is the generation of the one live tick; see tickMsg.
	tickGen uint64

	// body scrolls the status and workload rows: a family with several Deployments, each
	// carrying a digest-pinned image, is taller than a 24-row terminal.
	body          viewport.Model
	width, height int
}

// newViewport is this screen's own scrolling body, bound to keys.ViewportKeyMap
// rather than left on viewport.New()'s bubbles-library default, which binds
// space/f/b to page and bare "d"/"u" to half-page — none of it shown on this screen's footer
// or help overlay.
func newViewport() viewport.Model {
	v := viewport.New()
	v.KeyMap = keys.ViewportKeyMap()
	return v
}

// New builds the screen for one family in one env. Nothing is read until Init runs.
func New(family, env string, funcs Funcs, styles ui.Styles) Model {
	if funcs.Now == nil {
		funcs.Now = time.Now
	}
	return Model{styles: styles, funcs: funcs, family: family, env: env, scope: scope.Open(), body: newViewport()}
}

// Init takes the first snapshot — `hoist watch --once` is this screen's first paint.
func (m Model) Init() tea.Cmd { return m.poll() }

// Close cancels this instance's outstanding poll. Called by the root's own pop when this screen
// is actually removed from the stack (AGENTS.md §4.8), never by this package on its own — this
// screen has no early-leave gesture of its own to call it from; esc's own BackMsg is exactly
// what triggers that pop.
func (m Model) Close() { m.scope.Close() }

func (m Model) poll() tea.Cmd {
	read, now, sc := m.funcs.Read, m.funcs.Now, m.scope
	if read == nil {
		return scope.DoCtx(sc, scope.Drift, func(context.Context) snapshotMsg {
			return snapshotMsg{err: fmt.Errorf("watching is not wired up"), at: now()}
		})
	}
	return scope.DoCtx(sc, scope.Drift, func(ctx context.Context) snapshotMsg {
		at := now()
		snap, err := read(ctx)
		return snapshotMsg{snap: snap, err: err, at: at}
	})
}

// tick schedules the next poll and retires every tick scheduled before it.
func (m Model) tick() (Model, tea.Cmd) {
	m.tickGen++
	gen := m.tickGen
	return m, scope.After(m.scope.ID, m.interval(), tickMsg{gen: gen})
}

// Update implements the screen contract.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if scope.Foreign(m.scope.ID, msg) {
		return m, nil
	}
	switch msg := msg.(type) {
	case scope.Result[snapshotMsg]:
		sm := msg.V
		m.polling = false
		m.lastPolled = sm.at
		m.polls++
		if sm.err != nil {
			// The last good snapshot stays on screen under the error: a transient plumbing
			// failure should not blank what the operator was reading.
			m.err = redact.Strings(sm.err.Error())
		} else {
			m.err = ""
			m.snap = sm.snap
		}
		return m.render().tick()
	case scope.Result[tickMsg]:
		if msg.V.gen != m.tickGen {
			return m, nil // retired by r or a later snapshot; its replacement is already pending
		}
		if m.polling {
			return m.tick()
		}
		m.polling = true
		return m, m.poll()
	case tea.KeyPressMsg:
		switch {
		case keys.Esc.Matches(msg):
			return m, func() tea.Msg { return BackMsg{} }
		case keys.Home.Matches(msg):
			m.body.GotoTop()
			return m, nil
		case keys.End.Matches(msg):
			m.body.GotoBottom()
			return m, nil
		case keys.Refresh.Matches(msg):
			// this matched only the literal string "r", so the registry's
			// own F5/ctrl+r aliases (keys.Refresh.Keys) did nothing, though both are listed on
			// the footer and help overlay.
			if m.polling {
				return m, nil
			}
			// The pending tick is retired: the snapshot this read yields schedules the next
			// one, so the cadence restarts from now rather than doubling.
			m.tickGen++
			m.polling = true
			return m, m.poll()
		}
	}
	var cmd tea.Cmd
	m.body, cmd = m.body.Update(msg)
	return m, cmd
}

// View renders the frame: the header (app, env, last poll), the Argo status and workload
// rows, the error when there is one, and the footer. Every string passes through
// redact.Strings at this one boundary, the convention the other screens use.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		m.width, m.height = 80, 24
	}
	m = m.layout()
	sections := []string{m.headerSection(), m.body.View()}
	if m.err != "" {
		sections = append(sections, m.styles.Notice.Render(m.wrappedErr()))
	}
	title := fmt.Sprintf("hoist · watch · %s/%s", m.family, m.env)
	view := ui.Frame{Title: title, Sections: sections, Footer: m.footer()}.Render(m.styles, m.width, m.height)
	return redact.Strings(view)
}

// footer renders through keys.Footer rather than a hand-built status bar, so this
// screen's own row in internal/ui/keys' registry is what the operator actually sees — UX-H9:
// the left side names the next poll countdown rather than the old "read-only · never
// refreshes", which the audit flagged as claiming a mechanism ("never refreshes") this screen
// does not run (it polls on m.interval()) right beside a verb ("poll now") that duplicates r's
// own "refresh" meaning everywhere else in the app.
func (m Model) footer() string {
	hints := []keys.Hint{
		{B: keys.Refresh, Long: "r refresh", Pri: 1},
		{B: keys.Log, Long: "l activity", Pri: 2},
		{B: keys.Esc, Long: "esc back", Pri: 0},
	}
	return keys.Footer(m.styles, m.width, m.status(), hints, true)
}

// KeyScreen implements the root's keyed interface (internal/app/screen.go): this screen's own
// row in internal/ui/keys' registry, used by both the help overlay and this footer.
func (m Model) KeyScreen() keys.Screen { return keys.ScrWatch }

func (m Model) wrappedErr() string { return ansi.Wrap(m.err, max(m.width-2, 20), "") }

func (m Model) headerSection() string {
	left := m.styles.Title.Render(m.env) + " / " + m.styles.Title.Render(m.family)
	if m.snap.App != "" {
		left += " · " + m.styles.Dim.Render(m.snap.App)
	}
	right := m.styles.Dim.Render(fmt.Sprintf("polled %s · every %s", ui.Ago(m.funcs.Now(), m.lastPolled), every(m.interval())))
	return ui.StatusBar(max(m.width-2, 1), left, right)
}

func (m Model) interval() time.Duration {
	if m.funcs.Interval <= 0 {
		return 3 * time.Second
	}
	return m.funcs.Interval
}

// every is a poll cadence as an operator reads one: "5s", "1m", "1h30m" — Duration.String
// without the trailing zero units, and never ui.Span, whose "just now" is for ages.
func every(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	mi := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if mi > 0 {
		fmt.Fprintf(&b, "%dm", mi)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// Lines is the body before any styling — what the screen shows, as plain rows, so a test
// can assert content without a golden (AGENTS.md §4.8: derived data lives apart from layout).
func (m Model) Lines() []string {
	if m.polls == 0 {
		return []string{"reading " + m.env + "…"}
	}
	if m.snap.App == "" {
		return nil
	}
	s := m.snap
	lines := []string{
		fmt.Sprintf("sync %s · health %s · revision %s · operation %s · reconciled %s",
			orNone(s.SyncStatus), orNone(s.HealthStatus), shortRev(s.Revision), orNone(s.OperationPhase), ui.Ago(m.funcs.Now(), s.ReconciledAt)),
	}
	for _, w := range s.Workloads {
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%s %s %s", w.Kind, w.Name, workloadState(w)), " "))
		if w.Detail != "" {
			lines = append(lines, "    "+w.Detail)
		}
		for _, img := range w.Images {
			lines = append(lines, "    "+img)
		}
	}
	return lines
}

// render refreshes the body for the current snapshot and width. Called wherever either
// changes, so View itself stays a pure read.
func (m Model) render() Model {
	var lines []string
	if m.polls == 0 {
		lines = []string{m.styles.Dim.Render("reading " + m.env + "…")}
	} else {
		s := m.snap
		status := fmt.Sprintf("%s %s   %s %s   %s %s   %s %s   %s %s",
			m.styles.Dim.Render("sync"), m.styleSync(s.SyncStatus),
			m.styles.Dim.Render("health"), m.styleHealth(s.HealthStatus),
			m.styles.Dim.Render("revision"), m.styles.Accent.Render(shortRev(s.Revision)),
			m.styles.Dim.Render("operation"), orNone(s.OperationPhase),
			m.styles.Dim.Render("reconciled"), ui.Ago(m.funcs.Now(), s.ReconciledAt))
		lines = append(lines, m.wrap(status, "", "  ")...)
		for _, w := range s.Workloads {
			lines = append(lines, "", fmt.Sprintf("%s %s  %s", m.styles.Dim.Render(w.Kind), m.styles.Accent.Render(w.Name), m.styleState(w)))
			if w.Detail != "" {
				lines = append(lines, m.wrap(w.Detail, "    ", "      ")...)
			}
			for _, img := range w.Images {
				// A digest-pinned reference has no space to break at, so it is hard-wrapped:
				// the operator is comparing digests, and a truncated one compares nothing.
				lines = append(lines, m.wrap(m.styles.Dim.Render(img), "    ", "      ")...)
			}
		}
	}
	m.body.SetContent(strings.Join(lines, "\n"))
	return m
}

// wrap breaks s to the frame's inner width, the first line under indent and every
// continuation under more, so a wrapped kubectl sentence or image reads as one row.
func (m Model) wrap(s, indent, more string) []string {
	inner := max(m.width-2-len(more), 20)
	wrapped := ansi.Hardwrap(ansi.Wrap(s, inner, ""), inner, true)
	var out []string
	for i, line := range strings.Split(wrapped, "\n") {
		pad := indent
		if i > 0 {
			pad = more
		}
		out = append(out, pad+line)
	}
	return out
}

// layout sizes the body viewport to what the frame leaves.
func (m Model) layout() Model {
	sections := 2
	fixed := 1
	if m.err != "" {
		sections++
		fixed += strings.Count(m.wrappedErr(), "\n") + 1
	}
	m.body.SetWidth(m.width - 2)
	m.body.SetHeight(max(ui.BodyHeight(m.height, sections)-fixed, 3))
	return m
}

func (m Model) styleSync(s string) string {
	if s == "Synced" {
		return m.styles.Good.Render(s)
	}
	return m.styles.Warn.Render(orNone(s))
}

func (m Model) styleHealth(s string) string {
	switch s {
	case "Healthy":
		return m.styles.Good.Render(s)
	case "Degraded", "Missing":
		return m.styles.Bad.Render(s)
	}
	return m.styles.Warn.Render(orNone(s))
}

func (m Model) styleState(w Workload) string {
	word := workloadState(w)
	switch {
	case w.DeadlineExceeded:
		return m.styles.Bad.Render(word)
	case w.Kind == "Deployment" && w.Complete:
		return m.styles.Good.Render(word)
	case w.Kind == "Deployment":
		return m.styles.Warn.Render(word)
	}
	return m.styles.Dim.Render(word)
}

// workloadState is the one word a workload row carries beside its name: a Deployment's
// rollout verdict with its replica count, or nothing for a Job/CronJob (Detail says it all).
func workloadState(w Workload) string {
	if w.Kind != "Deployment" {
		return ""
	}
	replicas := ui.Plural(int(w.Replicas), "replica")
	switch {
	case w.DeadlineExceeded:
		return "deadline exceeded · " + replicas
	case w.Complete:
		return "rolled out · " + replicas
	default:
		return "rolling · " + replicas
	}
}

// status is the footer's own left-hand text (UX-H9): "next poll in Ns" replaces the old
// "read-only · never refreshes", which claimed a mechanism this screen does not run (it polls
// on m.interval() — Principle 1) right beside a verb ("poll now") that duplicated r's own
// "refresh" meaning everywhere else in the app.
func (m Model) status() string {
	switch {
	case m.polls == 0:
		return "reading"
	case m.polling:
		return "polling…"
	default:
		remain := m.lastPolled.Add(m.interval()).Sub(m.funcs.Now())
		if remain < 0 {
			remain = 0
		}
		return fmt.Sprintf("next poll in %ds", int(remain.Round(time.Second).Seconds()))
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func shortRev(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// SetSize implements the screen contract. The body is wrapped for a width, so a width
// change re-renders it.
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

// CapturesText implements the screen contract: this screen has no text input.
func (m Model) CapturesText() bool { return false }

// LastPolled reports when the most recent snapshot was taken, zero before the first.
func (m Model) LastPolled() time.Time { return m.lastPolled }
