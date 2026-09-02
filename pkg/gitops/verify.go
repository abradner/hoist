package gitops

import (
	"bytes"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// Verify checks that after differs from before only by the planned edits. Both maps are
// keyed by file path and must hold the same files. For each file it requires an unchanged
// line count, byte-identical unedited lines, edited lines equal to the reconstruction of the
// planned replacement, and yaml.v3 node trees that match in lockstep — kind, tag, style,
// anchors, comments (head, line and foot), line numbers and children — except at the planned
// scalars, each of which must be matched exactly once.
func Verify(before, after map[string][]byte, edits []Edit) error {
	byFile := map[string][]Edit{}
	for _, e := range edits {
		if _, ok := before[e.File]; !ok {
			return fmt.Errorf("edit targets %s, which is not among the verified files", e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	files := make([]string, 0, len(before))
	for f := range before {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		a, ok := after[f]
		if !ok {
			return fmt.Errorf("%s: missing from the after set", f)
		}
		if err := verifyLines(f, before[f], a, byFile[f]); err != nil {
			return err
		}
		if err := verifyStructure(f, before[f], a, byFile[f]); err != nil {
			return err
		}
	}
	for f := range after {
		if _, ok := before[f]; !ok {
			return fmt.Errorf("%s: present in the after set but not in before", f)
		}
	}
	return nil
}

// verifyLines is the byte-level half: line count, unedited lines identical, edited lines
// exactly the planned replacement. It catches what a node walk cannot see — trailing
// whitespace, a changed comment spelling that yaml.v3 normalises, CRLF flips.
func verifyLines(file string, before, after []byte, edits []Edit) error {
	bl := bytes.Split(before, []byte{'\n'})
	al := bytes.Split(after, []byte{'\n'})
	if len(bl) != len(al) {
		return fmt.Errorf("%s: line count changed from %d to %d", file, len(bl), len(al))
	}
	byLine := map[int][]*Edit{}
	for i := range edits {
		byLine[edits[i].Line] = append(byLine[edits[i].Line], &edits[i])
	}
	for i := range bl {
		ln := i + 1
		es := byLine[ln]
		if len(es) == 0 {
			if !bytes.Equal(bl[i], al[i]) {
				return fmt.Errorf("%s:%d: line changed but no edit was planned there: %q -> %q", file, ln, bl[i], al[i])
			}
			continue
		}
		sort.SliceStable(es, func(x, y int) bool { return es[x].Col > es[y].Col })
		want := bl[i]
		for _, e := range es {
			var err error
			if want, err = replaceInLine(want, e); err != nil {
				return err
			}
		}
		if !bytes.Equal(want, al[i]) {
			return fmt.Errorf("%s:%d: edited line is %q, expected %q", file, ln, al[i], want)
		}
	}
	return nil
}

type editKey struct{ line, col int }

// verifyStructure is the node-tree half: re-parse both versions and walk them in lockstep.
func verifyStructure(file string, before, after []byte, edits []Edit) error {
	bd, err := parseDocs(before)
	if err != nil {
		return fmt.Errorf("%s: before does not parse: %w", file, err)
	}
	ad, err := parseDocs(after)
	if err != nil {
		return fmt.Errorf("%s: after does not parse: %w", file, err)
	}
	if len(bd) != len(ad) {
		return fmt.Errorf("%s: document count changed from %d to %d", file, len(bd), len(ad))
	}
	want := map[editKey]*Edit{}
	for i := range edits {
		k := editKey{edits[i].Line, edits[i].Col}
		if _, dup := want[k]; dup {
			return fmt.Errorf("%s:%d:%d: two edits planned at one position", file, k.line, k.col)
		}
		want[k] = &edits[i]
	}
	matched := map[editKey]bool{}
	for i := range bd {
		if err := walkPair(file, bd[i], ad[i], "", want, matched); err != nil {
			return err
		}
	}
	for k, e := range want {
		if !matched[k] {
			return fmt.Errorf("%s:%d:%d: planned edit at %s was not found in the re-parsed result", file, k.line, k.col, e.Path)
		}
	}
	return nil
}

func walkPair(file string, b, a *yaml.Node, p string, want map[editKey]*Edit, matched map[editKey]bool) error {
	where := fmt.Sprintf("%s:%d %s", file, b.Line, pathLabel(p))
	switch {
	case b.Kind != a.Kind:
		return fmt.Errorf("%s: node kind changed", where)
	case b.Tag != a.Tag:
		return fmt.Errorf("%s: tag changed from %s to %s", where, b.Tag, a.Tag)
	case b.Style != a.Style:
		return fmt.Errorf("%s: scalar style changed", where)
	case b.Anchor != a.Anchor:
		return fmt.Errorf("%s: anchor changed", where)
	case b.HeadComment != a.HeadComment:
		return fmt.Errorf("%s: head comment changed: %q -> %q", where, b.HeadComment, a.HeadComment)
	case b.LineComment != a.LineComment:
		return fmt.Errorf("%s: line comment changed: %q -> %q", where, b.LineComment, a.LineComment)
	case b.FootComment != a.FootComment:
		return fmt.Errorf("%s: foot comment changed: %q -> %q", where, b.FootComment, a.FootComment)
	case b.Line != a.Line:
		return fmt.Errorf("%s: node moved to line %d", where, a.Line)
	case len(b.Content) != len(a.Content):
		return fmt.Errorf("%s: child count changed from %d to %d", where, len(b.Content), len(a.Content))
	}
	if b.Kind == yaml.ScalarNode || b.Kind == yaml.AliasNode {
		k := editKey{b.Line, b.Column}
		e := want[k]
		switch {
		case e == nil && b.Value != a.Value:
			return fmt.Errorf("%s: unplanned scalar change: %q -> %q", where, b.Value, a.Value)
		case e != nil && b.Kind != yaml.ScalarNode:
			return fmt.Errorf("%s: planned edit sits on an alias", where)
		case e != nil && p != e.Path:
			return fmt.Errorf("%s: planned edit is for path %s but the scalar at that position is at %s", where, e.Path, p)
		case e != nil && b.Value != e.Raw:
			return fmt.Errorf("%s: scalar before the edit is %q, plan expected %q", where, b.Value, e.Raw)
		case e != nil && a.Value != e.New.String():
			return fmt.Errorf("%s: scalar after the edit is %q, plan expected %q", where, a.Value, e.New.String())
		case e != nil:
			matched[k] = true
		}
	}
	for i := range b.Content {
		if err := walkPair(file, b.Content[i], a.Content[i], childPathOf(b, p, i), want, matched); err != nil {
			return err
		}
	}
	return nil
}

// childPathOf mirrors the path construction in scanDoc so planned Paths line up.
func childPathOf(parent *yaml.Node, p string, i int) string {
	switch parent.Kind {
	case yaml.MappingNode:
		if i%2 == 0 {
			return p // a key carries its parent's path
		}
		return childPath(p, parent.Content[i-1].Value)
	case yaml.SequenceNode:
		return indexPath(p, i)
	default:
		return p
	}
}

func pathLabel(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}
