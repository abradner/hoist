package rollout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The workload kinds Restart can roll: the three apps/v1 controllers whose pods come from a pod
// template that `kubectl rollout restart` stamps. A Job or CronJob has no rollout to trigger, and
// re-running one is a different operation with different consequences, so neither is here.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// RestartableKinds lists the kinds Restart and Workload accept, in the order a restart lists
// them. A fresh slice per call, so no caller can change what another one sees.
func RestartableKinds() []string { return []string{KindDeployment, KindStatefulSet, KindDaemonSet} }

// IsRestartable reports whether kind is one RestartableKinds lists.
func IsRestartable(kind string) bool {
	for _, k := range RestartableKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// StrategyOnDelete is the update strategy, available to StatefulSets and DaemonSets, under which
// the controller never replaces a pod itself: a changed pod template reaches a pod only when that
// pod is deleted. A restart stamp on such a workload is written and then does nothing on its own,
// which is why both GracefulRestartConcerns and the rollout check name it rather than treat it as
// either a rollout in progress or a finished one.
const StrategyOnDelete = "OnDelete"

// WorkloadStatus is one restartable workload's current state — a Deployment, StatefulSet or
// DaemonSet — read straight from its own object, with no knowledge of what any caller wanted it to
// say. It is what a restart reads before writing, and again while watching what followed.
//
// DeploymentStatus stays the read RolledOutStep and `hoist watch` use; a Deployment's
// WorkloadStatus is that same read (DeploymentStatus.Workload), so the two cannot disagree about
// one Deployment.
type WorkloadStatus struct {
	Kind, Namespace, Name string
	Images                []ContainerImage
	// Complete and DeadlineExceeded mean what they mean on DeploymentStatus, by kubectl's own
	// per-kind `rollout status` test. Only a Deployment has a progress deadline. An OnDelete
	// workload is never Complete: its rollout does not happen until someone deletes its pods.
	Complete         bool
	DeadlineExceeded bool
	Detail           string
	// RestartedAt is the pod template's RestartAnnotation value, "" when never restarted this way.
	RestartedAt string
	// Replicas is spec.replicas for a Deployment or StatefulSet (1 when unset), and for a
	// DaemonSet status.desiredNumberScheduled — one pod per node it should run on, which is the
	// number a DaemonSet's own maxUnavailable and maxSurge are resolved against.
	Replicas int32
	// Strategy is spec.strategy.type for a Deployment and spec.updateStrategy.type otherwise,
	// with Kubernetes' own default ("RollingUpdate") when unset.
	Strategy string
	// MaxUnavailable and MaxSurge are resolved against Replicas by the kind's own controller
	// rules (DeploymentStatus documents a Deployment's; see Workload for the others). A
	// StatefulSet never surges, so its MaxSurge is always 0.
	MaxUnavailable int32
	MaxSurge       int32
	// Partition is a StatefulSet's rollingUpdate.partition: pods with an ordinal below it keep
	// the old template through a rolling update. 0 for every other kind.
	Partition int32
	// ParallelPods is true for a StatefulSet whose podManagementPolicy is Parallel, the one
	// policy under which its rollingUpdate.maxUnavailable is honoured; under the default
	// OrderedReady pods are replaced one at a time whatever that field says.
	ParallelPods bool
	// ReadinessProbes counts containers that declare one.
	ReadinessProbes int
}

// Workload converts a Deployment's read into the WorkloadStatus a restart works with.
func (d DeploymentStatus) Workload() WorkloadStatus {
	return WorkloadStatus{
		Kind: KindDeployment, Namespace: d.Namespace, Name: d.Name,
		Images:   d.Images,
		Complete: d.Complete, DeadlineExceeded: d.DeadlineExceeded, Detail: d.Detail,
		RestartedAt: d.RestartedAt,
		Replicas:    d.Replicas, Strategy: d.Strategy,
		MaxUnavailable: d.MaxUnavailable, MaxSurge: d.MaxSurge,
		ReadinessProbes: d.ReadinessProbes,
	}
}

// GracefulRestartConcerns lists, in a stable order, the reasons a restart of this workload is
// unlikely to be seamless — or, for an OnDelete workload, will not happen on its own at all.
// Empty when there is nothing to say. Informational: the caller shows them and proceeds
// (principle 5).
//
// Each is stated only when it is actually true of this workload's own settings. An earlier
// version warned "only 1 replica: nothing serves while the new pod starts" on replica count
// alone, which is wrong for a Deployment under the default strategy: 25% maxUnavailable of one
// replica rounds down to zero, so the old pod keeps serving until the new one is ready. A
// warning that fires on a workload that is in fact fine is worse than none, because it teaches
// the operator to skip reading them. The same rule is why a StatefulSet's maxUnavailable is
// consulted only under the Parallel policy, the one where the controller honours it.
func (w WorkloadStatus) GracefulRestartConcerns() []string {
	if w.Replicas == 0 {
		// Nothing to roll, and none of the rest applies.
		if w.Kind == KindDaemonSet {
			return []string{"scheduled on no node: a restart changes the pod template but starts no pod"}
		}
		return []string{"scaled to 0 replicas: a restart changes the pod template but starts no pod"}
	}
	var out []string
	switch {
	case w.Strategy == StrategyOnDelete:
		// First, and instead of any strategy-shaped warning: there is no rollout to be graceful
		// or not. The stamp lands and every pod keeps the template it started with.
		out = append(out, fmt.Sprintf("update strategy is OnDelete: the restart stamps the pod template, but no pod restarts until it is deleted, and hoist will not report this %s as rolled", w.Kind))
	case w.Kind == KindDeployment:
		switch {
		case w.Strategy == "Recreate":
			out = append(out, "strategy is Recreate: every pod stops before any new one starts")
		case w.MaxUnavailable >= w.Replicas:
			out = append(out, fmt.Sprintf("maxUnavailable is %d of %d replica(s): every pod can be down at once", w.MaxUnavailable, w.Replicas))
		case w.Replicas == 1:
			// The old pod does keep serving here — the risk is what is behind it, which is nothing.
			out = append(out, "only 1 replica: it keeps serving until the replacement is ready, but there is no redundancy if the replacement fails")
		}
	case w.Kind == KindStatefulSet:
		if w.Partition > 0 {
			if w.Partition >= w.Replicas {
				out = append(out, fmt.Sprintf("rollingUpdate.partition is %d of %d replica(s): no pod restarts", w.Partition, w.Replicas))
			} else {
				out = append(out, fmt.Sprintf("rollingUpdate.partition is %d: only pods with ordinal %d and above restart; the other %d keep running", w.Partition, w.Partition, w.Partition))
			}
		}
		switch {
		case w.Replicas == 1:
			// A StatefulSet never surges: it deletes a pod and only then creates its successor
			// under the same identity, so one replica is a real gap, not a redundancy question.
			out = append(out, "only 1 replica: a StatefulSet stops its pod before starting the replacement, so nothing serves until the new one is ready")
		case w.ParallelPods && w.MaxUnavailable >= w.Replicas:
			out = append(out, fmt.Sprintf("maxUnavailable is %d of %d replica(s) under the Parallel policy: every pod can be down at once", w.MaxUnavailable, w.Replicas))
		}
	case w.Kind == KindDaemonSet:
		if w.MaxSurge == 0 {
			if w.MaxUnavailable >= w.Replicas {
				out = append(out, fmt.Sprintf("maxUnavailable is %d of %d pod(s) and maxSurge is 0: every node's pod can be down at once", w.MaxUnavailable, w.Replicas))
			} else {
				out = append(out, "maxSurge is 0: on each node the old pod stops before its replacement starts, so that node runs none in between")
			}
		}
	}
	if w.ReadinessProbes == 0 {
		out = append(out, "no readiness probe: a new pod counts as available the moment it starts, before it can serve")
	}
	// The one that undercuts "a restart changes nothing about what runs". It is true of the
	// DECLARED reference and only that: a container on a mutable tag can pull a different digest
	// when its replacement lands on a node without the image cached, or on every pull under
	// imagePullPolicy: Always. This repo's own fixtures carry bare-tag Deployments, and so does
	// the target repo, so this is not a hypothetical.
	if un := w.unpinnedImages(); len(un) > 0 {
		out = append(out, fmt.Sprintf("unpinned image(s) %s: a replacement pod can pull a different build than the one running now, so this restart may not be a no-op",
			strings.Join(un, ", ")))
	}
	return out
}

// unpinnedImages lists this workload's live container references that name no digest, sorted.
func (w WorkloadStatus) unpinnedImages() []string {
	var out []string
	for _, img := range w.Images {
		if !strings.Contains(img.Image, "@sha256:") {
			out = append(out, img.Image)
		}
	}
	sort.Strings(out)
	return out
}

// Workload implements Rollout.
//
// A Deployment is read through Deployment itself, so a restart and RolledOutStep see one
// Deployment identically. A StatefulSet's and a DaemonSet's settings are resolved by their own
// controllers' rules, not a Deployment's: a StatefulSet's maxUnavailable defaults to 1 and a
// percentage rounds up, never below 1; a DaemonSet's maxUnavailable defaults to 1 and its
// maxSurge to 0, both rounding up against desiredNumberScheduled.
func (c *client) Workload(ctx context.Context, namespace, name, kind string) (WorkloadStatus, error) {
	if namespace == "" || name == "" {
		return WorkloadStatus{}, errors.New("rollout: workload needs a namespace and a name")
	}
	switch kind {
	case KindDeployment:
		d, err := c.Deployment(ctx, namespace, name)
		if err != nil {
			return WorkloadStatus{}, err
		}
		return d.Workload(), nil
	case KindStatefulSet:
		s, err := c.cs.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatus{}, c.readErr(kind, namespace, name, err)
		}
		return statefulSetStatus(s), nil
	case KindDaemonSet:
		d, err := c.cs.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return WorkloadStatus{}, c.readErr(kind, namespace, name, err)
		}
		return daemonSetStatus(d), nil
	default:
		return WorkloadStatus{}, fmt.Errorf("rollout: cannot read kind %q as a restartable workload, want one of %s", kind, strings.Join(RestartableKinds(), ", "))
	}
}

