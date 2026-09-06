package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

func TestDiscoverEnvsAndFamiliesComeFromWrappers(t *testing.T) {
	r := discoverFixture(t)
	if r.AppsRoot != DefaultAppsRoot {
		t.Errorf("AppsRoot = %q", r.AppsRoot)
	}
	var envs []string
	for n := range r.Envs {
		envs = append(envs, n)
	}
	sort.Strings(envs)
	if diff := cmp.Diff([]string{"app-production", "app-staging"}, envs); diff != "" {
		t.Fatalf("envs: %s", diff)
	}
	for _, env := range r.Envs {
		var fams []string
		for n := range env.Families {
			fams = append(fams, n)
		}
		sort.Strings(fams)
		if diff := cmp.Diff([]string{"counta", "marketing", "temporal", "web"}, fams); diff != "" {
			t.Errorf("%s families: %s", env.Name, diff)
		}
		if want := "cluster/apps/" + env.Name; env.Dir != want {
			t.Errorf("%s Dir = %q, want %q", env.Name, env.Dir, want)
		}
	}
	// The wrapper is named site-app-production-app.yaml; the family is marketing.
	fam := r.Envs["app-production"].Families["marketing"]
	if fam.App != "site-production" || fam.Dir != "cluster/apps/app-production/marketing" {
		t.Errorf("production marketing family = %+v", *fam)
	}
	var wrapperFile string
	for _, a := range r.Apps {
		if a.Name == "site-production" {
			wrapperFile = a.File
		}
	}
	if wrapperFile != "cluster/apps/site-app-production-app.yaml" {
		t.Errorf("site-production wrapper file = %q", wrapperFile)
	}
	if len(r.Apps) != 8 {
		t.Errorf("Apps = %d, want 8 (the AppProject document must not count)", len(r.Apps))
	}
}

func TestDiscoverUnmanagedDirectories(t *testing.T) {
	r := discoverFixture(t)
	want := []string{"cluster/apps/app-production/hello-world", "cluster/apps/app-staging/hello-world"}
	if diff := cmp.Diff(want, r.Unmanaged); diff != "" {
		t.Errorf("Unmanaged: %s", diff)
	}
	for _, env := range r.Envs {
		for _, o := range envOccurrences(env) {
			if strings.Contains(o.File, "hello-world") || strings.Contains(o.Ref.Repo, "hello-world") {
				t.Errorf("unmanaged directory was scanned: %+v", o)
			}
		}
	}
}

func TestDiscoverOccurrencesOnlyInsideContainerItems(t *testing.T) {
	r := discoverFixture(t)
	n := 0
	for _, env := range r.Envs {
		for _, o := range envOccurrences(env) {
			n++
			if o.Kind == "ConfigMap" || strings.HasPrefix(o.Path, "data") {
				t.Errorf("ConfigMap image key recorded as an occurrence: %+v", o)
			}
			if !strings.Contains(o.Path, "ontainers[") {
				t.Errorf("occurrence outside a containers sequence: %+v", o)
			}
		}
	}
	// staging: web(2) marketing(1) counta(1 + 1 worker in a later doc + 1 cronjob) temporal(2) = 8
	// production: web(3) marketing(1) counta(1 init + 1 + 1 worker + 1 cronjob) temporal(2) = 10
	if n != 18 {
		t.Errorf("occurrences = %d, want 18", n)
	}
	// Positive control: the ConfigMap really does carry an image key the scanner had to skip.
	if !bytes.Contains(readFixture(t, "cluster/apps/app-staging/counta/app.yaml"), []byte("kind: ConfigMap")) {
		t.Fatal("fixture lost its ConfigMap; this test no longer proves anything")
	}
}

