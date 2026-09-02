package gitops

import (
	"bytes"
	"strings"
	"testing"
)

// applied returns the fixture file, its correctly applied version, and the edits for it.
func applied(t *testing.T, file string) (before, after []byte, edits []Edit) {
	t.Helper()
	p := planFixture(t)
	edits = editsFor(p, file)
	if len(edits) == 0 {
		t.Fatalf("no edits for %s", file)
	}
	before = readFixture(t, file)
	after, err := ApplyBytes(before, edits)
	if err != nil {
		t.Fatal(err)
	}
	return before, after, edits
}

const (
	webFile    = "cluster/apps/app-production/web/app.yaml"
	cronFile   = "cluster/apps/app-production/counta/purge-cronjob.yaml"
	countaFile = "cluster/apps/app-production/counta/app.yaml"
)

func one(file string, b []byte) map[string][]byte { return map[string][]byte{file: b} }

func mustReplace(t *testing.T, b []byte, old, repl string) []byte {
	t.Helper()
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("mutation target %q not present; the test would pass vacuously", old)
	}
	return bytes.Replace(b, []byte(old), []byte(repl), 1)
}

func TestVerifyAcceptsAppliedResult(t *testing.T) {
	for _, f := range []string{webFile, cronFile, countaFile, "cluster/apps/app-production/marketing/app.yaml"} {
		before, after, edits := applied(t, f)
		if err := Verify(one(f, before), one(f, after), edits); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestVerifyRejectsChangedComment(t *testing.T) {
	before, after, edits := applied(t, webFile)
	mut := mustReplace(t, after, "# Production runs three processes", "# Production runs 3 processes")
	err := Verify(one(webFile, before), one(webFile, mut), edits)
	if err == nil {
		t.Fatal("comment change accepted")
	}
	// The node walk must catch it on its own, without the byte-level pass.
	if err := verifyStructure(webFile, before, mut, edits); err == nil || !strings.Contains(err.Error(), "comment changed") {
		t.Errorf("node walk did not flag the comment: %v", err)
	}
	// An inline comment on the edited line itself, too.
	before, after, edits = applied(t, cronFile)
	mut = mustReplace(t, after, "# keep in step with app.yaml", "# keep in step with app.yml")
	if err := Verify(one(cronFile, before), one(cronFile, mut), edits); err == nil {
		t.Error("inline comment change on the edited line accepted")
	}
	if err := verifyStructure(cronFile, before, mut, edits); err == nil || !strings.Contains(err.Error(), "comment changed") {
		t.Errorf("node walk did not flag the inline comment: %v", err)
	}
}

func TestVerifyRejectsScalarChangeAtUnplannedPath(t *testing.T) {
	before, after, edits := applied(t, countaFile)
	// The initContainer image is a real image scalar, but no edit is planned for it.
	mut := mustReplace(t, after, "ghcr.io/example/dbwait:v202601010101@sha256:dbfa1e01", "ghcr.io/example/dbwait:v202601010102@sha256:dbfa1e01")
	if err := Verify(one(countaFile, before), one(countaFile, mut), edits); err == nil {
		t.Fatal("unplanned image change accepted")
	}
	if err := verifyStructure(countaFile, before, mut, edits); err == nil || !strings.Contains(err.Error(), "unplanned scalar change") {
		t.Errorf("node walk did not flag the unplanned scalar: %v", err)
	}
	// A non-image scalar as well.
	mut = mustReplace(t, after, "replicas: 2", "replicas: 3")
	if err := verifyStructure(countaFile, before, mut, edits); err == nil || !strings.Contains(err.Error(), "spec.replicas") {
		t.Errorf("replicas change not flagged at its path: %v", err)
	}
}

func TestVerifyRejectsTrailingWhitespaceOnCommentLine(t *testing.T) {
	before, after, edits := applied(t, webFile)
	mut := mustReplace(t, after, "# Production runs three processes from the one image; staging folds queue into worker.\n",
		"# Production runs three processes from the one image; staging folds queue into worker. \n")
	err := Verify(one(webFile, before), one(webFile, mut), edits)
	if err == nil || !strings.Contains(err.Error(), "no edit was planned") {
		t.Errorf("trailing space not caught by the byte-level pass: %v", err)
	}
}

func TestVerifyRejectsLineCountChange(t *testing.T) {
	before, after, edits := applied(t, webFile)
	if err := Verify(one(webFile, before), one(webFile, append(after, '\n')), edits); err == nil || !strings.Contains(err.Error(), "line count") {
		t.Errorf("extra blank line accepted: %v", err)
	}
	mut := mustReplace(t, after, "      # Production runs three processes from the one image; staging folds queue into worker.\n", "")
	if err := Verify(one(webFile, before), one(webFile, mut), edits); err == nil {
		t.Error("deleted comment line accepted")
	}
}

func TestVerifyRejectsReorderedKeys(t *testing.T) {
	before, after, edits := applied(t, countaFile)
	// Swap two sibling keys in the ConfigMap data. Same node set, different order.
	mut := mustReplace(t, after, "  image: ghcr.io/example/counta:v202601151010\n  purge_after_days: \"90\"\n",
		"  purge_after_days: \"90\"\n  image: ghcr.io/example/counta:v202601151010\n")
	if err := Verify(one(countaFile, before), one(countaFile, mut), edits); err == nil {
		t.Error("reordered keys accepted")
	}
	if err := verifyStructure(countaFile, before, mut, edits); err == nil {
		t.Error("node walk accepted reordered keys")
	}
}

func TestVerifyRejectsEditNotApplied(t *testing.T) {
	before, _, edits := applied(t, webFile)
	if err := Verify(one(webFile, before), one(webFile, before), edits); err == nil {
		t.Error("identical before/after accepted although edits were planned")
	}
	if err := verifyStructure(webFile, before, before, edits); err == nil || !strings.Contains(err.Error(), "scalar after the edit") {
		t.Errorf("node walk did not notice the unapplied edit: %v", err)
	}
}

func TestVerifyRejectsWrongPathOrPosition(t *testing.T) {
	before, after, edits := applied(t, cronFile)
	wrongPath := append([]Edit(nil), edits...)
	wrongPath[0].Path = "spec.template.spec.containers[0].image"
	if err := verifyStructure(cronFile, before, after, wrongPath); err == nil || !strings.Contains(err.Error(), "planned edit is for path") {
		t.Errorf("wrong path accepted: %v", err)
	}
	wrongLine := append([]Edit(nil), edits...)
	wrongLine[0].Line++
	if err := Verify(one(cronFile, before), one(cronFile, after), wrongLine); err == nil {
		t.Error("edit recorded on the wrong line accepted")
	}
}

// TestVerifyWiresStructureLayer goes through Verify itself, not verifyStructure: the after
// set is the exact applied result, so the byte-level pass has nothing to fault, but the plan
// names the wrong path for the edited scalar, which only the node walk compares. A Verify
// that dropped its verifyStructure call would accept this and still pass every test that
// calls verifyStructure directly.
func TestVerifyWiresStructureLayer(t *testing.T) {
	before, after, edits := applied(t, cronFile)
	wrongPath := append([]Edit(nil), edits...)
	wrongPath[0].Path = "spec.template.spec.containers[0].image"
	// Control: the byte-level half really is blind to the path, so the assertion below can
	// be satisfied by the structure layer alone.
	if err := verifyLines(cronFile, before, after, wrongPath); err != nil {
		t.Fatalf("byte-level pass rejected the wrong-path plan on its own, so this test would not isolate the structure layer: %v", err)
	}
	err := Verify(one(cronFile, before), one(cronFile, after), wrongPath)
	if err == nil {
		t.Fatal("Verify accepted a plan whose path is wrong for the edited scalar; the structure layer is not wired in")
	}
	if !strings.Contains(err.Error(), "planned edit is for path") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// TestVerifyRejectsEditOutsideContainerItem: the fixture's ConfigMap carries an image: key
// that discovery deliberately skips. An Edit built by hand for that scalar — with a Path
// that is true for its position — passes the byte-level pass, so only the walk's own
// eligibility test (shared with discovery) can refuse it.
func TestVerifyRejectsEditOutsideContainerItem(t *testing.T) {
	before := readFixture(t, countaFile)
	docs, err := parseDocs(before)
	if err != nil {
		t.Fatal(err)
	}
	var edits []Edit
	for i, d := range docs {
		if scalarAt(d, "kind") != "ConfigMap" {
			continue
		}
		img := lookup(d, "data", "image")
		if img == nil {
			continue
		}
		edits = append(edits, Edit{Occurrence: Occurrence{
			File: countaFile, Doc: i, Line: img.Line, Col: img.Column, Style: img.Style,
			Kind: "ConfigMap", Name: scalarAt(d, "metadata", "name"), Path: "data.image",
			Raw: img.Value, Ref: mustRef(t, img.Value),
		}, New: mustRef(t, countaNew)})
	}
	if len(edits) != 1 {
		t.Fatalf("fixture has %d ConfigMap image keys, want 1; this test no longer proves anything", len(edits))
	}
	after, err := ApplyBytes(before, edits)
	if err != nil {
		t.Fatalf("ApplyBytes is structure-blind and should have rewritten the scalar: %v", err)
	}
	// Control: the byte-level half accepts the result, so the refusal below is the walk's.
	if err := verifyLines(countaFile, before, after, edits); err != nil {
		t.Fatalf("byte-level pass rejected the ConfigMap edit on its own, so this test would not isolate the walk: %v", err)
	}
	err = Verify(one(countaFile, before), one(countaFile, after), edits)
	if err == nil {
		t.Fatal("Verify accepted an edit at the ConfigMap's data.image, which discovery excludes")
	}
	if !strings.Contains(err.Error(), "not the image of a container item") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

func TestVerifyFileSetMismatch(t *testing.T) {
	before, after, edits := applied(t, webFile)
	if err := Verify(one(webFile, before), map[string][]byte{}, edits); err == nil {
		t.Error("missing after file accepted")
	}
	both := map[string][]byte{webFile: after, "extra.yaml": []byte("a: 1\n")}
	if err := Verify(one(webFile, before), both, edits); err == nil {
		t.Error("extra after file accepted")
	}
	if err := Verify(one("other.yaml", before), one("other.yaml", after), edits); err == nil {
		t.Error("edits for a file outside the set accepted")
	}
	// An unedited file in the set must still be byte-identical.
	sets := map[string][]byte{webFile: before, cronFile: readFixture(t, cronFile)}
	afterSets := map[string][]byte{webFile: after, cronFile: append(readFixture(t, cronFile), ' ')}
	if err := Verify(sets, afterSets, edits); err == nil {
		t.Error("change in an unedited file accepted")
	}
}
