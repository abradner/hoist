// Package rollout reads Deployment rollout completeness, and reports current Job/CronJob
// state, through the typed client-go clientset (k8s.io/api/apps/v1, k8s.io/api/batch/v1) — no
// dynamic client needed, since these are stable, already-vendored Go types — following the same
// client-go adaptor shape pkg/k8s established (cluster.go/kubeconfig.go/fake.go): a thin
// wrapper over kubernetes.Interface, redaction at every error boundary, a fake clientset for
// tests. Nothing here knows what a "promotion" is or what image a caller wanted — that
// comparison is internal/engine's job (AGENTS.md §4.3: pkg/* is activity-shaped, with no
// domain knowledge of the orchestration above it); this package only reads cluster facts.
//
// The rollout-completeness check is deploymentRolloutComplete below: logic ported from
// k8s.io/kubectl/pkg/polymorphichelpers/rollout_status.go (Apache License 2.0) rather than
// importing k8s.io/kubectl itself for the ~60 lines this needs (AGENTS.md §4.7).
package rollout

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abradner/hoist/pkg/redact"
)

// ErrNotFound is wrapped by Deployment and JobLike when the named object does not exist.
var ErrNotFound = errors.New("rollout: object not found")

// ContainerImage is one container's current, live image reference, as the Deployment's own
// spec.template.spec.containers[]/initContainers[].image field holds it — the field a
// promotion actually writes (AGENTS.md invariant 4), not a runtime pod status.
type ContainerImage struct {
	Name  string // the container's own name field
	Init  bool   // an initContainer
	Image string
}

// DeploymentStatus is one Deployment's current rollout state, read straight from its own
// object — no domain knowledge of what any caller wanted it to say.
type DeploymentStatus struct {
	Namespace, Name string
	Images          []ContainerImage
	// Complete reports whether the rollout has finished, by kubectl's own four-condition test
	// (deploymentRolloutComplete). DeadlineExceeded is the one condition kubectl treats as a
	// hard failure rather than "still rolling out" — the Deployment's own progressDeadlineSeconds
	// has been exceeded — and is reported here rather than as a Go error, since it is a fact
	// about cluster state, not a call failure.
	Complete         bool
	DeadlineExceeded bool
	// Detail is a short, kubectl-style human-readable line: what's still pending, or why the
	// deadline was exceeded, or that the rollout finished.
	Detail string
}

// JobLikeStatus is a Job or CronJob's current state, report-only (AGENTS.md invariant 4:
// hoist never gates on these, only surfaces them).
type JobLikeStatus struct {
	Namespace, Name, Kind string // Kind is "Job" or "CronJob"
	Detail                string
}

// Rollout is what internal/engine's RolledOutStep (and `hoist watch`) need from the cluster's
// workloads. Every method reads; nothing here writes.
type Rollout interface {
	// Deployment reads namespace/name's current images and rollout completeness.
	Deployment(ctx context.Context, namespace, name string) (DeploymentStatus, error)
	// JobLike reads namespace/name's current state, for kind "Job" or "CronJob".
	JobLike(ctx context.Context, namespace, name, kind string) (JobLikeStatus, error)
}

// client is Rollout over a client-go clientset, mirroring pkg/k8s.client's shape exactly.
type client struct {
	cs   kubernetes.Interface
	hide []string
}

// FromClientset wraps an existing clientset — client-go's fake in tests. Every string in hide
// is scrubbed from every error message the returned Rollout produces.
func FromClientset(cs kubernetes.Interface, hide ...string) Rollout {
	return &client{cs: cs, hide: hide}
}

// NewFromKubeconfig builds a Rollout over the user's kubeconfig, the same loading rules and
// the same "duplicated on purpose, not shared" reasoning as pkg/argo.NewFromKubeconfig's doc
// comment explains (pkg/k8s.NewCluster's own shape, without a cross-package dependency between
// self-contained adaptors — AGENTS.md §4.3). The second result is the context in use.
func NewFromKubeconfig(kubeconfigContext string) (Rollout, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeconfigContext}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, "", fmt.Errorf("rollout: reading kubeconfig: %w", err)
	}
	name := kubeconfigContext
	if name == "" {
		name = raw.CurrentContext
	}
	if name == "" {
		return nil, "", fmt.Errorf("rollout: kubeconfig has no current context; pass --kube-context")
	}
	if _, ok := raw.Contexts[name]; !ok {
		return nil, "", fmt.Errorf("rollout: kube context %q is not in the kubeconfig", name)
	}
	rest, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("rollout: kube context %q: %w", name, err)
	}
	hide := redact.Host(rest.Host)
	cs, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return nil, "", fmt.Errorf("rollout: kube context %q: %s", name, redact.Error(err, hide...))
	}
	return FromClientset(cs, hide...), name, nil
}