func TestDiscoverCronJobInSecondFile(t *testing.T) {
	r := discoverFixture(t)
	var found *Occurrence
	for _, o := range r.Envs["app-staging"].Families["counta"].Occurrences {
		if o.Kind == "CronJob" {
			o := o
			found = &o
		}
	}
	if found == nil {
		t.Fatal("no CronJob occurrence in staging counta")
	}
	if found.File != "cluster/apps/app-staging/counta/purge-cronjob.yaml" || found.Container != "purge" {
		t.Errorf("CronJob occurrence = %+v", *found)
	}
	if want := "spec.jobTemplate.spec.template.spec.containers[0].image"; found.Path != want {
		t.Errorf("Path = %q, want %q", found.Path, want)
	}
	if found.Style != 0 || strings.Contains(found.Raw, "#") {
		t.Errorf("inline comment leaked into the scalar: style=%d raw=%q", found.Style, found.Raw)
	}
}

func TestDiscoverInitContainersAndQuotingStyles(t *testing.T) {
	r := discoverFixture(t)
	byContainer := map[string]Occurrence{}
	for _, o := range envOccurrences(r.Envs["app-production"]) {
		byContainer[o.Name+"/"+o.Container] = o
	}
	init, ok := byContainer["counta/dbwait"]
	if !ok || init.Path != "spec.template.spec.initContainers[0].image" {
		t.Errorf("initContainer occurrence = %+v", init)
	}
	if o := byContainer["counta/counta"]; o.Style != yaml.SingleQuotedStyle {
		t.Errorf("single-quoted image recorded with style %d", o.Style)
	}
	if o := byContainer["marketing/marketing"]; o.Style != yaml.DoubleQuotedStyle {
		t.Errorf("double-quoted image recorded with style %d", o.Style)
	}
	if o := byContainer["web/queue"]; o.Path != "spec.template.spec.containers[2].image" {
		t.Errorf("third container path = %q", o.Path)
	}
}

// Every recorded (Line, Col, Style, Raw) must point at the literal bytes in the file: this
// is the positive control for the replacement logic and for the quote-column semantics.
func TestDiscoverPositionsMatchFileBytes(t *testing.T) {
	r := discoverFixture(t)
	for _, env := range r.Envs {
		for _, o := range envOccurrences(env) {
			lines := bytes.Split(readFixture(t, o.File), []byte{'\n'})
			if o.Line < 1 || o.Line > len(lines) {
				t.Errorf("%s: line %d out of range", o.File, o.Line)
				continue
			}
			line := lines[o.Line-1]
			off, ok := byteOffset(line, o.Col)
			if !ok {
				t.Errorf("%s:%d: column %d beyond line", o.File, o.Line, o.Col)
				continue
			}
			switch o.Style {
			case yaml.DoubleQuotedStyle:
				if line[off] != '"' {
					t.Errorf("%s:%d:%d: expected opening quote, got %q", o.File, o.Line, o.Col, line[off:])
				}
				off++
			case yaml.SingleQuotedStyle:
				if line[off] != '\'' {
					t.Errorf("%s:%d:%d: expected opening quote, got %q", o.File, o.Line, o.Col, line[off:])
				}
				off++
			}
			if !bytes.HasPrefix(line[off:], []byte(o.Raw)) {
				t.Errorf("%s:%d:%d: file has %q, occurrence says %q", o.File, o.Line, o.Col, line[off:], o.Raw)
			}
		}
	}
}

