package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// fileStore avoids the composite-literal-in-if-condition ambiguity FileStore{}.Save(...) would
// otherwise need parens for at every call site below.
var fileStore = FileStore{}

// The three EnsureArgoApps tests below moved unchanged from cmd/hoist/resume_test.go, alongside
// the function itself (PR E).

// TestEnsureArgoAppsLeavesAlreadyPopulatedStateAlone is EnsureArgoApps' carve-out for the common
// case (every post-M5 promotion): a state that already carries ArgoApps must never be
// recomputed — state.go's own doc comment ("computed once ... then carried unchanged across
// every resume") still governs. CloneDir/AppsRoot deliberately name a path that doesn't exist,
// so a call into gitops.Discover here would fail loudly — proving this path never attempts one.
func TestEnsureArgoAppsLeavesAlreadyPopulatedStateAlone(t *testing.T) {
	s := &engine.PromotionState{
		ArgoApps: []string{"already-set"},
		EditApps: map[string]string{"cluster/apps/app-production/app/deployment.yaml": "already-set"},
		Edits:    []gitops.Edit{{Occurrence: gitops.Occurrence{File: "cluster/apps/app-production/app/deployment.yaml"}}},
		CloneDir: "/does/not/exist",
	}
	rc := config.RepoConfig{AppsRoot: "cluster/apps"}
	if err := EnsureArgoApps(s, rc); err != nil {
		t.Fatalf("EnsureArgoApps = %v, want nil (already populated, must never attempt discovery)", err)
	}
	if len(s.ArgoApps) != 1 || s.ArgoApps[0] != "already-set" {
		t.Fatalf("ArgoApps = %v, want left untouched", s.ArgoApps)
	}
	if len(s.EditApps) != 1 || s.EditApps["cluster/apps/app-production/app/deployment.yaml"] != "already-set" {
		t.Fatalf("EditApps = %v, want left untouched", s.EditApps)
	}
}

// TestEnsureArgoAppsBackfillsEditAppsWhenArgoAppsAlreadyPopulated proves the round-2 repair
// path: a state file saved any time between M5 (ArgoApps) and EditApps' own introduction has
// ArgoApps populated but EditApps nil, and the old "ArgoApps non-empty means fully populated,
// skip everything" check would leave EditApps nil forever. Drives a real gitops.Discover
// against the shared inflightFixture's own clone (in place of cmd/hoist's own
// newPromoteFixture, which this package does not have — the fixture's layout is identical: one
// family, "app", under cluster/apps/<env>).
func TestEnsureArgoAppsBackfillsEditAppsWhenArgoAppsAlreadyPopulated(t *testing.T) {
	fx := newInflightFixture(t)
	const file = "cluster/apps/app-production/app/deployment.yaml"
	s := &engine.PromotionState{
		TargetEnv: "app-production",
		CloneDir:  fx.clone,
		ArgoApps:  []string{"app-app-production"},
		Edits:     []gitops.Edit{{Occurrence: gitops.Occurrence{File: file}}},
	}
	rc := config.RepoConfig{AppsRoot: "cluster/apps"}
	if err := EnsureArgoApps(s, rc); err != nil {
		t.Fatalf("EnsureArgoApps = %v, want nil", err)
	}
	if len(s.ArgoApps) != 1 || s.ArgoApps[0] != "app-app-production" {
		t.Fatalf("ArgoApps = %v, want left untouched (only EditApps was missing)", s.ArgoApps)
	}
	if len(s.EditApps) != 1 || s.EditApps[file] != "app-app-production" {
		t.Fatalf("EditApps = %v, want {%q: \"app-app-production\"}", s.EditApps, file)
	}
}

// TestEnsureArgoAppsLeavesGenuinelyEditlessStateAlone is EnsureArgoApps' other carve-out: a
// state with no Edits at all has nothing for ArgoAppNames to have ever found regardless of when
// it was built — an empty ArgoApps here is not evidence of a pre-M5 state, so this must not
// attempt discovery either.
func TestEnsureArgoAppsLeavesGenuinelyEditlessStateAlone(t *testing.T) {
	s := &engine.PromotionState{
		ArgoApps: nil,
		Edits:    nil,
		CloneDir: "/does/not/exist",
	}
	rc := config.RepoConfig{AppsRoot: "cluster/apps"}
	if err := EnsureArgoApps(s, rc); err != nil {
		t.Fatalf("EnsureArgoApps = %v, want nil (no edits, nothing to rebuild)", err)
	}
	if s.ArgoApps != nil {
		t.Fatalf("ArgoApps = %v, want nil", s.ArgoApps)
	}
}

