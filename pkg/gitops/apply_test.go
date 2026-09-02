package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
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

// yaml.v3 also accepts blanks between the key and its colon (image : ref) and more than one
// blank after a "-" item marker; both are still one key: value pair on one line, so the
// scalar is rewritten byte-minimally like any other. Refusing them at write time — after the
// occurrence was discovered and planned — was the late failure Codex P2 named.
func TestApplyBytesKeyColonSpacingByteExact(t *testing.T) {
	for _, tc := range []struct{ before, want string }{
		{
			"spec:\n  containers:\n    - name: web\n      image : ghcr.io/x/y:v1\n",
			"spec:\n  containers:\n    - name: web\n      image : ghcr.io/x/y:v1@" + digestA + "\n",
		},
		{
			"spec:\n  containers:\n    - name: web\n      image  :\t\"ghcr.io/x/y:v1\"  # note\n",
			"spec:\n  containers:\n    - name: web\n      image  :\t\"ghcr.io/x/y:v1@" + digestA + "\"  # note\n",
		},
		{
			"spec:\n  containers:\n    -   name: web\n        image: ghcr.io/x/y:v1\n",
			"spec:\n  containers:\n    -   name: web\n        image: ghcr.io/x/y:v1@" + digestA + "\n",
		},
		{
			"spec:\n  containers:\n    -   image: ghcr.io/x/y:v1\n        name: web\n",
			"spec:\n  containers:\n    -   image: ghcr.io/x/y:v1@" + digestA + "\n        name: web\n",
		},
	} {
		got, err := ApplyBytes([]byte(tc.before), []Edit{singleEdit(t, tc.before, 0, "ghcr.io/x/y:v1@"+digestA)})
		if err != nil {
			t.Errorf("%q: %v", tc.before, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
		}
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

// An Edit whose Col and Raw name a suffix of the scalar passes the text match on its own:
// the bytes at Col really are the planned Raw. ApplyBytes must still refuse it, or the write
// keeps whatever preceded Col and produces image: junk@ghcr.io/x/y:v2@sha256:….
func TestApplyBytesRefusesColumnInsideScalar(t *testing.T) {
	before := "spec:\n  containers:\n    - name: a\n      image: junk@ghcr.io/x/y:v1\n"
	line := "      image: junk@ghcr.io/x/y:v1"
	e := Edit{Occurrence: Occurrence{
		File: "f.yaml", Line: 4, Col: strings.Index(line, "ghcr.io") + 1,
		Path: "spec.containers[0].image", Raw: "ghcr.io/x/y:v1", Ref: mustRef(t, "ghcr.io/x/y:v1"),
	}, New: mustRef(t, "ghcr.io/x/y:v2@"+digestA)}
	got, err := ApplyBytes([]byte(before), []Edit{e})
	if err == nil {
		t.Fatalf("edit inside the scalar applied:\n%s", got)
	}
	if !strings.Contains(err.Error(), `not the start of a "image: value" pair`) {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	// Positive control: the same edit with Col at the scalar's start is applied.
	ok := "spec:\n  containers:\n    - name: a\n      image: ghcr.io/x/y:v1\n"
	if _, err := ApplyBytes([]byte(ok), []Edit{singleEdit(t, ok, 0, "ghcr.io/x/y:v2@"+digestA)}); err != nil {
		t.Errorf("edit at the scalar's start refused: %v", err)
	}
	// And the key is taken from the path, so a path that names another key is refused too.
	wrongKey := singleEdit(t, ok, 0, "ghcr.io/x/y:v2@"+digestA)
	wrongKey.Path = "spec.containers[0].imag"
	if _, err := ApplyBytes([]byte(ok), []Edit{wrongKey}); err == nil {
		t.Error("edit whose path names a different key than the line applied")
	}
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

// TestApplyRefusesPathEscape names the attacker: an Edit whose File leaves the repo. The
// victim is a real, applicable copy of the edited fixture file placed one level above the
// root, so a containment check that only looked at the first path element would read it,
// verify it and write it.
func TestApplyRefusesPathEscape(t *testing.T) {
	p := planFixture(t)
	e := p.Edits[0]
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(parent, "victim.yaml")
	orig := readFixture(t, e.File)
	if err := os.WriteFile(victim, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.yaml")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	// Positive control: the same edit against the victim's bytes really does apply, so an
	// accepted escape below would have written.
	if _, err := ApplyBytes(orig, []Edit{e}); err != nil {
		t.Fatalf("control: edit does not apply to the victim file: %v", err)
	}
	for _, file := range []string{"../victim.yaml", "a/../../victim.yaml", "link.yaml", "/" + victim} {
		esc := e
		esc.File = file
		changed, err := Apply(root, []Edit{esc})
		if err == nil {
			t.Errorf("%s: edit outside root accepted (changed %v)", file, changed)
		}
		got, readErr := os.ReadFile(victim)
		if readErr != nil || !bytes.Equal(got, orig) {
			t.Fatalf("%s: victim file was modified (read err %v)", file, readErr)
		}
	}
}

func TestResolvePath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dir/in.yaml", "top.yaml"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(parent, "out.yaml"), []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "out.yaml"), filepath.Join(root, "dir", "escape.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "top.yaml"), filepath.Join(root, "dir", "stay.yaml")); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"dir/in.yaml", "top.yaml", "dir/../top.yaml", "dir/stay.yaml", "dir/missing.yaml"} {
		if _, err := ResolvePath(root, ok); err != nil {
			t.Errorf("%s: refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"../out.yaml", "dir/../../out.yaml", "dir/escape.yaml", ".", "..", "/etc/passwd", filepath.Join(parent, "out.yaml")} {
		if p, err := ResolvePath(root, bad); err == nil {
			t.Errorf("%s: accepted as %s", bad, p)
		}
	}
}

// ResolvePath hands back the path it checked, symlinks resolved, so the caller's open does
// not follow the link a second time; a target that does not exist yet gets its deepest
// existing ancestor resolved and the missing tail appended. Discover keeps recording the
// repo-relative link path in Occurrence.File (TestDiscoverFollowsSymlinkInsideRepo) — that
// is the file the operator committed; this is the file the process opens.
func TestResolvePathReturnsResolvedPath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dir/in.yaml", "top.yaml"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"dir/stay.yaml": filepath.Join(root, "top.yaml"),
		"linkdir":       filepath.Join(root, "dir"),
		"escapedir":     parent,
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	// t.TempDir is itself under a symlink on macOS (/var -> /private/var), so the expected
	// paths are built on the resolved root, not root.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"dir/in.yaml":          "dir/in.yaml",
		"dir/stay.yaml":        "top.yaml",
		"linkdir/in.yaml":      "dir/in.yaml",
		"dir/missing.yaml":     "dir/missing.yaml",
		"linkdir/missing.yaml": "dir/missing.yaml",
		"a/b/missing.yaml":     "a/b/missing.yaml",
	} {
		got, err := ResolvePath(root, rel)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if want := filepath.Join(realRoot, filepath.FromSlash(want)); got != want {
			t.Errorf("%s: resolved to %s, want %s", rel, got, want)
		}
	}
	// A file that does not exist yet under a directory link that leaves the repo is refused:
	// its parent is what gets resolved, and that parent is outside.
	if p, err := ResolvePath(root, "escapedir/new.yaml"); err == nil {
		t.Errorf("escapedir/new.yaml: accepted as %s", p)
	}
}

// keyBeforeRegex is the regex keyBefore replaced, kept here as the oracle: the byte scanner
// must accept and reject exactly the prefixes this pattern does. Compiled once per key only
// so the exhaustive table below runs in a second, not twenty.
var keyBeforeOracles = map[string]*regexp.Regexp{}

func keyBeforeRegex(prefix []byte, key string) bool {
	re := keyBeforeOracles[key]
	if re == nil {
		re = regexp.MustCompile(`(?:^[ \t]*(?:-[ \t]+)?|[,{\[][ \t]*)` + regexp.QuoteMeta(key) + `[ \t]*:[ \t]+$`)
		keyBeforeOracles[key] = re
	}
	return re.Match(prefix)
}

// TestKeyBeforeMatchesRegex is the proof for the scanner: a named table of the shapes the
// grammar is about, then every prefix built from a small alphabet on each side of the key,
// all compared against the regex. Both accepted and rejected counts are asserted non-zero so
// an oracle that answered one thing for everything could not pass.
func TestKeyBeforeMatchesRegex(t *testing.T) {
	named := []string{
		"", " ", "image", "image:", "image: ", "image:\t", "  image: ", "\timage:  ", "image : ", "image\t:\t",
		"- image: ", "-   image: ", "-\timage: ", "  - image: ", "-image: ", "- - image: ", "-- image: ",
		"{image: ", "{ image: ", "[image: ", "[ image: ", "a: {b: 1, image: ", "x,image: ", ", - image: ", "[ - image: ",
		"ximage: ", "images: ", "imag: ", "image: ghcr.io/x/y:v1 ", "name: a image: ", "  image:x ", "- image :",
		"# image: ", "image:: ", "image: image: ", "\"image\": ", "'image': ", "image\r: ", "\rimage: ", "image: \r",
		"- {image: ", "-, image: ", "- [ image: ", "[[image: ", "{,image: ", " \t image \t : \t ",
	}
	var accepted, rejected int
	check := func(prefix, key string) {
		got, want := keyBefore([]byte(prefix), key), keyBeforeRegex([]byte(prefix), key)
		if got != want {
			t.Errorf("key %q prefix %q: keyBefore = %v, regex = %v", key, prefix, got, want)
		}
		if want {
			accepted++
		} else {
			rejected++
		}
	}
	for _, p := range named {
		check(p, "image")
	}
	check("image: ", "images")
	check("images: ", "image")
	check("a.b: ", "a.b")
	check("axb: ", "a.b") // QuoteMeta: the key is literal
	// Exhaustive: lead (before the key) up to 3 bytes, mid (key..colon) and tail (after the
	// colon) up to 2 bytes each, over the bytes the grammar distinguishes plus one it does not.
	lead := []byte{' ', '\t', '-', ',', '{', '[', 'x'}
	side := []byte{' ', '\t', 'x'}
	var leads, sides []string
	for n := 0; n <= 3; n++ {
		leads = append(leads, combos(lead, n)...)
	}
	for n := 0; n <= 2; n++ {
		sides = append(sides, combos(side, n)...)
	}
	for _, l := range leads {
		for _, m := range sides {
			for _, r := range sides {
				check(l+"image"+m+":"+r, "image")
			}
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("table is one-sided: %d accepted, %d rejected", accepted, rejected)
	}
	t.Logf("%d accepted, %d rejected, identical to the regex", accepted, rejected)
}

// combos returns every string of length n over alphabet, in order.
func combos(alphabet []byte, n int) []string {
	if n == 0 {
		return []string{""}
	}
	var out []string
	for _, s := range combos(alphabet, n-1) {
		for _, b := range alphabet {
			out = append(out, s+string(b))
		}
	}
	return out
}

func BenchmarkCheckScalarStart(b *testing.B) {
	line := []byte(`          image: "ghcr.io/example/web:v1"`)
	o := &Occurrence{Path: "spec.template.spec.containers[0].image", Col: 18}
	for i := 0; i < b.N; i++ {
		if _, err := checkScalarStart(line, o); err != nil {
			b.Fatal(err)
		}
	}
}