func TestDiscoverLineNumbersAreFileAbsoluteAcrossDocuments(t *testing.T) {
	svc := "apiVersion: v1\nkind: Service\nmetadata:\n  name: api\nspec:\n  ports:\n    - port: 80\n---\n"
	dep := deployment("api", "api", "ghcr.io/example/api:v1@"+digestA)
	root := writeRepo(t, map[string]string{
		"cluster/apps/api.yaml":             wrapper("api", "cluster/apps/staging/api", "staging"),
		"cluster/apps/staging/api/app.yaml": svc + dep,
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	occ := r.Envs["staging"].Families["api"].Occurrences
	if len(occ) != 1 {
		t.Fatalf("occurrences = %+v", occ)
	}
	imageAt := strings.Index(dep, "image:")
	if imageAt < 0 {
		t.Fatal("deployment helper lost its image line")
	}
	wantLine := strings.Count(svc, "\n") + strings.Count(dep[:imageAt], "\n") + 1
	if occ[0].Doc != 1 || occ[0].Line != wantLine {
		t.Errorf("occurrence doc=%d line=%d, want doc=1 line=%d (file-absolute)", occ[0].Doc, occ[0].Line, wantLine)
	}
}

func TestDiscoverEnvIsNamespaceNotDirectoryName(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"cluster/apps/whatever.yaml":        wrapper("thing", "cluster/apps/blue/api", "green"),
		"cluster/apps/blue/api/deploy.yaml": deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Envs["blue"]; ok {
		t.Error("env derived from directory name")
	}
	env, ok := r.Envs["green"]
	if !ok {
		t.Fatal("env not keyed by destination namespace")
	}
	if fam := env.Families["api"]; fam == nil || fam.App != "thing" || len(fam.Occurrences) != 1 {
		t.Errorf("family = %+v", fam)
	}
}

func TestDiscoverSameFamilyNameInTwoEnvsIsTwoFamilies(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"cluster/apps/a.yaml":               wrapper("api-a", "cluster/apps/a/api", "a"),
		"cluster/apps/b.yaml":               wrapper("api-b", "cluster/apps/b/api", "b"),
		"cluster/apps/a/api/app.yaml":       deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		"cluster/apps/b/api/app.yaml":       deployment("api", "api", "ghcr.io/example/api:v2@"+digestB),
		"cluster/apps/b/api/extra-job.yaml": deployment("job", "job", "ghcr.io/example/api:v2@"+digestB),
		"cluster/apps/b/scratch/notes.yaml": "kind: ConfigMap\nmetadata:\n  name: n\n",
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Envs["a"].Families["api"].Occurrences); got != 1 {
		t.Errorf("env a api occurrences = %d", got)
	}
	if got := len(r.Envs["b"].Families["api"].Occurrences); got != 2 {
		t.Errorf("env b api occurrences = %d (both files in the family dir must be scanned)", got)
	}
	if diff := cmp.Diff([]string{"cluster/apps/b/scratch"}, r.Unmanaged); diff != "" {
		t.Errorf("Unmanaged: %s", diff)
	}
}

// The family scan is one level deep (as Argo's directory source is without recurse), so a
// manifest in a nested subdirectory is never planned. That must be visible, not silent: the
// nested directory is reported as unmanaged. A nested directory with no YAML is not noise.
func TestDiscoverNestedYAMLInsideFamilyIsReportedUnmanaged(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"cluster/apps/api.yaml":                       wrapper("api", "cluster/apps/staging/api", "staging"),
		"cluster/apps/staging/api/app.yaml":           deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		"cluster/apps/staging/api/extras/job.yaml":    deployment("job", "job", "ghcr.io/example/side:v1@"+digestB),
		"cluster/apps/staging/api/docs/README.md":     "not a manifest\n",
		"cluster/apps/staging/api/.hidden/thing.yaml": deployment("h", "h", "ghcr.io/example/hidden:v1@"+digestC),
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"cluster/apps/staging/api/extras"}, r.Unmanaged); diff != "" {
		t.Errorf("Unmanaged: %s", diff)
	}
	occ := r.Envs["staging"].Families["api"].Occurrences
	if len(occ) != 1 || occ[0].Ref.Repo != "ghcr.io/example/api" {
		t.Errorf("family scan must stay one level deep; occurrences = %+v", occ)
	}
}