// withConfig returns a Service built exactly like fx.svc, except its Settings.Config now names
// fx's own repo (config.RepoConfig, dereferenced from set.Repo) — the inflightFixture leaves
// Config nil, since FindInFlight/claimTarget's own tests never need it, but List/Resume/Abandon
// all resolve RepoConfigFor(s.settings.Config, ...) and need a real one to find.
func withConfig(fx inflightFixture) *Service {
	set := fx.svc.Settings()
	rc := *set.Repo
	// The fixture's own Argo fake is keyed under the "argocd" namespace (newInflightFixture's
	// own argo.Application{Namespace: "argocd", ...}), but the bare RepoConfig it builds never
	// sets Kube.ArgoNamespace (FindInFlight/claimTarget's own tests never reach Argo at all) —
	// List/Resume both read it from here (EnsureArgoApps, and Resume's own re-read), so it has
	// to be set for either to find the fixture's own Application.
	rc.Kube.ArgoNamespace = "argocd"
	set.Config = &config.Config{Repos: []config.RepoConfig{rc}}
	return New(set, fx.svc.deps)
}

// buildPROpenedPromotionsState drives fx's own app-staging -> app-production plan through
// exactly the PR-opening steps (branch, commit, push, open the PR) — the same shape
// cmd/hoist/abandon_test.go's buildPROpenedState builds, reimplemented here since that
// function lives in package main.
func buildPROpenedPromotionsState(t *testing.T, fx inflightFixture) *engine.PromotionState {
	t.Helper()
	r, err := gitops.Discover(fx.clone, "cluster/apps")
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
	s := &engine.PromotionState{
		ID: id, RepoFullName: "example/gitops", SourceEnv: plan.SourceEnv, TargetEnv: plan.TargetEnv,
		Branch: engine.BranchName(plan.TargetEnv, id), CloneDir: fx.clone, WorktreeDir: wt, Base: "main",
		Edits: plan.Edits, CommitMessage: engine.RenderCommitMessage(id, plan),
		PRTitle: engine.PRTitle(plan), PRBody: engine.RenderPRBody(id, plan),
		Approval: "auto", CINone: "green",
	}
	if err := engine.Drive(context.Background(), engine.Steps(git.Exec{}, fx.f, nil), s, nil); err != nil {
		t.Fatalf("driving to PR-opened: %v", err)
	}
	return s
}

// TestFindReturnsNotFoundForUnknownID is Find's own contract: an id with no state file is a
// *NotFoundError naming that id, not a plain error a caller has to string-match.
func TestFindReturnsNotFoundForUnknownID(t *testing.T) {
	fx := newInflightFixture(t)
	_, err := fx.svc.Find("no-such-id")
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("Find = %v (%T), want *NotFoundError", err, err)
	}
	if nf.ID != "no-such-id" {
		t.Fatalf("NotFoundError.ID = %q, want no-such-id", nf.ID)
	}
}

// TestFindRefusesAnIDContainingAPathSeparator pins t1-review.md P3: engine.StatePath joins id
// straight into the promotions directory with no containment check of its own, so an id naming
// a path — most concretely "archive/<real-id>", reaching into ArchiveDir where an archived or
// retention-aged-out promotion lives — must be refused by Find itself, before it ever reaches
// that join, rather than resolving to a file `hoist promotions` would never have listed as live.
func TestFindRefusesAnIDContainingAPathSeparator(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)

	st := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := fileStore.Archive(st.ID); err != nil {
		t.Fatal(err)
	}

	archived := "archive/" + st.ID
	if _, err := svc.Find(archived); err == nil {
		t.Fatalf("Find(%q) succeeded, want a path-separator id refused outright", archived)
	} else {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("Find(%q) = %v (%T), want *NotFoundError", archived, err, err)
		}
	}
	if _, err := svc.Resume(context.Background(), archived, ResumeOpts{}); err == nil {
		t.Fatalf("Resume(%q) succeeded, want a path-separator id refused outright", archived)
	}
	if _, err := svc.Abandon(context.Background(), archived); err == nil {
		t.Fatalf("Abandon(%q) succeeded, want a path-separator id refused outright", archived)
	}
}

