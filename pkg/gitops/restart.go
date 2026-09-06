package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RestartAnnotation is the pod-template annotation whose value hoist rewrites to trigger a
// restart. It is deliberately kubectl's own key rather than a hoist-specific one: `kubectl
// rollout restart` writes exactly this, so an operator reading the manifest, or running
// `kubectl` against the cluster afterwards, sees the field they already know. Nothing in
// Kubernetes treats the key specially — any change to the pod template starts a rollout — so
// sharing it costs nothing and buys recognition.
const RestartAnnotation = "kubectl.kubernetes.io/restartedAt"

// Restart-plan refusals. Each names a manifest shape hoist will not edit rather than guess at
// (the same stance apply.go takes for a block scalar): a restart is a write to someone's
// production manifest, and a wrong guess about indentation or nesting corrupts it.
const (
	// WarnRestartNoPodTemplate: the document has no spec.template.metadata block map to hang
	// an annotation on. Reported rather than fixed — inventing a metadata block means guessing
	// the file's indentation step with nothing to derive it from.
	WarnRestartNoPodTemplate = "restart-no-pod-template"
	// WarnRestartFlowMapping: spec.template.metadata (or its annotations) is a flow mapping
	// ({a: b}), which cannot take a block-style child line.
	WarnRestartFlowMapping = "restart-flow-mapping"
)

// RestartEdit is one planned write of the restart annotation into one document's pod template.
//
// It is deliberately NOT an Edit. Edit is a scalar replacement within a line — apply.go rewrites
// lines[Line-1] in place and verify.go fails outright if the line count changes — and setting an
// annotation that is not already there means adding lines. Rather than widen that invariant,
// which is load-bearing for every promotion hoist has ever made, this is a second, narrower kind
// with its own apply and its own verification (AGENTS.md §4.2).
type RestartEdit struct {
	// File is the manifest path relative to the repo root, slash-separated.
	File string
	// Doc is the 0-based index of the document within File.
	Doc int
	// Kind and Name identify the enclosing document, for reporting.
	Kind, Name string
	// Line is 1-based and file-absolute: the line this write anchors to.
	Line int
	// Replace says how Line is used. True: lines[Line-1] is rewritten (the annotation is
	// already present, so this is an ordinary in-place scalar change and the line count does
	// not move). False: Lines are inserted directly after lines[Line-1].
	Replace bool
	// Lines are the exact, fully-indented lines to write — one when Replace, one or two when
	// inserting (the annotation alone, or an `annotations:` block introducing it).
	Lines []string
	// Old is the timestamp already there, "" when the annotation is new. Reported so a diff
	// can show what is being superseded.
	Old string
	// New is the timestamp being written.
	New string
}

