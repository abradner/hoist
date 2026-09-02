package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

// singleEdit scans content, takes the occurrence at index i and plans newRef for it.
func singleEdit(t *testing.T, content string, i int, newRef string) Edit {
	t.Helper()
	occ := occurrencesOf(t, "f.yaml", content)
	if i >= len(occ) {
		t.Fatalf("only %d occurrences in %q", len(occ), content)
	}
	return Edit{Occurrence: occ[i], New: mustRef(t, newRef)}
}

func TestApplyBytesDoubleQuotedByteExact(t *testing.T) {
	before := "kind: Pod\nspec:\n  containers:\n    - name: web\n      image: \"ghcr.io/x/y:v1\"\n      ports: []\n"
	want := "kind: Pod\nspec:\n  containers:\n    - name: web\n      image: \"ghcr.io/x/y:v1@" + digestA + "\"\n      ports: []\n"
	got, err := ApplyBytes([]byte(before), []Edit{singleEdit(t, before, 0, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyBytesSingleQuotedByteExact(t *testing.T) {
	before := "spec:\n  containers:\n    - {name: web, image: 'ghcr.io/x/y:v1'}\n"
	want := "spec:\n  containers:\n    - {name: web, image: 'ghcr.io/x/y:v1@" + digestA + "'}\n"
	got, err := ApplyBytes([]byte(before), []Edit{singleEdit(t, before, 0, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyBytesInlineCommentByteExact(t *testing.T) {
	before := "spec:\n  containers:\n    - name: web\n      image: ghcr.io/x/y:v1   # pinned by hand, see runbook\n"
	want := "spec:\n  containers:\n    - name: web\n      image: ghcr.io/x/y:v1@" + digestA + "   # pinned by hand, see runbook\n"
	got, err := ApplyBytes([]byte(before), []Edit{singleEdit(t, before, 0, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Two byte-identical image lines: only the recorded one may change. A whole-file text
// replacement would rewrite the first (or both).
func TestApplyBytesIdenticalLinesOnlyRecordedOneChanges(t *testing.T) {
	line := "      image: ghcr.io/x/y:v1\n"
	before := "spec:\n  containers:\n    - name: a\n" + line + "    - name: b\n" + line
	want := "spec:\n  containers:\n    - name: a\n" + line + "    - name: b\n" + "      image: ghcr.io/x/y:v1@" + digestA + "\n"
	got, err := ApplyBytes([]byte(before), []Edit{singleEdit(t, before, 1, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyBytesTwoEditsOnOneFlowLine(t *testing.T) {
	before := "spec:\n  containers: [{name: a, image: ghcr.io/x/a:v1}, {name: b, image: ghcr.io/x/b:v1}]\n"
	want := "spec:\n  containers: [{name: a, image: ghcr.io/x/a:v1@" + digestA + "}, {name: b, image: ghcr.io/x/b:v1@" + digestB + "}]\n"
	edits := []Edit{
		singleEdit(t, before, 0, "ghcr.io/x/a:v1@"+digestA),
		singleEdit(t, before, 1, "ghcr.io/x/b:v1@"+digestB),
	}
	got, err := ApplyBytes([]byte(before), edits)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := Verify(map[string][]byte{"f.yaml": []byte(before)}, map[string][]byte{"f.yaml": got}, edits); err != nil {
		t.Errorf("Verify rejected a correct two-edit line: %v", err)
	}
}

func TestApplyBytesPreservesLineEndings(t *testing.T) {
	// No trailing newline.
	before := "spec:\n  containers:\n    - name: a\n      image: ghcr.io/x/y:v1"
	got, err := ApplyBytes([]byte(before), []Edit{singleEdit(t, before, 0, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasSuffix(got, []byte{'\n'}) || !bytes.HasSuffix(got, []byte(digestA)) {
		t.Errorf("trailing newline state changed: %q", got)
	}
	// CRLF stays CRLF.
	crlf := strings.ReplaceAll(before, "\n", "\r\n") + "\r\n"
	got, err = ApplyBytes([]byte(crlf), []Edit{singleEdit(t, crlf, 0, "ghcr.io/x/y:v1@"+digestA)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(got, []byte("\r\n")) != 4 || bytes.Contains(got, []byte(digestA+"\n\r")) {
		t.Errorf("CRLF not preserved: %q", got)
	}
}

func TestApplyBytesRefusals(t *testing.T) {
	plain := "spec:\n  containers:\n    - name: a\n      image: ghcr.io/x/y:v1\n"
	ok := singleEdit(t, plain, 0, "ghcr.io/x/y:v1@"+digestA)

	t.Run("block scalar", func(t *testing.T) {
		block := "spec:\n  containers:\n    - name: a\n      image: |\n        ghcr.io/x/y:v1\n"
		e := singleEdit(t, block, 0, "ghcr.io/x/y:v1@"+digestA)
		if e.Style != yaml.LiteralStyle {
			t.Fatalf("fixture style = %d", e.Style)
		}
		if got, err := ApplyBytes([]byte(block), []Edit{e}); err == nil {
			t.Errorf("block scalar rewritten:\n%s", got)
		}
	})
	t.Run("unpinned", func(t *testing.T) {
		e := ok
		e.New = mustRef(t, "ghcr.io/x/y:v2")
		if _, err := ApplyBytes([]byte(plain), []Edit{e}); err == nil || !strings.Contains(err.Error(), "not pinned") {
			t.Errorf("err = %v", err)
		}
	})
	// Defence in depth: an Edit built by hand (or a plan whose validation was bypassed) with a
	// digest that is not sha256:<64 lowercase hex> must be refused at the write, not written.
	t.Run("malformed digest", func(t *testing.T) {
		e := ok
		e.New = e.Ref
		e.New.Digest = "sha256:DEADBEEF"
		got, err := ApplyBytes([]byte(plain), []Edit{e})
		if err == nil || !strings.Contains(err.Error(), "not a digest") {
			t.Errorf("err = %v, want a malformed-digest refusal; got:\n%s", err, got)
		}
	})
	// Defence in depth for the tagless-digest rule: pinned, well-formed, same repo — and
	// still refused, because the written form is <repo>:<tag>@sha256:<digest>.
	t.Run("tagless digest", func(t *testing.T) {
		e := ok
		e.New = mustRef(t, "ghcr.io/x/y@"+digestA)
		got, err := ApplyBytes([]byte(plain), []Edit{e})
		if err == nil || !strings.Contains(err.Error(), "no tag") {
			t.Errorf("err = %v, want a tagless-digest refusal; got:\n%s", err, got)
		}
	})
	t.Run("different repo", func(t *testing.T) {
		e := ok
		e.New = mustRef(t, "ghcr.io/x/z:v1@"+digestA)
		if _, err := ApplyBytes([]byte(plain), []Edit{e}); err == nil {
			t.Error("repo swap accepted")
		}
	})
	t.Run("stale text", func(t *testing.T) {
		e := ok
		e.Raw = "ghcr.io/x/y:v0"
		if _, err := ApplyBytes([]byte(plain), []Edit{e}); err == nil {
			t.Error("edit applied over text that no longer matches the plan")
		}
	})
	t.Run("line out of range", func(t *testing.T) {
		e := ok
		e.Line = 99
		if _, err := ApplyBytes([]byte(plain), []Edit{e}); err == nil {
			t.Error("out-of-range line accepted")
		}
	})
	t.Run("wrong quote for style", func(t *testing.T) {
		e := ok
		e.Style = yaml.DoubleQuotedStyle
		if _, err := ApplyBytes([]byte(plain), []Edit{e}); err == nil {
			t.Error("plain scalar rewritten as if quoted")
		}
	})
}

func TestApplyWritesOnlyPlannedFiles(t *testing.T) {
	p := planFixture(t)
	tmp := copyDir(t, fixtureRoot)
	changed, err := Apply(tmp, p.Edits)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"cluster/apps/app-production/counta/app.yaml",
		"cluster/apps/app-production/counta/purge-cronjob.yaml",
		"cluster/apps/app-production/marketing/app.yaml",
		"cluster/apps/app-production/web/app.yaml",
	}
	if diff := cmp.Diff(want, changed); diff != "" {
		t.Fatalf("changed: %s", diff)
	}
	changedSet := map[string]bool{}
	for _, f := range changed {
		changedSet[f] = true
	}
	err = filepath.WalkDir(tmp, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tmp, path)
		rel = filepath.ToSlash(rel)
		got, _ := os.ReadFile(path)
		orig := readFixture(t, rel)
		if changedSet[rel] == bytes.Equal(got, orig) {
			t.Errorf("%s: changed=%v but bytes equal=%v", rel, changedSet[rel], bytes.Equal(got, orig))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Re-discover the written tree: every planned occurrence now carries the planned ref,
	// nothing else moved, and the second Apply is a no-op.
	r2, err := Discover(tmp, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Edits {
		found := false
		for _, o := range envOccurrences(r2.Envs["app-production"]) {
			if o.File == e.File && o.Path == e.Path {
				found = true
				if o.Ref != e.New || o.Style != e.Style {
					t.Errorf("%s %s: after apply %s style=%d, want %s style=%d", o.File, o.Path, o.Ref, o.Style, e.New, e.Style)
				}
			}
		}
		if !found {
			t.Errorf("%s %s vanished after apply", e.File, e.Path)
		}
	}
	p2, err := BuildPlan(r2, "app-staging", "app-production", promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p2.Edits {
		if !e.NoOp() {
			t.Errorf("second plan still wants to change %s %s: %s -> %s", e.File, e.Container, e.Ref, e.New)
		}
	}
}

func TestApplyRefusesPathEscape(t *testing.T) {
	p := planFixture(t)
	e := p.Edits[0]
	e.File = "../" + e.File
	if _, err := Apply(t.TempDir(), []Edit{e}); err == nil {
		t.Error("edit outside root accepted")
	}
}
