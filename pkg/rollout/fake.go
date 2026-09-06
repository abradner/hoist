package rollout

import (
	"context"
	"fmt"
	"sync"
	"time"
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
	DeploymentErr, JobLikeErr, RestartErr error
	// Strict makes an unknown Deployment return ErrNotFound, as the real client does, instead
	// of a zero-value status. Opt-in rather than the default because most tests here configure
	// only the Deployments they care about and rely on the zero value for the rest — but a test
	// about ABSENCE cannot use a fake that reports everything as present, and shipping code that
	// distinguishes the two deserves a fake that can too.
	Strict bool
	// OnRestart, when set, runs on every successful Restart — how a test models what the real
	// API server does next (the pods actually rolling), since the fake has no controller.
	OnRestart func(namespace, name string, at time.Time)

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

// Restart implements Rollout: records the call, stamps the recorded status's annotation so a
// later Deployment read can see it, and runs OnRestart.
func (f *Fake) Restart(_ context.Context, namespace, name string, at time.Time) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, fmt.Sprintf("Restart %s/%s", namespace, name))
	err, hook := f.RestartErr, f.OnRestart
	st, known := f.Deployments[depKey{namespace, name}]
	if err == nil && known {
		st.RestartedAt = at.UTC().Format(time.RFC3339)
		f.Deployments[depKey{namespace, name}] = st
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("restarting Deployment %s/%s: %w", namespace, name, ErrNotFound)
	}
	if hook != nil {
		hook(namespace, name, at)
	}
	return nil
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
	st, ok := f.Deployments[depKey{namespace, name}]
	if !ok && f.Strict {
		return DeploymentStatus{}, fmt.Errorf("reading Deployment %s/%s: %w", namespace, name, ErrNotFound)
	}
	return st, nil
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