func (c *client) readErr(kind, namespace, name string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, ErrNotFound)
	}
	return fmt.Errorf("reading %s %s/%s: %s", kind, namespace, name, c.describe(err))
}

func statefulSetStatus(s *appsv1.StatefulSet) WorkloadStatus {
	st := WorkloadStatus{Kind: KindStatefulSet, Namespace: s.Namespace, Name: s.Name}
	st.Images = templateImages(s.Spec.Template.Spec.Containers, s.Spec.Template.Spec.InitContainers)
	st.RestartedAt = s.Spec.Template.Annotations[RestartAnnotation]
	st.Replicas = 1
	if s.Spec.Replicas != nil {
		st.Replicas = *s.Spec.Replicas
	}
	st.Strategy = string(s.Spec.UpdateStrategy.Type)
	if st.Strategy == "" {
		st.Strategy = string(appsv1.RollingUpdateStatefulSetStrategyType)
	}
	st.ParallelPods = s.Spec.PodManagementPolicy == appsv1.ParallelPodManagement
	if st.Strategy == string(appsv1.RollingUpdateStatefulSetStrategyType) {
		mu := intstr.FromInt32(1)
		if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil {
			if ru.Partition != nil {
				st.Partition = *ru.Partition
			}
			if ru.MaxUnavailable != nil {
				mu = *ru.MaxUnavailable
			}
		}
		u, _ := intstr.GetScaledValueFromIntOrPercent(&mu, int(st.Replicas), true)
		st.MaxUnavailable = int32(max(u, 1))
	}
	st.ReadinessProbes = readinessProbes(s.Spec.Template.Spec.Containers)
	st.Complete, st.Detail = statefulSetRolloutComplete(s)
	return st
}

