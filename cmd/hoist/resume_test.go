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
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
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

// TestResumeRebuildsArgoAppsForALegacyStateFile is round-1's regression for the ArgoApps gap:
// PromotionState.ArgoApps (its own doc comment, internal/engine/state.go) was added in M5 and is
// computed once when a promotion is first built — a state file saved before this field existed
// decodes it as empty. Before this fix, an empty ArgoApps read as "this promotion's plan touches
// no Argo Application" (ArgoRefreshedStep/ArgoSyncedStep's own `len(apps) == 0` shortcut), so
// resuming such a state reported the promotion done without ever checking whether the
// Application it edited actually synced — even while the fake Argo status this test configures
// is deliberately still OutOfSync throughout.
//
// This drives a real promotion to a genuine "waiting on Argo to sync" stop — its own onMerge
// hook below sets the Application's status to OutOfSync rather than newPromoteFixture's own
// already-converged default (that default exists precisely so most tests don't have to reach
// into Argo/rollout convergence at all; this one specifically needs a not-yet-synced moment to
// still be observable after the merge, so it supplies its own) — confirms ArgoApps was correctly
// populated at that point, then blanks it out to stand in for a pre-M5 state file and confirms
// `hoist resume` still correctly keeps waiting, rather than reporting false success, because it
// rebuilds ArgoApps before ever asking whether the promotion is done.
func TestResumeRebuildsArgoAppsForALegacyStateFile(t *testing.T) {
	cfgPath, clone, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// A shorter poll.deadline than the fixture's default (not the fixture's own 10s, which would
	// make this test needlessly slow), and poll.argo widened to LARGER than that deadline so
	// driveToCompletion's outer loop calls engine.Drive exactly once and is then purely sleeping
	// when ctx's deadline fires.
	//
	// That one-call shape is what keeps the phase asserted below deterministic. This test cares
	// specifically that the run is still *waiting on Argo to sync* rather than falsely reporting
	// done, and it names argo-synced to say so. With a fast poll.argo the loop makes hundreds of
	// Drive passes instead, and the deadline can just as easily fire mid-pass at argo-refreshed
	// (whose own Observe reports "already satisfied") as at the argo-synced wait — a flake that
	// says nothing about ArgoApps, which is what this test is actually for.
	//
	// Historical note, since the widening predates the reason above: it originally existed to
	// dodge a real re-push/re-delete cycle — every post-merge Drive re-observed from the top,
	// found PushedStep unsatisfied because MergedStep's Act had deleted the branch, re-pushed it,
	// and so made MergedStep re-run its own Act, burning the deadline in real git subprocesses.
	// That cycle is fixed (Drive's Merged short-circuit, internal/engine/engine.go) and is
	// covered directly by TestDriveDoesNotChurnPushDeleteWhileWaitingOnArgoAfterMerge
	// (internal/engine/steps_m5_test.go), which counts pushes and deletes against a real remote
	// rather than inferring anything from timing. The widening stays only for determinism.
	shortDeadline := strings.NewReplacer(
		"deadline: 10s", "deadline: 2s",
		"argo: 5ms", "argo: 30s",
	).Replace(string(data))
	if shortDeadline == string(data) {
		t.Fatal("fixture config shape changed; deadline/poll.argo replacement points not found")
	}
	if err := os.WriteFile(cfgPath, []byte(shortDeadline), 0o644); err != nil {
		t.Fatal(err)
	}

	originOut, err := gitHostCmd("", "-C", clone, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("reading the fixture clone's own origin: %v", err)
	}
	origin := strings.TrimSpace(string(originOut))

	app := argo.Application{Namespace: "argocd", Name: "app-app-production"}
	fakeArgo := &argo.Fake{}
	wrapped := &mergeSimulatingForge{
		Fake: f,
		onMerge: func(mergeSHA string) {
			// What a real GitHub squash-merge does to the base branch (mirroring
			// newPromoteFixture's own onMerge) — but this test's own Argo status is
			// deliberately left OutOfSync, unlike the shared fixture's already-converged
			// default, so ArgoSyncedStep has something real to still be waiting on.
			runGitHost(t, origin, "update-ref", "refs/heads/main", mergeSHA)
			fakeArgo.SetStatus(app, argo.Status{
				SyncStatus:   "OutOfSync",
				SyncRevision: "some-earlier-commit",
				HealthStatus: argo.HealthStatusHealthy,
				ReconciledAt: time.Now().Add(time.Hour), // refreshed already; just not synced yet
			})
		},
	}
	prevForge, prevArgo := newForge, newArgo
	newForge = func(string) (forge.Forge, error) { return wrapped, nil }
	newArgo = func(string) (argo.Argo, string, error) { return fakeArgo, "test-context", nil }
	t.Cleanup(func() { newForge, newArgo = prevForge, prevArgo })

	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("expected the promotion to stop waiting on Argo sync (status is OutOfSync), got success: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "argo-synced") {
		t.Fatalf("expected to be stopped at argo-synced, got: %s", errOut.String())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected exactly one promotion state, got %d", len(states))
	}
	s := states[0]
	if s.MergeSHA == "" {
		t.Fatal("expected MergedStep to have completed before the Argo steps run")
	}
	if len(s.ArgoApps) != 1 || s.ArgoApps[0] != "app-app-production" {
		t.Fatalf("expected ArgoAppNames to have populated ArgoApps at build time, got %v", s.ArgoApps)
	}
	if s.ArgoNamespace == "" {
		t.Fatal("expected ArgoNamespace to have been populated at build time")
	}

	// Simulate a state file saved by a pre-M5 hoist: ArgoApps AND ArgoNamespace decode as empty
	// because neither field existed yet, while everything else (Edits, MergeSHA, History) is
	// exactly what a real in-flight promotion carries. An empty ArgoNamespace alone (Copilot
	// review) fails Argo.Get's own input validation outright, a sharper failure than ArgoApps'
	// own "reports done too early" gap — both must be rebuilt together.
	s.ArgoApps = nil
	s.ArgoNamespace = ""
	statePath, err := engine.StatePath(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "resume", s.ID}, &out, &errOut); got == 0 {
		t.Fatalf("expected resume to still wait on Argo sync (never falsely 'done') for a legacy state with empty ArgoApps, got success: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "argo-synced") {
		t.Fatalf("expected resume to still be stopped at argo-synced (ArgoApps must have been rebuilt, not skipped), got: %s", errOut.String())
	}

	resumed, err := engine.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.ArgoApps) != 1 || resumed.ArgoApps[0] != "app-app-production" {
		t.Fatalf("expected resume to have rebuilt ArgoApps, got %v", resumed.ArgoApps)
	}
	if resumed.ArgoNamespace == "" {
		t.Fatal("expected resume to have rebuilt ArgoNamespace too, got empty")
	}
}

