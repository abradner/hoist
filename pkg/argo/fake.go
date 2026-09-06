package argo

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Argo for tests in other packages (internal/engine's step tests in
// particular — invariant 7 of the M5 brief: engine-level step tests use a hand-rolled fake
// Argo interface, mirroring pkg/forge.Fake/pkg/git's test doubles, not the dynamic fake
// directly). Statuses is keyed by Application; an app absent from it reports ErrNotFound from
// Get, exactly like a real cluster with no such Application — mirroring pkg/k8s.Fake's
// DockerConfigSecret (a named object either exists or it doesn't), not its RunningImages
// (a list that can legitimately be empty). Calls records every method invocation, in order,
// for a test asserting call counts or their absence — "hoist watch never calls Refresh" in
// particular.
type Fake struct {
	mu sync.Mutex

	Statuses map[Application]Status
	// GetErr and RefreshErr, when set, are returned by every call to the matching method
	// instead of the configured/not-found behavior — simulating a transient plumbing error a
	// caller must retry rather than treat as authoritative.
	GetErr, RefreshErr error
	// OnRefresh, when set, runs on every Refresh — see Refresh's own doc comment.
	OnRefresh func(app Application)

	Calls []string
}

// SetStatus records app's current status, thread-safely — the way a test simulates Argo
// reconciling to a new state between polls.
func (f *Fake) SetStatus(app Application, st Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Statuses == nil {
		f.Statuses = map[Application]Status{}
	}
	f.Statuses[app] = st
}

// Refresh implements Argo. It never mutates Statuses itself — a test that wants Refresh to
// have an observable effect calls SetStatus afterward, exactly as the real controller's own
// reconcile (not this call) is what actually changes status.
func (f *Fake) Refresh(_ context.Context, app Application) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, "Refresh "+app.String())
	err, hook := f.RefreshErr, f.OnRefresh
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// Real Argo reacts to a refresh by reconciling against whatever is on the branch it
	// tracks — it does not care whether that commit arrived by merge or by a direct push. A
	// test that wants that behaviour sets OnRefresh; one that wants a frozen status (an
	// Application deliberately left OutOfSync, say) leaves it nil and the fake stays static.
	//
	// Called outside the lock: the hook is expected to call SetStatus, which takes it.
	if hook != nil {
		hook(app)
	}
	return nil
}

// Get implements Argo.
func (f *Fake) Get(_ context.Context, app Application) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Get "+app.String())
	if f.GetErr != nil {
		return Status{}, f.GetErr
	}
	st, ok := f.Statuses[app]
	if !ok {
		return Status{}, fmt.Errorf("reading %s: %w", app, ErrNotFound)
	}
	return st, nil
}
