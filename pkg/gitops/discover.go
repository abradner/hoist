package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abradner/hoist/pkg/image"
)

// DefaultAppsRoot is where Argo Application wrappers live when the caller names nothing.
const DefaultAppsRoot = "cluster/apps"

// containerKeys are the sequence keys whose items' image: scalars are occurrences. Matching
// is by enclosing key, not depth, so Deployment, Job and CronJob jobTemplate all count.
var containerKeys = map[string]bool{
	"containers":          true,
	"initContainers":      true,
	"ephemeralContainers": true,
}

// Discover reads the Application wrappers under root/appsRoot and scans every managed family
// directory. appsRoot defaults to DefaultAppsRoot. Envs and families come only from the
// wrappers' spec.destination.namespace and spec.source.path.
func Discover(root, appsRoot string) (*Repo, error) {
	if appsRoot == "" {
		appsRoot = DefaultAppsRoot
	}
	appsRoot = path.Clean(filepath.ToSlash(appsRoot))
	if err := checkRelative(appsRoot); err != nil {
		return nil, fmt.Errorf("apps root: %w", err)
	}
	root = filepath.Clean(root)
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("repo root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("repo root %s is not a directory", root)
	}
	apps, err := readApps(root, appsRoot)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("no kind: Application found under %s/*.yaml", appsRoot)
	}
	r := &Repo{Root: root, AppsRoot: appsRoot, Envs: map[string]*Env{}, Apps: apps}
	// A family is (namespace, source path): the same directory declared twice for one
	// namespace is a duplicate wrapper. The same directory declared for two namespaces is a
	// different mistake — in this layout the family directory lives under its env directory,
	// so one directory cannot be two envs' family — and gets its own message. Both errors
	// name the wrapper files, which is what the operator has to open.
	type famKey struct{ Namespace, Path string }
	declared := map[famKey]ArgoApp{}
	byPath := map[string]ArgoApp{} // family dir → its Application; findUnmanaged's managed set
	for _, a := range apps {
		k := famKey{a.Namespace, a.SourcePath}
		if prev, ok := declared[k]; ok {
			return nil, fmt.Errorf("%s and %s both declare an Application for namespace %q at %s (%q and %q); a family is backed by exactly one Application", prev.File, a.File, a.Namespace, a.SourcePath, prev.Name, a.Name)
		}
		declared[k] = a
		if prev, ok := byPath[a.SourcePath]; ok {
			return nil, fmt.Errorf("%s (Application %q, namespace %q) and %s (Application %q, namespace %q) point at the same directory %s; a family directory belongs to one env", prev.File, prev.Name, prev.Namespace, a.File, a.Name, a.Namespace, a.SourcePath)
		}
		byPath[a.SourcePath] = a
		env := r.Envs[a.Namespace]
		if env == nil {
			env = &Env{Name: a.Namespace, Families: map[string]*Family{}}
			r.Envs[a.Namespace] = env
		}
		name := path.Base(a.SourcePath)
		if prev, ok := env.Families[name]; ok {
			return nil, fmt.Errorf("env %q: family %q is declared by both Application %q (%s) and %q (%s)", env.Name, name, prev.App, prev.Dir, a.Name, a.SourcePath)
		}
		fam := &Family{Name: name, Dir: a.SourcePath, App: a.Name}
		if err := scanFamily(root, fam); err != nil {
			return nil, fmt.Errorf("scanning Application %q: %w", a.Name, err)
		}
		env.Families[name] = fam
	}
	for _, env := range r.Envs {
		env.Dir = commonParent(env)
	}
	r.Unmanaged, err = findUnmanaged(root, appsRoot, byPath)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// checkRelative is the lexical half of staying inside the repo: after cleaning, p must be
// relative and must not climb out. Cleaning first is what catches a/../../victim.yaml, which
// begins innocently and only resolves upward once joined to a root.
func checkRelative(p string) error {
	c := path.Clean(p)
	if path.IsAbs(c) || filepath.IsAbs(filepath.FromSlash(c)) || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("%q must be a relative path inside the repo", p)
	}
	return nil
}

// ResolvePath joins rel (slash-separated, relative) to root and proves the result stays
// inside root — lexically (checkRelative) and, when the file exists, physically: a symlink
// inside the repo that points outside it is refused, so nothing hoist reads or writes through
// an Edit.File can land outside the checkout. Every place an Edit.File is joined to a root
// goes through here.
func ResolvePath(root, rel string) (string, error) {
	if err := checkRelative(rel); err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(joined)
	if errors.Is(err, fs.ErrNotExist) {
		return joined, nil // the caller's own read reports the missing file
	}
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	inside, err := filepath.Rel(realRoot, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) || filepath.IsAbs(inside) {
		return "", fmt.Errorf("%q resolves to %s, outside the repo", rel, resolved)
	}
	return joined, nil
}