// BuildRestartPlan plans a restart of every Deployment in env, or only those in the named
// families when families is non-empty.
//
// Only Deployments are restarted. A Job or CronJob has no rollout to trigger — re-running one
// is a different operation with different consequences — so they are reported in Untouched and
// left alone, the same split RolledOutStep already makes when it watches a promotion land.
//
// at is the timestamp written. It is a parameter rather than time.Now() because it is the whole
// content of the change: two restarts of the same family must be distinguishable, and a caller
// that cannot control it cannot write a test.
func BuildRestartPlan(r *Repo, env string, families []string, at time.Time) (Plan, error) {
	// Same refusal, same wording as BuildPlan and BuildDeployPlan: a public plan builder
	// handed no repo returns an error, it does not panic (Copilot, PR #78).
	if r == nil {
		return Plan{}, errors.New("nil repo")
	}
	e, ok := r.Envs[env]
	if !ok {
		return Plan{}, fmt.Errorf("unknown env %q", env)
	}
	only := map[string]bool{}
	for _, f := range families {
		only[f] = true
	}
	for f := range only {
		if _, ok := e.Families[f]; !ok {
			return Plan{}, fmt.Errorf("%s has no family %q", env, f)
		}
	}

	plan := Plan{
		Variant:     VariantRestart,
		TargetEnv:   env,
		GeneratedAt: time.Now().UTC(),
	}
	// Quoted, always. An RFC3339 timestamp written as a plain scalar is resolved by YAML as a
	// !!timestamp, and a Kubernetes annotation value must be a string — the API server rejects
	// the object outright. Quoting keeps it a string in every parser that reads this file.
	stamp := at.UTC().Format(time.RFC3339)

	names := make([]string, 0, len(e.Families))
	for name := range e.Families {
		if len(only) == 0 || only[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		fam := e.Families[name]
		files, err := yamlFilesIn(r.Root, fam.Dir)
		if err != nil {
			return Plan{}, err
		}
		for _, f := range files {
			// yamlFilesIn returns base names; the family's own dir makes them repo-relative,
			// exactly as scanFamily does.
			rel := path.Join(fam.Dir, f)
			docs, err := parseFile(r.Root, rel)
			if err != nil {
				return Plan{}, err
			}
			for i, doc := range docs {
				re, warn, ok := planRestartForDoc(rel, i, doc, stamp)
				if warn != nil {
					plan.Warnings = append(plan.Warnings, *warn)
					continue
				}
				if !ok {
					continue
				}
				plan.Restarts = append(plan.Restarts, re)
			}
		}
	}
	if len(plan.Restarts) == 0 {
		return Plan{}, fmt.Errorf("%s has no Deployment to restart%s", env, onlyLabel(families))
	}
	sortRestarts(plan.Restarts)
	// RFC3339 has second resolution, so a Deployment already carrying this exact stamp would be
	// planned a byte-identical replacement: nothing changes, the pod template does not move, and
	// nothing rolls — while the command reports a restart (Copilot, PR #78). It happens for real
	// when two invocations land in the same second, or a restart is re-run immediately.
	//
	// Advancing by a second and replanning is the fix rather than switching to sub-second
	// precision, which would write a stamp unlike the one kubectl writes for no gain: the
	// operation being timestamped is "now", and one second later is still an honest answer.
	for _, re := range plan.Restarts {
		if re.Old == re.New {
			return BuildRestartPlan(r, env, families, at.Add(time.Second))
		}
	}
	return plan, nil
}

func onlyLabel(families []string) string {
	if len(families) == 0 {
		return ""
	}
	return " in " + strings.Join(families, ", ")
}

func sortRestarts(rs []RestartEdit) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].File != rs[j].File {
			return rs[i].File < rs[j].File
		}
		return rs[i].Line < rs[j].Line
	})
}