// TestListReportsUnconfiguredRepo is List's own carve-out for a state whose repo has since left
// the config file: it is reported (Listed.Unconfigured), never dropped, and never attempts to
// build a forge/Argo/rollout client for it.
func TestListReportsUnconfiguredRepo(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	orphan := &engine.PromotionState{ID: "orphan01", RepoFullName: "someone/else", TargetEnv: "app-production"}
	if err := fileStore.Save(orphan); err != nil {
		t.Fatal(err)
	}
	listed, err := svc.List(context.Background(), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Unconfigured || listed[0].State.ID != "orphan01" {
		t.Fatalf("List = %+v, want one Unconfigured entry for orphan01", listed)
	}
}

// TestListOfEmptyStoreListsNothing pins List's other end of TestListReportsUnconfiguredRepo:
// with no state files on disk at all, List must return an empty slice and no error, never a nil
// dereference or a spurious entry (t1-review.md P1 #2, restoring coverage lost when
// cmd/hoist/inflight_test.go's TestBuildInFlightFuncsListsAndNamesTheUnobservable was deleted).
func TestListOfEmptyStoreListsNothing(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	listed, err := svc.List(context.Background(), ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("List of an empty store = %+v, want none", listed)
	}
}

// TestResumeOfOrphanStateErrors pins the other half of the orphan-state coverage lost when
// cmd/hoist/inflight_test.go's TestBuildInFlightFuncsListsAndNamesTheUnobservable was deleted
// (t1-review.md P1 #2): Resume of a state whose repo has since left the config file must error
// naming "not in the config file", not build a Driver against a repo it cannot configure a
// forge/Argo/rollout client for.
func TestResumeOfOrphanStateErrors(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	orphan := &engine.PromotionState{ID: "orphan01", RepoFullName: "someone/else", TargetEnv: "app-production"}
	if err := fileStore.Save(orphan); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Resume(context.Background(), "orphan01", ResumeOpts{})
	if err == nil || !strings.Contains(err.Error(), "not in the config file") {
		t.Fatalf("Resume(orphan) = %v, want an error containing %q", err, "not in the config file")
	}
}

// TestResumeOfUnknownIDErrors pins Resume's own contract for an id with no state file at all —
// restored alongside TestResumeOfOrphanStateErrors from the same deleted coverage.
func TestResumeOfUnknownIDErrors(t *testing.T) {
	fx := newInflightFixture(t)
	svc := withConfig(fx)
	if _, err := svc.Resume(context.Background(), "no-such-id", ResumeOpts{}); err == nil {
		t.Fatal("Resume of an unknown id: expected an error, got nil")
	}
}

// TestListArchivesDoneOlderThanRetention is the #181 retention behaviour List must preserve
// exactly: a promotion List itself just confirmed Done, whose LastActivity is older than
// ArchiveDoneOlderThan, is archived as part of the SAME call (Listed.Archived), and a later
// ListArchived call finds it there instead.
func TestListArchivesDoneOlderThanRetention(t *testing.T) {
	fx := newInflightFixture(t)
	r, err := gitops.Discover(fx.clone, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	s := buildPROpenedPromotionsState(t, fx)
	s.Approval = "auto"
	// ArgoApps/EditApps/ArgoNamespace set exactly as StartPromotion always computes them at
	// construction time, so List's own EnsureArgoApps repair never rebuilds them from a stale
	// gitops.Discover after the fact. The forge here must be the WRAPPED one (fx.svc.deps.Forge,
	// which shares fx.f's own underlying *forge.Fake but also runs the fixture's onMerge
	// callback) rather than fx.f directly — driving a real merge through the raw fake would
	// never update origin's own ref or the fixture's fake Argo status, leaving ArgoSyncedStep
	// comparing against origin's ORIGINAL tip forever (found by direct inspection: Argo's own
	// SyncRevision read back the seed commit, not the merge, because fx.f skips exactly the
	// onMerge wiring inflight_test.go's own scenarios never need since they all drive with a nil
	// Argo client).
	argoApps, err := engine.ArgoAppNames(r, s.TargetEnv, s.Edits)
	if err != nil {
		t.Fatal(err)
	}
	editApps, err := engine.EditApps(r, s.TargetEnv, s.Edits)
	if err != nil {
		t.Fatal(err)
	}
	s.ArgoNamespace = "argocd"
	s.ArgoApps = argoApps
	s.EditApps = editApps

	wrappedForge, err := fx.svc.deps.Forge("example/gitops")
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := fx.svc.deps.Argo("")
	if err != nil {
		t.Fatal(err)
	}
	ro, _, err := fx.svc.deps.Rollout("")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Drive(context.Background(), engine.AllSteps(git.Exec{}, wrappedForge, a, ro, nil), s, nil); err != nil {
		t.Fatalf("driving to done: %v (phase=%s)", err, s.Phase)
	}
	runGitHost(t, fx.clone, "push", "-q", "origin", s.CommitSHA+":refs/heads/"+s.Base)

	// Push the promotion's LastActivity well outside a short retention window, exactly as an
	// old, terminal promotion would look days or weeks later.
	old := time.Now().Add(-48 * time.Hour)
	s.GeneratedAt = old
	for i := range s.History {
		s.History[i].At = old
	}
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}

	svc := withConfig(fx)
	listed, err := svc.List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Done || !listed[0].Archived || listed[0].ArchiveErr != nil {
		t.Fatalf("List = %+v, want one Done, Archived entry", listed)
	}

	live, err := fileStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("expected the archived promotion to be gone from the live listing, got %+v", live)
	}
	arch, err := svc.ListArchived("")
	if err != nil {
		t.Fatal(err)
	}
	if len(arch) != 1 || arch[0].ID != s.ID {
		t.Fatalf("ListArchived = %+v, want %s archived", arch, s.ID)
	}
}

