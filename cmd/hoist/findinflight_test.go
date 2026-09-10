// The findInFlight regression tests: restored verbatim from before M10/4 replaced this file's
// predecessor with the in-flight pane adaptor's tests. findInFlight itself (drive.go) never
// left, and neither may the cases its comment still cites.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// satisfiedRollout builds a rollout.Fake pre-configured so every Deployment edits touches
// already reports its image matching and its rollout complete — the M4-era in-flight tests
// below don't care about M5's own rollout mechanics, only that AllSteps (now ten steps, not
// seven) can still reach "done" in one pass. Argo is left nil throughout: these fixtures never
// set PromotionState.ArgoApps, so ArgoRefreshedStep/ArgoSyncedStep's own "no Application in
// this promotion's plan" short-circuit means neither ever calls it.
func satisfiedRollout(namespace string, edits []gitops.Edit) *rollout.Fake {
	f := &rollout.Fake{}
	byName := map[string][]rollout.ContainerImage{}
	for _, e := range edits {
		if e.Kind != "Deployment" {
			continue
		}
		byName[e.Name] = append(byName[e.Name], rollout.ContainerImage{
			Name: e.Container, Init: strings.Contains(e.Path, "initContainers"), Image: e.New.String(),
		})
	}
	for name, imgs := range byName {
		f.SetDeployment(namespace, name, rollout.DeploymentStatus{Namespace: namespace, Name: name, Images: imgs, Complete: true})
	}
	return f
}

// TestFindInFlightRefusesWhenAnotherPromotionIsMidFlight is AGENTS.md invariant 5 (one
// in-flight promotion per target env), exercised directly against findInFlight rather than the
// full CLI: a promotion left waiting on approval must be reported in flight for its target env
// (blocking a would-be second `hoist promote` for the same env), and once it actually finishes
// (merged, branch deleted) it must no longer conflict with anything.
func TestFindInFlightRefusesWhenAnotherPromotionIsMidFlight(t *testing.T) {
	_, clone, f := newPromoteFixture(t)

	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "comment",
		Approvers:     []string{"alice"},
		CINone:        "green",
	}

	ro := satisfiedRollout(plan.TargetEnv, plan.Edits)

	// Drive to PROpened only (the four M3 steps) and leave it there — never approved, so
	// ObserveAll's own walk through CIGreen/Approved must stop at Approved and report "not
	// done".
	if err := engine.Drive(context.Background(), engine.Steps(newGit, f, nil), s, nil); err != nil {
		t.Fatalf("driving to PROpened: %v", err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	conflict, status, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-production", "a-different-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict == nil {
		t.Fatal("expected the mid-flight promotion to be reported as in flight")
	}
	if conflict.ID != id {
		t.Fatalf("conflict.ID = %s, want %s", conflict.ID, id)
	}
	if status.Step != engine.StepApproved {
		t.Fatalf("expected to be stuck at %s, got %s", engine.StepApproved, status.Step)
	}

	// A distinct id for a *different* target env must never conflict, in-flight or not.
	if conflict, _, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-staging", "a-different-id"); err != nil {
		t.Fatal(err)
	} else if conflict != nil {
		t.Fatalf("a different target env must never conflict: %+v", conflict)
	}

	// Finish it for real (auto approval, then merge) and persist that — findInFlight must then
	// see it as done and stop conflicting.
	s.Approval = "auto"
	if err := engine.Drive(context.Background(), engine.AllSteps(newGit, f, nil, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v", err)
	}
	// Simulate what a real GitHub squash-merge would actually do to the base branch: forge.Fake
	// never touches real git, but MergedStep's Observe now revalidates origin/main's live tip
	// against this promotion's own edits before trusting a historical merge record (M4
	// hardening, finding #1) — findInFlight's own ObserveAll call below would otherwise
	// correctly refuse to call this promotion done.
	runGitHost(t, clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}
	if conflict, status, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-production", "a-different-id"); err != nil {
		t.Fatal(err)
	} else if conflict != nil {
		t.Fatalf("a fully done (merged, branch deleted) promotion must no longer conflict: stuck at %s: %+v", status.Step, status.Observation)
	}
}

