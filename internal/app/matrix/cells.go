// Package matrix is the env × family screen: one row per family, one column per env, the
// family's image tag in each cell with its state spelled out — pinned, unpinned, split,
// external, drifted — as a word an operator can read without a legend. cells.go derives the
// cell values from a discovered repo (and, when the cluster has answered, from what each env
// is actually running) with no terminal dependency; model.go lays them out.
package matrix

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// State is the one word a cell shows beside its tag. Colour reinforces it; it never replaces
// it (docs/tui/mockups.html: "state is a word, not a glyph").
type State string

const (
	// StatePinned means every first-party occurrence carries a digest.
	StatePinned State = "pinned"
	// StateUnpinned means at least one first-party occurrence is a bare tag — a moved tag is
	// invisible to imagePullPolicy: IfNotPresent (AGENTS.md principle 3).
	StateUnpinned State = "unpinned"
	// StateSplit means one image repo runs under more than one reference inside the env — the
	// condition the runbook blocks a promotion on, so it should look like a problem.
	StateSplit State = "split"
	// StateExternal means the family has no first-party image at all.
	StateExternal State = "external"
	// StateDrifted means the cluster runs a build the manifest does not declare. The single most
	// important fact on the screen when it is true, so it gets a sentence under the table too.
	StateDrifted State = "drifted"
)

// Cell is one family × env intersection.
type Cell struct {
	// Present is false when the family has no Application in this env; the other fields
	// are then zero and String renders blank.
	Present bool
	// Text is the tag shown: the single first-party tag, "N images" when the family's
	// first-party occurrences span N image repos, or "N versions" when one image repo runs
	// under N references inside the env. A third-party-only family shows the same for its
	// third-party images.
	Text string
	// State is the word beside Text; "" for a present cell with no images at all.
	State State
	// Pinned: every first-party occurrence carries a digest.
	Pinned bool
	// Differs: the set of refs differs from the previous env column's. Never set on the
	// first column or when the previous column is blank. Kept as data for a caller that
	// wants to colour a change across columns; it is no longer a glyph.
	Differs bool
	// ThirdParty: the family has no first-party image at all.
	ThirdParty bool
	// Running is what the cluster reports for a drifted cell — the tag, or a short digest —
	// for the sentence under the table ("marketing runs sha-77c0ffe here; manifest says …").
	// Empty unless State is StateDrifted.
	Running string
	// key is the sorted distinct ref set the Differs comparison runs on.
	key string
}

// String is the cell as the table shows it: text, two spaces, state — or blank when absent.
// The table aligns the state to the right of the column itself (see rows in model.go); this
// is the unaligned form for tests and logs.
func (c Cell) String() string {
	if !c.Present {
		return ""
	}
	if c.State == "" {
		return c.Text
	}
	return c.Text + "  " + string(c.State)
}

// Row is one family across every env; Cells is aligned with Table.Envs.
type Row struct {
	Family string
	Cells  []Cell
}

// Table is the whole matrix: envs sorted by name, families sorted by name (the union across
// envs).
type Table struct {
	Envs []string
	Rows []Row
}

// Running is what each env's cluster is actually running, keyed env → image repo → the
// running reference (tag and digest as the pod reports them). nil, or a missing env, means
// the cluster has not answered for that env — no cell there can be drifted, and none is
// claimed not to be.
type Running map[string]map[string]image.Ref