// Two wrappers on one directory are rejected either way, but the operator needs the two
// wrapper files (which may be named anything) and, when the namespaces differ, both of them.
func TestDiscoverDuplicateApplicationErrorsNameBothWrappers(t *testing.T) {
	dep := deployment("api", "api", "ghcr.io/example/api:v1@"+digestA)
	t.Run("same namespace and path", func(t *testing.T) {
		root := writeRepo(t, map[string]string{
			"cluster/apps/first.yaml":     wrapper("one", "cluster/apps/x/api", "env"),
			"cluster/apps/second.yaml":    wrapper("two", "cluster/apps/x/api", "env"),
			"cluster/apps/x/api/app.yaml": dep,
		})
		_, err := Discover(root, "")
		if err == nil {
			t.Fatal("duplicate (namespace, path) accepted")
		}
		for _, want := range []string{"cluster/apps/first.yaml", "cluster/apps/second.yaml", `"env"`, "cluster/apps/x/api"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
	})
	t.Run("same path in two namespaces", func(t *testing.T) {
		root := writeRepo(t, map[string]string{
			"cluster/apps/first.yaml":     wrapper("one", "cluster/apps/x/api", "env1"),
			"cluster/apps/second.yaml":    wrapper("two", "cluster/apps/x/api", "env2"),
			"cluster/apps/x/api/app.yaml": dep,
		})
		_, err := Discover(root, "")
		if err == nil {
			t.Fatal("one directory in two namespaces accepted")
		}
		for _, want := range []string{"cluster/apps/first.yaml", "cluster/apps/second.yaml", `"env1"`, `"env2"`, "same directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
	})
}

func TestDiscoverErrors(t *testing.T) {
	dep := deployment("api", "api", "ghcr.io/example/api:v1@"+digestA)
	cases := map[string]map[string]string{
		"duplicate family in one env": {
			"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env"),
			"cluster/apps/b.yaml":         wrapper("two", "cluster/apps/y/api", "env"),
			"cluster/apps/x/api/app.yaml": dep,
			"cluster/apps/y/api/app.yaml": dep,
		},
		"two applications on one directory": {
			"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env1"),
			"cluster/apps/b.yaml":         wrapper("two", "cluster/apps/x/api", "env2"),
			"cluster/apps/x/api/app.yaml": dep,
		},
		"missing source directory": {
			"cluster/apps/a.yaml": wrapper("one", "cluster/apps/x/api", "env"),
		},
		"image via alias": {
			"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env"),
			"cluster/apps/x/api/app.yaml": "kind: Deployment\nmetadata:\n  name: api\n  annotations:\n    img: &img ghcr.io/example/api:v1\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: *img\n",
		},
		"multi-source application": {
			"cluster/apps/a.yaml": "kind: Application\nmetadata:\n  name: one\nspec:\n  sources:\n    - path: cluster/apps/x/api\n  destination:\n    namespace: env\n",
		},
		"no applications": {
			"cluster/apps/readme.yaml": "kind: ConfigMap\nmetadata:\n  name: n\n",
		},
		"unparseable image": {
			"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env"),
			"cluster/apps/x/api/app.yaml": deployment("api", "api", "ghcr.io/example/api:v1@sha256:notadigest"),
		},
	}
	for name, files := range cases {
		root := writeRepo(t, files)
		if r, err := Discover(root, ""); err == nil {
			t.Errorf("%s: Discover succeeded: %+v", name, r)
		}
	}
	if _, err := Discover(fixtureRoot, "/abs"); err == nil {
		t.Error("absolute apps root accepted")
	}
}