// TestFindInFlightDoesNotBlockNewPromotionAfterPriorOneFullyMerged is the dedicated regression
// test for engine.ObserveAll's last-step-first short-circuit (engine.go), independent of the
// mid-flight scenario above: a promotion driven all the way through MergedStep (merged, origin
// branch deleted by MergedStep's own Act) must be recognized as done, never as "stuck", so it
// never blocks a brand-new promotion id for the same target env. Without the short-circuit, a
// naive in-order re-observation would hit PushedStep first, find the branch MergedStep just
// deleted, and misreport this completed promotion as unsatisfied at Pushed forever — exactly
// the bug engine.go's doc comment names ("a *finished* promotion would block every future one
// for the same env, forever").
func TestFindInFlightDoesNotBlockNewPromotionAfterPriorOneFullyMerged(t *testing.T) {
	_, clone, f := newPromoteFixture(t)

	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "auto",
		CINone:        "green",
	}

	ro := satisfiedRollout(plan.TargetEnv, plan.Edits)

	// Drive this one promotion all the way through Merged: merged, and its origin branch
	// deleted by MergedStep's own Act (AllSteps' full ten steps, not just the four M3 ones).
	if err := engine.Drive(context.Background(), engine.AllSteps(newGit, f, nil, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v", err)
	}
	// Simulate what a real GitHub squash-merge would actually do to the base branch: forge.Fake
	// never touches real git, but MergedStep's Observe now revalidates origin/main's live tip
	// against this promotion's own edits before trusting a historical merge record (M4
	// hardening, finding #1) — findInFlight's own ObserveAll call below would otherwise
	// correctly refuse to call this promotion done.
	runGitHost(t, clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	// A brand-new promotion id targeting the same env must not be refused: the completed one
	// re-observes as done, not as an in-flight conflict.
	conflict, status, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a fully completed (merged, branch deleted) promotion must not block a new one for the same env: reported stuck at %s: %+v", status.Step, status.Observation)
	}
}

// TestFindInFlightDoesNotBlockAfterMergeWithRolloutPending is the disputed scenario named in
// findInFlight's own doc comment: a promotion driven to Merged, but whose Argo sync/rollout has
// not converged yet, must not be treated as still "in flight" for AGENTS.md invariant 5 — that
// invariant exists to stop two promotions racing to create separate branches/PRs/merges for the
// same target env, a risk fully retired the instant a merge lands. Unlike
// TestFindInFlightDoesNotBlockNewPromotionAfterPriorOneFullyMerged above (which uses
// satisfiedRollout so the *whole* ten-step AllSteps reaches done, exercising ObserveAll's
// last-step short-circuit instead), this test deliberately leaves the rollout fake unconfigured
// so RolledOutStep never reports Satisfied — proving findInFlight's own CoreSteps scoping is
// what makes this promotion stop conflicting, not incidental full completion.
func TestFindInFlightDoesNotBlockAfterMergeWithRolloutPending(t *testing.T) {
	_, clone, f := newPromoteFixture(t)

	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      clone,
		WorktreeDir:   wt,
		Base:          "main",
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle:       engine.PRTitle(plan),
		PRBody:        engine.RenderPRBody(id, plan),
		Approval:      "auto",
		CINone:        "green",
	}

	// Deliberately unconfigured: no Deployment status is ever set, so RolledOutStep's Observe
	// never finds a matching live image and never reports Satisfied — the rollout "never
	// converges", standing in for a slow or stuck Deployment.
	ro := &rollout.Fake{}

	err = engine.Drive(context.Background(), engine.AllSteps(newGit, f, nil, ro, nil), s, nil)
	if !errors.Is(err, engine.ErrWaiting) {
		t.Fatalf("expected Drive to stop waiting on the rollout, got %v", err)
	}
	if s.MergeSHA == "" {
		t.Fatal("expected the promotion to have actually merged before Drive stopped at the rollout")
	}
	if s.Phase != engine.StepRolledOut {
		t.Fatalf("expected Drive to be stuck at %s, got %s", engine.StepRolledOut, s.Phase)
	}
	// Simulate what a real GitHub squash-merge would actually do to the base branch: forge.Fake
	// never touches real git, but MergedStep's Observe now revalidates origin/main's live tip
	// against this promotion's own edits before trusting a historical merge record (M4
	// hardening, finding #1) — findInFlight's own ObserveAll call below would otherwise
	// correctly refuse to call this promotion done, exactly as in
	// TestFindInFlightDoesNotBlockNewPromotionAfterPriorOneFullyMerged above.
	runGitHost(t, clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	// The disputed question: does a brand-new promotion for the same target env conflict with
	// this one? Design choice (see findInFlight's doc comment): no — Merged already retired the
	// only conflict invariant 5 protects against.
	conflict, status, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a merged promotion must not block a new one for the same env just because its Argo/rollout convergence is still pending: reported stuck at %s: %+v", status.Step, status.Observation)
	}
}

