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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
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
	// RestartedAt is the pod template's RestartAnnotation value, "" when it has never been
	// restarted this way. Read so a restart can say what it supersedes, and so a caller can
	// tell its own stamp from someone else's.
	RestartedAt string
	// The fields below describe whether a restart of this Deployment can actually be graceful.
	// They are read here because the read that fetches them is already being made; hoist warns
	// on them and never blocks (AGENTS.md principle 5), since "roll it anyway" is a legitimate
	// thing to want and the operator is the one who knows.
	//
	// Replicas is spec.replicas (1 when unset, matching Kubernetes' own default). Strategy is
	// spec.strategy.type ("RollingUpdate" when unset). MaxUnavailable and MaxSurge are that
	// strategy's own values already RESOLVED against Replicas by Kubernetes' own rules —
	// percentages round down for unavailable and up for surge — because the raw 25%/25%
	// defaults say nothing on their own: at one replica they resolve to 0 and 1, which is
	// precisely the case where a naive "one replica means downtime" claim is wrong.
	// ReadinessProbes counts containers that declare one.
	Replicas        int32
	Strategy        string
	MaxUnavailable  int32
	MaxSurge        int32
	ReadinessProbes int
}

// GracefulRestartConcerns lists, in a stable order, the reasons a restart of this Deployment is
// unlikely to be seamless. Empty when there is nothing to say. Informational: the caller shows
// them and proceeds (principle 5).
//
// Each is stated only when it is actually true of this Deployment's own settings. An earlier
// version warned "only 1 replica: nothing serves while the new pod starts" on replica count
// alone, which is wrong under the default strategy: 25% maxUnavailable of one replica rounds
// down to zero, so the old pod keeps serving until the new one is ready. A warning that fires on
// a Deployment that is in fact fine is worse than none, because it teaches the operator to skip
// reading them.
func (d DeploymentStatus) GracefulRestartConcerns() []string {
	var out []string
	switch {
	case d.Replicas == 0:
		// Nothing to roll, and none of the rest applies.
		return []string{"scaled to 0 replicas: a restart changes the pod template but starts no pod"}
	case d.Strategy == "Recreate":
		out = append(out, "strategy is Recreate: every pod stops before any new one starts")
	case d.MaxUnavailable >= d.Replicas:
		out = append(out, fmt.Sprintf("maxUnavailable is %d of %d replica(s): every pod can be down at once", d.MaxUnavailable, d.Replicas))
	case d.Replicas == 1:
		// The old pod does keep serving here — the risk is what is behind it, which is nothing.
		out = append(out, "only 1 replica: it keeps serving until the replacement is ready, but there is no redundancy if the replacement fails")
	}
	if d.ReadinessProbes == 0 {
		out = append(out, "no readiness probe: a new pod counts as available the moment it starts, before it can serve")
	}
	return out
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
	// Restart rolls namespace/name's pods without changing what it runs, by stamping the pod
	// template's restart annotation — exactly what `kubectl rollout restart` does. It is the
	// ONE write in this package; everything else here reads (see the package doc, and
	// pkg/argo.Refresh, which holds the same position there).
	Restart(ctx context.Context, namespace, name string, at time.Time) error
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
	// client-go's default warning handler writes API-server warnings straight to stderr through
	// klog, in klog's own format, unfiltered and outside every boundary this package maintains:
	// a `restricted:latest` PodSecurity warning landed mid-output on the first real restart,
	// between hoist's own lines, with a raw server message hoist had never seen (§4.4 —
	// everything this package emits goes through pkg/redact first). hoist reports what it needs
	// to itself, so the handler is silenced rather than re-plumbed.
	rest.WarningHandler = restclient.NoWarnings{}
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
	st.RestartedAt = d.Spec.Template.Annotations[RestartAnnotation]
	st.Replicas = 1 // Kubernetes' own default when spec.replicas is unset.
	if d.Spec.Replicas != nil {
		st.Replicas = *d.Spec.Replicas
	}
	st.Strategy = string(d.Spec.Strategy.Type)
	if st.Strategy == "" {
		st.Strategy = "RollingUpdate"
	}
	// Resolved by Kubernetes' own rules, not left as raw percentages: maxUnavailable rounds
	// down, maxSurge rounds up, and both default to 25%. At one replica that is 0 and 1 — the
	// numbers that decide whether a single-replica rollout actually drops traffic.
	if st.Strategy == "RollingUpdate" {
		mu := intstr.FromString("25%")
		ms := intstr.FromString("25%")
		if ru := d.Spec.Strategy.RollingUpdate; ru != nil {
			if ru.MaxUnavailable != nil {
				mu = *ru.MaxUnavailable
			}
			if ru.MaxSurge != nil {
				ms = *ru.MaxSurge
			}
		}
		u, _ := intstr.GetScaledValueFromIntOrPercent(&mu, int(st.Replicas), false)
		sg, _ := intstr.GetScaledValueFromIntOrPercent(&ms, int(st.Replicas), true)
		st.MaxUnavailable, st.MaxSurge = int32(u), int32(sg)
	}
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.ReadinessProbe != nil {
			st.ReadinessProbes++
		}
	}
	st.Complete, st.DeadlineExceeded, st.Detail = deploymentRolloutComplete(d)
	return st, nil
}

