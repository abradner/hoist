package rollout

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

func podTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"prometheus.io/scrape": "true"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "db", Image: "ghcr.io/example/db:v1@sha256:" + strings.Repeat("3", 64),
			ReadinessProbe: &corev1.Probe{},
		}}},
	}
}

func baseStatefulSet(ns, name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec: appsv1.StatefulSetSpec{
			Replicas: i32(3),
			Template: podTemplate(),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: i32(0)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1, Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, CurrentReplicas: 3,
			CurrentRevision: "db-1", UpdateRevision: "db-1",
		},
	}
}

func baseDaemonSet(ns, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec: appsv1.DaemonSetSpec{
			Template:       podTemplate(),
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 1, DesiredNumberScheduled: 4, UpdatedNumberScheduled: 4, NumberAvailable: 4,
		},
	}
}

// #171: a StatefulSet's and a DaemonSet's pod templates take the same stamp a Deployment's does,
// through the same one write, and nothing else on them moves. Before this, Restart patched
// Deployments only, so neither kind could be restarted at all.
func TestRestartStampsStatefulSetAndDaemonSetPodTemplates(t *testing.T) {
	sts, ds := baseStatefulSet("app-staging", "db"), baseDaemonSet("app-staging", "agent")
	cs := fake.NewSimpleClientset(sts, ds)
	r := FromClientset(cs)
	at := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	want := at.Format(RestartStampLayout)
	ctx := context.Background()

	if err := r.Restart(ctx, "app-staging", "db", KindStatefulSet, at); err != nil {
		t.Fatalf("Restart StatefulSet: %v", err)
	}
	if err := r.Restart(ctx, "app-staging", "agent", KindDaemonSet, at); err != nil {
		t.Fatalf("Restart DaemonSet: %v", err)
	}
	gotS, err := cs.AppsV1().StatefulSets("app-staging").Get(ctx, "db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotD, err := cs.AppsV1().DaemonSets("app-staging").Get(ctx, "agent", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for kind, tmpl := range map[string]corev1.PodTemplateSpec{KindStatefulSet: gotS.Spec.Template, KindDaemonSet: gotD.Spec.Template} {
		if tmpl.Annotations[RestartAnnotation] != want {
			t.Errorf("%s annotation = %q, want %q", kind, tmpl.Annotations[RestartAnnotation], want)
		}
		if tmpl.Annotations["prometheus.io/scrape"] != "true" {
			t.Errorf("%s lost an unrelated annotation: %v", kind, tmpl.Annotations)
		}
		if tmpl.Spec.Containers[0].Image != sts.Spec.Template.Spec.Containers[0].Image {
			t.Errorf("%s image changed to %q — a restart changes no image", kind, tmpl.Spec.Containers[0].Image)
		}
	}
	// The read path sees each stamp, so a watch can tell its own restart from someone else's.
	for _, k := range []struct{ name, kind string }{{"db", KindStatefulSet}, {"agent", KindDaemonSet}} {
		st, err := r.Workload(ctx, "app-staging", k.name, k.kind)
		if err != nil {
			t.Fatal(err)
		}
		if st.RestartedAt != want || st.Kind != k.kind {
			t.Errorf("%s read back as %+v, want kind %s stamped %q", k.name, st, k.kind, want)
		}
	}
}