// TestFindInFlightDoesNotBlockAfterASupersededDirectDeploy is #166's operator-visible defect,
// written from the attacker's side: the state that wedged a real env, and the request it
// refused.
//
// findInFlight observes a DIRECT state through DirectPushedStep, which judges whether the base
// still carries the promotion's planned content. A later deploy into the same env rewrites the
// very same image scalar, so by blob hash that is indistinguishable from a revert — and the
// step reported unsatisfied, i.e. still in flight, i.e. every subsequent promotion or deploy
// into that env refused, permanently, with no recovery but deleting the state file by hand.
// spritz-staging sat that way for four days.
func TestFindInFlightDoesNotBlockAfterASupersededDirectDeploy(t *testing.T) {
	_, clone, f := newPromoteFixture(t)

	r, err := gitops.Discover(clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := engine.DeriveID("example/gitops", plan)
	wt, err := engine.WorktreeDir(id)
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := engine.StatePath(id)
	if err != nil {
		t.Fatal(err)
	}
	s := &engine.PromotionState{
		ID:            id,
		RepoFullName:  "example/gitops",
		SourceEnv:     plan.SourceEnv,
		TargetEnv:     plan.TargetEnv,
		Branch:        engine.BranchName(plan.TargetEnv, id),
		CloneDir:      clone,
		WorktreeDir:   wt,
		Base:          "main",
		Direct:        true,
		Edits:         plan.Edits,
		CommitMessage: engine.RenderCommitMessage(id, plan),
	}
	if err := engine.Drive(context.Background(), engine.DirectSteps(newGit, nil, true, nil), s, nil); err != nil {
		t.Fatalf("driving the direct deploy: %v", err)
	}
	if err := engine.SaveState(statePath, s); err != nil {
		t.Fatal(err)
	}

	// A LATER deploy into the same env replaces the reference this one wrote, from a separate
	// clone of the same origin — exactly what pressing enter on the tag picker a second time
	// does.
	origin := strings.TrimSpace(outGitHost(t, clone, "remote", "get-url", "origin"))
	other := filepath.Join(t.TempDir(), "later-deploy")
	runGitHost(t, "", "clone", "-q", origin, other)
	p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	newer := "ghcr.io/example/app:v3@sha256:" + strings.Repeat("2", 64)
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.Contains(line, "image: ghcr.io/") {
			lines[i] = "          image: " + newer
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitHost(t, other, "commit", "-q", "-a", "-m", "a later deploy into the same env")
	runGitHost(t, other, "push", "-q", "origin", "main")

	conflict, status, err := findInFlight(context.Background(), newGit, f, "example/gitops", "app-production", "a-brand-new-id")
	if err != nil {
		t.Fatal(err)
	}
	if conflict != nil {
		t.Fatalf("a direct deploy superseded by a later one has landed and is finished; it must not refuse every later promotion into %s forever — reported stuck at %s: %+v",
			"app-production", status.Step, status.Observation)
	}
}
