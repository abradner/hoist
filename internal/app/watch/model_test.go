package watch

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
)

// clock is a pinned now that a test advances by hand, so "polled … ago" is deterministic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var t0 = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// healthy is the control snapshot: synced, healthy, every Deployment rolled out, a CronJob
// with nothing to say. Placeholder names only (AGENTS.md §4.4).
func healthy() Snapshot {
	return Snapshot{
		App: "web-app-staging", Namespace: "app-staging",
		SyncStatus: "Synced", HealthStatus: "Healthy", Revision: "0123456789abcdef0123456789abcdef01234567",
		OperationPhase: "Succeeded", ReconciledAt: t0.Add(-2 * time.Minute),
		Workloads: []Workload{
			{Kind: "Deployment", Name: "web", Replicas: 2, Complete: true, Detail: `deployment "web" successfully rolled out`,
				Images: []string{"container web=ghcr.io/example/web:v1.4.0@sha256:" + strings.Repeat("a", 64)}},
			{Kind: "CronJob", Name: "web-purge", Detail: "schedule=0 3 * * * suspend=false active=0 lastSchedule=(never)"},
		},
	}
}

// progressing is the case under test: out of sync, one Deployment mid-rollout with kubectl's
// own "1 of 2 updated" detail, a second not yet started.
func progressing() Snapshot {
	s := healthy()
	s.SyncStatus, s.HealthStatus, s.OperationPhase = "OutOfSync", "Progressing", "Running"
	s.Workloads = []Workload{
		{Kind: "Deployment", Name: "web", Replicas: 2, Detail: `Waiting for deployment "web" rollout to finish: 1 of 2 updated replicas are available...`,
			Images: []string{"container web=ghcr.io/example/web:v1.5.0@sha256:" + strings.Repeat("b", 64)}},
		{Kind: "Deployment", Name: "web-worker", Replicas: 1, Detail: `Waiting for deployment "web-worker" rollout to finish: 0 of 1 updated replicas are available...`,
			Images: []string{"initContainer migrate=ghcr.io/example/web:v1.5.0@sha256:" + strings.Repeat("b", 64), "container worker=ghcr.io/example/web:v1.5.0@sha256:" + strings.Repeat("b", 64)}},
		{Kind: "Job", Name: "web-migrate", Detail: "active=1 succeeded=0 failed=0"},
	}
	return s
}

// reader is the screen's world: a fixed sequence of snapshots, counted.
type reader struct {
	snaps []Snapshot
	err   error
	calls int
}

func (r *reader) read(context.Context) (Snapshot, error) {
	i := min(r.calls, len(r.snaps)-1)
	r.calls++
	if r.err != nil {
		return Snapshot{}, r.err
	}
	return r.snaps[i], nil
}

func ready(t *testing.T, r *reader, c *clock, w, h int) Model {
	t.Helper()
	m := New("web", "app-staging", Funcs{Read: r.read, Interval: 5 * time.Second, Now: c.now}, ui.NewStyles(true)).SetSize(w, h)
	// Init's command yields the first snapshot; the tick it schedules is not drained (a
	// tea.Tick sleeps), so the snapshot is applied by hand the way the runtime would.
	m, _ = m.Update(m.Init()())
	return m
}

func TestGoldenHealthy(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		c := &clock{t0}
		m := ready(t, &reader{snaps: []Snapshot{healthy()}}, c, sz[0], sz[1])
		uitest.Golden(t, "watch-healthy", m.View(), sz[0], sz[1])
	}
}

func TestGoldenProgressing(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		c := &clock{t0}
		m := ready(t, &reader{snaps: []Snapshot{progressing()}}, c, sz[0], sz[1])
		uitest.Golden(t, "watch-progressing", m.View(), sz[0], sz[1])
	}
}

func TestFirstPaintIsOneSnapshot(t *testing.T) {
	r := &reader{snaps: []Snapshot{healthy()}}
	m := ready(t, r, &clock{t0}, 80, 24)
	if r.calls != 1 {
		t.Fatalf("Init read %d snapshots, want exactly 1 (--once is the first paint)", r.calls)
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"web-app-staging", "Synced", "Healthy", "0123456789ab", "Deployment web", "rolled out · 2 replica(s)", "CronJob web-purge", "polled just now · every 5s"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "0123456789abcdef0123456789abcdef01234567") {
		t.Errorf("view shows the full revision, want it shortened:\n%s", v)
	}
}

