// Package k8s reads what a namespace is running and, opt-in, one image pull secret.
//
// It is the read-only, scoped Kubernetes adaptor of AGENTS.md §4.2 and the repo map's
// "kubeconfig" boundary: one kubeconfig context named by the caller, one namespace listed,
// one secret read by name. Nothing here writes, and no error, log line or return value
// names the API server (§4.4 — the context *name* may reach output, its address never;
// every client error goes through pkg/redact and the server host is scrubbed from it as
// a second guard). The pull secret's contents never leave the package except as an
// authn.Keychain (R-002), which pkg/registry hands to go-containerregistry.
//
// Which containers count (a stated decision, see RunningImages): pods in phase Running or
// Pending that are not terminating, their app and init containers with a non-empty
// imageID. Completed Jobs, failed pods, pods on the way out and ephemeral debug
// containers do not describe what the env is running.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/go-containerregistry/pkg/authn"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/redact"
)

// RunningImage is one container of one pod and the image it is actually running, as the
// runtime reported it in status.*containerStatuses[].imageID: repo plus digest, never a
// tag (imageID carries none). The repo is the one from imageID, which can differ from the
// manifest's image field (a mirror, a docker.io alias); callers compare repos through
// image.Canonical rather than by string.
type RunningImage struct {
	Pod       string
	Container string
	Init      bool // an initContainer
	Ref       image.Ref
}

// Cluster is what the resolver and the registry auth chain need from a cluster. Both
// methods read; there is no method that writes.
type Cluster interface {
	// RunningImages lists the images running in exactly namespace (see the package doc for
	// which containers count), sorted by pod, then app containers before init containers,
	// then container name.
	RunningImages(ctx context.Context, namespace string) ([]RunningImage, error)
	// DockerConfigSecret reads the kubernetes.io/dockerconfigjson Secret namespace/name
	// and returns its credentials as a keychain. The credentials are not otherwise
	// exposed; the error for a missing or mistyped secret names the secret, never its data.
	DockerConfigSecret(ctx context.Context, namespace, name string) (authn.Keychain, error)
}

// client is Cluster over a client-go clientset.
type client struct {
	cs kubernetes.Interface
	// hide holds the API server address in every spelling the client could echo; each is
	// scrubbed from every error (§4.4). Empty for a clientset that has none (the fake).
	hide []string
}

// FromClientset wraps an existing clientset — client-go's fake in tests. Every string in
// hide is scrubbed from every error message the returned Cluster produces.
func FromClientset(cs kubernetes.Interface, hide ...string) Cluster {
	return &client{cs: cs, hide: hide}
}

// RunningImages implements Cluster.
func (c *client) RunningImages(ctx context.Context, namespace string) ([]RunningImage, error) {
	if namespace == "" {
		return nil, errors.New("k8s: namespace is required")
	}
	pods, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: listing pods in namespace %s: %s", namespace, c.describe(err))
	}
	var out []RunningImage
	for i := range pods.Items {
		out = append(out, runningImages(&pods.Items[i])...)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		if a.Init != b.Init {
			return !a.Init
		}
		return a.Container < b.Container
	})
	return out, nil
}

// runningImages applies the counting rule from the package doc to one pod.
func runningImages(pod *corev1.Pod) []RunningImage {
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
		return nil
	}
	if pod.DeletionTimestamp != nil {
		return nil
	}
	var out []RunningImage
	add := func(statuses []corev1.ContainerStatus, init bool) {
		for _, s := range statuses {
			if s.ImageID == "" {
				continue
			}
			ref, err := image.Parse(s.ImageID)
			// An imageID that is only "sha256:…" (a locally loaded image) or otherwise
			// unparsable names no repo and cannot be matched to a manifest; it is skipped,
			// not an error — the resolver reports the repo as having no running pods.
			if err != nil || ref.Repo == "" || ref.Digest == "" {
				continue
			}
			ref.Tag = ""
			out = append(out, RunningImage{Pod: pod.Name, Container: s.Name, Init: init, Ref: ref})
		}
	}
	add(pod.Status.ContainerStatuses, false)
	add(pod.Status.InitContainerStatuses, true)
	return out
}

// DockerConfigSecret implements Cluster.
func (c *client) DockerConfigSecret(ctx context.Context, namespace, name string) (authn.Keychain, error) {
	if namespace == "" || name == "" {
		return nil, errors.New("k8s: cluster secret needs namespace/name")
	}
	which := "cluster secret " + namespace + "/" + name
	sec, err := c.cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: %s", which, c.describe(err))
	}
	if sec.Type != corev1.SecretTypeDockerConfigJson {
		return nil, fmt.Errorf("%s: type %s, want %s", which, sec.Type, corev1.SecretTypeDockerConfigJson)
	}
	kc, err := parseDockerConfig(sec.Data[corev1.DockerConfigJsonKey])
	if err != nil {
		// The parse error is ours and carries no data; the JSON never reaches a message.
		return nil, fmt.Errorf("%s: %w", which, err)
	}
	return kc, nil
}

// describe renders a client-go error without the API server or any response detail: the
// status reason for an API error, the redacted cause for a transport error.
func (c *client) describe(err error) string {
	if reason := apierrors.ReasonForError(err); reason != metav1.StatusReasonUnknown {
		return redact.Strings(string(reason), c.hide...)
	}
	return redact.Error(err, c.hide...)
}