// yaml.v3 parses the explicit-key form ("? image" / ": ref") and a plain value on the line
// after "image:" as the same mapping as image: ref, but nothing precedes the value on its own
// line, so Apply could never rewrite it. Discover refuses those layouts up front, through the
// predicate Apply uses, naming the file and the value's line — instead of recording an
// occurrence that is planned and only refused at write time. A blank before the colon is a
// one-line layout and stays discoverable (the positive control).
func TestDiscoverRefusesKeyAndValueOnDifferentLines(t *testing.T) {
	head := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n" // 9 lines
	ref := "ghcr.io/example/api:v1@" + digestA
	for name, item := range map[string]string{
		"explicit key":       "          ? image\n          : " + ref + "\n",
		"value on next line": "          image:\n            " + ref + "\n",
	} {
		root := writeRepo(t, map[string]string{
			"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env"),
			"cluster/apps/x/api/app.yaml": head + item,
		})
		r, err := Discover(root, "")
		if err == nil {
			t.Errorf("%s: discovered as %+v", name, r.Envs["env"].Families["api"].Occurrences)
			continue
		}
		for _, want := range []string{"cluster/apps/x/api/app.yaml:11", "spec.template.spec.containers[0].image", "shares its line with its key"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error lacks %q: %v", name, want, err)
			}
		}
	}
	root := writeRepo(t, map[string]string{
		"cluster/apps/a.yaml":         wrapper("one", "cluster/apps/x/api", "env"),
		"cluster/apps/x/api/app.yaml": head + "          image : " + ref + "\n",
	})
	r, err := Discover(root, "")
	if err != nil {
		t.Fatalf("image : ref refused: %v", err)
	}
	occ := r.Envs["env"].Families["api"].Occurrences
	if len(occ) != 1 || occ[0].Line != 10 || occ[0].Col != 19 || occ[0].Raw != ref {
		t.Errorf("occurrences = %+v", occ)
	}
}

