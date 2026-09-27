package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/matrix"
	"github.com/abradner/hoist/internal/app/watch"
)

// TestWatchMsgPushesWatchScreen: w's message reaches the root, which asks the builder for the
// cell's funcs, pushes the screen sized like the matrix, and its Init reads once; esc pops
// back to the matrix.
func TestWatchMsgPushesWatchScreen(t *testing.T) {
	var asked []string
	reads := 0
	build := func(family, env string) (watch.Funcs, error) {
		asked = append(asked, family+"/"+env)
		return watch.Funcs{
			Read: func(context.Context) (watch.Snapshot, error) {
				reads++
				return watch.Snapshot{App: "counta-app-production", Namespace: env, SyncStatus: "Synced", HealthStatus: "Healthy"}, nil
			},
			Interval: time.Minute,
			Now:      func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
		}, nil
	}
	m := sized(t).(Model).WithWatch(build)
	var tm tea.Model = m
	tm, cmd := tm.Update(matrix.OpenWatchMsg{Family: "counta", Target: "app-production"})
	if len(asked) != 1 || asked[0] != "counta/app-production" {
		t.Fatalf("builder asked for %v, want [counta/app-production]", asked)
	}
	if n := len(tm.(Model).stack); n != 2 {
		t.Fatalf("stack has %d screens after OpenWatchMsg, want 2", n)
	}
	if cmd == nil {
		t.Fatal("pushing the watch screen produced no Init command")
	}
	tm, _ = tm.Update(cmd())
	if reads != 1 {
		t.Fatalf("Init read %d times, want 1", reads)
	}
	if v := plain(tm); !strings.Contains(v, "hoist · watch") || !strings.Contains(v, "counta-app-production") || !strings.Contains(v, "Synced") {
		t.Errorf("watch screen view:\n%s", v)
	}
	tm, back := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if back == nil {
		t.Fatal("esc on the watch screen produced no command")
	}
	tm, _ = tm.Update(back())
	if n := len(tm.(Model).stack); n != 1 {
		t.Errorf("esc did not pop back to the matrix: stack has %d screens", n)
	}
}

// TestWatchEarlierSnapshotDropped proves internal/app/scope's own Foreign guard for the watch
// screen (Train 2 design PR 5, audit FB-M4): a poll is still a live tea.Cmd after the operator
// backs out with w — esc pops the screen, but nothing cancels the outstanding read (that is
// PR 6's owned context.Context, a separate piece of work) — so a second w for a different family
// can already be showing its own snapshot by the time the first read finally answers. Attacker:
// the first watch instance's own snapshotMsg, released last. Control: the second instance's own
// snapshot is what actually lands, checked before the attacker is ever released.
func TestWatchEarlierSnapshotDropped(t *testing.T) {
	reads := map[string]int{}
	build := func(family, env string) (watch.Funcs, error) {
		key := family + "/" + env
		return watch.Funcs{
			Read: func(context.Context) (watch.Snapshot, error) {
				reads[key]++
				return watch.Snapshot{App: key, Namespace: env, SyncStatus: "Synced", HealthStatus: "Healthy"}, nil
			},
			Interval: time.Minute,
			Now:      func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) },
		}, nil
	}
	var tm tea.Model = sized(t).(Model).WithWatch(build)

	// w on family1/app-staging: the first watch instance. Its Init() is m.poll() itself (no
	// batch to unwrap — watch.Model has no spinner), left uncalled.
	tm, cmd := tm.Update(matrix.OpenWatchMsg{Family: "counta", Target: "app-staging"})
	pollCmd1 := cmd

	// esc: pops back to the matrix; the read above is still outstanding.
	tm, back := tm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	tm, _ = tm.Update(back())
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens after esc, want 1 (matrix only)", n)
	}

	// w again, on a different family: a second, distinct watch instance.
	tm, cmd = tm.Update(matrix.OpenWatchMsg{Family: "marketing", Target: "app-production"})
	pollCmd2 := cmd

	// Control: the second instance's own snapshot lands.
	tm, _ = tm.Update(pollCmd2())
	want := plain(tm)
	if !strings.Contains(want, "marketing") || !strings.Contains(want, "app-production") {
		t.Fatalf("second watch's view does not name its own family/env:\n%s", want)
	}

	// Attacker, released last: the first instance's snapshot, superseded before it ever
	// answered. Without internal/app/scope's Foreign guard this lands on the screen now on top
	// (the second instance) and overwrites what the operator is looking at.
	tm, _ = tm.Update(pollCmd1())
	got := plain(tm)
	if got != want {
		t.Fatalf("an earlier watch screen's late snapshot changed what is on screen:\nbefore:\n%s\nafter:\n%s", want, got)
	}
}

func TestWatchMsgWithoutClusterIsANotice(t *testing.T) {
	tm, _ := sized(t).Update(matrix.OpenWatchMsg{Family: "counta", Target: "app-production"})
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens, want the matrix alone", n)
	}
	if v := plain(tm); !strings.Contains(v, "watching needs a cluster connection") {
		t.Errorf("want the no-cluster notice on the matrix:\n%s", v)
	}
}

func TestWatchMsgBuilderErrorIsANotice(t *testing.T) {
	build := func(_, _ string) (watch.Funcs, error) {
		return watch.Funcs{}, errors.New("no family \"ghost\" in app-production")
	}
	tm, _ := sized(t).(Model).WithWatch(build).Update(matrix.OpenWatchMsg{Family: "ghost", Target: "app-production"})
	if n := len(tm.(Model).stack); n != 1 {
		t.Fatalf("stack has %d screens, want the matrix alone", n)
	}
	if v := plain(tm); !strings.Contains(v, "cannot watch ghost in app-production") {
		t.Errorf("want the builder's reason on the matrix:\n%s", v)
	}
}