// TestEnsureArgoAppsLeavesAlreadyPopulatedStateAlone is ensureArgoApps' carve-out for the common
// case (every post-M5 promotion): a state that already carries ArgoApps must never be
// recomputed — state.go's own doc comment ("computed once ... then carried unchanged across
// every resume") still governs. CloneDir/AppsRoot deliberately name a path that doesn't exist,
// so a call into gitops.Discover here would fail loudly — proving this path never attempts one.
func TestEnsureArgoAppsLeavesAlreadyPopulatedStateAlone(t *testing.T) {
	s := &engine.PromotionState{
		ArgoApps: []string{"already-set"},
		Edits:    []gitops.Edit{{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/app/deployment.yaml"}}},
		CloneDir: "/does/not/exist",
	}
	rc := config.RepoConfig{AppsRoot: "cluster/apps"}
	if err := ensureArgoApps(s, rc); err != nil {
		t.Fatalf("ensureArgoApps = %v, want nil (already populated, must never attempt discovery)", err)
	}
	if len(s.ArgoApps) != 1 || s.ArgoApps[0] != "already-set" {
		t.Fatalf("ArgoApps = %v, want left untouched", s.ArgoApps)
	}
}

// TestEnsureArgoAppsLeavesGenuinelyEditlessStateAlone is ensureArgoApps' other carve-out: a
// state with no Edits at all has nothing for ArgoAppNames to have ever found regardless of when
// it was built (ArgoAppNames' own contract: it only ever produces an app name from an edit's own
// directory) — an empty ArgoApps here is not evidence of a pre-M5 state, so this must not attempt
// discovery either. Same deliberately-nonexistent CloneDir/AppsRoot as the sibling test above.
func TestEnsureArgoAppsLeavesGenuinelyEditlessStateAlone(t *testing.T) {
	s := &engine.PromotionState{
		ArgoApps: nil,
		Edits:    nil,
		CloneDir: "/does/not/exist",
	}
	rc := config.RepoConfig{AppsRoot: "cluster/apps"}
	if err := ensureArgoApps(s, rc); err != nil {
		t.Fatalf("ensureArgoApps = %v, want nil (no edits, nothing to rebuild)", err)
	}
	if s.ArgoApps != nil {
		t.Fatalf("ArgoApps = %v, want nil", s.ArgoApps)
	}
}

// TestResumeDrivesADirectPromotionAsDirectNotAsAPR is the regression for the P1 Codex found on
// PR #43, now that resume can actually do something about it. A --direct promotion pushes to
// Base and never populates s.Branch on origin, so re-driving it through AllSteps finds
// PushedStep unsatisfied, pushes the branch, opens a PR and merges it — the exact shape
// --direct exists to avoid, with three real writes.
//
// The first fix was a refusal, on the belief that runResume could not reach envs.production for
// DirectCommitGateStep. It can (repoConfigFor resolves the repo for the loaded state), so resume
// now drives AllDirectSteps instead of declining. This asserts the promotion completes as a
// direct one and that no PR is ever created.
func TestResumeDrivesADirectPromotionAsDirectNotAsAPR(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withEnvs := strings.Replace(string(data),
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    envs:\n      production: [somewhere-else]\n",
		1)
	if withEnvs == string(data) {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(withEnvs), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production",
		"--direct", "--confirm-direct", "app-production"}
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("direct promote failed: %s / %s", out.String(), errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("direct mode must not open a PR: %+v", f.PRs())
	}

	states, err := engine.ListStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || !states[0].Direct {
		t.Fatalf("expected one state recording Direct, got %+v", states)
	}
	id := states[0].ID

	// The regression: resuming it must re-drive it as a direct promotion.
	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "resume", id}, &out, &errOut); got != 0 {
		t.Fatalf("resume of a direct promotion failed: %s / %s", out.String(), errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("resume must not open a PR for a direct promotion: %+v", f.PRs())
	}
	if strings.Contains(out.String(), "PR:") {
		t.Errorf("resume output should not report a PR for a direct promotion:\n%s", out.String())
	}
}
