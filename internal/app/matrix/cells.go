// Package matrix is the env × family screen: one row per family, one column per env, the
// family's image tag in each cell with a marker for pin state, drift and third-party-only
// families. cells.go derives the cell values from a discovered repo with no terminal
// dependency; model.go lays them out.
package matrix

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// Cell is one family × env intersection.
type Cell struct {
	// Present is false when the family has no Application in this env; the other fields
	// are then zero and String renders blank.
	Present bool
	// Text is the tag shown: the single first-party tag, "N images" when the family's
	// first-party occurrences span N image repos, or "tagA,tagB" when one image repo runs
	// under more than one tag inside the env. A third-party-only family shows the same for
	// its third-party images.
	Text string
	// Pinned: every first-party occurrence carries a digest (marker @).
	Pinned bool
	// Differs: the set of refs differs from the previous env column's (marker ≠). Never set
	// on the first column or when the previous column is blank.
	Differs bool
	// ThirdParty: the family has no first-party image at all (marker !).
	ThirdParty bool
	// key is the sorted distinct ref set the Differs comparison runs on.
	key string
}

// Marker is two cells wide so tags stay aligned: pin state (@, or ! for third-party-only)
// then drift (≠), each blank when not set.
func (c Cell) Marker() string {
	if !c.Present {
		return "  "
	}
	pin, drift := ' ', ' '
	switch {
	case c.ThirdParty:
		pin = '!'
	case c.Pinned:
		pin = '@'
	}
	if c.Differs {
		drift = '≠'
	}
	return string([]rune{pin, drift})
}

// String is the cell as the table shows it: marker, space, text — or blank when absent.
func (c Cell) String() string {
	if !c.Present {
		return ""
	}
	return c.Marker() + " " + c.Text
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

// Compute derives the matrix from a discovered repo. promotable lists the image repo
// prefixes that count as first-party (what hoist plan --promotable takes); an occurrence
// matching none is third-party.
func Compute(r *gitops.Repo, promotable []string) Table {
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
			c := cellFor(fam, promotable)
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
func cellFor(fam *gitops.Family, promotable []string) Cell {
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
	if len(first) > 0 {
		c.Pinned = true
		for _, ref := range first {
			if !ref.Pinned() {
				c.Pinned = false
				break
			}
		}
	}
	return c
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
		return strings.Join(distinct(refs, tagOrDigest), ",")
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
