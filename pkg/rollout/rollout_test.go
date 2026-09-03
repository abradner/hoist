package rollout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testServer = "https://192.0.2.10:6443" // RFC 5737 TEST-NET-1: never routable, never real

func i32(v int32) *int32 { return &v }

func baseDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: i32(2),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:     []corev1.Container{{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)}},
					InitContainers: []corev1.Container{{Name: "init", Image: "ghcr.io/example/init:v1@sha256:" + strings.Repeat("2", 64)}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           2,
			UpdatedReplicas:    2,
			AvailableReplicas:  2,
		},
	}
}

// TestDeploymentReadsImagesFromSpecTemplate: invariant 4's read side — images come from
// spec.template.spec.containers[]/initContainers[].image, not pod status.
func TestDeploymentReadsImagesFromSpecTemplate(t *testing.T) {
	cs := fake.NewSimpleClientset(baseDeployment("app-production", "app"))
	r := FromClientset(cs)
	st, err := r.Deployment(context.Background(), "app-production", "app")
	if err != nil {
		t.Fatal(err)
	}
	want := []ContainerImage{
		{Name: "app", Image: "ghcr.io/example/app:v2@sha256:" + strings.Repeat("1", 64)},
		{Name: "init", Init: true, Image: "ghcr.io/example/init:v1@sha256:" + strings.Repeat("2", 64)},
	}
	if len(st.Images) != 2 || st.Images[0] != want[0] || st.Images[1] != want[1] {
		t.Errorf("Images = %+v, want %+v", st.Images, want)
	}
	if !st.Complete {
		t.Errorf("expected Complete for a fully-rolled-out fixture: %+v", st)
	}
}

// TestDeploymentRolloutCompleteConditions is the ported kubectl logic's own four gates, each
// exercised in isolation, plus the deadline-exceeded hard failure and the happy path.
func TestDeploymentRolloutCompleteConditions(t *testing.T) {
	cases := []struct {
		name             string
		mutate           func(*appsv1.Deployment)
		wantComplete     bool
		wantDeadline     bool
		wantDetailSubstr string
	}{
		{"spec update not observed", func(d *appsv1.Deployment) { d.Generation = 2 }, false, false, "spec update to be observed"},
		{"deadline exceeded", func(d *appsv1.Deployment) {
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded"}}
		}, false, true, "exceeded its progress deadline"},
		{"updated replicas short", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 }, false, false, "new replicas have been updated"},
		{"old replicas pending termination", func(d *appsv1.Deployment) { d.Status.Replicas = 3 }, false, false, "old replicas are pending termination"},
		{"available replicas short", func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 1 }, false, false, "updated replicas are available"},
		{"happy path", func(*appsv1.Deployment) {}, true, false, "successfully rolled out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := baseDeployment("app-production", "app")
			tc.mutate(d)
			// "old replicas pending termination" must not simultaneously trip the "available
			// replicas short" branch (kubectl's own condition order): keep AvailableReplicas
			// in step with UpdatedReplicas unless the case is testing that branch itself.
			if tc.name == "old replicas pending termination" {
				d.Status.AvailableReplicas = d.Status.UpdatedReplicas
			}
			complete, deadline, detail := deploymentRolloutComplete(d)
			if complete != tc.wantComplete || deadline != tc.wantDeadline {
				t.Errorf("complete=%v deadline=%v, want complete=%v deadline=%v (detail=%q)", complete, deadline, tc.wantComplete, tc.wantDeadline, detail)
			}
			if !strings.Contains(detail, tc.wantDetailSubstr) {
				t.Errorf("detail = %q, want it to contain %q", detail, tc.wantDetailSubstr)
			}
		})
	}
}

// TestDeploymentDefaultsReplicasToOne: spec.replicas is a *int32; nil means the API server's
// own default of 1, and the completeness check must use that default, not zero (which would
// make an unset replicas field trivially "complete" with zero updated replicas).
func TestDeploymentDefaultsReplicasToOne(t *testing.T) {
	d := baseDeployment("app-production", "app")
	d.Spec.Replicas = nil
	d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas = 1, 1, 1
	complete, _, detail := deploymentRolloutComplete(d)
	if !complete {
		t.Errorf("complete = false, detail = %q; want true with the implicit 1-replica default", detail)
	}
}

// TestDeploymentMissingWrapsErrNotFound: a Deployment this promotion supposedly edited but
// that does not exist on the cluster is a real anomaly the caller must Block on, not silently
// treat as "still rolling out".
func TestDeploymentMissingWrapsErrNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := FromClientset(cs)
	_, err := r.Deployment(context.Background(), "app-production", "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Deployment on a missing object = %v, want ErrNotFound", err)
	}
}

