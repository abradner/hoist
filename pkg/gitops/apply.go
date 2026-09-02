package gitops

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// plainSafe is what may be written as a plain (unquoted) scalar without changing meaning.
// Image references never need more; anything else is refused rather than quoted.
var plainSafe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]*$`)

// Apply rewrites the files named by edits under root, one ApplyBytes + Verify per file, and
// returns the files whose bytes changed. A file is written only after Verify accepts it; a
// failure on any file stops before that file is written (earlier files stay written — the
// caller's git worktree is the unit of rollback).
func Apply(root string, edits []Edit) (changed []string, err error) {
	byFile := map[string][]Edit{}
	var files []string
	for _, e := range edits {
		if err := checkRelative(e.File); err != nil {
			return nil, fmt.Errorf("edit file: %w", err)
		}
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		info, err := os.Stat(p)
		if err != nil {
			return changed, err
		}
		before, err := os.ReadFile(p)
		if err != nil {
			return changed, err
		}
		after, err := ApplyBytes(before, byFile[f])
		if err != nil {
			return changed, err
		}
		if err := Verify(map[string][]byte{f: before}, map[string][]byte{f: after}, byFile[f]); err != nil {
			return changed, err
		}
		if bytes.Equal(before, after) {
			continue
		}
		// Keep the file's own mode rather than inheriting the umask (AGENTS.md §8).
		if err := os.WriteFile(p, after, info.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// ApplyBytes returns before with each edit's scalar replaced at its recorded line and
// column. It is pure: nothing but the scalar text changes, and the result has the same line
// count, quoting, comments and trailing newline as the input. Edits on one line are applied
// right to left so flow-style items keep their columns valid.
func ApplyBytes(before []byte, edits []Edit) ([]byte, error) {
	lines := bytes.Split(before, []byte{'\n'})
	sorted := make([]*Edit, len(edits))
	for i := range edits {
		sorted[i] = &edits[i]
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Line != sorted[j].Line {
			return sorted[i].Line > sorted[j].Line
		}
		return sorted[i].Col > sorted[j].Col
	})
	for _, e := range sorted {
		if e.Line < 1 || e.Line > len(lines) {
			return nil, fmt.Errorf("%s: line %d is outside the file (%d lines)", e.File, e.Line, len(lines))
		}
		nl, err := replaceInLine(lines[e.Line-1], e)
		if err != nil {
			return nil, err
		}
		lines[e.Line-1] = nl
	}
	return bytes.Join(lines, []byte{'\n'}), nil
}

// checkEditable is the one place that decides whether an edit may be written: well-formed
// reference (image.Ref.Validate), pinned, tagged, same repo, and a scalar style that can be
// rewritten in place. BuildPlan calls it early for a friendlier failure; ApplyBytes calls it before
// touching bytes.
func checkEditable(e *Edit) error {
	where := fmt.Sprintf("%s:%d:%d", e.File, e.Line, e.Col)
	if err := e.New.Validate(); err != nil {
		return fmt.Errorf("%s: refusing to write a malformed reference: %w", where, err)
	}
	if !e.New.Pinned() {
		return fmt.Errorf("%s: refusing to write %s: not pinned to a digest (AGENTS.md §4.2)", where, e.New)
	}
	if e.New.Tag == "" {
		return fmt.Errorf("%s: refusing to write %s: digest with no tag; the written form is <repo>:<tag>@sha256:<digest>", where, e.New)
	}
	if e.New.Repo != e.Ref.Repo {
		return fmt.Errorf("%s: refusing to replace %s with a different repo %s", where, e.Ref.Repo, e.New.Repo)
	}
	switch e.Style {
	case 0, yaml.DoubleQuotedStyle, yaml.SingleQuotedStyle:
		return nil
	case yaml.LiteralStyle, yaml.FoldedStyle:
		return fmt.Errorf("%s: image is a block scalar (| or >); hoist refuses to rewrite it", where)
	default:
		return fmt.Errorf("%s: unsupported scalar style %d", where, e.Style)
	}
}

// replaceInLine swaps e.Raw for e.New.String() at e.Col on one line, checking the
// surrounding bytes (quotes, terminator) so a stale or shifted plan is refused.
func replaceInLine(line []byte, e *Edit) ([]byte, error) {
	if err := checkEditable(e); err != nil {
		return nil, err
	}
	where := fmt.Sprintf("%s:%d:%d", e.File, e.Line, e.Col)
	off, ok := byteOffset(line, e.Col)
	if !ok {
		return nil, fmt.Errorf("%s: column is beyond the end of the line", where)
	}
	var quote byte
	switch e.Style {
	case yaml.DoubleQuotedStyle:
		quote = '"'
	case yaml.SingleQuotedStyle:
		quote = '\''
	}
	start := off
	if quote != 0 {
		if off >= len(line) || line[off] != quote {
			return nil, fmt.Errorf("%s: expected opening %c at the recorded column", where, quote)
		}
		start++
	}
	raw := []byte(e.Raw)
	if !bytes.HasPrefix(line[start:], raw) {
		return nil, fmt.Errorf("%s: text there is %q, not the planned %q; the file changed after the plan was built", where, truncate(line[start:]), e.Raw)
	}
	end := start + len(raw)
	if quote != 0 {
		if end >= len(line) || line[end] != quote {
			return nil, fmt.Errorf("%s: expected closing %c after the scalar", where, quote)
		}
	} else if end < len(line) && !plainTerminator(line[end]) {
		return nil, fmt.Errorf("%s: scalar does not end where the plan says", where)
	}
	repl := e.New.String()
	switch quote {
	case 0:
		if !plainSafe.MatchString(repl) {
			return nil, fmt.Errorf("%s: %q cannot be written as a plain scalar", where, repl)
		}
	case '"':
		if strings.ContainsAny(repl, `"\`) {
			return nil, fmt.Errorf("%s: %q cannot be written inside double quotes", where, repl)
		}
	case '\'':
		if strings.ContainsRune(repl, '\'') {
			return nil, fmt.Errorf("%s: %q cannot be written inside single quotes", where, repl)
		}
	}
	out := make([]byte, 0, len(line)-len(raw)+len(repl))
	out = append(out, line[:start]...)
	out = append(out, repl...)
	out = append(out, line[end:]...)
	return out, nil
}

// byteOffset converts yaml.v3's 1-based character column into a byte offset.
func byteOffset(line []byte, col int) (int, bool) {
	n, off := 1, 0
	for n < col {
		if off >= len(line) {
			return 0, false
		}
		_, sz := utf8.DecodeRune(line[off:])
		off += sz
		n++
	}
	return off, true
}

func plainTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\r', ',', ']', '}':
		return true
	}
	return false
}

func truncate(b []byte) string {
	const n = 80
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
