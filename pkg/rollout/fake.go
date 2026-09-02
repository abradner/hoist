package rollout

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Rollout for tests in other packages (internal/engine's step tests in
// particular — mirroring pkg/argo.Fake/pkg/forge.Fake/pkg/git's test doubles). An unconfigured
// Deployment or JobLike reports the zero status with a nil error — mirroring pkg/k8s.Fake's
// RunningImages (a legitimately-empty answer, not a NotFound), since a test that hasn't
// configured one is not opting into the not-found scenario; a test that wants ErrNotFound sets
// DeploymentErr/JobLikeErr itself. Calls records every method invocation, in order.
type Fake struct {
	mu sync.Mutex

	Deployments map[depKey]DeploymentStatus
	JobLikes    map[jobKey]JobLikeStatus
	// DeploymentErr and JobLikeErr, when set, are returned by every call to the matching
	// method instead of the configured/zero-value behavior.
	DeploymentErr, JobLikeErr error

	Calls []string
}

type depKey struct{ Namespace, Name string }
type jobKey struct{ Namespace, Name, Kind string }

// SetDeployment records namespace/name's current status, thread-safely.
func (f *Fake) SetDeployment(namespace, name string, st DeploymentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Deployments == nil {
		f.Deployments = map[depKey]DeploymentStatus{}
	}
	f.Deployments[depKey{namespace, name}] = st
}

// SetJobLike records namespace/name/kind's current status, thread-safely.
func (f *Fake) SetJobLike(namespace, name, kind string, st JobLikeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.JobLikes == nil {
		f.JobLikes = map[jobKey]JobLikeStatus{}
	}
	f.JobLikes[jobKey{namespace, name, kind}] = st
}

// Deployment implements Rollout.
func (f *Fake) Deployment(_ context.Context, namespace, name string) (DeploymentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("Deployment %s/%s", namespace, name))
	if f.DeploymentErr != nil {
		return DeploymentStatus{}, f.DeploymentErr
	}
	return f.Deployments[depKey{namespace, name}], nil
}

// JobLike implements Rollout.
func (f *Fake) JobLike(_ context.Context, namespace, name, kind string) (JobLikeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("JobLike %s %s/%s", kind, namespace, name))
	if f.JobLikeErr != nil {
		return JobLikeStatus{}, f.JobLikeErr
	}
	return f.JobLikes[jobKey{namespace, name, kind}], nil
}
