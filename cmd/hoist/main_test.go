package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/gitops"
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

// The root flagset advertises --repo/--apps-root/--promotable; given there, they must reach
// the plan command as its defaults, and a plan-level flag must still win.
func TestPlanTakesRootFlagsAsDefaults(t *testing.T) {
	var out, errOut bytes.Buffer
	args := []string{"--repo", fixture, "plan", "--from", "app-staging", "--to", "app-production", "--dry-run"}
	if got := run(args, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	plus := 0
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++ ") {
			plus++
		}
	}
	if plus != 6 {
		t.Errorf("diff has %d added lines, want 6:\n%s", plus, out.String())
	}
	// The other shared flags travel the same way: a root --promotable that excludes
	// everything is BuildPlan's "no promotable" error, not the default prefix.
	errOut.Reset()
	if got := run([]string{"--repo", fixture, "--promotable", " ", "plan", "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, &errOut); got != exitFailure || !strings.Contains(errOut.String(), "no promotable") {
		t.Errorf("root --promotable not passed through: exit %d, stderr: %s", got, errOut.String())
	}
	errOut.Reset()
	if got := run([]string{"--repo", fixture, "--apps-root", "nowhere", "plan", "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, &errOut); got != exitFailure || !strings.Contains(errOut.String(), "apps root nowhere") {
		t.Errorf("root --apps-root not passed through: exit %d, stderr: %s", got, errOut.String())
	}
	// A plan-level flag overrides the root value.
	errOut.Reset()
	if got := run([]string{"--repo", t.TempDir(), "plan", "--repo", fixture, "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, &errOut); got != 0 {
		t.Errorf("plan-level --repo did not override the root value: exit %d, stderr: %s", got, errOut.String())
	}
	// Positive control: the unchanged form still works.
	if got := run(planArgs("--dry-run"), io.Discard, io.Discard); got != 0 {
		t.Errorf("plan --repo form: exit %d, want 0", got)
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

// printPlan reads every edited file from disk; a plan whose Edit.File escapes the repo
// must be refused there too, not only in Apply.
func TestPrintPlanRefusesFileOutsideRepo(t *testing.T) {
	r, err := gitops.Discover(fixture, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := gitops.BuildPlan(r, "app-staging", "app-production", []string{"ghcr.io/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the unmodified plan prints.
	if err := printPlan(io.Discard, r, &plan, []string{"ghcr.io/"}); err != nil {
		t.Fatalf("control: %v", err)
	}
	for _, file := range []string{"../" + plan.Edits[0].File, "x/../../" + plan.Edits[0].File} {
		esc := plan
		esc.Edits = append([]gitops.Edit(nil), plan.Edits...)
		esc.Edits[0].File = file
		if err := printPlan(io.Discard, r, &esc, []string{"ghcr.io/"}); err == nil || !strings.Contains(err.Error(), "relative path inside the repo") {
			t.Errorf("%s: printPlan err = %v, want a containment refusal", file, err)
		}
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

// TestMain points the default config location at an empty directory so the developer's
// own ~/.config/hoist/config.yaml cannot leak into the M1 behaviour the tests above pin.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hoist-test-xdg")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir) // best effort; the OS reaps its temp dir anyway
	os.Exit(code)
}

// writeConfig writes body to a temp config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureTUI swaps the TUI runner for one that records its arguments.
func captureTUI(t *testing.T) *struct {
	repo, appsRoot string
	promotable     []string
} {
	t.Helper()
	orig := tuiRunner
	t.Cleanup(func() { tuiRunner = orig })
	got := &struct {
		repo, appsRoot string
		promotable     []string
	}{}
	tuiRunner = func(repo, appsRoot string, promotable []string, _, _ io.Writer) int {
		got.repo, got.appsRoot, got.promotable = repo, appsRoot, promotable
		return 42
	}
	return got
}

func absFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// With no flags and exactly one configured repo, both the matrix and plan pick it, with
// its apps_root and promotable; a flag given on the command line still wins.
func TestConfigSingleRepoIsPickedWithNoFlags(t *testing.T) {
	cfgPath := writeConfig(t, "repos:\n  - path: "+absFixture(t)+"\n    promotable: [ghcr.io/example/]\n")
	got := captureTUI(t)
	if code := run([]string{"--config", cfgPath}, io.Discard, io.Discard); code != 42 {
		t.Fatalf("no flags: exit %d, want the runner's 42", code)
	}
	if got.repo != absFixture(t) || got.appsRoot != "cluster/apps" || strings.Join(got.promotable, ",") != "ghcr.io/example/" {
		t.Errorf("runner got %+v", *got)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", cfgPath, "plan", "--from", "app-staging", "--to", "app-production", "--dry-run"}, &out, &errOut); code != 0 {
		t.Fatalf("plan without --repo: exit %d; stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "(6 edits in 4 files)") || !strings.Contains(out.String(), "third-party: outside ghcr.io/example/") {
		t.Errorf("plan did not use the configured repo and prefixes:\n%s", out.String())
	}
	// Command-line flags beat the file at either level.
	if code := run([]string{"--config", cfgPath, "--apps-root", "x", "--promotable", "a/"}, io.Discard, io.Discard); code != 42 || got.appsRoot != "x" || strings.Join(got.promotable, ",") != "a/" {
		t.Errorf("root flags did not override the config: exit %d, %+v", code, *got)
	}
	errOut.Reset()
	if code := run([]string{"--config", cfgPath, "plan", "--apps-root", "nowhere", "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), "apps root nowhere") {
		t.Errorf("plan-level --apps-root did not override the config: exit %d, stderr: %s", code, errOut.String())
	}
	// Positive control for the mutant: without the file, no flags is still a usage error.
	if code := run(nil, io.Discard, io.Discard); code != exitUsage {
		t.Errorf("no config, no flags: exit %d, want %d", code, exitUsage)
	}
}

// --repo on the command line beats the config: a path that matches no entry is used as
// given with the flag defaults, even when the file's only repo is somewhere else.
func TestConfigRepoFlagBeatsConfig(t *testing.T) {
	elsewhere := t.TempDir()
	cfgPath := writeConfig(t, "repos:\n  - name: other\n    path: "+elsewhere+"\n    apps_root: nowhere\n    promotable: [example.test/]\n")
	got := captureTUI(t)
	if code := run([]string{"--config", cfgPath, "--repo", fixture}, io.Discard, io.Discard); code != 42 {
		t.Fatalf("exit %d, want 42", code)
	}
	if got.repo != fixture || got.appsRoot != "cluster/apps" || strings.Join(got.promotable, ",") != "ghcr.io/" {
		t.Errorf("config leaked onto an explicit --repo: %+v", *got)
	}
	if code := run([]string{"--config", cfgPath, "plan", "--repo", fixture, "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("plan --repo with a config pointing elsewhere: exit %d, want 0", code)
	}
	// Positive control: with no --repo the configured (non-repo) path is used and fails.
	var errOut bytes.Buffer
	if code := run([]string{"--config", cfgPath, "plan", "--from", "app-staging", "--to", "app-production", "--dry-run"}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), elsewhere) {
		t.Errorf("configured repo not used without --repo: exit %d, stderr: %s", code, errOut.String())
	}
	// --repo <name> selects the entry and uses its path.
	if code := run([]string{"--config", cfgPath, "--repo", "other"}, io.Discard, io.Discard); code != 42 || got.repo != elsewhere || got.appsRoot != "nowhere" {
		t.Errorf("--repo by name: exit %d, %+v", code, *got)
	}
}

func TestConfigSelectionErrors(t *testing.T) {
	captureTUI(t)
	two := writeConfig(t, "repos:\n  - name: a\n    path: /src/a\n  - name: b\n    path: /src/b\n")
	var errOut bytes.Buffer
	if code := run([]string{"--config", two}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), "choose one with --repo <name|path>: a, b") {
		t.Errorf("ambiguous: exit %d, stderr: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"--config", two, "plan", "--from", "x", "--to", "y", "--dry-run"}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), "choose one") {
		t.Errorf("ambiguous plan: exit %d, stderr: %s", code, errOut.String())
	}
	// A repo without a path is fine to list, but not to open without --repo.
	noPath := writeConfig(t, "repos:\n  - name: a\n    github: me/a\n")
	errOut.Reset()
	if code := run([]string{"--config", noPath}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), noPath+": repos[0].path is required") {
		t.Errorf("pathless repo: exit %d, stderr: %s", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"--config", noPath, "--repo", "a"}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), "repos[0].path is required") {
		t.Errorf("pathless repo by name: exit %d, stderr: %s", code, errOut.String())
	}
	// An explicit --config that does not exist is an error; the default may be missing.
	errOut.Reset()
	if code := run([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml"), "--repo", fixture}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), "no such file") {
		t.Errorf("missing --config: exit %d, stderr: %s", code, errOut.String())
	}
	if code := run([]string{"--repo", fixture}, io.Discard, io.Discard); code != 42 {
		t.Errorf("missing default config: exit %d, want 42", code)
	}
}

// A broken file at the default location stops every command, never a silent fall-back
// to flags — the lesson AGENTS.md §11 records for a sibling repo's .env.
func TestConfigBrokenDefaultFileIsAnError(t *testing.T) {
	captureTUI(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "hoist"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(xdg, "hoist", "config.yaml")
	if err := os.WriteFile(file, []byte("repos:\n  - path: /x\n    apps_roots: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	if code := run([]string{"--repo", fixture}, io.Discard, &errOut); code != exitFailure || !strings.Contains(errOut.String(), file) || !strings.Contains(errOut.String(), "apps_roots") {
		t.Errorf("exit %d, stderr: %s", code, errOut.String())
	}
	if code := run([]string{"config", "path"}, io.Discard, io.Discard); code != exitFailure {
		t.Errorf("config path on a broken file: exit %d, want %d", code, exitFailure)
	}
}

func TestConfigShowAndPath(t *testing.T) {
	cfgPath := writeConfig(t, "repos:\n  - path: ~/src/my-gitops\nregistries:\n  - prefix: ghcr.io/me/\n    op: op://vault/item/field\n")
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", cfgPath, "config", "path"}, &out, &errOut); code != 0 || out.String() != cfgPath+"\n" || errOut.Len() != 0 {
		t.Errorf("config path: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"--config", cfgPath, "config", "show"}, &out, &errOut); code != 0 {
		t.Fatalf("config show: exit %d, stderr: %s", code, errOut.String())
	}
	s := out.String()
	for _, want := range []string{"# " + cfgPath + "\n", "path: ~/src/my-gitops", "name: my-gitops", "apps_root: cluster/apps", "op: <redacted>", "ci: 20s", "deadline: 4h0m0s"} {
		if !strings.Contains(s, want) {
			t.Errorf("show lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "op://") || strings.Contains(s, os.Getenv("HOME")+"/src") {
		t.Errorf("show leaked a secret or resolved ~:\n%s", s)
	}
	// With no file, path still says where it looked and show prints the defaults.
	out.Reset()
	errOut.Reset()
	missing := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "hoist", "config.yaml")
	if code := run([]string{"config", "path"}, &out, &errOut); code != 0 || out.String() != missing+"\n" || !strings.Contains(errOut.String(), "no file there") {
		t.Errorf("config path without a file: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"config", "show"}, &out, io.Discard); code != 0 || !strings.Contains(out.String(), "ci: 20s") {
		t.Errorf("config show without a file: exit %d:\n%s", code, out.String())
	}
	for _, args := range [][]string{{"config"}, {"config", "bogus"}, {"config", "show", "extra"}} {
		if code := run(args, io.Discard, io.Discard); code != exitUsage {
			t.Errorf("run(%q) exit %d, want %d", args, code, exitUsage)
		}
	}
}
