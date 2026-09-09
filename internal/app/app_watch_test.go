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
