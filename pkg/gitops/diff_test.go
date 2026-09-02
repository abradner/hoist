package gitops

import (
	"fmt"
	"strings"
	"testing"
)

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", i+1)
	}
	return out
}

func joinNL(lines []string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }

func countPrefix(s, prefix string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) && !strings.HasPrefix(l, prefix+prefix+prefix[:1]) {
			n++
		}
	}
	return n
}

func TestUnifiedDiffSingleChangedLine(t *testing.T) {
	before := numbered(6)
	after := numbered(6)
	after[2] = "line 3 changed"
	d := UnifiedDiff("dir/f.yaml", joinNL(before), joinNL(after))
	if !strings.HasPrefix(d, "--- a/dir/f.yaml\n+++ b/dir/f.yaml\n@@ -1,6 +1,6 @@\n") {
		t.Errorf("header:\n%s", d)
	}
	if countPrefix(d, "-") != 1 || countPrefix(d, "+") != 1 {
		t.Errorf("want exactly one -/+ pair:\n%s", d)
	}
	if !strings.Contains(d, "-line 3\n+line 3 changed\n") {
		t.Errorf("replacement not adjacent:\n%s", d)
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	if d := UnifiedDiff("f", joinNL(numbered(3)), joinNL(numbered(3))); d != "" {
		t.Errorf("identical inputs produced a diff:\n%s", d)
	}
}

func TestUnifiedDiffHunks(t *testing.T) {
	before := numbered(20)
	far := numbered(20)
	far[1], far[17] = "x", "y"
	d := UnifiedDiff("f", joinNL(before), joinNL(far))
	if n := strings.Count(d, "@@ -"); n != 2 {
		t.Errorf("far-apart changes: %d hunks, want 2:\n%s", n, d)
	}
	if !strings.Contains(d, "@@ -1,5 +1,5 @@\n") || !strings.Contains(d, "@@ -15,6 +15,6 @@\n") {
		t.Errorf("hunk ranges:\n%s", d)
	}
	near := numbered(20)
	near[4], near[8] = "x", "y"
	d = UnifiedDiff("f", joinNL(before), joinNL(near))
	if n := strings.Count(d, "@@ -"); n != 1 {
		t.Errorf("nearby changes: %d hunks, want 1:\n%s", n, d)
	}
	if !strings.Contains(d, "@@ -2,11 +2,11 @@\n") {
		t.Errorf("merged hunk range:\n%s", d)
	}
}

func TestUnifiedDiffNoTrailingNewline(t *testing.T) {
	before := []byte("a\nb")
	after := []byte("a\nc")
	d := UnifiedDiff("f", before, after)
	want := "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n"
	if d != want {
		t.Errorf("got:\n%s\nwant:\n%s", d, want)
	}
}

func TestUnifiedDiffLineCountFallback(t *testing.T) {
	d := UnifiedDiff("f", joinNL(numbered(2)), joinNL(numbered(3)))
	if !strings.Contains(d, "@@ -1,2 +1,3 @@\n-line 1\n-line 2\n+line 1\n+line 2\n+line 3\n") {
		t.Errorf("fallback hunk:\n%s", d)
	}
}
