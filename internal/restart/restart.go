// Package restart is the shared core of `hoist restart` and the matrix's R key: what a restart
// targets, what is worth warning about before it runs, how the write is made safely, and how the
// rollout that follows is observed.
//
// It exists so the CLI and the TUI cannot drift. Both surfaces answer the same three questions —
// which workloads, is this safe, did it work — and answering them twice is how one of them ends
// up quietly wrong. Nothing here knows about terminals or flags; the callers own their own output.
//
// A restart rolls Deployments, StatefulSets and DaemonSets (rollout.RestartableKinds) — the
// kinds `kubectl rollout restart` stamps. A target is a gitops.Workload, kind and name together,
// because a family may declare a Deployment and a StatefulSet of the same name and a name alone
// would restart the wrong one.
package restart

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/redact"
	"github.com/abradner/hoist/pkg/rollout"
)

// StampLayout is how a restart timestamp is rendered — see rollout.Restart for why the
// sub-second part is load-bearing. Re-exported so callers need not reach past this package.
const StampLayout = rollout.RestartStampLayout

// Plan is what a restart would do: the workloads it will roll, with their current state, the ones
// the repo declares that the cluster does not have, and the ones the repo declares that a restart
// does not roll at all.
type Plan struct {
	Env string
	// Targets are the workloads that exist and will be restarted, in Targets' order.
	Targets []rollout.WorkloadStatus
	// Absent are workloads the repo declares but the cluster does not have. Reported, never
	// restarted, and never a reason to refuse the rest (AGENTS.md principle 5) — but named, so
	// the set is never quietly smaller than it looks.
	Absent []gitops.Workload
	// Skipped are workloads the family declares whose kind a restart does not roll — a Job, a
	// CronJob, anything outside rollout.RestartableKinds. Set by the caller from Targets' second
	// result; named for the same reason Absent is (#171: they used to vanish without a word).
	Skipped []gitops.Workload
}

// Workloads is what this plan will actually restart.
func (p Plan) Workloads() []gitops.Workload {
	out := make([]gitops.Workload, 0, len(p.Targets))
	for _, t := range p.Targets {
		out = append(out, gitops.Workload{Kind: t.Kind, Name: t.Name})
	}
	return out
}

// Noun is what to call the plan's targets in a count: their kind when they all share one
// ("Deployment"), "workload" when they mix. Callers pluralise it.
func (p Plan) Noun() string {
	noun := ""
	for _, t := range p.Targets {
		switch {
		case noun == "":
			noun = t.Kind
		case noun != t.Kind:
			return "workload"
		}
	}
	if noun == "" {
		return "workload"
	}
	return noun
}

// Label is how a surface names one target in a list under Noun's heading: the bare name when the
// heading already says the kind, kind/name when it does not.
func (p Plan) Label(w gitops.Workload) string {
	if p.Noun() == w.Kind {
		return w.Name
	}
	return Label(w)
}

// CountNoun is what a workload's Replicas counts: replicas, except for a DaemonSet, whose count is
// the pods it should run — one per node it is scheduled on. Callers pluralise it.
func CountNoun(kind string) string {
	if kind == rollout.KindDaemonSet {
		return "pod"
	}
	return "replica"
}

// Label names a workload unambiguously, kubectl-style: "StatefulSet/db". Used wherever a target
// appears without a heading to say its kind — a failure, a list of what already rolled.
func Label(w gitops.Workload) string { return w.Kind + "/" + w.Name }

// Labels is Label over a list, comma-joined.
func Labels(ws []gitops.Workload) string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, Label(w))
	}
	return joinComma(out)
}