func daemonSetStatus(d *appsv1.DaemonSet) WorkloadStatus {
	st := WorkloadStatus{Kind: KindDaemonSet, Namespace: d.Namespace, Name: d.Name}
	st.Images = templateImages(d.Spec.Template.Spec.Containers, d.Spec.Template.Spec.InitContainers)
	st.RestartedAt = d.Spec.Template.Annotations[RestartAnnotation]
	st.Replicas = d.Status.DesiredNumberScheduled
	st.Strategy = string(d.Spec.UpdateStrategy.Type)
	if st.Strategy == "" {
		st.Strategy = string(appsv1.RollingUpdateDaemonSetStrategyType)
	}
	if st.Strategy == string(appsv1.RollingUpdateDaemonSetStrategyType) {
		mu, ms := intstr.FromInt32(1), intstr.FromInt32(0)
		if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil {
			if ru.MaxUnavailable != nil {
				mu = *ru.MaxUnavailable
			}
			if ru.MaxSurge != nil {
				ms = *ru.MaxSurge
			}
		}
		u, _ := intstr.GetScaledValueFromIntOrPercent(&mu, int(st.Replicas), true)
		sg, _ := intstr.GetScaledValueFromIntOrPercent(&ms, int(st.Replicas), true)
		st.MaxUnavailable, st.MaxSurge = int32(u), int32(sg)
	}
	st.ReadinessProbes = readinessProbes(d.Spec.Template.Spec.Containers)
	st.Complete, st.Detail = daemonSetRolloutComplete(d)
	return st
}