// symlinkRepo lays out parent/repo (the checkout: files, slash paths → contents) beside
// parent/outside (an attacker-chosen location the checkout must never read): outside/apps
// holds a valid wrapper naming the in-repo family families/api, and outside/api/app.yaml and
// outside/app.yaml hold a Deployment whose reference exists nowhere in the repo, so a read
// that escaped would be visible as a successful Discover or in the occurrences. Callers add
// the symlink under test with symlink.
func symlinkRepo(t *testing.T, files map[string]string) (root, outside string) {
	t.Helper()
	parent := t.TempDir()
	root = filepath.Join(parent, "repo")
	outside = filepath.Join(parent, "outside")
	external := deployment("api", "api", "ghcr.io/example/external:v9@"+digestC)
	for p, c := range map[string]string{
		"apps/api.yaml": wrapper("api", "families/api", "staging"),
		"api/app.yaml":  external,
		"app.yaml":      external,
	} {
		full := filepath.Join(outside, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// symlink creates link (slash path under root) pointing at target (an absolute path).
func symlink(t *testing.T, root, link, target string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(link))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverRefusesSymlinkOutsideRepo names the attacker: a PR author who commits a symlink
// (git checks one out as a symlink) so that hoist, run against the branch, reads a manifest —
// or the wrapper set — from somewhere on the operator's machine instead of the checkout, and
// plans that external reference into an in-repo target. Each of the three joins Discover
// performs (apps root, family directory, YAML file) gets its own escape.
func TestDiscoverRefusesSymlinkOutsideRepo(t *testing.T) {
	staging := map[string]string{"cluster/apps/api.yaml": wrapper("api", "cluster/apps/staging/api", "staging")}
	cases := map[string]struct {
		files        map[string]string
		link, target string // link is repo-relative; target is under outside
	}{
		// The in-repo family the external wrapper names exists, so an escape here would be a
		// clean Discover from wrappers hoist never should have read.
		"apps root":        {map[string]string{"families/api/app.yaml": deployment("api", "api", "ghcr.io/example/api:v1@"+digestA)}, "cluster/apps", "apps"},
		"family directory": {staging, "cluster/apps/staging/api", "api"},
		"yaml file":        {staging, "cluster/apps/staging/api/app.yaml", "app.yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root, outside := symlinkRepo(t, tc.files)
			symlink(t, root, tc.link, filepath.Join(outside, tc.target))
			r, err := Discover(root, "")
			if err == nil {
				t.Fatalf("Discover followed %s out of the repo: %+v", tc.link, r)
			}
			if !strings.Contains(err.Error(), tc.link) || !strings.Contains(err.Error(), "outside the repo") {
				t.Errorf("error does not name %q as outside the repo: %v", tc.link, err)
			}
		})
	}
}

// A symlink that stays inside the checkout is an ordinary layout choice and still works, as
// a directory and as a file — the positive control for the refusal above.
func TestDiscoverFollowsSymlinkInsideRepo(t *testing.T) {
	for name, link := range map[string]string{"directory": "cluster/apps/staging/api", "file": "cluster/apps/staging/api/app.yaml"} {
		root, _ := symlinkRepo(t, map[string]string{
			"cluster/apps/api.yaml": wrapper("api", "cluster/apps/staging/api", "staging"),
			"shared/app.yaml":       deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		})
		target := filepath.Join(root, "shared")
		if name == "file" {
			target = filepath.Join(target, "app.yaml")
		}
		symlink(t, root, link, target)
		r, err := Discover(root, "")
		if err != nil {
			t.Fatalf("%s symlink inside the repo refused: %v", name, err)
		}
		occ := r.Envs["staging"].Families["api"].Occurrences
		if len(occ) != 1 || occ[0].Ref.Repo != "ghcr.io/example/api" || occ[0].File != "cluster/apps/staging/api/app.yaml" {
			t.Errorf("%s: occurrences = %+v", name, occ)
		}
	}
}

// A backslash is not a path separator to path.Clean, so on a POSIX host "..\..\victim.yaml"
// cleans to itself and is not a climb until Windows joins it — the helper's contract is
// slash-separated, and the contract is enforced rather than described (issue #18). The forward
// slash form is the positive control.
func TestCheckRelativeRefusesBackslash(t *testing.T) {
	for _, p := range []string{`..\..\victim.yaml`, `cluster\apps\x\api`, `cluster/apps\x`} {
		if err := checkRelative(p); err == nil || !strings.Contains(err.Error(), "backslash") {
			t.Errorf("checkRelative(%q) = %v; want a backslash refusal", p, err)
		}
	}
	if err := checkRelative("cluster/apps/x/api"); err != nil {
		t.Errorf("slash path refused: %v", err)
	}
	root := writeRepo(t, map[string]string{
		"cluster/apps/a.yaml":         wrapper("one", `cluster\apps\x\api`, "env"),
		"cluster/apps/x/api/app.yaml": deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
	})
	if r, err := Discover(root, ""); err == nil || !strings.Contains(err.Error(), "backslash") {
		t.Errorf("Discover accepted a backslash spec.source.path: err=%v repo=%+v", err, r)
	}
}

// findUnmanaged's walk roots are the apps root and every managed family's parent, all of
// which Discover has already resolved through scanFamily — so through Discover itself there
// is no escape to name. The attacker here is a future caller handing findUnmanaged a managed
// set nothing checked (the ResolvePath doc comment's claim is that every join is contained,
// not that every current caller happens to be): the parent of the managed directory is a
// symlink out of the checkout, and the walk must refuse it rather than list what is there
// (issue #19). A symlink that stays inside the repo is the positive control, and its
// unmanaged entries are still reported under the repo-relative name, not the resolved one.
func TestFindUnmanagedRefusesRootOutsideRepo(t *testing.T) {
	root, outside := symlinkRepo(t, map[string]string{"cluster/apps/api.yaml": "kind: ConfigMap\nmetadata:\n  name: n\n"})
	symlink(t, root, "cluster/apps/staging", outside)
	managed := map[string]ArgoApp{"cluster/apps/staging/api": {Name: "api"}}
	if out, err := findUnmanaged(root, "cluster/apps", managed); err == nil || !strings.Contains(err.Error(), "outside the repo") {
		t.Fatalf("walked out of the repo: out=%v err=%v", out, err)
	}

	root, _ = symlinkRepo(t, map[string]string{
		"cluster/apps/api.yaml":  "kind: ConfigMap\nmetadata:\n  name: n\n",
		"shared/api/app.yaml":    deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		"shared/extras/app.yaml": deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
	})
	symlink(t, root, "cluster/apps/staging", filepath.Join(root, "shared"))
	out, err := findUnmanaged(root, "cluster/apps", managed)
	if err != nil {
		t.Fatalf("symlink inside the repo refused: %v", err)
	}
	if len(out) != 1 || out[0] != "cluster/apps/staging/extras" {
		t.Errorf("unmanaged = %v; want [cluster/apps/staging/extras]", out)
	}
}

// A wrapper naming a directory that does not exist is fatal (Argo could not sync it either,
// and a half-read repo is not one to plan over), but the error names the wrapper file, the
// Application and the path it declares rather than the bare os error (issue #12).
func TestDiscoverMissingSourceDirNamesWrapperAndPath(t *testing.T) {
	root := writeRepo(t, map[string]string{"cluster/apps/a.yaml": wrapper("one", "cluster/apps/x/api", "env")})
	_, err := Discover(root, "")
	if err == nil {
		t.Fatal("Discover succeeded with a dangling wrapper")
	}
	for _, want := range []string{"cluster/apps/a.yaml", `Application "one"`, `spec.source.path "cluster/apps/x/api" does not exist`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// The fixture repo carries an occurrence in a later YAML document of a multi-document file,
// so every golden built from it (discover, plan, matrix) exercises file-absolute line numbers
// past a document boundary end to end, not only the unit test above (issue #14). Asserted here
// so a fixture edit cannot quietly drop the coverage.
func TestFixtureHasOccurrenceInLaterDocument(t *testing.T) {
	r, err := Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, env := range r.Envs {
		for _, f := range env.Families {
			for _, o := range f.Occurrences {
				if o.Doc > 0 {
					found = true
					if o.Line <= 30 {
						t.Errorf("%s doc %d: line %d is not file-absolute", o.File, o.Doc, o.Line)
					}
				}
			}
		}
	}
	if !found {
		t.Error("no fixture occurrence has Doc > 0")
	}
}

// A family declared at the top of the checkout (spec.source.path "api", no directory part)
// has "." as its parent; findUnmanaged walks that parent as the root itself rather than
// asking ResolvePath for ".", which it rightly refuses. Its sibling directories are still
// reported unmanaged under their repo-relative names.
func TestDiscoverTopLevelFamilyDirectory(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"apps/a.yaml":       wrapper("one", "api", "env"),
		"api/app.yaml":      deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		"extras/app.yaml":   deployment("api", "api", "ghcr.io/example/api:v1@"+digestA),
		"apps/nested/x.yml": "kind: ConfigMap\nmetadata:\n  name: n\n",
	})
	r, err := Discover(root, "apps")
	if err != nil {
		t.Fatalf("a top-level family directory was refused: %v", err)
	}
	if occ := r.Envs["env"].Families["api"].Occurrences; len(occ) != 1 || occ[0].File != "api/app.yaml" {
		t.Errorf("occurrences = %+v", occ)
	}
	want := []string{"apps/nested", "extras"}
	if strings.Join(r.Unmanaged, ",") != strings.Join(want, ",") {
		t.Errorf("Unmanaged = %v, want %v", r.Unmanaged, want)
	}
}

// The missing-family error is about the directory, not any file in it: a manifest that is
// itself missing inside a directory that exists (a dangling symlink here) keeps its own
// file-level error, so the operator is not sent looking for a directory that is there.
func TestDiscoverMissingFileInsideExistingFamilyIsNotAMissingFamily(t *testing.T) {
	root, outside := symlinkRepo(t, map[string]string{"cluster/apps/api.yaml": wrapper("api", "cluster/apps/staging/api", "staging")})
	symlink(t, root, "cluster/apps/staging/api/app.yaml", filepath.Join(outside, "nowhere", "app.yaml"))
	_, err := Discover(root, "")
	if err == nil {
		t.Fatal("Discover succeeded through a dangling symlink")
	}
	if strings.Contains(err.Error(), "does not exist in the repo") {
		t.Errorf("a missing file was reported as a missing family: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster/apps/staging/api/app.yaml") {
		t.Errorf("error does not name the file: %v", err)
	}
}
