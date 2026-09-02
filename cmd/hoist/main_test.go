package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = "../../testdata/repo"

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if got := run([]string{"-version"}, &out, io.Discard); got != 0 {
		t.Fatalf("exit %d, want 0", got)
	}
	if got := out.String(); got != version+"\n" {
		t.Fatalf("stdout %q, want %q", got, version+"\n")
	}
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"plan"}, {"plan", "--repo", fixture, "--from", "app-staging"}} {
		if got := run(args, io.Discard, io.Discard); got != exitUsage {
			t.Errorf("run(%q) exit %d, want %d", args, got, exitUsage)
		}
	}
}

// treeHash fingerprints every file in the fixture so a test can prove nothing was written.
func treeHash(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(fixture, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}

func planArgs(extra ...string) []string {
	return append([]string{"plan", "--repo", fixture, "--from", "app-staging", "--to", "app-production"}, extra...)
}

func TestPlanDryRunPrintsOneChangedLinePerOccurrence(t *testing.T) {
	before := treeHash(t)
	var out, errOut bytes.Buffer
	if got := run(planArgs("--dry-run"), &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if after := treeHash(t); after != before {
		t.Fatal("dry run modified the fixture")
	}
	s := out.String()
	var plus, minus int
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- "):
		case strings.HasPrefix(l, "+"):
			plus++
			if !strings.Contains(l, "@sha256:") {
				t.Errorf("added line is not pinned: %s", l)
			}
		case strings.HasPrefix(l, "-"):
			minus++
		}
	}
	// 6 occurrences in production: web ×3, counta ×2 (app + cronjob), marketing ×1.
	if plus != 6 || minus != 6 {
		t.Errorf("diff has %d added / %d removed lines, want 6/6:\n%s", plus, minus, s)
	}
	for _, want := range []string{
		"--- a/cluster/apps/app-production/counta/purge-cronjob.yaml",
		"Untouched (3):",
		"docker.io/temporalio/server:1.31.2  (third-party",
		"ghcr.io/example/dbwait:",
		"Warnings (1):",
		"[source-disagrees]",
		"Unmanaged (2):",
		"cluster/apps/app-production/hello-world",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "temporalio") && strings.Contains(s, "+      image: docker.io") {
		t.Error("third-party image appears in the diff")
	}
}

func TestPlanWithoutDryRunExitsThreeAndWritesNothing(t *testing.T) {
	before := treeHash(t)
	var out, errOut bytes.Buffer
	if got := run(planArgs(), &out, &errOut); got != exitNotImplemented {
		t.Fatalf("exit %d, want %d", got, exitNotImplemented)
	}
	if after := treeHash(t); after != before {
		t.Fatal("non-dry-run modified the fixture")
	}
	if !strings.Contains(errOut.String(), "later milestone") {
		t.Errorf("stderr does not say why: %s", errOut.String())
	}
	var dry bytes.Buffer
	run(planArgs("--dry-run"), &dry, io.Discard)
	if out.String() != dry.String() {
		t.Error("non-dry-run output differs from dry-run output")
	}
}

const digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestDigestFlagParsing(t *testing.T) {
	d := digestFlag{}
	if err := d.Set("ghcr.io/example/web=ghcr.io/example/web:v9@" + digestC); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
	if got := d["ghcr.io/example/web"]; got.Tag != "v9" || got.Digest != digestC {
		t.Errorf("parsed override = %+v", got)
	}
	if got, want := d.String(), "ghcr.io/example/web=ghcr.io/example/web:v9@"+digestC; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	bad := map[string]string{
		"no equals":        "ghcr.io/example/web:v9@" + digestC,
		"empty repo":       "=ghcr.io/example/web:v9@" + digestC,
		"malformed digest": "ghcr.io/example/web=ghcr.io/example/web:v9@sha256:DEADBEEF",
		"repo mismatch":    "ghcr.io/example/web=ghcr.io/example/other:v9@" + digestC,
		"repo given twice": "ghcr.io/example/web=ghcr.io/example/web:v10@" + digestC,
		"unparseable ref":  "ghcr.io/example/web=not an image",
	}
	for name, in := range bad {
		if err := d.Set(in); err == nil {
			t.Errorf("%s: Set(%q) accepted", name, in)
		}
	}
	if len(d) != 1 {
		t.Errorf("rejected values leaked into the map: %v", d)
	}
}

