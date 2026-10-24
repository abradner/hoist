package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abradner/hoist/pkg/argo"
)

// TestArgoSyncedDirectPathAnchorsOnItsOwnCommit (#326, foreman's read): on the direct path
// LandedSHA() is PushedSHA, which DirectPushedStep moves to the live tip on every walk. Anchoring
// ArgoSyncedStep's ancestry check there made any Argo revision older than that tip "not
// carried", so the Degraded Block fired before the supersede logic ran. The PR path, anchored on
// its merge commit, waits or is satisfied in the same two situations; the direct path must too.
func TestArgoSyncedDirectPathAnchorsOnItsOwnCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		then func(t *testing.T, w *world, s *PromotionState)
		// satisfied: the later deploy's health is not this promotion's. Otherwise: Waiting,
		// never Blocked, on the stale revision's health.
		satisfied bool
	}{
		{"an unrelated commit after the supersede, Argo still on the superseding revision", func(t *testing.T, w *world, _ *PromotionState) {
			other := filepath.Join(t.TempDir(), "unrelated-clone")
			runHost(t, "", "clone", "-q", w.fx.originDir, other)
			if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("unrelated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runHost(t, other, "add", "README.md")
			runHost(t, other, "commit", "-q", "-m", "an unrelated change")
			runHost(t, other, "push", "-q", "origin", "main")
		}, true},
		{"the supersede reverted, Argo still on the superseding revision", func(t *testing.T, w *world, s *PromotionState) {
			supersedeNth(t, w.fx, 0, s.Edits[0].Ref.String())
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			s := w.directDeploy()
			s.EditApps = map[string]string{}
			for _, e := range s.Edits {
				s.EditApps[e.File] = testApp
			}
			w.supersede(supersededRef)
			synced, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil || !ok {
				t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
			}
			runHost(t, s.CloneDir, "fetch", "-q", "origin")
			tc.then(t, w, s)
			// Every walk re-observes the push first, which is what moves PushedSHA.
			if _, err := (DirectPushedStep{Git: w.g}).Observe(context.Background(), s); err != nil {
				t.Fatalf("DirectPushedStep.Observe: %v", err)
			}
			a := &argo.Fake{}
			a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
				argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: synced, HealthStatus: argo.HealthStatusDegraded})
			obs, err := ArgoSyncedStep{Argo: a, Git: w.g}.Observe(context.Background(), s)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if obs.Blocked != "" {
				t.Fatalf("the superseding revision's health is not this promotion's; got Blocked %q", obs.Blocked)
			}
			if tc.satisfied && !obs.Satisfied {
				t.Fatalf("want Satisfied, got %+v", obs)
			}
			if !tc.satisfied && !obs.Waiting {
				t.Fatalf("want Waiting for Argo to sync the tip, got %+v", obs)
			}
		})
	}
}

// TestDirectModeMissingCommitIsNotLandedNotAnError (foreman's read of #307; #311): when the image
// repo has been superseded and this clone does not have the promotion's own commit (a re-clone,
// an unpushed commit gc'd), the ancestry query fails with "not a valid commit" on every walk.
// That must read "not landed", as before #307, not a retryable error that never clears and
// keeps every later promotion out of the env.
func TestDirectModeMissingCommitIsNotLandedNotAnError(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.supersede(supersededRef)
	s.CommitSHA = "0123456789abcdef0123456789abcdef01234567" // a commit this clone has never had
	for walk := 1; walk <= 3; walk++ {
		obs, err := (DirectPushedStep{Git: w.g}).Observe(context.Background(), s)
		if err != nil {
			t.Fatalf("walk %d: a commit the clone lacks is no landing, not an error: %v", walk, err)
		}
		if obs.Satisfied {
			t.Fatalf("walk %d: a commit the clone lacks is no evidence of landing: %+v", walk, obs)
		}
	}
}
