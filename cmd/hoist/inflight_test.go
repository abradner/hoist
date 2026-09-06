package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
)

// buildInFlightFuncs lists every state file, re-observed, and names the reason when one
// cannot be — a state whose repo left the config is listed with that as its error, never
// dropped. Resume of an unknown id is an error, not a zero state.
func TestBuildInFlightFuncsListsAndNamesTheUnobservable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{Repos: []config.RepoConfig{{Path: "/x", GitHub: "me/my-gitops"}}}
	f := buildInFlightFuncs(cfg)
	list, err := f.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("empty state dir: list=%v err=%v", list, err)
	}
	if _, _, err := f.Resume(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "no promotion nope") {
		t.Fatalf("resume of an unknown id: err=%v", err)
	}
	// A state whose repo is not in the config file.
	path, err := engine.StatePath("orphan01")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(path, &engine.PromotionState{ID: "orphan01", RepoFullName: "someone/else", TargetEnv: "app-production"}); err != nil {
		t.Fatal(err)
	}
	list, err = f.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].ID != "orphan01" || !strings.Contains(list[0].Err, "someone/else is not in the config file") || list[0].Verdict() != "cannot re-observe" {
		t.Fatalf("orphan summary = %+v", list[0])
	}
	if _, _, err := f.Resume(context.Background(), "orphan01"); err == nil || !strings.Contains(err.Error(), "not in the config file") {
		t.Fatalf("resume of an orphan: err=%v", err)
	}
	if buildInFlightFuncs(nil).List != nil {
		t.Fatal("no config: nothing wired")
	}
}
