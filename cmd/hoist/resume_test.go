package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/forge"
)

func TestPromotionsEmptyStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var out, errOut bytes.Buffer
	if got := runPromotions(nil, cfg, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "no promotions found") {
		t.Fatalf("stdout = %q, want a no-promotions message", out.String())
	}
}

func TestResumeRequiresExactlyOneOfIDOrEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume(nil, cfg, &bytes.Buffer{}, &errOut); got != exitUsage {
		t.Fatalf("neither id nor --env: exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	errOut.Reset()
	if got := runResume([]string{"--env", "app-production", "some-id"}, cfg, &bytes.Buffer{}, &errOut); got != exitUsage {
		t.Fatalf("both id and --env: exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
}

func TestResumeUnknownIDFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume([]string{"no-such-id"}, cfg, &bytes.Buffer{}, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if !strings.Contains(errOut.String(), "no-such-id") {
		t.Fatalf("stderr should name the missing id: %s", errOut.String())
	}
}

func TestResumeUnknownEnvFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume([]string{"--env", "app-production"}, cfg, &bytes.Buffer{}, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if !strings.Contains(errOut.String(), "app-production") {
		t.Fatalf("stderr should name the env: %s", errOut.String())
	}
}

// TestResumeNeverStraddlesPolicyAcrossConfigEdits is the regression for the docs/#7 finding:
// internal/engine/state.go's own doc comment states the invariant that a promotion's M4 policy
// fields (CINone, Approval, Approvers, Collaborators) are read once when the promotion starts,
// "so a promotion never straddles two different policies mid-flight" — but runResume used to
// re-read them from the CURRENT config file on every resume, overwriting whatever the state file
// persisted. This starts a promotion under one approvers list (alice), edits the config file to a
// DIFFERENT approvers list (mallory) before resuming, and confirms the ORIGINAL list is what's
// actually enforced: alice's approval comment must still satisfy Approved even though the config
// file resume reads no longer names her at all. Before the fix, resume would have overwritten
// s.Approvers with [mallory], alice's comment would never match an allowed author, and the
// promotion would sit Waiting forever instead of merging.
func TestResumeNeverStraddlesPolicyAcrossConfigEdits(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	original := string(data)

	// Add an explicit comment-gated approval naming alice, and cut poll.deadline down so the
	// first promote call (which will sit Waiting at Approved forever, since no comment exists
	// yet) returns quickly instead of the fixture's default 10s.
	withApprovers := strings.Replace(original,
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    approvers: [alice]\n    envs:\n      approval:\n        app-production: comment\n",
		1)
	if withApprovers == original {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	withApprovers = strings.Replace(withApprovers, "deadline: 10s", "deadline: 2s", 1)
	if err := os.WriteFile(cfgPath, []byte(withApprovers), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected the first promote to stop waiting on approval (no comment posted yet), got success: %s", out.String())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one promotion state, got %d", len(states))
	}
	s := states[0]
	if s.PR == nil {
		t.Fatalf("expected a PR to already be open: %+v", s)
	}
	if s.Approval != "comment" || len(s.Approvers) != 1 || s.Approvers[0] != "alice" {
		t.Fatalf("expected the original comment/alice policy persisted, got Approval=%q Approvers=%v", s.Approval, s.Approvers)
	}

	// The operator edits the config to a DIFFERENT approvers list while the promotion is still
	// in flight.
	changedApprovers := strings.Replace(withApprovers, "approvers: [alice]", "approvers: [mallory]", 1)
	if changedApprovers == withApprovers {
		t.Fatal("approvers replacement point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(changedApprovers), 0o644); err != nil {
		t.Fatal(err)
	}

	// alice's approval comment arrives, posted after the PR's head commit.
	f.AddComment(s.PR.Number, forge.Comment{Author: "alice", AuthorType: "User", Body: "hoist approve " + s.ID, CreatedAt: time.Now()})

	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "resume", s.ID}, &out, &errOut); got != 0 {
		t.Fatalf("resume should have completed using the ORIGINAL approvers list (alice), got exit %d; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "merged:") {
		t.Fatalf("expected the promotion to merge once alice (the ORIGINAL approver) approved, got: %s", out.String())
	}
}

// TestResumeEnvSurfacesObservationErrorInsteadOfSilentlyExcluding is the regression for finding
// #7: runResume's --env matching used to discard a re-observation error and treat the affected
// candidate as simply "not matching" (as absent as one that had genuinely already finished) —
// which can misleadingly report "no in-flight promotion" when a transient GitHub/git failure,
// not the promotion's actual state, was the real reason nothing matched. This starts a
// promotion that sits waiting on an approval comment (never posted, so it's genuinely still in
// flight and the only candidate for its env), then makes the fake forge's FindPR (which
// MergedStep.Observe calls first) return a transient error and confirms `hoist resume --env`
// surfaces THAT error rather than silently reporting "no in-flight promotion targets ...".
func TestResumeEnvSurfacesObservationErrorInsteadOfSilentlyExcluding(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	original := string(data)

	// Gate app-production on a comment so the first promote call sits waiting rather than
	// completing end-to-end (the fixture's default approval is auto) — this promotion must
	// still genuinely be in flight for --env to have a real candidate to observe.
	withApprovers := strings.Replace(original,
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    approvers: [alice]\n    envs:\n      approval:\n        app-production: comment\n",
		1)
	if withApprovers == original {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	withApprovers = strings.Replace(withApprovers, "deadline: 10s", "deadline: 2s", 1)
	if err := os.WriteFile(cfgPath, []byte(withApprovers), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected the first promote to stop waiting on approval (no comment posted yet), got success: %s", out.String())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one promotion state, got %d", len(states))
	}
	id := states[0].ID

	// A transient forge failure — nothing to do with this promotion's actual state — hits every
	// candidate's re-observation from here on.
	f.FindErr = errors.New("transient: GitHub API returned 502")

	out.Reset()
	errOut.Reset()
	got := run([]string{"--config", cfgPath, "resume", "--env", "app-production"}, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected failure while the only candidate's observation errors, got success: %s", out.String())
	}
	if strings.Contains(errOut.String(), "no in-flight promotion targets") {
		t.Fatalf("must not report 'no in-flight promotion' when the real reason is an observation error, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "transient: GitHub API returned 502") {
		t.Fatalf("expected the underlying observation error to be surfaced, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), id) {
		t.Fatalf("expected the affected candidate's id to be named, got: %s", errOut.String())
	}
}

// TestResumeEnvSurfacesMissingRepoConfigInsteadOfSilentlyExcluding is round-6's regression: a
// state file naming a repo that has since been removed (or renamed) in config was silently
// `continue`'d past — bypassing the exact obsErrs mechanism the sibling test above already
// proves works for a transient observation error — which could misleadingly report "no
// in-flight promotion" (this being the only candidate) rather than surfacing that the candidate
// couldn't be confirmed at all.
func TestResumeEnvSurfacesMissingRepoConfigInsteadOfSilentlyExcluding(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	original := string(data)

	withApprovers := strings.Replace(original,
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    approvers: [alice]\n    envs:\n      approval:\n        app-production: comment\n",
		1)
	if withApprovers == original {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	withApprovers = strings.Replace(withApprovers, "deadline: 10s", "deadline: 2s", 1)
	if err := os.WriteFile(cfgPath, []byte(withApprovers), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected the first promote to stop waiting on approval, got success: %s", out.String())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one promotion state, got %d", len(states))
	}
	id := states[0].ID
	// The fake forge is never reached on this path — the repo is removed from config entirely
	// before resume runs, so no forge client is ever built for this candidate.

	// Remove the repo from config entirely, as if it had been deleted or renamed — the promotion
	// state file itself still names the old RepoFullName. Truncate to just the top-level
	// "repos: []" key rather than matching an exact literal, which would be fragile against
	// fixture changes.
	idx := strings.Index(withApprovers, "repos:")
	if idx == -1 {
		t.Fatal("fixture config shape changed; no repos: key found")
	}
	pollIdx := strings.Index(withApprovers, "poll:")
	if pollIdx == -1 {
		t.Fatal("fixture config shape changed; no poll: key found")
	}
	noRepo := withApprovers[:idx] + "repos: []\n" + withApprovers[pollIdx:]
	if err := os.WriteFile(cfgPath, []byte(noRepo), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	got := run([]string{"--config", cfgPath, "resume", "--env", "app-production"}, &out, &errOut)
	if got == 0 {
		t.Fatalf("expected failure when the only candidate's repo is no longer in config, got success: %s", out.String())
	}
	if strings.Contains(errOut.String(), "no in-flight promotion targets") {
		t.Fatalf("must not report 'no in-flight promotion' when the real reason is a missing repo config, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), id) {
		t.Fatalf("expected the affected candidate's id to be named, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "not in the current config") {
		t.Fatalf("expected the missing-config reason to be named, got: %s", errOut.String())
	}
}