// TestResumeAppliesOverrideCINoneAndCarriesForward proves Resume seeds CINoneOverride from
// ResumeOpts and never re-reads the M4 policy fields (CINone/Approval/...) from the current
// config — the state file's own persisted values are trusted as-is, exactly as
// cmd/hoist/resume_test.go's TestResumeNeverStraddlesPolicyAcrossConfigEdits already proves at
// the CLI integration level; this pins the same invariant directly against Resume's own return.
func TestResumeAppliesOverrideCINoneAndCarriesForward(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	s.CINone = "prompt"
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	svc := withConfig(fx)
	d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{OverrideCINone: true})
	if err != nil {
		t.Fatal(err)
	}
	got := d.State()
	if !got.CINoneOverride {
		t.Fatal("expected Resume to seed CINoneOverride from ResumeOpts")
	}
	if got.CINone != "prompt" {
		t.Fatalf("CINone = %q, want the persisted prompt policy left untouched", got.CINone)
	}
}

// TestResumeDoesNotMirrorHistoryIntoProgress: a resumed drive's step outcomes are PromotionState
// History entries, which the flight screen already renders; Hooks.Progress is preflight-only, so
// driving a real promotion to completion must report no progress line at all (the earlier
// "<step>: <detail>" mirror put every event on screen twice). The positive control is that the
// drive did record history.
func TestResumeDoesNotMirrorHistoryIntoProgress(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}
	svc := withConfig(fx)

	var lines []string
	d, err := svc.Resume(context.Background(), s.ID, ResumeOpts{Hooks: Hooks{Progress: func(line string) {
		lines = append(lines, line)
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background(), RunHooks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.State().History) == 0 {
		t.Fatal("positive control: the drive recorded no history, so the assertion below proves nothing")
	}
	if len(lines) != 0 {
		t.Fatalf("Resume's Driver mirrored history into Progress: %q", lines)
	}
}

// TestAbandonRefusesALandedPromotion mirrors cmd/hoist/abandon_test.go's own CLI-level test at
// the service boundary: abandoning is not a rollback, so a promotion Abandon itself observes as
// already merged must be refused, leaving its state file untouched.
func TestAbandonRefusesALandedPromotion(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	s.Approval = "auto"
	// Abandon's own landed-check observes the git/forge-only core (Argo/rollout both nil,
	// promotions.go's own Abandon) — reaching Merged is enough to prove the refusal, exactly as
	// cmd/hoist/abandon_test.go's own TestAbandonRefusesAMergedPromotionEvenWithBranchStillPresent
	// only drives CoreSteps too.
	if err := engine.Drive(context.Background(), engine.CoreSteps(git.Exec{}, fx.f, nil), s, nil); err != nil {
		t.Fatalf("driving to merged: %v", err)
	}
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}

	svc := withConfig(fx)
	if _, err := svc.Abandon(context.Background(), s.ID); err == nil {
		t.Fatal("Abandon accepted a landed promotion")
	}
	if got, err2 := fileStore.Load(s.ID); err2 != nil || got == nil {
		t.Errorf("refused abandon must leave the state file in place: Load = %v, %v", got, err2)
	}
}

// TestAbandonClosesPRAndDeletesBranchAndState mirrors the sibling CLI-level test: abandoning a
// promotion that only ever reached PR-opened closes the PR, deletes the remote branch and
// deletes the state file.
func TestAbandonClosesPRAndDeletesBranchAndState(t *testing.T) {
	fx := newInflightFixture(t)
	s := buildPROpenedPromotionsState(t, fx)
	if s.PR == nil {
		t.Fatal("fixture precondition: a PR should be open")
	}
	if err := fileStore.Save(s); err != nil {
		t.Fatal(err)
	}

	svc := withConfig(fx)
	lines, err := svc.Abandon(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Abandon: %v (lines so far: %v)", err, lines)
	}

	pr, ok, err := fx.f.FindPR(context.Background(), s.Branch, engine.Marker(s.ID))
	if err != nil || !ok {
		t.Fatalf("finding the PR after abandon: ok=%v err=%v", ok, err)
	}
	if !pr.Closed {
		t.Errorf("PR #%d should be closed after abandon: %+v", pr.Number, pr)
	}
	var gitExec git.Exec
	if _, exists, err := gitExec.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Branch); err != nil || exists {
		t.Errorf("branch %s should be gone from origin after abandon: exists=%v err=%v", s.Branch, exists, err)
	}
	if got, err2 := fileStore.Load(s.ID); err2 != nil || got != nil {
		t.Errorf("state file should be deleted after abandon: Load = %v, %v", got, err2)
	}
}
