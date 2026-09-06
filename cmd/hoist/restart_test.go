package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

// restartFake wires newRollout to a fake holding the named Deployments in app-production, and
// returns it so a test can read what was called.
func restartFake(t *testing.T, sts ...rollout.DeploymentStatus) *rollout.Fake {
	t.Helper()
	f := &rollout.Fake{}
	for _, st := range sts {
		f.SetDeployment(st.Namespace, st.Name, st)
	}
	prev := newRollout
	newRollout = func(string) (rollout.Rollout, string, error) { return f, "test-context", nil }
	t.Cleanup(func() { newRollout = prev })
	return f
}

// rolled is a Deployment that is up to date and finished rolling — what the fake should report
// for a restart to converge. The name is the fixture repo's only Deployment.
func rolled(ns string) rollout.DeploymentStatus {
	const name = "app"
	return rollout.DeploymentStatus{
		Namespace: ns, Name: name,
		Replicas: 2, Strategy: "RollingUpdate", ReadinessProbes: 1,
		Complete: true, Detail: "deployment " + name + " successfully rolled out",
	}
}

// The end-to-end shape: name what will roll, patch each one, then watch it finish.
func TestRestartPatchesAndWatches(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t, rolled("app-production"))

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	var restarts, reads int
	for _, c := range f.Calls {
		switch {
		case strings.HasPrefix(c, "Restart "):
			restarts++
		case strings.HasPrefix(c, "Deployment "):
			reads++
		}
	}
	if restarts != 1 {
		t.Errorf("expected one Restart call, got %d: %v", restarts, f.Calls)
	}
	if reads < 2 {
		t.Errorf("expected a read before the restart and at least one after, got %d: %v", reads, f.Calls)
	}
	if !strings.Contains(out.String(), "all 1 Deployment(s) rolled") {
		t.Errorf("the run should report the rollout finishing:\n%s", out.String())
	}
	// Nothing about git: this command writes none of it.
	for _, forbidden := range []string{"branch", "PR", "commit"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("a restart touches no git; output mentions %q:\n%s", forbidden, out.String())
		}
	}
}

// --dry-run reads and reports, and patches nothing.
func TestRestartDryRunPatchesNothing(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.RestartedAt = ""
	f := restartFake(t, st)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production", "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("exit %d; stderr: %s", got, errOut.String())
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Errorf("a dry run must not restart anything: %v", f.Calls)
		}
	}
	for _, want := range []string{"restart app-production", "app", "never restarted this way", "dry run: nothing was restarted"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run missing %q:\n%s", want, out.String())
		}
	}
}

// Production takes a second, distinct acknowledgement. §4.5's PR-and-approval gate cannot apply
// to an operation that commits nothing, so this stands in its place — and, like --confirm-direct,
// it must repeat the env exactly rather than being a bare boolean.
func TestRestartProductionNeedsASecondAcknowledgement(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	withProd := strings.Replace(string(data),
		"    promotable: [ghcr.io/example/]\n",
		"    promotable: [ghcr.io/example/]\n    envs:\n      production: [app-production]\n",
		1)
	if withProd == string(data) {
		t.Fatal("fixture config shape changed; promotable insertion point not found")
	}
	if err := os.WriteFile(cfgPath, []byte(withProd), 0o644); err != nil {
		t.Fatal(err)
	}
	f := restartFake(t, rolled("app-production"))

	base := []string{"--config", cfgPath, "restart", "--env", "app-production"}
	var out, errOut bytes.Buffer
	if got := run(base, &out, &errOut); got == 0 {
		t.Fatal("restarting production with no acknowledgement must be refused")
	}
	if !strings.Contains(errOut.String(), "--confirm-production=app-production") {
		t.Errorf("the refusal should say exactly what is required:\n%s", errOut.String())
	}

	// A mismatched acknowledgement is not an acknowledgement.
	out.Reset()
	errOut.Reset()
	if got := run(append(append([]string{}, base...), "--confirm-production", "app-staging"), &out, &errOut); got == 0 {
		t.Error("a --confirm-production naming a different env must be refused")
	}

	// Nothing was touched by either refusal.
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Errorf("a refused production restart must not reach the cluster: %v", f.Calls)
		}
	}

	// And with the exact env, it proceeds.
	out.Reset()
	errOut.Reset()
	if got := run(append(append([]string{}, base...), "--confirm-production", "app-production"), &out, &errOut); got != 0 {
		t.Fatalf("an acknowledged production restart should proceed: %s", errOut.String())
	}

	// A non-production env needs none of this, which is what makes the gate mean something.
	out.Reset()
	errOut.Reset()
	restartFake(t, rolled("app-staging"))
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-staging"}, &out, &errOut); got != 0 {
		t.Errorf("a non-production restart needs no acknowledgement: %s", errOut.String())
	}
}

// A Deployment the repo declares but the cluster does not have is named and skipped, not a
// reason to refuse the ones that are there — but the set must never be quietly smaller than it
// looks (principle 5).
func TestRestartReportsDeploymentsMissingFromTheCluster(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t) // the fake knows nothing: every target is absent
	f.Strict = true     // ... and says so, the way the real client does

	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut)
	if got == 0 {
		t.Fatal("with nothing to restart the run must not report success")
	}
	if !strings.Contains(out.String(), "declared in the repo but not in the cluster") {
		t.Errorf("the absent Deployment should be named:\n%s", out.String())
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Errorf("nothing existed to restart: %v", f.Calls)
		}
	}
}

// A rollout that exceeds its progress deadline is a failure, reported as one.
func TestRestartFailsOnAStuckRollout(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.Complete = false
	st.DeadlineExceeded = true
	st.Detail = "progress deadline exceeded"
	restartFake(t, st)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got == 0 {
		t.Fatalf("a stuck rollout must not report success:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "progress deadline exceeded") {
		t.Errorf("the failure should say what the cluster said:\n%s", errOut.String())
	}
}

// The concerns the operator asked about ("I don't think our pods are set up for this yet") are
// surfaced per Deployment before anything rolls — and never block it.
func TestRestartWarnsWhenARestartWillNotBeGraceful(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.Replicas = 1
	st.ReadinessProbes = 0
	restartFake(t, st)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("the warnings must not block the restart: %s", errOut.String())
	}
	for _, want := range []string{"only 1 replica", "no readiness probe"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing the %q warning:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "all 1 Deployment(s) rolled") {
		t.Errorf("it should still have rolled:\n%s", out.String())
	}
}

// restartTargets reads the family→Deployment mapping out of the repo, because "family" is a
// concept the repo defines and the cluster does not.
func TestRestartTargetsAreScopedToTheNamedFamilies(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	all, err := restartTargets(r, "app-production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Skipf("fixture has %d Deployments in app-production; need 2+ to prove narrowing", len(all))
	}
	var fam string
	for name := range r.Envs["app-production"].Families {
		fam = name
		break
	}
	one, err := restartTargets(r, "app-production", []string{fam})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) >= len(all) {
		t.Errorf("--family should narrow the set: %d of %d", len(one), len(all))
	}
	if _, err := restartTargets(r, "app-production", []string{"no-such-family"}); err == nil {
		t.Error("naming a family that does not exist must be refused, not silently ignored")
	}
	if _, err := restartTargets(r, "no-such-env", nil); err == nil {
		t.Error("an unknown env must be refused")
	}
	// Jobs and CronJobs are not restarted: re-running one is a different operation.
	for _, name := range all {
		if strings.Contains(name, "purge") {
			t.Errorf("a CronJob was included as a restart target: %v", all)
		}
	}
}