// RestartAnnotation is the pod-template annotation Restart stamps. Deliberately kubectl's own
// key: `kubectl rollout restart` writes exactly this, so a restart hoist causes is
// indistinguishable from one an operator caused by hand, and either tool can see the other's.
//
// Nothing in Kubernetes treats the key specially. Any change to the pod template starts a
// rollout; this one is chosen because it changes nothing else.
const RestartAnnotation = "kubectl.kubernetes.io/restartedAt"

// Restart implements Rollout.
//
// The caller must pass an `at` whose RFC3339 rendering differs from the Deployment's current
// RestartedAt. RFC3339 has second resolution, so two calls in the same second patch the pod
// template to the value it already holds — which is not a change, so no rollout starts, and a
// caller watching for one would see the PREVIOUS rollout's completion and report success for a
// restart that never happened. This is not enforced here because enforcing it means a read, and
// a read here would both cost a round trip and race the write; cmd/hoist has already read the
// Deployment before it calls this, so it is the honest place to hold the constraint.
//
// A strategic-merge patch of one annotation, which is what kubectl issues for the same command.
// Argo does not treat this as drift even with selfHeal on: its diff is a three-way merge, so a
// field Argo never set and that is absent from the manifest is owned by someone else and left
// alone — the same reason `kubectl apply` does not delete fields it never wrote. (Verified
// against the target cluster: annotations added this way have survived weeks on self-healing
// Applications that report Synced. An earlier design wrote this into the manifest instead, on
// the assumption that Argo would revert it; that assumption was wrong and cost a commit, a PR,
// a CI run and an approval for what is one API call.)
func (c *client) Restart(ctx context.Context, namespace, name string, at time.Time) error {
	if namespace == "" || name == "" {
		return errors.New("rollout: restart needs a namespace and a name")
	}
	// RFC3339, quoted by the JSON encoding itself — the value must reach the API server as a
	// string, since an annotation value is one.
	stamp := at.UTC().Format(time.RFC3339)
	patch := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, RestartAnnotation, stamp))
	_, err := c.cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("restarting Deployment %s/%s: %w", namespace, name, ErrNotFound)
		}
		return fmt.Errorf("restarting Deployment %s/%s: %s", namespace, name, c.describe(err))
	}
	return nil
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
		// RFC3339, not a hardcoded "...Z" literal: a hardcoded Z claims UTC regardless of the
		// time's actual zone, printing a false offset if the controller ever returns a non-UTC
		// value. RFC3339's layout renders whatever zone the time actually carries.
		last = cj.Status.LastScheduleTime.Format(time.RFC3339)
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