// The kind is checked, not assumed: an unknown kind writes nothing, and a StatefulSet asked for
// as a DaemonSet is not found rather than patched under the wrong client.
func TestRestartRefusesAKindItCannotRoll(t *testing.T) {
	cs := fake.NewSimpleClientset(baseStatefulSet("app-staging", "db"))
	r := FromClientset(cs)
	if err := r.Restart(context.Background(), "app-staging", "db", "CronJob", time.Now()); err == nil {
		t.Error("restarting a CronJob must be refused")
	}
	if err := r.Restart(context.Background(), "app-staging", "db", KindDaemonSet, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("a StatefulSet restarted as a DaemonSet: err = %v, want ErrNotFound", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "statefulsets" {
			t.Errorf("the StatefulSet was patched through a refused call: %v", a)
		}
	}
	if _, err := r.Workload(context.Background(), "app-staging", "db", "CronJob"); err == nil {
		t.Error("reading a CronJob as a restartable workload must be refused")
	}
}

// The settings a restart warns on come off each kind's own spec, resolved by that kind's own
// controller rules.
func TestWorkloadReadsEachKindsOwnSettings(t *testing.T) {
	ctx := context.Background()

	sts := baseStatefulSet("ns", "db")
	sts.Spec.Replicas = nil // Kubernetes' default of 1
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{}
	ds := baseDaemonSet("ns", "agent")
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{} // RollingUpdate, 1 unavailable, 0 surge
	r := FromClientset(fake.NewSimpleClientset(sts, ds))

	s, err := r.Workload(ctx, "ns", "db", KindStatefulSet)
	if err != nil {
		t.Fatal(err)
	}
	if s.Replicas != 1 || s.Strategy != "RollingUpdate" || s.MaxUnavailable != 1 || s.MaxSurge != 0 || s.ReadinessProbes != 1 {
		t.Errorf("StatefulSet defaults read as %+v", s)
	}
	d, err := r.Workload(ctx, "ns", "agent", KindDaemonSet)
	if err != nil {
		t.Fatal(err)
	}
	if d.Replicas != 4 || d.Strategy != "RollingUpdate" || d.MaxUnavailable != 1 || d.MaxSurge != 0 {
		t.Errorf("DaemonSet defaults read as %+v, want 4 desired, RollingUpdate, 1 unavailable, 0 surge", d)
	}

	// Percentages round UP for both kinds' maxUnavailable, against the kind's own count.
	pct := intstr.FromString("50%")
	ds.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &pct}
	ds.Status.DesiredNumberScheduled = 3
	d, err = FromClientset(fake.NewSimpleClientset(ds)).Workload(ctx, "ns", "agent", KindDaemonSet)
	if err != nil {
		t.Fatal(err)
	}
	if d.MaxUnavailable != 2 {
		t.Errorf("50%% of 3 DaemonSet pods = %d unavailable, want 2 (rounded up)", d.MaxUnavailable)
	}

	// A Deployment through Workload is Deployment's own read, converted.
	dep := baseDeployment("ns", "app")
	w, err := FromClientset(fake.NewSimpleClientset(dep)).Workload(ctx, "ns", "app", KindDeployment)
	if err != nil {
		t.Fatal(err)
	}
	if w.Kind != KindDeployment || w.Replicas != 2 || !w.Complete {
		t.Errorf("Deployment through Workload = %+v", w)
	}
	if _, err := FromClientset(fake.NewSimpleClientset()).Workload(ctx, "ns", "gone", KindStatefulSet); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing StatefulSet: err = %v, want ErrNotFound", err)
	}
}

// Rollout completeness per kind, kubectl's tests ported — and the one place this differs from
// kubectl on purpose: an OnDelete workload is reported, never complete, because its pods take the
// new template only when someone deletes them. Claiming "rolled out" there would be the false
// success principle 1 forbids.
func TestWorkloadRolloutCompleteness(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		obj      func() (kind string, r Rollout)
		complete bool
		detail   string
	}{
		{"statefulset rolled", func() (string, Rollout) {
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(baseStatefulSet("ns", "x")))
		}, true, "rolling update complete"},
		{"statefulset still updating", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Status.UpdatedReplicas, s.Status.UpdateRevision = 1, "db-2"
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, false, "waiting for statefulset rolling update"},
		{"statefulset pod not ready", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Status.ReadyReplicas = 2
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, false, "waiting for 1 pods to be ready"},
		{"statefulset partition reached", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Spec.UpdateStrategy.RollingUpdate.Partition = i32(2)
			s.Status.UpdatedReplicas, s.Status.UpdateRevision = 1, "db-2"
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, true, "partitioned roll out complete"},
		// spec.ordinals.start makes partition an absolute ordinal (Codex on #350): start 10, three
		// replicas, partition 11 means ordinals 11 and 12 roll, so two pods must update.
		{"statefulset partition with a start ordinal waits for its pods", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: 10}
			s.Spec.UpdateStrategy.RollingUpdate.Partition = i32(11)
			s.Status.UpdatedReplicas, s.Status.UpdateRevision = 0, "db-2"
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, false, "0 out of 2 new pods"},
		{"statefulset partition with a start ordinal reached", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: 10}
			s.Spec.UpdateStrategy.RollingUpdate.Partition = i32(11)
			s.Status.UpdatedReplicas, s.Status.UpdateRevision = 2, "db-2"
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, true, "partitioned roll out complete"},
		{"statefulset spec not observed", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Generation = 2
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, false, "spec update to be observed"},
		{"statefulset OnDelete is never complete", func() (string, Rollout) {
			s := baseStatefulSet("ns", "x")
			s.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			return KindStatefulSet, FromClientset(fake.NewSimpleClientset(s))
		}, false, "OnDelete"},
		{"daemonset rolled", func() (string, Rollout) {
			return KindDaemonSet, FromClientset(fake.NewSimpleClientset(baseDaemonSet("ns", "x")))
		}, true, "successfully rolled out"},
		{"daemonset updating", func() (string, Rollout) {
			d := baseDaemonSet("ns", "x")
			d.Status.UpdatedNumberScheduled = 2
			return KindDaemonSet, FromClientset(fake.NewSimpleClientset(d))
		}, false, "2 out of 4 new pods"},
		{"daemonset not available", func() (string, Rollout) {
			d := baseDaemonSet("ns", "x")
			d.Status.NumberAvailable = 3
			return KindDaemonSet, FromClientset(fake.NewSimpleClientset(d))
		}, false, "3 of 4 updated pods are available"},
		{"daemonset OnDelete is never complete", func() (string, Rollout) {
			d := baseDaemonSet("ns", "x")
			d.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}
			return KindDaemonSet, FromClientset(fake.NewSimpleClientset(d))
		}, false, "OnDelete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, r := tc.obj()
			st, err := r.Workload(ctx, "ns", "x", kind)
			if err != nil {
				t.Fatal(err)
			}
			if st.Complete != tc.complete || !strings.Contains(st.Detail, tc.detail) {
				t.Errorf("complete=%v detail=%q, want complete=%v and a detail mentioning %q", st.Complete, st.Detail, tc.complete, tc.detail)
			}
		})
	}
}

