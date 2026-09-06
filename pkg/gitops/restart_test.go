package gitops

import (
	"fmt"
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

// RFC3339 has second resolution, so a Deployment already carrying this exact stamp would be
// planned a byte-identical replacement — nothing changes, nothing rolls, and the command
// reports a restart anyway (Copilot, PR #78). Planning at the same instant twice must produce
// a stamp that actually differs from what is already there.
func TestRestartNeverPlansAByteIdenticalWrite(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	if _, err := ApplyRestarts(r.Root, pl.Restarts); err != nil {
		t.Fatal(err)
	}
	// The same instant as the write that just landed.
	again, err := BuildRestartPlan(r, "app-staging", nil, restartAt)
	if err != nil {
		t.Fatal(err)
	}
	re := again.Restarts[0]
	if re.Old != restartStamp {
		t.Fatalf("fixture precondition: the manifest should already carry %q, got %q", restartStamp, re.Old)
	}
	if re.New == re.Old {
		t.Fatal("planned a write that changes nothing: the pod template would not move and nothing would roll")
	}
	// And it is still an honest timestamp, one second on, not a sub-second format kubectl
	// would never write.
	if _, perr := time.Parse(time.RFC3339, re.New); perr != nil {
		t.Errorf("the bumped stamp should still be plain RFC3339, got %q", re.New)
	}
	got := restartApplied(t, r, again)
	if strings.Contains(got, restartStamp) {
		t.Errorf("the superseded stamp should be gone:\n%s", got)
	}
}

// The diff an operator confirms must be a hunk, not the whole file. diff.go's UnifiedDiff was
// written when every edit was a same-line scalar replacement and falls back to printing the
// entire file as removed-then-added the moment the line counts differ — harmless for an edit,
// which never does, and useless for a restart, which always would.
func TestRestartDiffIsAHunkNotTheWholeFile(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	got := UnifiedRestartDiff(rel, before, pl.Restarts)

	added, removed, context := 0, 0, 0
	for _, l := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "@@"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		case strings.HasPrefix(l, " "):
			context++
		}
	}
	if added != 2 {
		t.Errorf("added lines = %d, want the annotations header and the key:\n%s", added, got)
	}
	if removed != 0 {
		t.Errorf("an insertion removes nothing, got %d removed lines:\n%s", removed, got)
	}
	total := len(strings.Split(strings.TrimRight(string(before), "\n"), "\n"))
	if context >= total {
		t.Errorf("the diff shows %d context lines of a %d-line file — that is the whole file, not a hunk:\n%s", context, total, got)
	}
	if !strings.Contains(got, "+      annotations:") {
		t.Errorf("the diff should show the inserted block at its real indentation:\n%s", got)
	}
}

// A second restart replaces in place, so its diff is an ordinary one-line -/+ pair.
func TestRestartDiffForAReplacementIsAOneLineChange(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	if _, err := ApplyRestarts(r.Root, pl.Restarts); err != nil {
		t.Fatal(err)
	}
	pl2, err := BuildRestartPlan(r, "app-staging", nil, restartAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rel := pl2.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	got := UnifiedRestartDiff(rel, before, pl2.Restarts)
	added, removed := 0, 0
	for _, l := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	if added != 1 || removed != 1 {
		t.Errorf("a replacement is one line out and one in, got +%d -%d:\n%s", added, removed, got)
	}
}

// UnifiedRestartDiff must never emit two hunks whose old-file ranges overlap: overlapping
// hunks repeat context lines and describe a file that does not exist (Copilot, PR #80).
//
// Driven at the function rather than through a plan, deliberately. A plan cannot currently
// reach this: one pod template per document puts consecutive anchors at least a dozen lines
// apart, wider than two three-line context windows, so BuildRestartPlan has no input that
// makes them overlap. The renderer is still written to be total over its input — it takes a
// []RestartEdit, not a plan — and this pins that property rather than pretending to exercise a
// scenario the planner can produce.
func TestRestartDiffMergesOverlappingHunks(t *testing.T) {
	r, pl, _ := planOne(t, deploymentWithLabels)
	rel := pl.Restarts[0].File
	before, err := os.ReadFile(filepath.Join(r.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	// Two writes two lines apart: their context windows overlap heavily.
	first := pl.Restarts[0]
	second := first
	second.Line = first.Line + 2
	got := UnifiedRestartDiff(rel, before, []RestartEdit{first, second})

	var starts, ends []int
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, "@@") {
			continue
		}
		var os1, oc, ns, nc int
		if _, err := fmt.Sscanf(l, "@@ -%d,%d +%d,%d @@", &os1, &oc, &ns, &nc); err != nil {
			t.Fatalf("unparseable hunk header %q: %v", l, err)
		}
		starts = append(starts, os1)
		ends = append(ends, os1+oc)
	}
	if len(starts) == 0 {
		t.Fatalf("no hunks rendered:\n%s", got)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] < ends[i-1] {
			t.Errorf("hunk %d starts at line %d, inside the previous hunk which ends at %d:\n%s",
				i+1, starts[i], ends[i-1], got)
		}
	}
	// Both writes survive the merge.
	if n := strings.Count(got, "+"+first.Lines[1]); n != 2 {
		t.Errorf("expected both writes in the diff, got %d:\n%s", n, got)
	}
	// And no line of the file appears twice as context.
	seen := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, " ") || strings.TrimSpace(l) == "" {
			continue
		}
		if seen[l] {
			t.Errorf("context line %q appears twice — the hunks overlap:\n%s", l, got)
		}
		seen[l] = true
	}
}

// UnifiedRestartDiff must render both EOF forms truthfully: no phantom blank context line for
// a file that ends in a newline, and the "\ No newline at end of file" marker for one that does
// not (Copilot, PR #80).
//
// Driven at the function, for the same reason as the overlapping-hunk test above: a valid
// Deployment's pod-template anchor is never within three lines of EOF, so no plan reaches
// either case. The renderer takes a []RestartEdit and should be total over it.
func TestRestartDiffHandlesBothEOFForms(t *testing.T) {
	edit := func(line int) RestartEdit {
		return RestartEdit{File: "m.yaml", Line: line, Lines: []string{"  x: y"}}
	}

	// Ends in a newline: no marker, and no line that is just the context prefix.
	withNL := []byte("a\nb\nc\nd\n")
	got := UnifiedRestartDiff("m.yaml", withNL, []RestartEdit{edit(4)})
	if strings.Contains(got, "No newline at end of file") {
		t.Errorf("this file ends in a newline; the marker must not appear:\n%s", got)
	}
	for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if l == " " {
			t.Errorf("a phantom blank context line was rendered at EOF:\n%s", got)
		}
	}
	// The hunk covers the whole four-line file and adds one.
	if !strings.Contains(got, "@@ -1,4 +1,5 @@") {
		t.Errorf("unexpected hunk header:\n%s", got)
	}

	// No trailing newline: the marker appears, once, on the last rendered line.
	noNL := []byte("a\nb\nc\nd")
	got2 := UnifiedRestartDiff("m.yaml", noNL, []RestartEdit{edit(4)})
	if n := strings.Count(got2, "No newline at end of file"); n != 1 {
		t.Errorf("expected exactly one no-newline marker, got %d:\n%s", n, got2)
	}
	lines := strings.Split(strings.TrimRight(got2, "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "\\ No newline") {
		t.Errorf("the marker should be on the last rendered line:\n%s", got2)
	}
}
