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
	// Workloads holds StatefulSets and DaemonSets, keyed by kind. A Deployment always lives in
	// Deployments, and Workload for one reads it from there, so a Deployment has one record in
	// the fake as it has one object in a cluster.
	Workloads map[jobKey]WorkloadStatus
	JobLikes  map[jobKey]JobLikeStatus
	// DeploymentErr, WorkloadErr and JobLikeErr, when set, are returned by every call to the
	// matching method instead of the configured/zero-value behavior. A Workload read of a
	// Deployment is a Deployment read and honours DeploymentErr; WorkloadErr covers the other
	// kinds.
	DeploymentErr, WorkloadErr, JobLikeErr, RestartErr error
	// Strict makes an unknown Deployment (or StatefulSet, or DaemonSet) return ErrNotFound, as the real client does, instead
	// of a zero-value status. Opt-in rather than the default because most tests here configure
	// only the Deployments they care about and rely on the zero value for the rest — but a test
	// about ABSENCE cannot use a fake that reports everything as present, and shipping code that
	// distinguishes the two deserves a fake that can too.
	Strict bool
	// OnRestart, when set, runs on every successful Restart — how a test models what the real
	// API server does next (the pods actually rolling), since the fake has no controller.
	OnRestart func(namespace, name string, at time.Time)
	// RestartLandsDespiteErr models the one outcome a patch cannot report: the API server
	// committed the write and the response was lost. With it set alongside RestartErr, the
	// stamp is recorded AND the error returned — which is the only way to exercise a caller's
	// did-it-actually-land recovery. Without it, RestartErr means the write did not happen.
	RestartLandsDespiteErr bool

	Calls []string
}

// Both implementations satisfy Rollout, checked at compile time rather than at the first caller.
var (
	_ Rollout = (*Fake)(nil)
	_ Rollout = (*client)(nil)
)

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

// SetWorkload records a StatefulSet's or DaemonSet's current status under st's own Kind,
// Namespace and Name, thread-safely. A Deployment is recorded with SetDeployment instead, and
// SetWorkload panics on one rather than keep a second copy the reads would never see.
func (f *Fake) SetWorkload(st WorkloadStatus) {
	if st.Kind == KindDeployment || !IsRestartable(st.Kind) {
		panic(fmt.Sprintf("rollout.Fake.SetWorkload: kind %q; use SetDeployment for a Deployment", st.Kind))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Workloads == nil {
		f.Workloads = map[jobKey]WorkloadStatus{}
	}
	f.Workloads[jobKey{st.Namespace, st.Name, st.Kind}] = st
}

// Restart implements Rollout: records the call ("Restart <Kind> <ns>/<name>"), stamps the
// recorded status's annotation so a later read can see it, and runs OnRestart.
func (f *Fake) Restart(_ context.Context, namespace, name, kind string, at time.Time) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, fmt.Sprintf("Restart %s %s/%s", kind, namespace, name))
	err, hook := f.RestartErr, f.OnRestart
	land := err == nil || f.RestartLandsDespiteErr
	stamp := at.UTC().Format(RestartStampLayout)
	var known bool
	switch kind {
	case KindDeployment:
		var st DeploymentStatus
		if st, known = f.Deployments[depKey{namespace, name}]; known && land {
			st.RestartedAt = stamp
			f.Deployments[depKey{namespace, name}] = st
		}
	case KindStatefulSet, KindDaemonSet:
		var st WorkloadStatus
		if st, known = f.Workloads[jobKey{namespace, name, kind}]; known && land {
			st.RestartedAt = stamp
			f.Workloads[jobKey{namespace, name, kind}] = st
		}
	default:
		f.mu.Unlock()
		return fmt.Errorf("rollout: cannot restart kind %q", kind)
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("restarting %s %s/%s: %w", kind, namespace, name, ErrNotFound)
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

// Workload implements Rollout. A Deployment is read exactly as Deployment reads it, and recorded
// as the same "Deployment <ns>/<name>" call; the other kinds record "<Kind> <ns>/<name>".
func (f *Fake) Workload(ctx context.Context, namespace, name, kind string) (WorkloadStatus, error) {
	if kind == KindDeployment {
		d, err := f.Deployment(ctx, namespace, name)
		if err != nil {
			return WorkloadStatus{}, err
		}
		w := d.Workload()
		w.Namespace, w.Name = namespace, name
		return w, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("%s %s/%s", kind, namespace, name))
	if !IsRestartable(kind) {
		return WorkloadStatus{}, fmt.Errorf("rollout: cannot read kind %q as a restartable workload", kind)
	}
	if f.WorkloadErr != nil {
		return WorkloadStatus{}, f.WorkloadErr
	}
	st, ok := f.Workloads[jobKey{namespace, name, kind}]
	if !ok {
		if f.Strict {
			return WorkloadStatus{}, fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, ErrNotFound)
		}
		return WorkloadStatus{Kind: kind, Namespace: namespace, Name: name}, nil
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
