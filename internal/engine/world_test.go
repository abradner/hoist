package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/rollout"
)

// world is the scenario harness's mutable universe (#PR3): one real git fixture (a bare origin
// plus a clone, from newFixture — every op Drive itself runs is real git) and one shared set of
// fakes (forge/argo/rollout), advanced between Drive passes by its own verbs rather than by a
// scenario reaching into engine internals directly. Every existing engine test asks "given this
// fake's answer, what does one step return"; a world asks "after this SEQUENCE of real events,
// can the system still make progress" — the class of bug #165/#166 were (the system stopped
// admitting progress; no single step's own Observe was wrong for its own inputs).
type world struct {
	t     *testing.T
	fx    fixture
	g     git.Git
	forge *mergeSimulatingForge
	argo  *argo.Fake
	ro    *rollout.Fake

	// wtDirs caches one worktree path per promotion id — a real process reuses the same
	// deterministic $XDG_CACHE_HOME/hoist/worktrees/<id> across a kill and a resume
	// (WorktreeDir), never a fresh one; without this, resume's own fresh PromotionState would
	// try to `git worktree add` the identical deterministic branch a second time while the
	// original's own worktree still had it checked out, and collide.
	wtDirs map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fx := newFixture(t)
	f := &mergeSimulatingForge{Fake: &forge.Fake{}, t: t, cloneDir: fx.cloneDir, base: "main"}
	return &world{t: t, fx: fx, g: git.Exec{}, forge: f, argo: &argo.Fake{}, ro: &rollout.Fake{}}
}

// mergeSimulatingForge wraps forge.Fake so a successful MergePR also pushes the merged commit
// onto origin's base branch. forge.Fake has no git access at all, but MergedStep.Observe's own
// revert-check (mergeWasReverted) needs the base to genuinely carry the merge commit, the same
// way a real GitHub squash-merge would leave it — without this, every PR-path scenario would
// wedge at MergedStep forever, reading its own successful merge as a revert. Mirrors
// cmd/hoist/promote_test.go's own wrapper of the same name and purpose, one package over.
type mergeSimulatingForge struct {
	*forge.Fake
	t        *testing.T
	cloneDir string
	base     string
}

func (m *mergeSimulatingForge) MergePR(ctx context.Context, prNumber int, expectedHeadSHA string) (forge.PR, error) {
	pr, err := m.Fake.MergePR(ctx, prNumber, expectedHeadSHA)
	if err != nil {
		return pr, err
	}
	sha := expectedHeadSHA
	if sha == "" {
		sha = pr.HeadSHA
	}
	runHost(m.t, m.cloneDir, "push", "-q", "origin", sha+":refs/heads/"+m.base)
	return pr, nil
}

// drive runs one full Drive pass over steps, tolerating ErrWaiting and *BlockedError as ordinary
// stopping points — a promotion waiting on CI or blocked on a real, named conflict is exactly
// what a scenario is testing, not a harness failure. Anything else (a *StepError, a plain error)
// fails the test loudly: the harness's own fakes should never produce one.
func (w *world) drive(s *PromotionState, steps []Step) {
	w.t.Helper()
	err := Drive(context.Background(), steps, s, nil)
	if err == nil || errors.Is(err, ErrWaiting) {
		return
	}
	var be *BlockedError
	if errors.As(err, &be) {
		return
	}
	w.t.Fatalf("Drive: %v", err)
}

func (w *world) prSteps() []Step { return AllSteps(w.g, w.forge, w.argo, w.ro, nil) }

func (w *world) directSteps() []Step {
	return AllDirectSteps(w.g, w.argo, w.ro, nil, true, nil)
}

func (w *world) stepsFor(s *PromotionState) []Step {
	if s.Direct {
		return w.directSteps()
	}
	return w.prSteps()
}

// newPromotionState builds a fresh, unresolved PromotionState for this world's one fixture
// plan — DeriveID is deterministic over (repoFullName, plan), so every state built this way for
// this world names the identical branch/PR/state id, exactly as a real restarted process would
// (AGENTS.md §4.1). Approval is auto: this harness exercises sequencing and re-observation, not
// the approval-comment matcher, which steps_m4_test.go already covers directly. ArgoNamespace/
// ArgoApps are always set, mirroring a real repo's config — a scenario that never calls
// argoSyncs/rollsOut just leaves those steps parked at their own natural Blocked/Waiting stop.
func (w *world) newPromotionState() *PromotionState {
	w.t.Helper()
	s := newState(w.fx, w.worktreeDir())
	s.Approval = approvalAuto
	s.ArgoNamespace = testArgoNamespace
	s.ArgoApps = []string{testApp}
	return s
}

// worktreeDir returns this world's one worktree path — deterministic per (repoFullName, plan)
// the same way DeriveID is (fixture_test.go's own newState builds from the same fx.plan every
// call), and cached across calls so a resumed PromotionState reuses the identical path a real
// process's cached $XDG_CACHE_HOME/hoist/worktrees/<id> would, rather than colliding with the
// original's own still-checked-out worktree on the same deterministic branch.
func (w *world) worktreeDir() string {
	id := DeriveID(repoFullName, w.fx.plan)
	if w.wtDirs == nil {
		w.wtDirs = map[string]string{}
	}
	if d, ok := w.wtDirs[id]; ok {
		return d
	}
	d := filepath.Join(w.t.TempDir(), "wt")
	w.wtDirs[id] = d
	return d
}

// promote starts a new PR-mode promotion and drives it as far as the world's current fakes
// allow — typically stopping at CIGreenStep, Waiting, until ciGreen is called.
func (w *world) promote() *PromotionState {
	w.t.Helper()
	s := w.newPromotionState()
	w.drive(s, w.prSteps())
	return s
}