// planRestartForDoc decides where one document's restart annotation goes. ok is false for a
// document that is simply not a Deployment; warn is non-nil for one that is but whose shape
// hoist refuses to edit.
func planRestartForDoc(file string, idx int, doc *yaml.Node, stamp string) (re RestartEdit, warn *Warning, ok bool) {
	root := unwrap(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return RestartEdit{}, nil, false
	}
	if scalarAt(root, "kind") != "Deployment" {
		return RestartEdit{}, nil, false
	}
	name := scalarAt(root, "metadata", "name")
	base := RestartEdit{File: file, Doc: idx, Kind: "Deployment", Name: name, New: stamp}
	where := fmt.Sprintf("%s:%d (Deployment %s)", file, idx, name)

	meta := lookup(root, "spec", "template", "metadata")
	if meta == nil || meta.Kind != yaml.MappingNode {
		return RestartEdit{}, &Warning{
			Code: WarnRestartNoPodTemplate,
			Message: fmt.Sprintf("%s: no spec.template.metadata block to hold %s; add one (a labels block is the usual place) and re-run",
				where, RestartAnnotation),
		}, false
	}
	if meta.Style&yaml.FlowStyle != 0 {
		return RestartEdit{}, &Warning{
			Code:    WarnRestartFlowMapping,
			Message: fmt.Sprintf("%s: spec.template.metadata is a flow mapping ({...}); hoist will not rewrite it into block style", where),
		}, false
	}
	if len(meta.Content) < 2 {
		return RestartEdit{}, &Warning{
			Code:    WarnRestartNoPodTemplate,
			Message: fmt.Sprintf("%s: spec.template.metadata is empty, so there is nothing to derive this file's indentation from", where),
		}, false
	}
	// Indentation is derived from the file itself, never assumed: metadata's own first key
	// tells us how deep its children sit, and yaml.v3's Column is 1-based.
	childIndent := strings.Repeat(" ", meta.Content[0].Column-1)

	ann := lookup(meta, "annotations")
	if ann == nil {
		// No annotations block: introduce one as metadata's first child, directly after the
		// `metadata:` key line. Its own children go one step deeper, and the step is the
		// difference this file already uses between metadata and its children.
		metaKey := mappingKeyNode(root, "spec", "template", "metadata")
		step := meta.Content[0].Column - metaKey.Column
		if step < 1 {
			return RestartEdit{}, &Warning{
				Code:    WarnRestartFlowMapping,
				Message: fmt.Sprintf("%s: cannot derive the indentation step under spec.template.metadata", where),
			}, false
		}
		deeper := childIndent + strings.Repeat(" ", step)
		base.Line = metaKey.Line
		base.Lines = []string{
			childIndent + "annotations:",
			deeper + RestartAnnotation + `: "` + stamp + `"`,
		}
		return base, nil, true
	}
	if ann.Kind != yaml.MappingNode || ann.Style&yaml.FlowStyle != 0 {
		return RestartEdit{}, &Warning{
			Code:    WarnRestartFlowMapping,
			Message: fmt.Sprintf("%s: spec.template.metadata.annotations is not a block mapping; hoist will not rewrite it", where),
		}, false
	}
	for i := 0; i+1 < len(ann.Content); i += 2 {
		if ann.Content[i].Value != RestartAnnotation {
			continue
		}
		// Already present: this is an ordinary in-place scalar change, so the line count does
		// not move at all and the whole insertion question falls away.
		val := ann.Content[i+1]
		if val.Kind != yaml.ScalarNode || val.Line != ann.Content[i].Line {
			return RestartEdit{}, &Warning{
				Code:    WarnRestartFlowMapping,
				Message: fmt.Sprintf("%s: the existing %s is not a plain scalar on its own line", where, RestartAnnotation),
			}, false
		}
		indent := strings.Repeat(" ", ann.Content[i].Column-1)
		base.Line = ann.Content[i].Line
		base.Replace = true
		base.Old = val.Value
		base.Lines = []string{indent + RestartAnnotation + `: "` + stamp + `"`}
		return base, nil, true
	}
	// An annotations block that does not carry ours: add it as the first entry, directly after
	// the `annotations:` key line, at the indentation its existing siblings already use.
	annKey := mappingKeyNode(meta, "annotations")
	if len(ann.Content) == 0 {
		return RestartEdit{}, &Warning{
			Code:    WarnRestartFlowMapping,
			Message: fmt.Sprintf("%s: spec.template.metadata.annotations is empty, so there is nothing to derive its indentation from", where),
		}, false
	}
	indent := strings.Repeat(" ", ann.Content[0].Column-1)
	base.Line = annKey.Line
	base.Lines = []string{indent + RestartAnnotation + `: "` + stamp + `"`}
	return base, nil, true
}

// mappingKeyNode returns the KEY node for the last of keys, where lookup returns the value.
// The key node is what carries the line the block header sits on and the column its own
// indentation starts at.
func mappingKeyNode(n *yaml.Node, keys ...string) *yaml.Node {
	cur := unwrap(n)
	var key *yaml.Node
	for _, k := range keys {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		key = nil
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == k {
				key = cur.Content[i]
				cur = unwrap(cur.Content[i+1])
				break
			}
		}
		if key == nil {
			return nil
		}
	}
	return key
}

