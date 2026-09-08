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
	// StateSplit means one image repo runs under more than one build inside the env (two
	// digests, or two tags — see builds) — the condition the runbook blocks a promotion on,
	// so it should look like a problem. Judged per repo, so a multi-repo cell can be split too.
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
	// Text is the tag shown: the single first-party tag, "v1/v2" when one build is pinned
	// under several tags (sorted, so the text does not depend on file order), "N images"
	// when the family's first-party occurrences span N image repos, or "N versions" when the
	// one image repo runs under N builds inside the env (see builds). A third-party-only
	// family shows the same for its third-party images.
	Text string
	// State is the word beside Text; "" for a present cell with no images at all. The split
	// word is judged per image repo, so a "N images" cell is split whenever any one of its
	// repos runs under two builds, and a "N versions" cell is always split: Text counts repos
	// or builds, State says whether any repo's builds disagree, and both come from builds.
	State State
	// Pinned: every first-party occurrence carries a digest.
	Pinned bool
	// Differs: the set of refs differs from the previous env column's. Never set on the
	// first column or when the previous column is blank. Kept as data for a caller that
	// wants to colour a change across columns; it is no longer a glyph.
	Differs bool
	// ThirdParty: the family has no first-party image at all.
	ThirdParty bool
	// Running is what the cluster reports for a drifted cell, for the sentence under the
	// table ("marketing runs sha-77c0ffe here; manifest says …"): the one running build's
	// tag or short digest, or "N builds running (a, b)" when the pods run several. Builds
	// are counted by digest where the pod reported one (else by tag), and two builds under
	// the same tag are each named "tag · <short digest>" so they can be told apart — the
	// case a mutable tag makes most likely. Empty unless State is StateDrifted.
	Running string
	// Compared names the comparison that decided the drift — "by digest" when any manifest
	// reference for the repo is pinned and was compared with the undeclared build's digest,
	// "by tag" only when every comparison made for that build was against a bare manifest
	// reference — so the sentence says the strongest evidence it has. Empty unless State is
	// StateDrifted.
	Compared string
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

// Running is what each env's cluster is actually running, keyed env → image repo (in the
// registry's canonical spelling, image.Canonical) → every distinct running reference (the
// digest each pod reports, and the tag the pod was started from when it reported one).
// Every build is kept: a partial rollout is two entries, never the one a resolver would
// choose (#122). nil, or a missing env, means the cluster has not answered for that env —
// no cell there can be drifted, and none is claimed not to be.
type Running map[string]map[string][]image.Ref

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
func cellFor(fam *gitops.Family, promotable []string, running map[string][]image.Ref) Cell {
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
	// Split is judged per image repo on builds (see builds): a repo that runs under two
	// different builds is split whether or not the cell holds other repos too (#118).
	// cellText counts the same builds, so "N versions" and the word never disagree.
	for _, repo := range distinct(first, func(r image.Ref) string { return r.Repo }) {
		if len(builds(first, repo)) > 1 {
			c.State = StateSplit
			break
		}
	}
	if runs, compared, ok := drifted(first, running); ok {
		c.State = StateDrifted
		c.Compared = compared
		c.Running = runningText(runs)
	}
	return c
}

// drifted reports every running reference of the first repo whose pods run a build none of
// its manifest references declare, and which comparison found it. The rule (#122): a repo
// is drifted when ANY running build matches no manifest reference — a partial rollout
// where one build is declared and one is not is drifted, because the cluster runs
// something the manifest does not say; two running builds both declared (a split
// manifest mid-rollout) are not. Each running build is compared by digest against a
// pinned manifest reference and by tag against a bare one (sameBuild); a running
// reference that reports no tag cannot be compared with a bare manifest at all, and is
// neither drifted nor confirmed. A repo the cluster did not report is not drifted —
// absence of evidence is not drift.
func drifted(first []image.Ref, running map[string][]image.Ref) ([]image.Ref, string, bool) {
	if len(running) == 0 {
		return nil, "", false
	}
	for _, repo := range distinct(first, func(r image.Ref) string { return r.Repo }) {
		runs := running[repo]
		if runs == nil {
			runs = running[image.Canonical(repo)]
		}
		if len(runs) == 0 {
			continue
		}
		for _, run := range runs {
			matched, compared := false, ""
			for _, ref := range first {
				if ref.Repo != repo {
					continue
				}
				same, how := sameBuild(ref, run)
				if how == "" {
					continue
				}
				// A mixed manifest (one pinned reference, one bare) makes both comparisons;
				// the digest is the stronger evidence, whichever reference came last.
				if compared == "" || how == "by digest" {
					compared = how
				}
				if same {
					matched = true
					break
				}
			}
			if !matched && compared != "" {
				return runs, compared, true
			}
		}
	}
	return nil, "", false
}