// Deployment implements Rollout.
func (c *client) Deployment(ctx context.Context, namespace, name string) (DeploymentStatus, error) {
	if namespace == "" || name == "" {
		return DeploymentStatus{}, errors.New("rollout: deployment needs a namespace and a name")
	}
	d, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return DeploymentStatus{}, fmt.Errorf("reading Deployment %s/%s: %w", namespace, name, ErrNotFound)
		}
		return DeploymentStatus{}, fmt.Errorf("reading Deployment %s/%s: %s", namespace, name, c.describe(err))
	}
	st := DeploymentStatus{Namespace: namespace, Name: name}
	for _, ctr := range d.Spec.Template.Spec.Containers {
		st.Images = append(st.Images, ContainerImage{Name: ctr.Name, Image: ctr.Image})
	}
	for _, ctr := range d.Spec.Template.Spec.InitContainers {
		st.Images = append(st.Images, ContainerImage{Name: ctr.Name, Init: true, Image: ctr.Image})
	}
	st.Complete, st.DeadlineExceeded, st.Detail = deploymentRolloutComplete(d)
	return st, nil
}

// JobLike implements Rollout.
func (c *client) JobLike(ctx context.Context, namespace, name, kind string) (JobLikeStatus, error) {
	if namespace == "" || name == "" {
		return JobLikeStatus{}, errors.New("rollout: job needs a namespace and a name")
	}
	st := JobLikeStatus{Namespace: namespace, Name: name, Kind: kind}
	switch kind {
	case "Job":
		j, err := c.cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return JobLikeStatus{}, fmt.Errorf("reading Job %s/%s: %w", namespace, name, ErrNotFound)
			}
			return JobLikeStatus{}, fmt.Errorf("reading Job %s/%s: %s", namespace, name, c.describe(err))
		}
		st.Detail = jobDetail(j)
	case "CronJob":
		cj, err := c.cs.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return JobLikeStatus{}, fmt.Errorf("reading CronJob %s/%s: %w", namespace, name, ErrNotFound)
			}
			return JobLikeStatus{}, fmt.Errorf("reading CronJob %s/%s: %s", namespace, name, c.describe(err))
		}
		st.Detail = cronJobDetail(cj)
	default:
		return JobLikeStatus{}, fmt.Errorf("rollout: unknown kind %q, want Job or CronJob", kind)
	}
	return st, nil
}

// jobDetail is a short, report-only summary of a Job's current state (AGENTS.md invariant 4:
// never gated on, only surfaced).
func jobDetail(j *batchv1.Job) string {
	return fmt.Sprintf("active=%d succeeded=%d failed=%d", j.Status.Active, j.Status.Succeeded, j.Status.Failed)
}

// cronJobDetail is the same, for a CronJob.
func cronJobDetail(cj *batchv1.CronJob) string {
	last := "never"
	if cj.Status.LastScheduleTime != nil {
		last = cj.Status.LastScheduleTime.Format("2006-01-02T15:04:05Z")
	}
	return fmt.Sprintf("active=%d last-scheduled=%s", len(cj.Status.Active), last)
}

// deploymentRolloutComplete reports whether d's rollout has finished, the same four conditions
// kubectl's own `kubectl rollout status` checks (see the package doc comment for the
// attribution): the spec update has been observed, the rollout has not exceeded its own
// progress deadline, every replica has been updated, no old replica is left, and every updated
// replica is available. detail is a short kubectl-style progress line either way;
// deadlineExceeded is true only for the one condition kubectl treats as a hard failure rather
// than "still waiting" — the Deployment's own Progressing condition carries Reason
// "ProgressDeadlineExceeded" when the controller gives up.
func deploymentRolloutComplete(d *appsv1.Deployment) (complete, deadlineExceeded bool, detail string) {
	if d.Generation > d.Status.ObservedGeneration {
		return false, false, "waiting for the deployment spec update to be observed"
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return false, true, fmt.Sprintf("deployment %q exceeded its progress deadline", d.Name)
		}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	switch {
	case d.Status.UpdatedReplicas < want:
		return false, false, fmt.Sprintf("waiting for rollout to finish: %d out of %d new replicas have been updated", d.Status.UpdatedReplicas, want)
	case d.Status.Replicas > d.Status.UpdatedReplicas:
		return false, false, fmt.Sprintf("waiting for rollout to finish: %d old replicas are pending termination", d.Status.Replicas-d.Status.UpdatedReplicas)
	case d.Status.AvailableReplicas < d.Status.UpdatedReplicas:
		return false, false, fmt.Sprintf("waiting for rollout to finish: %d of %d updated replicas are available", d.Status.AvailableReplicas, d.Status.UpdatedReplicas)
	default:
		return true, false, fmt.Sprintf("deployment %q successfully rolled out", d.Name)
	}
}

// describe renders a client-go error without the API server or any response detail, mirroring
// pkg/k8s's client.describe exactly (AGENTS.md §4.4/§4.6 in this package).
func (c *client) describe(err error) string {
	if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
		return redact.Strings(string(reason), c.hide...)
	}
	return redact.Error(err, c.hide...)
}
