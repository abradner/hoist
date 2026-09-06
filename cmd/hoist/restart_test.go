package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// A flags-only run has no envs.production list, so hoist cannot tell whether the target is
// production. It must fail closed: the same omission hazard checkDirectPreflight refuses, and
// the one I cited as precedent while leaving this branch open (Copilot, PR #81).
func TestRestartFailsClosedWithNoConfiguredRepo(t *testing.T) {
	// The fixture repo's app-production holds several families; register every Deployment it
	// declares so the run is exercising the gate, not a missing fixture.
	r, err := gitops.Discover("../../testdata/repo", "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	names, err := restartTargets(r, "app-production", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sts []rollout.DeploymentStatus
	for _, n := range names {
		st := rolled("app-production")
		st.Name = n
		sts = append(sts, st)
	}
	restartFake(t, sts...)
	// No --config at all: flags only.
	args := []string{"--repo", "../../testdata/repo", "restart", "--env", "app-production"}

	var out, errOut bytes.Buffer
	if got := run(args, &out, &errOut); got == 0 {
		t.Fatalf("an unconfigured run must not restart an env it cannot classify:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "cannot tell whether app-production is production") {
		t.Errorf("the refusal should say why it cannot decide:\n%s", errOut.String())
	}

	// Acknowledged, it proceeds — the gate is "we cannot tell, so say so", not a blanket refusal.
	out.Reset()
	errOut.Reset()
	if got := run(append(args, "--confirm-production", "app-production"), &out, &errOut); got != 0 {
		t.Errorf("an acknowledged unconfigured restart should proceed: %s", errOut.String())
	}
}

// A failed patch has one ambiguous outcome: the API server can commit and the connection can
// still fail before the response arrives. Reporting that as failure is wrong in the way that
// matters — the pods are already rolling, and re-running would roll them again (Copilot, PR #81).
func TestRestartTreatsALandedPatchAsSuccessDespiteAFailedCall(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t, rolled("app-production"))
	// The write lands and the response is lost — the one outcome a patch cannot report, and the
	// only shape that exercises the recovery. An earlier version of this test set RestartErr
	// alone, which the fake treats as "the write did not happen", so it asserted the failure
	// path and never reached the branch it was named for.
	f.RestartErr = errors.New("connection reset by peer")
	f.RestartLandsDespiteErr = true

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("a write that landed must not be reported as a failure:\nstdout: %s\nstderr: %s", out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "all 1 Deployment(s) rolled") {
		t.Errorf("it should have gone on to watch the rollout:\n%s", out.String())
	}
}

// And a patch that genuinely did not land is still a failure.
func TestRestartReportsAPatchThatNeverLanded(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t, rolled("app-production"))
	f.RestartErr = errors.New("connection reset by peer") // and the stamp is NOT recorded

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got == 0 {
		t.Fatalf("a patch that never landed must be reported:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "connection reset") {
		t.Errorf("the real failure should be reported:\n%s", errOut.String())
	}
}

// An explicitly supplied --family that names nothing is refused, not read as "all": the widest
// possible blast radius from what reads like a narrowing (Copilot, PR #81).
func TestRestartRefusesAnEmptyFamilySelector(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t, rolled("app-production"))

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production", "--family", ""}, &out, &errOut); got == 0 {
		t.Fatal("`--family \"\"` must not restart every family")
	}
	if !strings.Contains(errOut.String(), "omit it to restart every family") {
		t.Errorf("the refusal should say what to do instead:\n%s", errOut.String())
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Errorf("nothing should have been restarted: %v", f.Calls)
		}
	}
}

// A restart that someone else supersedes mid-flight is not this command's success to claim.
func TestRestartDoesNotClaimSomeoneElsesRollout(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	f := restartFake(t, rolled("app-production"))
	// Whatever stamp this run writes, the world reports a different one — a concurrent
	// `kubectl rollout restart`, or a second hoist.
	f.OnRestart = func(ns, name string, _ time.Time) {
		st, _ := f.Deployment(context.Background(), ns, name)
		st.RestartedAt = "2099-01-01T00:00:00.000000000Z"
		f.SetDeployment(ns, name, st)
	}

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got == 0 {
		t.Fatalf("hoist must not report someone else's rollout as its own:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "restarted by something else") {
		t.Errorf("the report should say what actually happened:\n%s", errOut.String())
	}
}

// The stamp carries nanoseconds so two restarts can never collide, whatever the resolution of
// the clock that asked for them (Copilot, PR #81).
func TestRestartStampIsSubSecond(t *testing.T) {
	a := time.Date(2026, 9, 6, 5, 0, 0, 1, time.UTC).Format(rollout.RestartStampLayout)
	b := time.Date(2026, 9, 6, 5, 0, 0, 2, time.UTC).Format(rollout.RestartStampLayout)
	if a == b {
		t.Fatalf("two instants one nanosecond apart rendered identically as %q: a second restart would be a no-op", a)
	}
	// Nine digits always, so an instant that happens to land on a whole second cannot collapse
	// back to second resolution (time.RFC3339Nano strips trailing zeros; this layout does not).
	whole := time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC).Format(rollout.RestartStampLayout)
	if !strings.Contains(whole, ".000000000") {
		t.Errorf("a whole-second instant lost its fractional part: %q", whole)
	}
	if _, err := time.Parse(time.RFC3339, whole); err != nil {
		t.Errorf("the stamp should still be valid RFC3339: %v", err)
	}
}

// The claim "a restart changes nothing about what runs" holds only for a pinned reference. A
// container on a mutable tag can pull a different build when its replacement lands, and this
// repo's own fixtures carry bare-tag Deployments (Copilot, PR #81).
func TestRestartWarnsAboutUnpinnedImages(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.Images = []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v1"}}
	restartFake(t, st)

	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatalf("the warning must not block the restart: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "unpinned image(s) ghcr.io/example/app:v1") {
		t.Errorf("an unpinned reference should be named:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "may not be a no-op") {
		t.Errorf("the warning should say what it means:\n%s", out.String())
	}

	// A pinned one says nothing.
	out.Reset()
	pinned := rolled("app-production")
	pinned.Images = []rollout.ContainerImage{{Name: "app", Image: "ghcr.io/example/app:v1@sha256:" + strings.Repeat("a", 64)}}
	restartFake(t, pinned)
	if got := run([]string{"--config", cfgPath, "restart", "--env", "app-production"}, &out, &errOut); got != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), "unpinned") {
		t.Errorf("a pinned reference should draw no warning:\n%s", out.String())
	}
}
