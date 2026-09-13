package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
)

// TestPromotionsArchivesOldTerminalPromotions drives a promotion all the way through `hoist
// promote` (this fixture's config auto-converges), then re-runs `hoist promotions` with
// state.retain cut to a nanosecond — old enough the instant it's terminal — and confirms it
// gets moved to the archive.
func TestPromotionsArchivesOldTerminalPromotions(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("promote: exit %d, want 0; stderr: %s", got, errOut.String())
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("fixture precondition: want exactly one state file, got %d", len(states))
	}
	id := states[0].ID

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.State.Retain = config.Duration(time.Nanosecond)

	out.Reset()
	errOut.Reset()
	got := runPromotions(nil, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "archived") {
		t.Fatalf("stdout should report the promotion was archived:\n%s", out.String())
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := engine.LoadState(statePath); err != nil || st != nil {
		t.Errorf("live state file should be gone after archiving: LoadState = %v, %v", st, err)
	}
	archived, err := engine.ListArchivedStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != id {
		t.Errorf("ListArchivedStates() = %v, want exactly [%s]", archived, id)
	}
}

// TestPromotionsNeverArchivesAnInFlightPromotionRegardlessOfRetain is state retention's own
// safety property, stated explicitly in StateConfig's own doc comment: archiving must never
// change what findInFlight (or anything else re-observing) concludes — only a promotion already
// confirmed done is ever archived, never one that is merely old. A cut-to-a-nanosecond retain
// would archive everything if age alone decided it; it must still leave a genuinely in-flight
// promotion (PR opened, never merged) untouched.
func TestPromotionsNeverArchivesAnInFlightPromotionRegardlessOfRetain(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.State.Retain = config.Duration(time.Nanosecond)

	var out, errOut bytes.Buffer
	got := runPromotions(nil, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if strings.Contains(out.String(), "archived") {
		t.Fatalf("an in-flight (not done) promotion must never be archived, regardless of how small retain is:\n%s", out.String())
	}
	if st, err := engine.LoadState(statePath); err != nil || st == nil {
		t.Errorf("an in-flight promotion's live state file must remain: LoadState = %v, %v", st, err)
	}
	if archived, err := engine.ListArchivedStates(); err != nil || len(archived) != 0 {
		t.Errorf("nothing should have been archived: %v, %v", archived, err)
	}
}

// TestPromotionsRepoFlagScopesListing confirms --repo filters both the live and (with
// --archived) archived listings to the named repo, leaving other repos' promotions untouched.
func TestPromotionsRepoFlagScopesListing(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	s := buildPROpenedState(t, clone)
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated repo's own state file — never resolvable via this fixture's config,
	// so it must be filtered out by --repo before ever reaching the "repo not in config file"
	// branch, not merely skipped by it.
	other := &engine.PromotionState{ID: "other-repo-promo", RepoFullName: "someone/else", TargetEnv: "prod"}
	otherPath, err := engine.StatePath(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(otherPath, other); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	got := runPromotions([]string{"--repo", "example/gitops"}, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), s.ID) {
		t.Errorf("stdout should list %s (example/gitops), got:\n%s", s.ID, out.String())
	}
	if strings.Contains(out.String(), other.ID) {
		t.Errorf("stdout should NOT list %s (a different repo), got:\n%s", other.ID, out.String())
	}
}

// TestPromotionsArchivedFlagDoesNotDoublePrintWhatThisRunJustArchived is the round-2 review's
// own regression test: a promotion that goes from live to archived DURING this same
// `hoist promotions --archived` invocation was printed once by the main loop ("done (...) —
// archived...") and then a second time by the --archived block below it, since
// ListArchivedStates() (read fresh, after the loop) already includes the file the loop itself
// just moved there.
func TestPromotionsArchivedFlagDoesNotDoublePrintWhatThisRunJustArchived(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("promote: exit %d, want 0; stderr: %s", got, errOut.String())
	}
	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("fixture precondition: want exactly one state file, got %d", len(states))
	}
	id := states[0].ID

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.State.Retain = config.Duration(time.Nanosecond)

	out.Reset()
	errOut.Reset()
	got := runPromotions([]string{"--archived"}, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	if got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if n := strings.Count(out.String(), id); n != 1 {
		t.Errorf("%s appears %d time(s) in stdout, want exactly 1:\n%s", id, n, out.String())
	}
}