func readApps(root, appsRoot string) ([]ArgoApp, error) {
	dir := filepath.Join(root, filepath.FromSlash(appsRoot))
	files, err := yamlFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("apps root %s: %w", appsRoot, err)
	}
	var apps []ArgoApp
	for _, f := range files {
		rel := path.Join(appsRoot, f)
		docs, err := parseFile(filepath.Join(dir, f))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		for i, doc := range docs {
			if scalarAt(doc, "kind") != "Application" {
				continue
			}
			a := ArgoApp{
				File:       rel,
				Name:       scalarAt(doc, "metadata", "name"),
				SourcePath: scalarAt(doc, "spec", "source", "path"),
				Namespace:  scalarAt(doc, "spec", "destination", "namespace"),
			}
			switch {
			case a.Name == "":
				return nil, fmt.Errorf("%s document %d: Application without metadata.name", rel, i)
			case a.SourcePath == "" && lookup(doc, "spec", "sources") != nil:
				return nil, fmt.Errorf("%s: Application %q uses spec.sources (multi-source), which hoist does not support", rel, a.Name)
			case a.SourcePath == "":
				return nil, fmt.Errorf("%s: Application %q has no spec.source.path", rel, a.Name)
			case a.Namespace == "":
				return nil, fmt.Errorf("%s: Application %q has no spec.destination.namespace", rel, a.Name)
			}
			a.SourcePath = path.Clean(filepath.ToSlash(a.SourcePath))
			if err := checkRelative(a.SourcePath); err != nil {
				return nil, fmt.Errorf("%s: Application %q: spec.source.path %w", rel, a.Name, err)
			}
			apps = append(apps, a)
		}
	}
	return apps, nil
}

// yamlFiles lists the *.yaml and *.yml regular files directly inside dir, sorted by name.
func yamlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".yaml" || ext == ".yml" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func parseFile(p string) ([]*yaml.Node, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return parseDocs(b)
}

// parseDocs decodes a multi-document stream into its DocumentNodes, in order.
func parseDocs(b []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, &doc)
	}
}

// lookup follows mapping keys from n (unwrapping a DocumentNode) and returns the node at
// the end of the path, or nil.
func lookup(n *yaml.Node, keys ...string) *yaml.Node {
	cur := unwrap(n)
	for _, k := range keys {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == k {
				next = cur.Content[i+1]
				break
			}
		}
		cur = next
	}
	return cur
}