// sameBuild compares a manifest reference with a running one and names how: "by digest"
// when the manifest is pinned and the pod reported a digest, "by tag" when the manifest is
// bare and the pod reported the tag it was started from; "" when the two cannot be
// compared (a bare manifest against a digest-only pod).
func sameBuild(manifest, running image.Ref) (same bool, compared string) {
	if manifest.Digest != "" && running.Digest != "" {
		return manifest.Digest == running.Digest, "by digest"
	}
	if manifest.Digest == "" && manifest.Tag != "" && running.Tag != "" {
		return manifest.Tag == running.Tag, "by tag"
	}
	return false, ""
}

// runningText is the drifted sentence's subject: one build's tag or short digest, or "N
// builds running (a, b)" when the pods run several. A build is a digest where the pod
// reported one, else a tag — never the tag alone, which would fold v1@A and v1@B into one
// "v1" and hide exactly the rollout a moved tag produces. Two builds under one tag are told
// apart as "v1 · aaaaaaaaaaaa"; names are sorted so the sentence is stable.
func runningText(runs []image.Ref) string {
	seen := map[string]bool{}
	var found []image.Ref
	tagCount := map[string]int{}
	for _, r := range runs {
		key := r.Digest
		if key == "" {
			key = "tag:" + r.Tag
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, r)
		if r.Tag != "" {
			tagCount[r.Tag]++
		}
	}
	names := make([]string, 0, len(found))
	for _, r := range found {
		name := tagOrDigest(r)
		if r.Tag != "" && r.Digest != "" && tagCount[r.Tag] > 1 {
			name = r.Tag + " · " + shortHex(r.Digest)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 1 {
		return names[0]
	}
	return fmt.Sprintf("%d builds running (%s)", len(names), strings.Join(names, ", "))
}

// shortHex is the first 12 hex characters of a digest, without its algorithm prefix.
func shortHex(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
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
		if b := builds(refs, repos[0]); len(b) > 1 {
			return fmt.Sprintf("%d versions", len(b))
		}
		// One build — but it may be pinned under several tags (v1@A and v2@A): name every
		// tag, sorted, rather than whichever occurrence came first in the file.
		var tagged []image.Ref
		for _, r := range refs {
			if r.Tag != "" {
				tagged = append(tagged, r)
			}
		}
		if tags := distinct(tagged, func(r image.Ref) string { return r.Tag }); len(tags) > 0 {
			return strings.Join(tags, "/")
		}
		return tagOrDigest(refs[0])
	default:
		return fmt.Sprintf("%d images", len(repos))
	}
}

// builds is the distinct set of builds one image repo runs under among refs — the basis of
// the split word and of cellText's "N versions". Two references are the same build when
// both carry a digest and the digests agree, or when one carries no digest and the tags
// agree; so v1@A and a bare v1 are one build (unpinned, not split), v1@A and v1@B are two
// (one tag pinned to two builds — split), and v1 and v2 are two. Each entry is the digest
// where one is known, else the tag.
func builds(refs []image.Ref, repo string) []string {
	var pinnedTags []string
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		if r.Repo != repo || !r.Pinned() {
			continue
		}
		pinnedTags = append(pinnedTags, r.Tag)
		if !seen[r.Digest] {
			seen[r.Digest] = true
			out = append(out, r.Digest)
		}
	}
	for _, r := range refs {
		if r.Repo != repo || r.Pinned() || seen["tag:"+r.Tag] {
			continue
		}
		covered := false
		for _, t := range pinnedTags {
			if t != "" && t == r.Tag {
				covered = true
				break
			}
		}
		if !covered {
			seen["tag:"+r.Tag] = true
			out = append(out, r.Tag)
		}
	}
	sort.Strings(out)
	return out
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
