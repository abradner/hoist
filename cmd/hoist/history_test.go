package main

import (
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
)

// The history use case itself — revisions, deltas, the live age, what is remembered and what is
// retried — is tested at internal/service (history_test.go, tags_test.go). What is left here is
// the wiring: which of the screen's functions exist for which kind of run.
func TestBuildHistoryFuncsShapes(t *testing.T) {
	if h := buildHistoryFuncs(service.New(service.Settings{}, serviceDeps())); h.Mapped != nil || h.Revision != nil || h.Delta != nil || h.LiveAge != nil {
		t.Error("a flags-only run has no repos[].apps and no gitops forge: every function must be nil, so screens degrade without a call")
	}

	// No apps mapping means no delta — but the live age blames the gitops repo, which needs no
	// mapping, so it is still wired (an empty repos[].apps used to lose "since 4 weeks ago" too).
	h := buildHistoryFuncs(service.New(service.Settings{Repo: &config.RepoConfig{}}, serviceDeps()))
	if h.Delta != nil || h.Revision != nil {
		t.Error("no apps mapping: no delta or revision func")
	}
	if h.Mapped == nil || h.Mapped("ghcr.io/example/x") {
		t.Error("Mapped must answer false for every repo")
	}
	if h.LiveAge == nil {
		t.Error("LiveAge must be wired without an apps mapping")
	}

	mapped := &config.RepoConfig{Apps: map[string]string{"ghcr.io/example/app": "example/app"}}
	h = buildHistoryFuncs(service.New(service.Settings{Repo: mapped}, serviceDeps()))
	if h.Delta == nil || h.Revision == nil || h.LiveAge == nil || !h.Mapped("ghcr.io/example/app") || h.Mapped("ghcr.io/example/x") {
		t.Error("a mapped repo wires every function, and Mapped follows repos[].apps")
	}
}