// Targets is the distinct restartable workloads the named families declare in env, or every
// family's when none are named, ordered by kind (rollout.RestartableKinds) and then name.
// skipped is every other workload those families declare — a kind a restart does not roll —
// so a caller can name it rather than leave the set quietly smaller than the family.
//
// Read from the GitOps repo rather than by listing the namespace, because "family" is a concept
// the repo defines and the cluster does not: a namespace holds several families' workloads, so
// listing it would restart far more than was asked for. gitops.Discover already records each
// occurrence's Kind and Name, so this needs no new parsing.
func Targets(r *gitops.Repo, env string, families []string) (targets, skipped []gitops.Workload, err error) {
	if r == nil {
		return nil, nil, errors.New("nil repo")
	}
	e, ok := r.Envs[env]
	if !ok {
		return nil, nil, fmt.Errorf("unknown env %q", env)
	}
	only := map[string]bool{}
	for _, f := range families {
		if _, ok := e.Families[f]; !ok {
			return nil, nil, fmt.Errorf("%s has no family %q", env, f)
		}
		only[f] = true
	}
	seen := map[gitops.Workload]bool{}
	for name, fam := range e.Families {
		if len(only) > 0 && !only[name] {
			continue
		}
		for _, w := range fam.Workloads() {
			if seen[w] {
				continue
			}
			seen[w] = true
			// A Job or CronJob has no rollout to trigger, and re-running one is a different
			// operation with different consequences — so it is skipped, and said to be.
			if rollout.IsRestartable(w.Kind) {
				targets = append(targets, w)
			} else {
				skipped = append(skipped, w)
			}
		}
	}
	order := map[string]int{}
	for i, k := range rollout.RestartableKinds() {
		order[k] = i
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return order[targets[i].Kind] < order[targets[j].Kind]
		}
		return targets[i].Name < targets[j].Name
	})
	sort.Slice(skipped, func(i, j int) bool {
		if skipped[i].Kind != skipped[j].Kind {
			return skipped[i].Kind < skipped[j].Kind
		}
		return skipped[i].Name < skipped[j].Name
	})
	if len(targets) == 0 {
		found := ""
		if len(skipped) > 0 {
			found = " (it declares only " + Labels(skipped) + ", which a restart does not roll)"
		}
		return nil, nil, fmt.Errorf("%s has no %s to restart%s%s", env, kindList(), onlyLabel(families), found)
	}
	return targets, skipped, nil
}

