package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func podTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "migrate", Image: "ghcr.io/example/app:v2"}},
		Containers:     []corev1.Container{{Name: "app", Image: "ghcr.io/example/app:v2"}},
	}}
}

func statefulSet(mut func(*appsv1.StatefulSet)) *appsv1.StatefulSet {
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app-production", Name: "db", Generation: 2},
		Spec: appsv1.StatefulSetSpec{
			Replicas:       i32(3),
			Template:       podTemplate(),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, ReadyReplicas: 3, UpdatedReplicas: 3, CurrentReplicas: 3,
			CurrentRevision: "db-2", UpdateRevision: "db-2",
		},
	}
	mut(ss)
	return ss
}

func daemonSet(mut func(*appsv1.DaemonSet)) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app-production", Name: "agent", Generation: 2},
		Spec: appsv1.DaemonSetSpec{
			Template:       podTemplate(),
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 2, DesiredNumberScheduled: 4, UpdatedNumberScheduled: 4, NumberAvailable: 4,
		},
	}
	mut(ds)
	return ds
}

// TestWorkloadStatefulSetRolloutStatus is kubectl's StatefulSet rollout-status test, case by
// case, plus OnDelete, which kubectl refuses and this reports as not complete (#171).
func TestWorkloadStatefulSetRolloutStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mut      func(*appsv1.StatefulSet)
		complete bool
		detail   string
	}{
		{"rolled out", func(*appsv1.StatefulSet) {}, true, "rolling update complete"},
		{"spec not observed", func(ss *appsv1.StatefulSet) { ss.Status.ObservedGeneration = 1 }, false, "spec update to be observed"},
		{"not all ready", func(ss *appsv1.StatefulSet) { ss.Status.ReadyReplicas = 1 }, false, "waiting for 2 pods to be ready"},
		{"revision still rolling", func(ss *appsv1.StatefulSet) { ss.Status.UpdateRevision = "db-3"; ss.Status.UpdatedReplicas = 1 }, false, "rolling update to complete"},
		{"partition not reached", func(ss *appsv1.StatefulSet) {
			ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: i32(1)}
			ss.Status.UpdateRevision, ss.Status.UpdatedReplicas = "db-3", 1
		}, false, "partitioned roll out to finish: 1 out of 2"},
		{"partition reached", func(ss *appsv1.StatefulSet) {
			ss.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: i32(1)}
			ss.Status.UpdateRevision, ss.Status.UpdatedReplicas = "db-3", 2
		}, true, "partitioned roll out complete"},
		{"OnDelete", func(ss *appsv1.StatefulSet) { ss.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType }, false, "only when deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := FromClientset(fake.NewSimpleClientset(statefulSet(tc.mut))).Workload(context.Background(), "app-production", "db", "StatefulSet")
			if err != nil {
				t.Fatalf("Workload: %v", err)
			}
			if st.Complete != tc.complete || !strings.Contains(st.Detail, tc.detail) || st.DeadlineExceeded {
				t.Errorf("got complete=%v detail=%q deadline=%v, want complete=%v detail containing %q", st.Complete, st.Detail, st.DeadlineExceeded, tc.complete, tc.detail)
			}
			if len(st.Images) != 2 || st.Images[0] != (ContainerImage{Name: "app", Image: "ghcr.io/example/app:v2"}) || !st.Images[1].Init {
				t.Errorf("Images = %+v, want the template's container and init container", st.Images)
			}
		})
	}
}

// TestWorkloadDaemonSetRolloutStatus is kubectl's DaemonSet rollout-status test, case by case.
func TestWorkloadDaemonSetRolloutStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mut      func(*appsv1.DaemonSet)
		complete bool
		detail   string
	}{
		{"rolled out", func(*appsv1.DaemonSet) {}, true, "successfully rolled out"},
		{"spec not observed", func(ds *appsv1.DaemonSet) { ds.Status.ObservedGeneration = 1 }, false, "spec update to be observed"},
		{"not all updated", func(ds *appsv1.DaemonSet) { ds.Status.UpdatedNumberScheduled = 2 }, false, "2 out of 4 new pods"},
		{"not all available", func(ds *appsv1.DaemonSet) { ds.Status.NumberAvailable = 3 }, false, "3 of 4 updated pods"},
		{"OnDelete", func(ds *appsv1.DaemonSet) { ds.Spec.UpdateStrategy.Type = appsv1.OnDeleteDaemonSetStrategyType }, false, "only when deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := FromClientset(fake.NewSimpleClientset(daemonSet(tc.mut))).Workload(context.Background(), "app-production", "agent", "DaemonSet")
			if err != nil {
				t.Fatalf("Workload: %v", err)
			}
			if st.Complete != tc.complete || !strings.Contains(st.Detail, tc.detail) {
				t.Errorf("got complete=%v detail=%q, want complete=%v detail containing %q", st.Complete, st.Detail, tc.complete, tc.detail)
			}
			if len(st.Images) != 2 {
				t.Errorf("Images = %+v, want the template's two containers", st.Images)
			}
		})
	}
}

// TestWorkloadMissingWrapsErrNotFound, and an unsupported kind is an error rather than a guess.
func TestWorkloadMissingWrapsErrNotFound(t *testing.T) {
	r := FromClientset(fake.NewSimpleClientset())
	for _, kind := range []string{"StatefulSet", "DaemonSet"} {
		if _, err := r.Workload(context.Background(), "app-production", "gone", kind); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", kind, err)
		}
	}
	if _, err := r.Workload(context.Background(), "app-production", "x", "ReplicaSet"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("an unsupported kind must be an error, and not a not-found one; got %v", err)
	}
}
