package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/rollout"
)

// TestListArchivesAPromotionALaterDeploySuperseded is #168 as `hoist promotions` sees it: a
// finished promotion, then a later deploy of the same image repo into the same env, which the
// cluster rolls out. The promotion is done — its change landed and was replaced — so the listing
// reports it done, says it was superseded, and archives it once it is old enough, rather than
// re-observing it live on every listing forever.
//
// The control is the shape the scan tests keep (TestListSharesOriginAcrossPromotionsThatNeverGoTerminal):
// the cluster running something else while the base still declares this promotion's reference
// is drift, not a supersede, and stays at rolled-out.
func TestListArchivesAPromotionALaterDeploySuperseded(t *testing.T) {
	later := "ghcr.io/example/app:v9@sha256:" + strings.Repeat("9", 64)
	for _, tc := range []struct {
		name      string
		supersede bool
	}{{"superseded at the base", true}, {"drift only (control)", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newInflightFixture(t)
			s := finishedPromotion(t, fx)
			if tc.supersede {
				out, err := gitHostCmd(fx.clone, "remote", "get-url", "origin").Output()
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(t.TempDir(), "later-deploy")
				runGitHost(t, "", "clone", "-q", strings.TrimSpace(string(out)), other)
				p := filepath.Join(other, "cluster/apps/app-production/app/deployment.yaml")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(b), s.Edits[0].New.String()) {
					t.Fatalf("setup: the base should declare this promotion's reference:\n%s", b)
				}
				if err := os.WriteFile(p, []byte(strings.Replace(string(b), s.Edits[0].New.String(), later, 1)), 0o644); err != nil {
					t.Fatal(err)
				}
				runGitHost(t, other, "commit", "-q", "-a", "-m", "a later deploy")
				runGitHost(t, other, "push", "-q", "origin", "main")
			}
			ro, err := fx.svc.Rollout("")
			if err != nil {
				t.Fatal(err)
			}
			ro.(*rollout.Fake).SetDeployment("app-production", "app", rollout.DeploymentStatus{
				Namespace: "app-production", Name: "app", Complete: true,
				Images: []rollout.ContainerImage{{Name: "app", Image: later}},
			})

			listed, err := withConfig(fx).List(context.Background(), ListOpts{ArchiveDoneOlderThan: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Err != nil {
				t.Fatalf("List = %+v", listed)
			}
			l := listed[0]
			if !tc.supersede {
				if l.Done || l.Archived {
					t.Fatalf("drift with the base still declaring this promotion is not finished: done=%v archived=%v %+v", l.Done, l.Archived, l.Last)
				}
				return
			}
			if !l.Done || !l.Archived {
				t.Fatalf("a superseded promotion is finished and old enough to archive: done=%v archived=%v at %s: %s", l.Done, l.Archived, l.Last.Step, Detail(l.Last.Observation))
			}
			if !strings.Contains(l.Last.Detail, "superseded") {
				t.Errorf("the listing should say it was superseded, got %q", l.Last.Detail)
			}
		})
	}
}