// kindList is rollout.RestartableKinds as prose: "Deployment, StatefulSet or DaemonSet".
func kindList() string {
	ks := rollout.RestartableKinds()
	return joinComma(ks[:len(ks)-1]) + " or " + ks[len(ks)-1]
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

// Read builds the Plan: every named workload's current state, so the caller can show what will
// roll — and every reason it will not be graceful — before anything is written.
func Read(ctx context.Context, ro rollout.Rollout, env string, targets []gitops.Workload) (Plan, error) {
	p := Plan{Env: env}
	for _, w := range targets {
		st, err := ro.Workload(ctx, env, w.Name, w.Kind)
		if err != nil {
			if errors.Is(err, rollout.ErrNotFound) {
				p.Absent = append(p.Absent, w)
				continue
			}
			return Plan{}, err
		}
		// The kind and name asked for, never whatever the read echoed back: these are what Do
		// will patch.
		st.Kind, st.Name = w.Kind, w.Name
		p.Targets = append(p.Targets, st)
	}
	return p, nil
}

// Do restarts every target in the plan, stopping at the first genuine failure and reporting
// which ones had already been restarted by then — some pods are already rolling, and the caller
// needs to be able to say which.
func Do(ctx context.Context, ro rollout.Rollout, p Plan, at time.Time) (done []gitops.Workload, err error) {
	for _, w := range p.Workloads() {
		if rerr := one(ctx, ro, p.Env, w, at); rerr != nil {
			return done, rerr
		}
		done = append(done, w)
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

// one patches a single workload and resolves the one ambiguous outcome a patch has: the API
// server can commit the change and the connection can still fail before the response arrives.
// Reporting that as a failure is wrong in the way that matters — the pods are already rolling,
// and an operator who re-runs would roll them a second time.
//
// So a failed patch is followed by a read, and by a retry of the patch itself, a few times: the
// outage that lost the response can lose the read too, and re-patching with the same stamp is
// idempotent, which is the property that makes retrying safe at all. Finding this invocation's
// own stamp there means the write landed and only the acknowledgement was lost.
//
// ErrNotFound is terminal either way: a workload that is not there was not restarted, and no
// amount of re-reading changes that.
func one(ctx context.Context, ro rollout.Rollout, env string, w gitops.Workload, at time.Time) error {
	err := ro.Restart(ctx, env, w.Name, w.Kind, at)
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
		st, rerr := ro.Workload(ctx, env, w.Name, w.Kind)
		if rerr != nil {
			continue
		}
		if st.RestartedAt == want {
			return nil
		}
		if perr := ro.Restart(ctx, env, w.Name, w.Kind, at); perr == nil {
			return nil
		} else if errors.Is(perr, rollout.ErrNotFound) {
			return perr
		}
	}
	return err
}

// Stamp renders at the way Restart writes it, for comparing against what is on a workload.
func Stamp(at time.Time) string { return at.UTC().Format(StampLayout) }

// Progress is one workload's state part-way through a restart.
type Progress struct {
	Kind, Name string
	// Done is true once the rollout has finished.
	Done bool
	// Manual is set, with the reason, when the stamp landed but the workload's controller will
	// not act on it: an OnDelete StatefulSet or DaemonSet, whose pods take the new template only
	// when someone deletes them. It is final — waiting longer changes nothing — and it is never
	// Done, because nothing rolled: a surface reports it as stamped and not rolled, never as a
	// finished rollout.
	Manual string
	// Superseded is true when the pod template carries a stamp that is not this restart's:
	// something else restarted it while this one was in flight, so the rollout now running is
	// not this command's to report.
	Superseded bool
	// Blocked is set when the rollout will not finish on its own (a progress deadline).
	Blocked string
	Detail  string
}

// Observe reads where each named workload has got to, against the stamp this restart wrote.
//
// A transient read failure leaves that workload simply unfinished rather than failing the
// whole watch. The pods have already been restarted by the time anything calls this, and there
// is no resume mode to come back through — so one connection reset would otherwise force the
// operator to either abandon monitoring or restart everything a second time to watch it. This
// is the same call the promotion path's own loop already retries (cmd/hoist/drive.go).
// ErrNotFound is the exception: a workload that has gone away is not coming back mid-rollout,
// and waiting for it is waiting for nothing.
func Observe(ctx context.Context, ro rollout.Rollout, env string, targets []gitops.Workload, at time.Time) ([]Progress, error) {
	want := Stamp(at)
	out := make([]Progress, 0, len(targets))
	for _, w := range targets {
		st, err := ro.Workload(ctx, env, w.Name, w.Kind)
		if err != nil {
			if errors.Is(err, rollout.ErrNotFound) {
				return nil, err
			}
			// Redacted here rather than at each caller's own render: this is the one place a
			// raw cluster error enters a Progress, and pkg/rollout's own errors are scrubbed
			// only of what it knows to scrub.
			out = append(out, Progress{Kind: w.Kind, Name: w.Name, Detail: "could not read it just now: " + redact.Strings(err.Error())})
			continue
		}
		pr := Progress{Kind: w.Kind, Name: w.Name, Detail: st.Detail}
		switch {
		case st.RestartedAt != want:
			pr.Superseded = true
		case st.Strategy == rollout.StrategyOnDelete:
			// Checked before Complete, which an OnDelete read never sets anyway: this is the one
			// outcome that is neither a rollout in progress nor a finished one.
			pr.Manual = "update strategy is OnDelete: the pod template is stamped, but its pods restart only when deleted"
		case st.DeadlineExceeded:
			pr.Blocked = st.Detail
		case st.Complete:
			pr.Done = true
		}
		out = append(out, pr)
	}
	return out, nil
}