func TestPlanDigestFlagOverridesSource(t *testing.T) {
	before := treeHash(t)
	var out, errOut bytes.Buffer
	args := planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web:v9@"+digestC)
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if after := treeHash(t); after != before {
		t.Fatal("dry run with --digest modified the fixture")
	}
	s := out.String()
	if n := strings.Count(s, "+          image: ghcr.io/example/web:v9@"+digestC); n != 3 {
		t.Errorf("override should land on all three production web containers, got %d:\n%s", n, s)
	}
	if strings.Contains(s, "+          image: ghcr.io/example/counta:v9@") {
		t.Error("override leaked onto a repo it does not name")
	}

	// A malformed digest fails at flag parsing: exit non-zero, nothing read or written.
	out.Reset()
	errOut.Reset()
	args = planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web:v9@sha256:DEADBEEF")
	if got := run(args, &out, &errOut); got != exitUsage {
		t.Fatalf("malformed --digest: exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	if !strings.Contains(errOut.String(), "not a digest") {
		t.Errorf("stderr does not say what is wrong: %s", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout should be empty when the flag is refused:\n%s", out.String())
	}
	// A tagless override is refused by BuildPlan, still before anything is written.
	out.Reset()
	errOut.Reset()
	args = planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web@"+digestC)
	if got := run(args, &out, &errOut); got != exitFailure || !strings.Contains(errOut.String(), "a tag is required") {
		t.Errorf("tagless --digest: exit %d, stderr: %s", got, errOut.String())
	}
	if after := treeHash(t); after != before {
		t.Fatal("refused --digest modified the fixture")
	}
}

// A --digest override naming a repo the source env does not run must be an error: BuildPlan
// never looks it up, so without the check a typo in the repo name plans the source env's
// ref while -h promises the override wins.
func TestPlanDigestOverrideForUnknownRepoIsAnError(t *testing.T) {
	before := treeHash(t)
	var out, errOut bytes.Buffer
	args := planArgs("--dry-run", "--digest", "ghcr.io/example/nothere=ghcr.io/example/nothere:v9@"+digestC)
	if got := run(args, &out, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if after := treeHash(t); after != before {
		t.Fatal("refused --digest modified the fixture")
	}
	for _, want := range []string{
		"override for ghcr.io/example/nothere matches no image in app-staging",
		"ghcr.io/example/web",
		"ghcr.io/example/counta",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q: %s", want, errOut.String())
		}
	}
	if out.Len() != 0 {
		t.Errorf("a plan was printed anyway:\n%s", out.String())
	}
	// Positive control: the same override for a repo the source env does run is planned.
	if got := run(planArgs("--dry-run", "--digest", "ghcr.io/example/web=ghcr.io/example/web:v9@"+digestC), io.Discard, io.Discard); got != 0 {
		t.Errorf("override for a present repo: exit %d, want 0", got)
	}
}

// A --digest override for a repo outside the promotable prefixes must be an error: BuildPlan
// iterates promotable repos only, so the override would pass the source-env check (the
// fixture runs temporal in both envs) and then change nothing.
func TestPlanDigestOverrideForThirdPartyRepoIsAnError(t *testing.T) {
	before := treeHash(t)
	var out, errOut bytes.Buffer
	args := planArgs("--dry-run", "--digest", "docker.io/temporalio/server=docker.io/temporalio/server:1.31.2@"+digestC)
	if got := run(args, &out, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if after := treeHash(t); after != before {
		t.Fatal("refused --digest modified the fixture")
	}
	if want := "override for docker.io/temporalio/server is not a promotable repo; prefixes: ghcr.io/"; !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr lacks %q: %s", want, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("a plan was printed anyway:\n%s", out.String())
	}
	// Positive control: the same override is planned once its repo is inside the prefixes.
	out.Reset()
	errOut.Reset()
	args = planArgs("--dry-run", "--promotable", "ghcr.io/,docker.io/temporalio/server", "--digest", "docker.io/temporalio/server=docker.io/temporalio/server:1.31.2@"+digestC)
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("override inside the prefixes: exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if n := strings.Count(out.String(), "+          image: docker.io/temporalio/server:1.31.2@"+digestC); n != 1 {
		t.Errorf("override should land on the one production temporal server container, got %d:\n%s", n, out.String())
	}
}

// An empty --promotable must be refused, not read as "everything is promotable": a plan
// that silently promoted third-party images would be the worst possible reading.
func TestPlanEmptyPromotableIsAnError(t *testing.T) {
	for _, val := range []string{"", " , "} {
		var out, errOut bytes.Buffer
		if got := run(planArgs("--dry-run", "--promotable", val), &out, &errOut); got != exitFailure {
			t.Errorf("--promotable %q: exit %d, want %d; stdout:\n%s", val, got, exitFailure, out.String())
		}
		if !strings.Contains(errOut.String(), "no promotable") {
			t.Errorf("--promotable %q: stderr does not say why: %s", val, errOut.String())
		}
		if strings.Contains(out.String(), "hoist plan: app-staging -> app-production") {
			t.Errorf("--promotable %q: a plan was printed anyway:\n%s", val, out.String())
		}
	}
	// Positive control: the same invocation with a prefix plans as usual.
	if got := run(planArgs("--dry-run", "--promotable", "ghcr.io/example/"), io.Discard, io.Discard); got != 0 {
		t.Errorf("explicit --promotable: exit %d, want 0", got)
	}
}

func TestPlanFailsCleanlyOnBadRepo(t *testing.T) {
	var errOut bytes.Buffer
	if got := run([]string{"plan", "--repo", t.TempDir(), "--from", "a", "--to", "b", "--dry-run"}, io.Discard, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d", got, exitFailure)
	}
	if got := run(planArgs("--dry-run", "--to", "nope"), io.Discard, &errOut); got != exitFailure {
		t.Fatalf("unknown env: exit %d, want %d", got, exitFailure)
	}
}

func TestNoCommandWithRepoDispatchesToTUI(t *testing.T) {
	orig := tuiRunner
	t.Cleanup(func() { tuiRunner = orig })
	var got struct {
		repo, appsRoot string
		promotable     []string
		calls          int
	}
	tuiRunner = func(repo, appsRoot string, promotable []string, _, _ io.Writer) int {
		got.repo, got.appsRoot, got.promotable = repo, appsRoot, promotable
		got.calls++
		return 42
	}
	if code := run([]string{"--repo", fixture}, io.Discard, io.Discard); code != 42 {
		t.Fatalf("exit %d, want the runner's 42", code)
	}
	if got.repo != fixture || got.appsRoot != "cluster/apps" || strings.Join(got.promotable, ",") != "ghcr.io/" {
		t.Errorf("runner got %+v", got)
	}
	if code := run([]string{"--repo", fixture, "--apps-root", "x", "--promotable", "a/,b/"}, io.Discard, io.Discard); code != 42 {
		t.Fatalf("exit %d, want 42", code)
	}
	if got.appsRoot != "x" || strings.Join(got.promotable, ",") != "a/,b/" {
		t.Errorf("flags not passed through: %+v", got)
	}
	// A command after the flags goes to the command, never to the TUI.
	if code := run([]string{"--repo", fixture, "bogus"}, io.Discard, io.Discard); code != exitUsage {
		t.Errorf("--repo with a command: exit %d, want %d", code, exitUsage)
	}
	if got.calls != 2 {
		t.Errorf("runner called %d times, want 2", got.calls)
	}
}

func TestRunTUIFailsBeforeStartingOnBadRepo(t *testing.T) {
	var errOut bytes.Buffer
	if code := runTUI(t.TempDir(), "", []string{"ghcr.io/"}, io.Discard, &errOut); code != exitFailure {
		t.Fatalf("exit %d, want %d", code, exitFailure)
	}
	if s := errOut.String(); !strings.HasPrefix(s, "hoist: ") || !strings.Contains(s, "apps root cluster/apps") {
		t.Errorf("stderr: %s", s)
	}
}
