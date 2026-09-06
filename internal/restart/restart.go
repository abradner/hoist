// Package restart is the shared core of `hoist restart` and the matrix's R key: what a restart
// targets, what is worth warning about before it runs, how the write is made safely, and how the
// rollout that follows is observed.
//
// It exists so the CLI and the TUI cannot drift. Both surfaces answer the same three questions —
// which Deployments, is this safe, did it work — and answering them twice is how one of them ends
// up quietly wrong. Nothing here knows about terminals or flags; the callers own their own output.
package restart

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// StampLayout is how a restart timestamp is rendered — see rollout.Restart for why the
// sub-second part is load-bearing. Re-exported so callers need not reach past this package.
const StampLayout = rollout.RestartStampLayout

// Plan is what a restart would do: the Deployments it will roll, with their current state, and
// the ones the repo declares that the cluster does not have.
type Plan struct {
	Env string
	// Targets are the Deployments that exist and will be restarted, in name order.
	Targets []rollout.DeploymentStatus
	// Absent are names the repo declares but the cluster does not have. Reported, never
	// restarted, and never a reason to refuse the rest (AGENTS.md principle 5) — but named, so
	// the set is never quietly smaller than it looks.
	Absent []string
}

// Names is the Deployments this plan will actually restart.
func (p Plan) Names() []string {
	out := make([]string, 0, len(p.Targets))
	for _, t := range p.Targets {
		out = append(out, t.Name)
	}
	return out
}

// Targets is the distinct Deployment names the named families declare in env, or every family's
// when none are named.
//
// Read from the GitOps repo rather than by listing the namespace, because "family" is a concept
// the repo defines and the cluster does not: a namespace holds several families' workloads, so
// listing it would restart far more than was asked for. gitops.Discover already records each
// occurrence's Kind and Name, so this needs no new parsing.
func Targets(r *gitops.Repo, env string, families []string) ([]string, error) {
	if r == nil {
		return nil, errors.New("nil repo")
	}
	e, ok := r.Envs[env]
	if !ok {
		return nil, fmt.Errorf("unknown env %q", env)
	}
	only := map[string]bool{}
	for _, f := range families {
		if _, ok := e.Families[f]; !ok {
			return nil, fmt.Errorf("%s has no family %q", env, f)
		}
		only[f] = true
	}
	seen := map[string]bool{}
	var out []string
	for name, fam := range e.Families {
		if len(only) > 0 && !only[name] {
			continue
		}
		for _, occ := range fam.Occurrences {
			// Deployments only. A Job or CronJob has no rollout to trigger, and re-running one
			// is a different operation with different consequences.
			if occ.Kind != "Deployment" || seen[occ.Name] {
				continue
			}
			seen[occ.Name] = true
			out = append(out, occ.Name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no Deployment to restart%s", env, onlyLabel(families))
	}
	sort.Strings(out)
	return out, nil
}

func onlyLabel(families []string) string {
	if len(families) == 0 {
		return ""
	}
	return " in " + joinComma(families)
}

func joinComma(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// Read builds the Plan: every named Deployment's current state, so the caller can show what will
// roll — and every reason it will not be graceful — before anything is written.
func Read(ctx context.Context, ro rollout.Rollout, env string, names []string) (Plan, error) {
	p := Plan{Env: env}
	for _, name := range names {
		st, err := ro.Deployment(ctx, env, name)
		if err != nil {
			if errors.Is(err, rollout.ErrNotFound) {
				p.Absent = append(p.Absent, name)
				continue
			}
			return Plan{}, err
		}
		st.Name = name // the name asked for, never whatever the read echoed back
		p.Targets = append(p.Targets, st)
	}
	return p, nil
}

// Do restarts every target in the plan, stopping at the first genuine failure and reporting
// which ones had already been restarted by then — some pods are already rolling, and the caller
// needs to be able to say which.
func Do(ctx context.Context, ro rollout.Rollout, p Plan, at time.Time) (done []string, err error) {
	for _, st := range p.Targets {
		if rerr := one(ctx, ro, p.Env, st.Name, at); rerr != nil {
			return done, rerr
		}
		done = append(done, st.Name)
	}
	return done, nil
}

// How hard one() tries to learn whether an ambiguous patch landed. Deliberately small: the
// question is "did the write take", not "wait out an outage", and the caller's own deadline
// still bounds everything above this.
const (
	confirmAttempts = 3
	confirmBackoff  = 500 * time.Millisecond
)

// one patches a single Deployment and resolves the one ambiguous outcome a patch has: the API
// server can commit the change and the connection can still fail before the response arrives.
// Reporting that as a failure is wrong in the way that matters — the pods are already rolling,
// and an operator who re-runs would roll them a second time.
//
// So a failed patch is followed by a read, and by a retry of the patch itself, a few times: the
// outage that lost the response can lose the read too, and re-patching with the same stamp is
// idempotent, which is the property that makes retrying safe at all. Finding this invocation's
// own stamp there means the write landed and only the acknowledgement was lost.
//
// ErrNotFound is terminal either way: a Deployment that is not there was not restarted, and no
// amount of re-reading changes that.
func one(ctx context.Context, ro rollout.Rollout, env, name string, at time.Time) error {
	err := ro.Restart(ctx, env, name, at)
	if err == nil || errors.Is(err, rollout.ErrNotFound) {
		return err
	}
	want := Stamp(at)
	for attempt := 0; attempt < confirmAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(confirmBackoff):
		}
		st, rerr := ro.Deployment(ctx, env, name)
		if rerr != nil {
			continue
		}
		if st.RestartedAt == want {
			return nil
		}
		if perr := ro.Restart(ctx, env, name, at); perr == nil {
			return nil
		} else if errors.Is(perr, rollout.ErrNotFound) {
			return perr
		}
	}
	return err
}

// Stamp renders at the way Restart writes it, for comparing against what is on a Deployment.
func Stamp(at time.Time) string { return at.UTC().Format(StampLayout) }

// Progress is one Deployment's state part-way through a restart.
type Progress struct {
	Name string
	// Done is true once the rollout has finished.
	Done bool
	// Superseded is true when the pod template carries a stamp that is not this restart's:
	// something else restarted it while this one was in flight, so the rollout now running is
	// not this command's to report.
	Superseded bool
	// Blocked is set when the rollout will not finish on its own (a progress deadline).
	Blocked string
	Detail  string
}

// Observe reads where each named Deployment has got to, against the stamp this restart wrote.
func Observe(ctx context.Context, ro rollout.Rollout, env string, names []string, at time.Time) ([]Progress, error) {
	want := Stamp(at)
	out := make([]Progress, 0, len(names))
	for _, name := range names {
		st, err := ro.Deployment(ctx, env, name)
		if err != nil {
			return nil, err
		}
		pr := Progress{Name: name, Detail: st.Detail}
		switch {
		case st.RestartedAt != want:
			pr.Superseded = true
		case st.DeadlineExceeded:
			pr.Blocked = st.Detail
		case st.Complete:
			pr.Done = true
		}
		out = append(out, pr)
	}
	return out, nil
}