// directDeploy starts a new direct-mode promotion and drives it to completion in one pass — the
// direct pipeline (gate, branch, commit, push) has nothing to wait on.
func (w *world) directDeploy() *PromotionState {
	w.t.Helper()
	s := w.newPromotionState()
	s.Direct = true
	w.drive(s, w.directSteps())
	return s
}

// ciGreen makes the world's forge report one passing check for s's currently-pushed commit,
// then re-drives — the way a test simulates CI going green between polls
// (forge.Fake.SetChecks's own doc comment).
func (w *world) ciGreen(s *PromotionState) {
	w.t.Helper()
	sha := s.PushedSHA
	if sha == "" {
		sha = s.CommitSHA
	}
	w.forge.SetChecks(sha, forge.CheckSummary{Total: 1, Success: 1})
	w.drive(s, w.stepsFor(s))
}

// merge drives a PR-mode promotion through to a landed merge: CI green, then a further Drive
// pass to let MergedStep's Act actually call the forge (mergeSimulatingForge's own push is what
// makes the merge real in git terms, so the very next Observe sees it as landed rather than
// reverted).
func (w *world) merge(s *PromotionState) {
	w.t.Helper()
	w.ciGreen(s)
	w.drive(s, w.stepsFor(s))
}

// argoSyncs sets Argo's reported status for s's target env to Synced/Healthy at origin/<base>'s
// current tip, reconciled after this promotion's own merge/push landed — the way a test
// simulates Argo reconciling between polls (argo.Fake.SetStatus's own doc comment) — then
// re-drives.
func (w *world) argoSyncs(s *PromotionState) {
	w.t.Helper()
	tip, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
	if err != nil || !ok {
		w.t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
	}
	w.argo.SetStatus(argo.Application{Namespace: s.ArgoNamespace, Name: testApp}, argo.Status{
		SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: argo.HealthStatusHealthy,
		ReconciledAt: time.Now(),
	})
	w.drive(s, w.stepsFor(s))
}

// rollsOut marks every Deployment s's own edits touch as fully rolled out — fixture_test.go's
// own satisfiedRollout builds exactly this "nothing left to converge" baseline; applied here to
// this world's shared rollout.Fake rather than a throwaway one, so stepsFor(s) (built fresh
// each call, closing over w.ro's current value) sees it on the very next Drive.
func (w *world) rollsOut(s *PromotionState) {
	w.t.Helper()
	// RolledOutStep.Observe queries rollout.Rollout by s.TargetEnv (AGENTS.md: "Env" IS the
	// k8s namespace), never s.ArgoNamespace — the namespace Argo itself lives in (commonly
	// "argocd") is a different one from the env it manages.
	w.ro = satisfiedRollout(s.TargetEnv, s.Edits)
	w.drive(s, w.stepsFor(s))
}

// supersede writes ref as a LATER, independent deploy into the fixture's target env — from a
// separate clone of the same origin, exactly as a real second deploy (hoist's own, or anyone
// else's) would, never going through this world's own steps. Reuses direct_test.go's own
// supersedeBase, the same adversary steps_m5_test.go's own TestArgoSyncedAcceptsARevisionThat…
// tests already build this exact way.
func (w *world) supersede(ref string) {
	w.t.Helper()
	supersedeBase(w.t, w.fx, ref)
}

// revert reverts s's own landed change (the merge commit for a PR-mode promotion, the direct
// push itself for a direct one) from a separate clone and pushes — a genuine revert, the mirror
// case of supersede: the promoted content is gone from origin/<base>, not merely superseded by
// something newer of the same image repo.
func (w *world) revert(s *PromotionState) {
	w.t.Helper()
	other := filepath.Join(w.t.TempDir(), "revert-clone")
	runHost(w.t, "", "clone", "-q", w.fx.originDir, other)
	runHost(w.t, other, "revert", "--no-edit", s.LandedSHA())
	runHost(w.t, other, "push", "-q", "origin", s.Base)
}

// resume simulates `hoist resume` (or a killed-and-restarted process): a brand-new
// PromotionState for the SAME fixture/plan — deterministic id, so it names the same
// branch/PR/state a live process would — with no memory of anything the original ever recorded,
// re-driven from the top. AGENTS.md §4.1: Observe, never a recorded Phase, decides what's left;
// this is what proves that in practice, not by assertion on the type's own contract.
func (w *world) resume(direct bool) *PromotionState {
	w.t.Helper()
	s := w.newPromotionState()
	s.Direct = direct
	w.drive(s, w.stepsFor(s))
	return s
}

// stillInFlight mirrors cmd/hoist/drive.go's own findInFlight: re-observing a PRIOR promotion
// (Observe only, never Act) via ObserveSteps, deliberately without Argo/rollout convergence in
// the observation — drive.go's own doc comment: invariant 5 (one promotion per target env) is
// fully retired the moment a merge/direct-push lands, since a later promotion for the same env
// gets its own distinct deterministic id regardless of what Argo/rollout still have left to do.
// Not imported from cmd/hoist (pkg/ activity-shape, §4.3, and cmd/hoist is a separate module
// boundary this package never reaches into) — the same read, local to the package that actually
// owns ObserveSteps/ObserveAll.
func (w *world) stillInFlight(s *PromotionState) bool {
	w.t.Helper()
	done, _, err := ObserveAll(context.Background(), ObserveSteps(s, w.g, w.forge, nil, nil, nil), s)
	if err != nil {
		w.t.Fatalf("ObserveAll: %v", err)
	}
	return !done
}