// ApplyRestarts writes the planned annotation into each file, verifying every file before it is
// written (the same order gitops.Apply uses: nothing reaches disk unverified).
func ApplyRestarts(root string, restarts []RestartEdit) (changed []string, err error) {
	byFile := map[string][]RestartEdit{}
	var files []string
	paths := map[string]string{}
	for _, r := range restarts {
		if _, ok := byFile[r.File]; !ok {
			p, perr := ResolvePath(root, r.File)
			if perr != nil {
				return nil, fmt.Errorf("restart file: %w", perr)
			}
			paths[r.File] = p
			files = append(files, r.File)
		}
		byFile[r.File] = append(byFile[r.File], r)
	}
	sort.Strings(files)

	before := map[string][]byte{}
	after := map[string][]byte{}
	for _, f := range files {
		// Read the path ResolvePath already checked and that the write below will use, never
		// re-resolve: an in-repo symlink changed in between would otherwise have the bytes read
		// from the new target and written to the old one (Copilot, PR #78). gitops.Apply reads
		// through its own stored path for the same reason.
		b, rerr := os.ReadFile(paths[f])
		if rerr != nil {
			return nil, rerr
		}
		a, aerr := ApplyRestartsToBytes(b, byFile[f])
		if aerr != nil {
			return nil, aerr
		}
		before[f], after[f] = b, a
	}
	if verr := VerifyRestarts(before, after, restarts); verr != nil {
		return nil, verr
	}
	for _, f := range files {
		if bytes.Equal(before[f], after[f]) {
			continue
		}
		info, serr := os.Stat(paths[f])
		if serr != nil {
			return nil, serr
		}
		// Keep the file's own mode rather than inheriting the umask (AGENTS.md §8).
		if werr := os.WriteFile(paths[f], after[f], info.Mode().Perm()); werr != nil {
			return nil, werr
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// ApplyRestartsToBytes is the pure half: the planned writes applied to one file's bytes.
// Writes are applied from the bottom of the file up, so an insertion never invalidates the
// recorded line of one planned above it.
func ApplyRestartsToBytes(before []byte, restarts []RestartEdit) ([]byte, error) {
	lines := bytes.Split(before, []byte{'\n'})
	sorted := append([]RestartEdit(nil), restarts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Line > sorted[j].Line })
	for _, r := range sorted {
		if r.Line < 1 || r.Line > len(lines) {
			return nil, fmt.Errorf("%s: line %d is outside the file (%d lines)", r.File, r.Line, len(lines))
		}
		if len(r.Lines) == 0 {
			return nil, fmt.Errorf("%s:%d: restart plans no lines to write", r.File, r.Line)
		}
		if r.Replace {
			if len(r.Lines) != 1 {
				return nil, fmt.Errorf("%s:%d: an in-place restart writes exactly one line, plan has %d", r.File, r.Line, len(r.Lines))
			}
			lines[r.Line-1] = []byte(r.Lines[0])
			continue
		}
		ins := make([][]byte, 0, len(r.Lines))
		for _, l := range r.Lines {
			ins = append(ins, []byte(l))
		}
		out := make([][]byte, 0, len(lines)+len(ins))
		out = append(out, lines[:r.Line]...)
		out = append(out, ins...)
		out = append(out, lines[r.Line:]...)
		lines = out
	}
	return bytes.Join(lines, []byte{'\n'}), nil
}

// VerifyRestarts checks that after differs from before only by the planned annotation writes.
//
// It proves that two independent ways, because this is the one operation in hoist that adds
// lines to a manifest and the ordinary Verify's line-count rule cannot cover it:
//
//   - Byte level: the planned writes replayed onto before must reproduce after exactly, whole
//     file. This is strictly stronger than the per-line check Verify does for an Edit — it
//     pins every byte of the result, comments and trailing whitespace included — and it is
//     available here only because a restart's own content is fully known in advance.
//   - Semantic level: after, decoded and with the planned annotation taken back out, must be
//     deeply equal to before decoded. This is what catches a byte-identical result that
//     nonetheless changed the document's meaning — an insertion landing inside the wrong
//     block, at the wrong depth, still yields the exact lines that were planned, but the
//     decoded tree would no longer match.
//
// Between them: nothing else moved, and what moved means what it was supposed to mean.
func VerifyRestarts(before, after map[string][]byte, restarts []RestartEdit) error {
	byFile := map[string][]RestartEdit{}
	for _, r := range restarts {
		if _, ok := before[r.File]; !ok {
			return fmt.Errorf("restart targets %s, which is not among the verified files", r.File)
		}
		byFile[r.File] = append(byFile[r.File], r)
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
		want, err := ApplyRestartsToBytes(before[f], byFile[f])
		if err != nil {
			return err
		}
		if !bytes.Equal(want, a) {
			return fmt.Errorf("%s: result does not match the planned restart writes", f)
		}
		if err := verifyRestartMeaning(f, before[f], a, byFile[f]); err != nil {
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

// verifyRestartMeaning is the semantic half: decode both, remove the annotation this plan
// wrote from every document it targeted, and require what is left to equal before exactly.
func verifyRestartMeaning(file string, before, after []byte, restarts []RestartEdit) error {
	bd, err := decodeDocs(before)
	if err != nil {
		return fmt.Errorf("%s: before does not decode: %w", file, err)
	}
	ad, err := decodeDocs(after)
	if err != nil {
		return fmt.Errorf("%s: after does not decode: %w", file, err)
	}
	if len(bd) != len(ad) {
		return fmt.Errorf("%s: document count changed from %d to %d", file, len(bd), len(ad))
	}
	for _, r := range restarts {
		if r.Doc < 0 || r.Doc >= len(ad) {
			return fmt.Errorf("%s: restart targets document %d, which does not exist", file, r.Doc)
		}
		got, ok := podTemplateAnnotation(ad[r.Doc])
		if !ok {
			return fmt.Errorf("%s: document %d has no %s after the write", file, r.Doc, RestartAnnotation)
		}
		if got != r.New {
			return fmt.Errorf("%s: document %d has %s = %q after the write, planned %q", file, r.Doc, RestartAnnotation, got, r.New)
		}
		undoRestartWrite(ad[r.Doc], r)
	}
	for i := range bd {
		if !reflect.DeepEqual(bd[i], ad[i]) {
			return fmt.Errorf("%s: document %d changed by more than the restart annotation", file, i)
		}
	}
	return nil
}

func decodeDocs(b []byte) ([]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []any
	for {
		var v any
		err := dec.Decode(&v)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		out = append(out, v)
	}
}

// podTemplateAnnotation reads spec.template.metadata.annotations[RestartAnnotation] out of a
// decoded document.
func podTemplateAnnotation(doc any) (string, bool) {
	ann, ok := podTemplateAnnotations(doc)
	if !ok {
		return "", false
	}
	v, ok := ann[RestartAnnotation].(string)
	return v, ok
}

func podTemplateAnnotations(doc any) (map[string]any, bool) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, k := range []string{"spec", "template", "metadata"} {
		next, ok := m[k].(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	ann, ok := m["annotations"].(map[string]any)
	return ann, ok
}

// undoRestartWrite reverses this plan's own write on a decoded document, so what is left can
// be compared with before. Three cases, and they are distinguishable from the plan alone:
// a replacement puts the superseded timestamp back; an insertion into an annotations map that
// already existed removes just the key; an insertion that introduced the map (the plan wrote
// two lines, the `annotations:` header and the key) removes the map with it.
func undoRestartWrite(doc any, r RestartEdit) {
	ann, ok := podTemplateAnnotations(doc)
	if !ok {
		return
	}
	if r.Replace {
		ann[RestartAnnotation] = r.Old
		return
	}
	delete(ann, RestartAnnotation)
	if len(r.Lines) < 2 {
		// The map was already there; only the key was ours.
		return
	}
	m, _ := doc.(map[string]any)
	for _, k := range []string{"spec", "template", "metadata"} {
		m, _ = m[k].(map[string]any)
	}
	delete(m, "annotations")
}

// restartDiffContext is how many unchanged lines frame each hunk, matching diff.go's own.
const restartDiffContext = 3

// UnifiedRestartDiff renders the planned restart writes as a unified diff.
//
// It builds the hunks from the plan rather than by diffing before against after, because the
// plan already says exactly what moves and where — and because diff.go's UnifiedDiff, written
// when every edit was a same-line scalar replacement, falls back to printing the whole file as
// removed-then-added the moment the line counts differ. That fallback is harmless for an edit
// (it never happens) and useless for a restart (it always would): an operator confirming a
// two-line insertion would be shown their entire manifest twice.
func UnifiedRestartDiff(path string, before []byte, restarts []RestartEdit) string {
	if len(restarts) == 0 {
		return ""
	}
	// splitForDiff, not bytes.Split: the latter leaves a synthetic empty final element for a
	// file ending in a newline, which a hunk reaching EOF would render as a phantom blank
	// context line — and it loses the fact that a file did NOT end in one, which a truthful
	// unified diff has to mark (Copilot, PR #80). This is the same pair UnifiedDiff uses.
	lines, trailingNewline := splitForDiff(before)
	last := len(lines) - 1
	sorted := append([]RestartEdit(nil), restarts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })

	// Group writes whose context windows touch or overlap into one hunk. Emitting a hunk per
	// write is only correct while they are far apart; two Deployments near each other in one
	// manifest would otherwise produce hunks with overlapping old-file ranges, repeating the
	// same context lines and describing a file that does not exist (Copilot, PR #80).
	type group struct {
		start, end int // 0-based, [start,end)
		writes     []RestartEdit
	}
	var groups []group
	for _, r := range sorted {
		if r.Line < 1 || r.Line > len(lines) {
			continue
		}
		anchor := r.Line - 1
		gs := max(0, anchor-restartDiffContext)
		ge := min(len(lines), anchor+1+restartDiffContext)
		if n := len(groups); n > 0 && gs <= groups[n-1].end {
			groups[n-1].end = max(groups[n-1].end, ge)
			groups[n-1].writes = append(groups[n-1].writes, r)
			continue
		}
		groups = append(groups, group{start: gs, end: ge, writes: []RestartEdit{r}})
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	// Offset tracks how far the after-file's numbering has drifted from the before-file's as
	// earlier insertions accumulate, so each hunk header names the line an operator would
	// actually find it at in the result.
	offset := 0
	for _, g := range groups {
		byAnchor := make(map[int]RestartEdit, len(g.writes))
		added := 0
		for _, r := range g.writes {
			byAnchor[r.Line-1] = r
			if !r.Replace {
				added += len(r.Lines)
			}
		}
		oldCount := g.end - g.start
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", g.start+1, oldCount, g.start+1+offset, oldCount+added)
		for k := g.start; k < g.end; k++ {
			// The "\ No newline at end of file" marker belongs on the last line of a file that
			// has no trailing newline, and on nothing else.
			atEOF := k == last && !trailingNewline
			r, isAnchor := byAnchor[k]
			switch {
			case isAnchor && r.Replace:
				writeDiffLine(&sb, '-', lines[k], atEOF)
				writeDiffLine(&sb, '+', []byte(r.Lines[0]), atEOF)
			case isAnchor:
				writeDiffLine(&sb, ' ', lines[k], atEOF && len(r.Lines) == 0)
				for i, l := range r.Lines {
					writeDiffLine(&sb, '+', []byte(l), atEOF && i == len(r.Lines)-1)
				}
			default:
				writeDiffLine(&sb, ' ', lines[k], atEOF)
			}
		}
		offset += added
	}
	return sb.String()
}
