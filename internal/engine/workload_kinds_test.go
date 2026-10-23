package engine

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/rollout"
)

// These tests are #171's rollout half: discovery records an image in any workload with a
// containers list, so a promotion can edit a StatefulSet or a DaemonSet, and RolledOutStep used
// to gate on Deployments only — a promotion whose only edit was a StatefulSet read "rolled out"
// without anything having been read from the cluster (principle 1).

// kindFixture is newFixture's one-Deployment family with the workload's kind replaced.
func kindFixture(t *testing.T, kind string) func(env, ref string) string {
	t.Helper()
	return func(env, ref string) string {
		return strings.Replace(oneDeployment(env, ref), "kind: Deployment", "kind: "+kind, 1)
	}
}

// TestRolledOutGatesStatefulSetsAndDaemonSets: a promotion of a StatefulSet or a DaemonSet must
// not read rolled out until that workload has the new image and has finished rolling out; one
// the cluster does not have Blocks, as a missing Deployment does.
func TestRolledOutGatesStatefulSetsAndDaemonSets(t *testing.T) {
	for _, kind := range []string{"StatefulSet", "DaemonSet"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorldWith(t, newFixtureWith(t, kindFixture(t, kind), 1))
			s := w.promote()
			w.merge(s)
			if len(s.Edits) != 1 || s.Edits[0].Kind != kind {
				t.Fatalf("fixture precondition: want one %s edit, got %+v", kind, s.Edits)
			}

			// Absent from the cluster: nothing was read, so nothing may be claimed.
			w.ro = &rollout.Fake{Strict: true}
			if obs := w.rolledOutObservation(s); obs.Satisfied || !strings.Contains(obs.Blocked, kind+" "+s.TargetEnv+"/app not found") {
				t.Fatalf("a %s the cluster does not have must Block naming it, got %+v", kind, obs)
			}
			ours, old := s.Edits[0].New.String(), s.Edits[0].Ref.String()
			for _, tc := range []struct {
				name      string
				image     string
				complete  bool
				satisfied bool
			}{
				{"still on the old image", old, true, false},
				{"new image, rollout in progress", ours, false, false},
				{"new image, rolled out", ours, true, true},
			} {
				w.ro = &rollout.Fake{Strict: true}
				w.ro.SetWorkload(s.TargetEnv, "app", kind, rollout.DeploymentStatus{
					Namespace: s.TargetEnv, Name: "app",
					Images:   []rollout.ContainerImage{{Name: "app", Image: tc.image}},
					Complete: tc.complete, Detail: "status of the " + kind,
				})
				obs := w.rolledOutObservation(s)
				if obs.Satisfied != tc.satisfied {
					t.Errorf("%s: Satisfied = %v, want %v: %+v", tc.name, obs.Satisfied, tc.satisfied, obs)
				}
				if !tc.satisfied && (!obs.Waiting || !strings.Contains(obs.Detail, kind+" app:")) {
					t.Errorf("%s: want Waiting naming the %s, got %+v", tc.name, kind, obs)
				}
			}
		})
	}
}
