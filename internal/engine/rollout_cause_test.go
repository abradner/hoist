package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/rollout"
)

// These tests are #259's: ArgoSyncedStep.rolloutCause explains one synced-but-not-healthy
// Application, and its deadline Block is a gate, not a courtesy — so it must read only that
// Application's own Deployments, and only the occurrences in them still this promotion's at the
// revision Argo synced.

// TestArgoSyncedRolloutCauseReadsOnlyItsOwnApplication: the promotion touches two
// Applications, and "app" is intact and still rolling out this promotion's change. Explaining
// app must not Block on, or report, web's Deployment: it is another Application's. In the first
// two cases web was superseded by a later deploy, whose rollout is that deploy's business; in
// the last two web is intact and Argo already reports its Application Healthy (Argo's health can
// lag a Deployment's own progress deadline), so the only iteration with standing to read web is
// web's own, and with web Healthy that iteration asks nothing.
func TestArgoSyncedRolloutCauseReadsOnlyItsOwnApplication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supersede bool
		deadline  bool
	}{
		{"web superseded, its later rollout past its deadline", true, true},
		{"web superseded, its later rollout still progressing", true, false},
		{"web intact and Healthy, its Deployment past its deadline", false, true},
		{"web intact and Healthy, its Deployment still progressing", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, originDir, webApp := twoAppGitBackedState(t)
			g := git.Exec{}
			webV3 := "ghcr.io/example/web:v3@sha256:" + strings.Repeat("2", 64)
			var webOurs, appOurs string
			for _, e := range s.Edits {
				switch e.Name {
				case "web":
					webOurs = e.New.String()
				case "app":
					appOurs = e.New.String()
				}
			}
			other := filepath.Join(t.TempDir(), "other-clone")
			runHost(t, "", "clone", "-q", originDir, other)
			p := filepath.Join(other, "cluster/apps/app-production/web/deployment.yaml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if webOurs == "" || appOurs == "" || !strings.Contains(string(b), webOurs) {
				t.Fatalf("fixture precondition: want one edit per family and web's file on this promotion's reference, got %+v\n%s", s.Edits, b)
			}
			webLive, webHealth := webOurs, argo.HealthStatusHealthy
			if tc.supersede {
				if err := os.WriteFile(p, []byte(strings.Replace(string(b), webOurs, webV3, 1)), 0o644); err != nil {
					t.Fatal(err)
				}
				runHost(t, other, "commit", "-q", "-a", "-m", "a later deploy superseding web alone")
				runHost(t, other, "push", "-q", "origin", "main")
				webLive, webHealth = webV3, "Progressing"
			}
			tip, ok, err := g.LsRemoteBranch(ctx(), other, "origin", "main")
			if err != nil || !ok {
				t.Fatalf("reading origin/main: %v (ok=%v)", err, ok)
			}

			a := &argo.Fake{}
			a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: "app-app-production"},
				argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing"})
			a.SetStatus(webApp, argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: webHealth})
			ro := &rollout.Fake{}
			ro.SetDeployment(s.TargetEnv, "app", rollout.DeploymentStatus{
				Namespace: s.TargetEnv, Name: "app", Images: []rollout.ContainerImage{{Name: "app", Image: appOurs}},
				Detail: "1 of 2 updated replicas are available",
			})
			ro.SetDeployment(s.TargetEnv, "web", rollout.DeploymentStatus{
				Namespace: s.TargetEnv, Name: "web", Images: []rollout.ContainerImage{{Name: "web", Image: webLive}},
				DeadlineExceeded: tc.deadline, Detail: "web's own rollout",
			})

			obs, err := (ArgoSyncedStep{Argo: a, Git: g, Rollout: ro}).Observe(ctx(), s)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if obs.Blocked != "" || !obs.Waiting {
				t.Fatalf("app is intact and still rolling out, and web is another Application's; want Waiting on app, got %+v", obs)
			}
			if !strings.Contains(obs.Detail, "1 of 2 updated replicas") {
				t.Errorf("Detail should name app's own rollout, got %q", obs.Detail)
			}
			if strings.Contains(obs.Detail, "web:") {
				t.Errorf("Detail must not report web's Deployment while explaining app, got %q", obs.Detail)
			}
		})
	}
}

// TestArgoSyncedRolloutCauseSkipsASupersededDeploymentInItsOwnApplication is the
// single-Application case: two Deployments in one Application, the worker superseded by a later
// deploy and the app still rolling out this promotion's change. The Application is health-gated
// (one occurrence is intact), so rolloutCause runs; it must read the app Deployment only. The
// control supersedes nothing: the worker is then this promotion's, and its deadline Blocks as it
// always has.
func TestArgoSyncedRolloutCauseSkipsASupersededDeploymentInItsOwnApplication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supersede bool
		deadline  bool
		blocked   bool
	}{
		{"worker superseded, its later rollout past its deadline", true, true, false},
		{"worker superseded, its later rollout still progressing", true, false, false},
		{"worker intact and past its deadline (control)", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldWith(t, newFixtureWith(t, appAndWorker, 2))
			s := w.promote()
			w.merge(s)
			s.EditApps = map[string]string{}
			for _, e := range s.Edits {
				s.EditApps[e.File] = testApp
			}
			ours := s.Edits[0].New.String()
			workerImage := ours
			if tc.supersede {
				supersedeNth(t, w.fx, 1, supersededRef) // the worker only
				workerImage = supersededRef
			}
			tip, ok, err := w.g.LsRemoteBranch(context.Background(), s.CloneDir, "origin", s.Base)
			if err != nil || !ok {
				t.Fatalf("reading origin/%s: %v (ok=%v)", s.Base, err, ok)
			}
			a := &argo.Fake{}
			a.SetStatus(argo.Application{Namespace: testArgoNamespace, Name: testApp},
				argo.Status{SyncStatus: argo.SyncStatusSynced, SyncRevision: tip, HealthStatus: "Progressing"})
			ro := &rollout.Fake{}
			live(ro, s.TargetEnv, "app", ours, false, false)
			live(ro, s.TargetEnv, "worker", workerImage, false, tc.deadline)

			obs, err := ArgoSyncedStep{Argo: a, Git: w.g, Rollout: ro}.Observe(context.Background(), s)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if (obs.Blocked != "") != tc.blocked {
				t.Fatalf("Blocked = %q, want blocked=%v: %+v", obs.Blocked, tc.blocked, obs)
			}
			if tc.blocked {
				return
			}
			if !obs.Waiting || !strings.Contains(obs.Detail, "status of app") {
				t.Errorf("want Waiting naming the app Deployment's rollout, got %+v", obs)
			}
			if strings.Contains(obs.Detail, "worker") {
				t.Errorf("the superseded worker's rollout is the later deploy's; Detail must not report it, got %q", obs.Detail)
			}
		})
	}
}
