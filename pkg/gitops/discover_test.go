package gitops

import (
	"bytes"
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
	// staging: web(2) marketing(1) counta(1+1 cronjob) temporal(2) = 7
	// production: web(3) marketing(1) counta(1 init + 1 + 1 cronjob) temporal(2) = 9
	if n != 16 {
		t.Errorf("occurrences = %d, want 16", n)
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