// Compute derives the matrix from a discovered repo. promotable lists the image repo
// prefixes that count as first-party (what hoist plan --promotable takes); an occurrence
// matching none is third-party. running may be nil.
func Compute(r *gitops.Repo, promotable []string, running Running) Table {
	t := Table{Envs: make([]string, 0, len(r.Envs))}
	famSet := map[string]bool{}
	for name, env := range r.Envs {
		t.Envs = append(t.Envs, name)
		for f := range env.Families {
			famSet[f] = true
		}
	}
	sort.Strings(t.Envs)
	fams := make([]string, 0, len(famSet))
	for f := range famSet {
		fams = append(fams, f)
	}
	sort.Strings(fams)
	for _, f := range fams {
		row := Row{Family: f, Cells: make([]Cell, len(t.Envs))}
		for i, e := range t.Envs {
			fam := r.Envs[e].Families[f]
			if fam == nil {
				continue
			}
			c := cellFor(fam, promotable, running[e])
			if i > 0 && row.Cells[i-1].Present && row.Cells[i-1].key != c.key {
				c.Differs = true
			}
			row.Cells[i] = c
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// cellFor computes everything about a cell except Differs, which needs its neighbour.
func cellFor(fam *gitops.Family, promotable []string, running map[string]image.Ref) Cell {
	var first, third []image.Ref
	for _, o := range fam.Occurrences {
		if isFirstParty(o.Ref.Repo, promotable) {
			first = append(first, o.Ref)
		} else {
			third = append(third, o.Ref)
		}
	}
	c := Cell{Present: true}
	refs := first
	if len(first) == 0 {
		// Only third-party images — or none at all, which is not third-party either.
		c.ThirdParty = len(third) > 0
		refs = third
	}
	c.key = refKey(refs)
	c.Text = cellText(refs)
	switch {
	case len(refs) == 0:
		return c
	case c.ThirdParty:
		c.State = StateExternal
		return c
	}
	c.Pinned = true
	for _, ref := range first {
		if !ref.Pinned() {
			c.Pinned = false
			break
		}
	}
	c.State = StateUnpinned
	if c.Pinned {
		c.State = StatePinned
	}
	// Split is judged on what the cell shows — distinct tags (or digests for tag-less refs),
	// the same basis as cellText's "N versions" — so the word and the text never disagree.
	// Two occurrences of one tag, one pinned and one not, are unpinned, not split.
	if repos := distinct(first, func(r image.Ref) string { return r.Repo }); len(repos) == 1 && len(distinct(first, tagOrDigest)) > 1 {
		c.State = StateSplit
	}
	if run, ok := drifted(first, running); ok {
		c.State = StateDrifted
		c.Running = tagOrDigest(run)
	}
	return c
}

// drifted reports the running reference for the first repo whose cluster image matches
// none of its manifest references. Digests decide when both sides carry one; a bare manifest
// tag compares by tag. A repo the cluster did not report is not drifted — absence of evidence
// is not drift.
func drifted(first []image.Ref, running map[string]image.Ref) (image.Ref, bool) {
	if len(running) == 0 {
		return image.Ref{}, false
	}
	for _, repo := range distinct(first, func(r image.Ref) string { return r.Repo }) {
		run, ok := running[repo]
		if !ok {
			continue
		}
		matched := false
		for _, ref := range first {
			if ref.Repo != repo {
				continue
			}
			if sameBuild(ref, run) {
				matched = true
				break
			}
		}
		if !matched {
			return run, true
		}
	}
	return image.Ref{}, false
}

// sameBuild compares a manifest reference with a running one: by digest when both have one,
// else by tag.
func sameBuild(manifest, running image.Ref) bool {
	if manifest.Digest != "" && running.Digest != "" {
		return manifest.Digest == running.Digest
	}
	return manifest.Tag != "" && manifest.Tag == running.Tag
}

func isFirstParty(repo string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

// refKey is the sorted distinct set of full references, joined — equal keys mean the env
// runs exactly the same images.
func refKey(refs []image.Ref) string {
	return strings.Join(distinct(refs, image.Ref.String), "\n")
}

func cellText(refs []image.Ref) string {
	repos := distinct(refs, func(r image.Ref) string { return r.Repo })
	switch len(repos) {
	case 0:
		return "no images"
	case 1:
		tags := distinct(refs, tagOrDigest)
		if len(tags) > 1 {
			return fmt.Sprintf("%d versions", len(tags))
		}
		return tags[0]
	default:
		return fmt.Sprintf("%d images", len(repos))
	}
}

// tagOrDigest is the tag, or a shortened digest for a tag-less reference.
func tagOrDigest(r image.Ref) string {
	if r.Tag != "" {
		return r.Tag
	}
	const short = len("sha256:") + 12
	if len(r.Digest) > short {
		return r.Digest[:short]
	}
	return r.Digest
}

func distinct(refs []image.Ref, f func(image.Ref) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if s := f(r); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// FirstPartyRepos lists, sorted, the distinct first-party image repos a family runs in one
// env — what d offers to deploy: one opens the picker directly, several open a chooser first.
func FirstPartyRepos(fam *gitops.Family, promotable []string) []string {
	if fam == nil {
		return nil
	}
	var repos []string
	seen := map[string]bool{}
	for _, o := range fam.Occurrences {
		if !isFirstParty(o.Ref.Repo, promotable) || seen[o.Ref.Repo] {
			continue
		}
		seen[o.Ref.Repo] = true
		repos = append(repos, o.Ref.Repo)
	}
	sort.Strings(repos)
	return repos
}