// The graceful-restart warnings for the two new kinds, each stated only where it is true. OnDelete
// is the one that matters most: the stamp lands and nothing happens until pods are deleted, so it
// must be said, never left for the operator to discover from a watch that never finishes.
func TestWorkloadGracefulRestartConcerns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		st    WorkloadStatus
		wants []string
	}{
		{"healthy statefulset says nothing",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 1, ReadinessProbes: 1}, nil},
		{"statefulset OnDelete warns that nothing restarts",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: StrategyOnDelete, ReadinessProbes: 1}, []string{"OnDelete: the restart stamps the pod template, but no pod restarts until it is deleted"}},
		{"daemonset OnDelete warns that nothing restarts",
			WorkloadStatus{Kind: KindDaemonSet, Replicas: 3, Strategy: StrategyOnDelete, ReadinessProbes: 1}, []string{"OnDelete"}},
		{"single-replica statefulset stops before it starts",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 1, Strategy: "RollingUpdate", MaxUnavailable: 1, ReadinessProbes: 1}, []string{"nothing serves until the new one is ready"}},
		{"statefulset partition leaves some pods alone",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 1, Partition: 1, ReadinessProbes: 1}, []string{"only pods with ordinal 1 and above restart"}},
		{"statefulset partition at or over replicas restarts nothing",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 1, Partition: 3, ReadinessProbes: 1}, []string{"no pod restarts"}},
		{"statefulset partition counts from its start ordinal",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 1, Partition: 11, OrdinalStart: 10, ReadinessProbes: 1}, []string{"only pods with ordinal 11 and above restart; the other 1 keep running"}},
		{"statefulset partition below its start ordinal restarts every pod",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 1, Partition: 5, OrdinalStart: 10, ReadinessProbes: 1}, nil},
		// maxUnavailable is honoured only under the Parallel policy, so it is warned on only there.
		{"statefulset maxUnavailable under OrderedReady says nothing",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 3, ReadinessProbes: 1}, nil},
		{"statefulset maxUnavailable under Parallel can take every pod",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 3, Strategy: "RollingUpdate", MaxUnavailable: 3, ParallelPods: true, ReadinessProbes: 1}, []string{"every pod can be down at once"}},
		{"daemonset default stops each node's pod first",
			WorkloadStatus{Kind: KindDaemonSet, Replicas: 4, Strategy: "RollingUpdate", MaxUnavailable: 1, ReadinessProbes: 1}, []string{"maxSurge is 0"}},
		{"daemonset with surge says nothing",
			WorkloadStatus{Kind: KindDaemonSet, Replicas: 4, Strategy: "RollingUpdate", MaxUnavailable: 0, MaxSurge: 1, ReadinessProbes: 1}, nil},
		{"daemonset on no node",
			WorkloadStatus{Kind: KindDaemonSet, Replicas: 0, Strategy: "RollingUpdate"}, []string{"scheduled on no node"}},
		{"statefulset OnDelete with no probe says both",
			WorkloadStatus{Kind: KindStatefulSet, Replicas: 2, Strategy: StrategyOnDelete}, []string{"OnDelete", "no readiness probe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.st.GracefulRestartConcerns()
			if len(got) != len(tc.wants) {
				t.Fatalf("got %d concerns %v, want %d mentioning %v", len(got), got, len(tc.wants), tc.wants)
			}
			for i, w := range tc.wants {
				if !strings.Contains(got[i], w) {
					t.Errorf("concern %d = %q, want it to mention %q", i, got[i], w)
				}
			}
		})
	}
}
