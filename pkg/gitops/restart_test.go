package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var restartAt = time.Date(2026, 9, 6, 2, 30, 0, 0, time.UTC)

const restartStamp = "2026-09-06T02:30:00Z"

// restartRepo lays out a one-family fixture repo whose Deployment manifest is whatever the test
// gives it, so each shape below is stated in full rather than assembled by mutation.
func restartRepo(t *testing.T, manifest string) *Repo {
	t.Helper()
	root := t.TempDir()
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		mkdir(filepath.Dir(rel))
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-staging-app.yaml", `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: app-staging
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://git.example.test/example/gitops.git
    targetRevision: main
    path: cluster/apps/app-staging/web
  destination:
    server: https://kubernetes.default.svc
    namespace: app-staging
`)
	write("cluster/apps/app-staging/web/app.yaml", manifest)
	r, err := Discover(root, "cluster/apps")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// deploymentWithLabels is the shape every Deployment in the real target repo has: a pod
// template whose metadata carries labels and nothing else. Adding the annotation here means
// introducing the annotations block, which is the case that adds lines.
const deploymentWithLabels = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: app-staging
spec:
  replicas: 2
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: ghcr.io/example/web:v1@sha256:1111111111111111111111111111111111111111111111111111111111111111
`

func planOne(t *testing.T, manifest string) (*Repo, Plan, RestartEdit) {
	t.Helper()
	r := restartRepo(t, manifest)
	pl, err := BuildRestartPlan(r, "app-staging", nil, restartAt)
	if err != nil {
		t.Fatalf("BuildRestartPlan: %v", err)
	}
	if len(pl.Restarts) != 1 {
		t.Fatalf("expected exactly one restart, got %d", len(pl.Restarts))
	}
	return r, pl, pl.Restarts[0]
}

// restartApplied returns the manifest as ApplyRestarts writes it to disk.
func restartApplied(t *testing.T, r *Repo, pl Plan) string {
	t.Helper()
	changed, err := ApplyRestarts(r.Root, pl.Restarts)
	if err != nil {
		t.Fatalf("ApplyRestarts: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("expected one changed file, got %v", changed)
	}
	b, err := os.ReadFile(filepath.Join(r.Root, changed[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The case the real repo actually has: labels and no annotations. The annotations block is
// introduced at the labels' own indentation, its child one step deeper, and every other byte
// of the file is untouched.
func TestRestartIntroducesTheAnnotationsBlock(t *testing.T) {
	r, pl, re := planOne(t, deploymentWithLabels)
	if re.Replace {
		t.Error("a Deployment with no annotations block needs an insertion, not a replacement")
	}
	if len(re.Lines) != 2 {
		t.Fatalf("expected the annotations header and the key, got %q", re.Lines)
	}
	if re.Old != "" {
		t.Errorf("Old = %q, want empty: nothing was superseded", re.Old)
	}

	got := restartApplied(t, r, pl)
	want := strings.Replace(deploymentWithLabels,
		"    metadata:\n      labels:\n",
		"    metadata:\n      annotations:\n        "+RestartAnnotation+`: "`+restartStamp+"\"\n      labels:\n",
		1)
	if got != want {
		t.Errorf("applied manifest:\n%s\nwant:\n%s", got, want)
	}
}

// Running it twice replaces in place rather than stacking a second annotation — and the line
// count stops moving, because the second write is an ordinary scalar change.
func TestSecondRestartReplacesInPlace(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	first := restartApplied(t, r, pl)

	later := restartAt.Add(time.Hour)
	pl2, err := BuildRestartPlan(r, "app-staging", nil, later)
	if err != nil {
		t.Fatalf("second BuildRestartPlan: %v", err)
	}
	re := pl2.Restarts[0]
	if !re.Replace {
		t.Fatal("a Deployment that already carries the annotation must be rewritten in place")
	}
	if re.Old != restartStamp {
		t.Errorf("Old = %q, want the superseded stamp %q", re.Old, restartStamp)
	}

	second := restartApplied(t, r, pl2)
	if strings.Count(second, RestartAnnotation) != 1 {
		t.Errorf("the annotation should appear exactly once:\n%s", second)
	}
	if strings.Count(first, "\n") != strings.Count(second, "\n") {
		t.Error("an in-place restart must not change the line count")
	}
	if !strings.Contains(second, later.UTC().Format(time.RFC3339)) {
		t.Errorf("the new stamp is missing:\n%s", second)
	}
}

// An annotations block that already exists takes the key as a new entry at its siblings' own
// indentation, and its existing entries survive untouched.
func TestRestartJoinsAnExistingAnnotationsBlock(t *testing.T) {
	manifest := strings.Replace(deploymentWithLabels,
		"    metadata:\n      labels:\n",
		"    metadata:\n      annotations:\n        prometheus.io/scrape: \"true\"\n      labels:\n",
		1)
	r, pl, re := planOne(t, manifest)
	if re.Replace {
		t.Error("a new key in an existing block is an insertion")
	}
	if len(re.Lines) != 1 {
		t.Fatalf("the block already exists, so only the key is written: %q", re.Lines)
	}
	got := restartApplied(t, r, pl)
	if !strings.Contains(got, `prometheus.io/scrape: "true"`) {
		t.Errorf("an existing annotation was lost:\n%s", got)
	}
	if !strings.Contains(got, "        "+RestartAnnotation+`: "`+restartStamp+`"`) {
		t.Errorf("the annotation was not written at its siblings' indentation:\n%s", got)
	}
}

// The timestamp must survive as a string. Written unquoted, YAML resolves an RFC3339 value as
// a !!timestamp, and Kubernetes rejects a non-string annotation value outright — so the object
// would be refused by the API server rather than restarted.
func TestRestartStampIsWrittenAsAString(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	got := restartApplied(t, r, pl)
	if !strings.Contains(got, `: "`+restartStamp+`"`) {
		t.Fatalf("the stamp must be quoted:\n%s", got)
	}
	docs, err := decodeDocs([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := podTemplateAnnotation(docs[0])
	if !ok {
		t.Fatalf("the annotation did not decode as a string: %#v", docs[0])
	}
	if v != restartStamp {
		t.Errorf("decoded stamp = %q, want %q", v, restartStamp)
	}
}

// Indentation is read out of the file, never assumed. A manifest indented in four-space steps
// must come back indented in four-space steps.
func TestRestartFollowsTheFilesOwnIndentation(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
    name: web
    namespace: app-staging
spec:
    replicas: 2
    selector:
        matchLabels:
            app: web
    template:
        metadata:
            labels:
                app: web
        spec:
            containers:
                - name: web
                  image: ghcr.io/example/web:v1@sha256:1111111111111111111111111111111111111111111111111111111111111111
`
	r, pl, _ := planOne(t, manifest)
	got := restartApplied(t, r, pl)
	if !strings.Contains(got, "\n            annotations:\n") {
		t.Errorf("the annotations block should sit at the labels' own depth:\n%s", got)
	}
	if !strings.Contains(got, "\n                "+RestartAnnotation+`: "`) {
		t.Errorf("the key should sit one four-space step deeper:\n%s", got)
	}
}

// A Job is not restarted. Re-running one is a different operation with different consequences,
// so it is left alone rather than quietly given a pod-template annotation.
func TestRestartLeavesJobsAlone(t *testing.T) {
	manifest := deploymentWithLabels + `---
apiVersion: batch/v1
kind: Job
metadata:
  name: purge
spec:
  template:
    metadata:
      labels:
        app: purge
    spec:
      restartPolicy: Never
      containers:
        - name: purge
          image: ghcr.io/example/web:v1@sha256:1111111111111111111111111111111111111111111111111111111111111111
`
	r, pl, re := planOne(t, manifest)
	if re.Kind != "Deployment" || re.Name != "web" {
		t.Errorf("the only restart should be the Deployment, got %s/%s", re.Kind, re.Name)
	}
	got := restartApplied(t, r, pl)
	// Exactly one annotation in the file, and it is above the Job's own document.
	if n := strings.Count(got, RestartAnnotation); n != 1 {
		t.Errorf("expected one annotation in the file, got %d:\n%s", n, got)
	}
	if strings.Index(got, RestartAnnotation) > strings.Index(got, "kind: Job") {
		t.Errorf("the annotation landed in the Job's document:\n%s", got)
	}
}

// A Deployment hoist cannot place the annotation in is reported, not guessed at. Inventing a
// metadata block means inventing this file's indentation step with nothing to derive it from.
func TestRestartRefusesADeploymentWithNoPodTemplateMetadata(t *testing.T) {
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels:
      app: web
  template:
    spec:
      containers:
        - name: web
          image: ghcr.io/example/web:v1@sha256:1111111111111111111111111111111111111111111111111111111111111111
`
	r := restartRepo(t, manifest)
	_, err := BuildRestartPlan(r, "app-staging", nil, restartAt)
	if err == nil {
		t.Fatal("expected an error: there is nothing hoist can restart here")
	}
	if !strings.Contains(err.Error(), "no Deployment to restart") {
		t.Errorf("unexpected error: %v", err)
	}
}

// VerifyRestarts' semantic half exists to catch a bad PLAN, not a bad apply. The byte half
// replays the plan and compares, so it can only ever confirm that the applier did what the
// plan said — if the plan itself puts the annotation at the wrong depth, the bytes match
// perfectly and the file is still wrong.
//
// Here the planned lines are indented to metadata's own level, which makes the annotation a
// sibling of metadata under spec.template rather than a child of it: valid YAML, exactly the
// planned bytes, and not a pod-template annotation at all. Nothing restarts.
func TestVerifyRestartsRejectsAPlanThatWritesAtTheWrongDepth(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}

	shallow := pl.Restarts[0]
	shallow.Lines = []string{
		"    annotations:",
		"      " + RestartAnnotation + `: "` + restartStamp + `"`,
	}
	after, err := ApplyRestartsToBytes(before, []RestartEdit{shallow})
	if err != nil {
		t.Fatal(err)
	}
	// The byte half cannot object: this is exactly what the plan asked for.
	want, err := ApplyRestartsToBytes(before, []RestartEdit{shallow})
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(after) {
		t.Fatal("fixture precondition: the write should match its own plan byte for byte")
	}

	err = VerifyRestarts(
		map[string][]byte{rel: before},
		map[string][]byte{rel: after},
		[]RestartEdit{shallow},
	)
	if err == nil {
		t.Fatal("VerifyRestarts accepted a plan whose annotation is not on the pod template")
	}
	if !strings.Contains(err.Error(), "no "+RestartAnnotation) {
		t.Errorf("the refusal should say the pod template has no such annotation, got: %v", err)
	}
}

// The other half of the same class: planned lines that are byte-faithful but land the
// annotation inside the labels block, silently inventing a label instead of an annotation.
func TestVerifyRestartsRejectsAPlanThatLandsInAnotherBlock(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	// Anchored on the labels line rather than metadata, at the labels' children's depth.
	intoLabels := pl.Restarts[0]
	for i, l := range strings.Split(string(before), "\n") {
		if strings.TrimSpace(l) == "labels:" {
			intoLabels.Line = i + 1
			break
		}
	}
	intoLabels.Lines = []string{"        " + RestartAnnotation + `: "` + restartStamp + `"`}
	after, err := ApplyRestartsToBytes(before, []RestartEdit{intoLabels})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRestarts(
		map[string][]byte{rel: before},
		map[string][]byte{rel: after},
		[]RestartEdit{intoLabels},
	); err == nil {
		t.Fatal("VerifyRestarts accepted an annotation written into the labels block")
	}
}

// And the ordinary tamper: any other change to the file, however small, is rejected.
func TestVerifyRestartsRejectsAnUnplannedChange(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ApplyRestartsToBytes(before, pl.Restarts)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(after), "replicas: 2", "replicas: 3", 1)
	if tampered == string(after) {
		t.Fatal("fixture precondition: replicas line not found")
	}
	err = VerifyRestarts(
		map[string][]byte{rel: before},
		map[string][]byte{rel: []byte(tampered)},
		pl.Restarts,
	)
	if err == nil {
		t.Fatal("VerifyRestarts accepted an unplanned change")
	}
}

// The happy path passes both halves, so the rejections above mean something.
func TestVerifyRestartsAcceptsThePlannedWrite(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ApplyRestartsToBytes(before, pl.Restarts)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRestarts(
		map[string][]byte{rel: before},
		map[string][]byte{rel: after},
		pl.Restarts,
	); err != nil {
		t.Fatalf("VerifyRestarts rejected its own planned write: %v", err)
	}
}

// A restart names no image, so it plans no Edits at all — the reason it needed a write kind of
// its own rather than a widened Edit.
func TestRestartPlanHasNoImageEdits(t *testing.T) {
	_, pl, _ := planOne(t, deploymentWithLabels)
	if len(pl.Edits) != 0 {
		t.Errorf("a restart changes no image, got %d edits", len(pl.Edits))
	}
	if !pl.IsRestart() || pl.IsDeploy() {
		t.Errorf("Variant = %q", pl.Variant)
	}
	if pl.SourceEnv != "" {
		t.Errorf("SourceEnv = %q, want empty", pl.SourceEnv)
	}
}

// A public plan builder handed no repo returns an error, like its two siblings — it does not
// panic (Copilot, PR #78).
func TestBuildRestartPlanRefusesANilRepo(t *testing.T) {
	if _, err := BuildRestartPlan(nil, "app-staging", nil, restartAt); err == nil {
		t.Fatal("expected an error for a nil repo")
	} else if !strings.Contains(err.Error(), "nil repo") {
		t.Errorf("wording should match BuildPlan's own, got %v", err)
	}
}