// TestJobLikeReportsJobAndCronJob is report-only reading, for both kinds AGENTS.md invariant 4
// names.
func TestJobLikeReportsJobAndCronJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app-production", Name: "migrate"},
		Status:     batchv1.JobStatus{Active: 1, Succeeded: 2, Failed: 0},
	}
	scheduled := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app-production", Name: "nightly"},
		Status:     batchv1.CronJobStatus{LastScheduleTime: &scheduled},
	}
	cs := fake.NewSimpleClientset(job, cron)
	r := FromClientset(cs)

	jst, err := r.JobLike(context.Background(), "app-production", "migrate", "Job")
	if err != nil {
		t.Fatal(err)
	}
	if jst.Kind != "Job" || !strings.Contains(jst.Detail, "active=1") || !strings.Contains(jst.Detail, "succeeded=2") {
		t.Errorf("Job detail = %+v", jst)
	}

	cst, err := r.JobLike(context.Background(), "app-production", "nightly", "CronJob")
	if err != nil {
		t.Fatal(err)
	}
	if cst.Kind != "CronJob" || !strings.Contains(cst.Detail, "2026-01-02") {
		t.Errorf("CronJob detail = %+v", cst)
	}

	if _, err := r.JobLike(context.Background(), "app-production", "migrate", "Deployment"); err == nil {
		t.Error("unknown kind: want an error")
	}
	if _, err := r.JobLike(context.Background(), "app-production", "does-not-exist", "Job"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing Job = %v, want ErrNotFound", err)
	}
}

// TestCronJobDetailReportsActualZoneNotHardcodedZ: cronJobDetail used to format
// LastScheduleTime with a literal "...Z" suffix regardless of the time's real zone, so a
// non-UTC value would print as if it were UTC — a truthfulness bug (Copilot, PR #51 round 2).
// Mutant-verified: reverting to the hardcoded "2006-01-02T15:04:05Z" layout makes this fail
// (the offset in the output no longer matches the fixture's own -07:00 zone).
func TestCronJobDetailReportsActualZoneNotHardcodedZ(t *testing.T) {
	loc := time.FixedZone("test", -7*60*60)
	scheduled := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, loc))
	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app-production", Name: "nightly"},
		Status:     batchv1.CronJobStatus{LastScheduleTime: &scheduled},
	}
	cs := fake.NewSimpleClientset(cron)
	r := FromClientset(cs)

	cst, err := r.JobLike(context.Background(), "app-production", "nightly", "CronJob")
	if err != nil {
		t.Fatal(err)
	}
	want := scheduled.Format(time.RFC3339)
	if !strings.Contains(cst.Detail, want) {
		t.Errorf("CronJob detail = %q, want it to contain the real zone offset %q, not a hardcoded Z", cst.Detail, want)
	}
	if strings.Contains(cst.Detail, "05Z") {
		t.Errorf("CronJob detail = %q, still claims UTC (Z) for a non-UTC time", cst.Detail)
	}
}

// TestNewFromKubeconfigSelectsContext mirrors pkg/argo's own test of the same name: same
// fixture shape, same TEST-NET-1 placeholders, confirming pkg/rollout follows the identical
// client-go adaptor conventions pkg/k8s established (AGENTS.md §4.4/§4.6).
func TestNewFromKubeconfigSelectsContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
current-context: staging
clusters:
- name: staging
  cluster: {server: "` + testServer + `", insecure-skip-tls-verify: true}
- name: closed
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: me
  user: {token: not-a-real-token}
contexts:
- name: staging
  context: {cluster: staging, user: me}
- name: closed-port
  context: {cluster: closed, user: me}
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	if _, got, err := NewFromKubeconfig(""); err != nil || got != "staging" {
		t.Errorf("NewFromKubeconfig(\"\") = %q, %v; want the current context staging", got, err)
	}
	_, _, err := NewFromKubeconfig("nope")
	if err == nil || !strings.Contains(err.Error(), `kube context "nope" is not in the kubeconfig`) {
		t.Errorf("unknown context: %v", err)
	}

	r, _, err := NewFromKubeconfig("closed-port")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = r.Deployment(ctx, "app-production", "app")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), ":1/") || strings.Contains(err.Error(), "https://") {
		t.Errorf("error names the server: %v", err)
	}
	if !strings.Contains(err.Error(), "reading Deployment app-production/app") {
		t.Errorf("error lost its context: %v", err)
	}
}