func templateImages(containers, inits []corev1.Container) []ContainerImage {
	var out []ContainerImage
	for _, ctr := range containers {
		out = append(out, ContainerImage{Name: ctr.Name, Image: ctr.Image})
	}
	for _, ctr := range inits {
		out = append(out, ContainerImage{Name: ctr.Name, Init: true, Image: ctr.Image})
	}
	return out
}

func readinessProbes(containers []corev1.Container) int {
	n := 0
	for _, ctr := range containers {
		if ctr.ReadinessProbe != nil {
			n++
		}
	}
	return n
}

// statefulSetRolloutComplete is kubectl's StatefulSetStatusViewer
// (k8s.io/kubectl/pkg/polymorphichelpers/rollout_status.go, Apache License 2.0), ported for the
// same reason deploymentRolloutComplete is (§4.7), with two deliberate differences. kubectl
// refuses an OnDelete StatefulSet outright; here it is reported, never complete, with a detail
// saying why, so a watch can name it instead of failing. And kubectl treats a RollingUpdate
// block with no partition as complete without looking at the replicas; here an unset partition
// is the API server's own default of 0, so the rollout is complete only once every pod is
// updated.
func statefulSetRolloutComplete(s *appsv1.StatefulSet) (complete bool, detail string) {
	if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return false, fmt.Sprintf("statefulset %q uses the OnDelete update strategy: its pods take the new template only when deleted", s.Name)
	}
	if s.Status.ObservedGeneration == 0 || s.Generation > s.Status.ObservedGeneration {
		return false, "waiting for the statefulset spec update to be observed"
	}
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	if s.Status.ReadyReplicas < replicas {
		return false, fmt.Sprintf("waiting for %d pods to be ready", replicas-s.Status.ReadyReplicas)
	}
	var partition int32
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		partition = *ru.Partition
	}
	if partition > 0 {
		if s.Status.UpdatedReplicas < replicas-partition {
			return false, fmt.Sprintf("waiting for partitioned roll out to finish: %d out of %d new pods have been updated", s.Status.UpdatedReplicas, replicas-partition)
		}
		return true, fmt.Sprintf("partitioned roll out complete: %d new pods have been updated; %d below the partition were not", s.Status.UpdatedReplicas, partition)
	}
	if s.Status.UpdatedReplicas < replicas || s.Status.UpdateRevision != s.Status.CurrentRevision {
		return false, fmt.Sprintf("waiting for statefulset rolling update to complete %d pods at revision %s", s.Status.UpdatedReplicas, s.Status.UpdateRevision)
	}
	return true, fmt.Sprintf("statefulset %q rolling update complete %d pods at revision %s", s.Name, s.Status.CurrentReplicas, s.Status.CurrentRevision)
}

// daemonSetRolloutComplete is kubectl's DaemonSetStatusViewer (same source and licence), with the
// same OnDelete difference as statefulSetRolloutComplete.
func daemonSetRolloutComplete(d *appsv1.DaemonSet) (complete bool, detail string) {
	if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
		return false, fmt.Sprintf("daemon set %q uses the OnDelete update strategy: its pods take the new template only when deleted", d.Name)
	}
	if d.Generation > d.Status.ObservedGeneration {
		return false, "waiting for the daemon set spec update to be observed"
	}
	if d.Status.UpdatedNumberScheduled < d.Status.DesiredNumberScheduled {
		return false, fmt.Sprintf("waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated", d.Name, d.Status.UpdatedNumberScheduled, d.Status.DesiredNumberScheduled)
	}
	if d.Status.NumberAvailable < d.Status.DesiredNumberScheduled {
		return false, fmt.Sprintf("waiting for daemon set %q rollout to finish: %d of %d updated pods are available", d.Name, d.Status.NumberAvailable, d.Status.DesiredNumberScheduled)
	}
	return true, fmt.Sprintf("daemon set %q successfully rolled out", d.Name)
}