func scalarAt(n *yaml.Node, keys ...string) string {
	v := lookup(n, keys...)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

func unwrap(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

func childPath(p, key string) string {
	if p == "" {
		return key
	}
	return p + "." + key
}

func indexPath(p string, i int) string { return p + "[" + strconv.Itoa(i) + "]" }

// cursor is where a walk is inside one document: the dotted path of the current node plus
// the facts that decide whether an image: scalar there is an occurrence. Discovery
// (scanDoc) and Verify (walkPair) both walk with it, so the eligibility test exists once
// (imageOfContainer) and a planned edit can only ever match a scalar discovery would have
// recorded — never, say, a ConfigMap's data.image.
type cursor struct {
	path string
	// key is the mapping key this node is the value of; "" for a key, a sequence item or the
	// root.
	key string
	// seqKey is, when this node is a sequence item, the mapping key that sequence is the
	// value of; "" otherwise.
	seqKey string
	// container is the enclosing container item when this node is a direct value of a
	// mapping that is itself an item of a containers/initContainers/ephemeralContainers
	// sequence; nil otherwise.
	container *yaml.Node
}

// child positions the cursor on parent.Content[i].
func (c cursor) child(parent *yaml.Node, i int) cursor {
	switch parent.Kind {
	case yaml.MappingNode:
		if i%2 == 0 {
			return cursor{path: c.path} // a key carries its parent's path and is never a value
		}
		key := parent.Content[i-1].Value
		next := cursor{path: childPath(c.path, key), key: key}
		if containerKeys[c.seqKey] {
			next.container = parent
		}
		return next
	case yaml.SequenceNode:
		return cursor{path: indexPath(c.path, i), seqKey: c.key}
	default:
		return cursor{path: c.path}
	}
}

// imageOfContainer reports whether the node under the cursor is the image: value of a
// container item — the one shape of scalar hoist records and rewrites.
func (c cursor) imageOfContainer() bool { return c.key == "image" && c.container != nil }

func scanFamily(root string, fam *Family) error {
	dir := filepath.Join(root, filepath.FromSlash(fam.Dir))
	files, err := yamlFiles(dir)
	if err != nil {
		return fmt.Errorf("family %q: %w", fam.Name, err)
	}
	for _, f := range files {
		rel := path.Join(fam.Dir, f)
		docs, err := parseFile(filepath.Join(dir, f))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		for i, doc := range docs {
			occ, err := scanDoc(rel, i, doc)
			if err != nil {
				return err
			}
			fam.Occurrences = append(fam.Occurrences, occ...)
		}
	}
	return nil
}

// scanDoc walks one document and records every image: scalar that is a direct field of an
// item in a containers/initContainers/ephemeralContainers sequence (cursor.imageOfContainer).
func scanDoc(file string, idx int, doc *yaml.Node) ([]Occurrence, error) {
	body := unwrap(doc)
	if body == nil {
		return nil, nil
	}
	kind := scalarAt(doc, "kind")
	name := scalarAt(doc, "metadata", "name")
	var out []Occurrence
	var walk func(n *yaml.Node, c cursor) error
	walk = func(n *yaml.Node, c cursor) error {
		if c.imageOfContainer() {
			occ, err := occurrenceAt(file, idx, kind, name, scalarAt(c.container, "name"), c.path, n)
			if err != nil {
				return err
			}
			out = append(out, occ)
			return nil
		}
		for i := range n.Content {
			if err := walk(n.Content[i], c.child(n, i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(body, cursor{}); err != nil {
		return nil, err
	}
	return out, nil
}

func occurrenceAt(file string, idx int, kind, name, container, p string, img *yaml.Node) (Occurrence, error) {
	switch img.Kind {
	case yaml.ScalarNode:
	case yaml.AliasNode:
		return Occurrence{}, fmt.Errorf("%s:%d: %s is a YAML alias; hoist edits scalars in place and cannot rewrite an alias", file, img.Line, p)
	default:
		return Occurrence{}, fmt.Errorf("%s:%d: %s is not a scalar", file, img.Line, p)
	}
	text := img.Value
	if img.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		// Block scalars carry their trailing newline in Value. Parse the reference so the
		// occurrence is visible; Apply refuses the style.
		text = strings.TrimRight(text, "\n")
	}
	ref, err := image.Parse(text)
	if err != nil {
		return Occurrence{}, fmt.Errorf("%s:%d: %s: %w", file, img.Line, p, err)
	}
	return Occurrence{
		File: file, Doc: idx, Line: img.Line, Col: img.Column, Style: img.Style,
		Kind: kind, Name: name, Container: container, Path: p, Raw: img.Value, Ref: ref,
	}, nil
}

func commonParent(env *Env) string {
	parent := ""
	for _, f := range env.Families {
		d := path.Dir(f.Dir)
		if parent == "" {
			parent = d
		} else if parent != d {
			return ""
		}
	}
	return parent
}

// findUnmanaged walks the apps root and every managed family's parent directory, reporting
// directories that hold YAML files but are the source path of no Application. That includes
// a subdirectory nested inside a managed family: scanFamily reads one level only (as Argo
// does without spec.source.directory.recurse), so manifests down there are never planned and
// the operator must be told rather than left to assume they were.
func findUnmanaged(root, appsRoot string, managed map[string]ArgoApp) ([]string, error) {
	rootSet := map[string]bool{appsRoot: true}
	for d := range managed {
		rootSet[path.Dir(d)] = true
	}
	roots := make([]string, 0, len(rootSet))
	for d := range rootSet {
		roots = append(roots, d)
	}
	sort.Strings(roots)
	var out []string
	seen := map[string]bool{}
	for _, rt := range roots {
		base := filepath.Join(root, filepath.FromSlash(rt))
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			relFS, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(relFS)
			if rel != rt && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			// A managed directory is not itself unmanaged, but its subdirectories may be:
			// keep descending rather than skipping the subtree.
			if _, isManaged := managed[rel]; isManaged || rel == rt || seen[rel] {
				return nil
			}
			files, err := yamlFiles(p)
			if err != nil {
				return err
			}
			if len(files) > 0 {
				seen[rel] = true
				out = append(out, rel)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scanning %s for unmanaged directories: %w", rt, err)
		}
	}
	sort.Strings(out)
	return out, nil
}