// TestTickRereadsAndMovesLastPolled: a tick re-invokes the function (the world is
// re-observed, never remembered) and the header's age follows the new poll.
func TestTickRereadsAndMovesLastPolled(t *testing.T) {
	r := &reader{snaps: []Snapshot{healthy(), progressing()}}
	c := &clock{t0}
	m := ready(t, r, c, 80, 24)
	first := m.LastPolled()

	c.t = t0.Add(3 * time.Minute)
	if got := ansi.Strip(m.headerSection()); !strings.Contains(got, "polled 3m ago") {
		t.Fatalf("header before the tick: %q, want 'polled 3m ago'", got)
	}
	m, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick produced no read")
	}
	m, _ = m.Update(cmd())
	if r.calls != 2 {
		t.Fatalf("after one tick the function was called %d times, want 2", r.calls)
	}
	if !m.LastPolled().After(first) {
		t.Fatalf("LastPolled did not move: %v then %v", first, m.LastPolled())
	}
	if got := ansi.Strip(m.headerSection()); !strings.Contains(got, "polled just now") {
		t.Errorf("header after the tick: %q, want 'polled just now'", got)
	}
	if !strings.Contains(ansi.Strip(m.View()), "OutOfSync") {
		t.Errorf("the second snapshot is not on screen:\n%s", ansi.Strip(m.View()))
	}
}

func TestRPollsNow(t *testing.T) {
	r := &reader{snaps: []Snapshot{healthy()}}
	m := ready(t, r, &clock{t0}, 80, 24)
	m, cmd := m.Update(uitest.Key("r"))
	if cmd == nil {
		t.Fatal("r produced no read")
	}
	// A second r while that read is outstanding starts nothing.
	if _, cmd := m.Update(uitest.Key("r")); cmd != nil {
		t.Error("r during an outstanding read started a second one")
	}
	if _, ok := cmd().(snapshotMsg); !ok {
		t.Fatal("r's command did not yield a snapshot")
	}
	if r.calls != 2 {
		t.Fatalf("r read %d times in total, want 2", r.calls)
	}
}

func TestEscEmitsBackMsg(t *testing.T) {
	m := ready(t, &reader{snaps: []Snapshot{healthy()}}, &clock{t0}, 80, 24)
	_, cmd := m.Update(uitest.Key("esc"))
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(BackMsg); !ok {
		t.Fatalf("esc emitted %T, want BackMsg", cmd())
	}
}

func TestReadErrorKeepsLastSnapshot(t *testing.T) {
	r := &reader{snaps: []Snapshot{healthy()}}
	m := ready(t, r, &clock{t0}, 80, 24)
	r.err = errors.New("dial tcp my-cluster:6443: i/o timeout")
	m, cmd := m.Update(tickMsg{})
	m, _ = m.Update(cmd())
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "Deployment web") || !strings.Contains(v, "i/o timeout") {
		t.Errorf("want the last good snapshot under the error:\n%s", v)
	}
}

func TestNilReadIsANamedGap(t *testing.T) {
	m := New("web", "app-staging", Funcs{Now: (&clock{t0}).now}, ui.NewStyles(true)).SetSize(80, 24)
	m, _ = m.Update(m.Init()())
	if v := ansi.Strip(m.View()); !strings.Contains(v, "not wired up") {
		t.Errorf("nil Read should say so:\n%s", v)
	}
}

// TestNeverImportsClusterPackages is the structural half of "watching never refreshes": the
// screen cannot call Argo.Refresh because it cannot name pkg/argo at all. The adapter side
// (cmd/hoist's TestBuildWatchFuncNeverCallsRefresh) pins the calls it does make.
func TestNeverImportsClusterPackages(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			seen++
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(p, "/pkg/argo") || strings.HasSuffix(p, "/pkg/rollout") || strings.HasSuffix(p, "/pkg/k8s") {
				t.Errorf("%s imports %s: the watch screen must reach the cluster only through watch.Func", e.Name(), p)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no imports parsed — the probe is broken")
	}
}
