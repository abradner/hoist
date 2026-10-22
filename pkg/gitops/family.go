package gitops

import "sort"

// Workload names one controller-managed object a family's manifests declare: the Kind and
// metadata.name of the document an image occurrence sits in.
type Workload struct {
	Kind, Name string
}

// Workloads lists the distinct (Kind, Name) pairs of the documents this family's occurrences
// sit in, sorted by kind and then name. It is what discovery found — every kind, not only the
// ones a caller can act on — so each consumer picks the kinds it handles and the question "which
// workloads does this family declare" has one answer (AR-M9) rather than a loop per caller.
//
// A nil family declares none.
func (f *Family) Workloads() []Workload {
	if f == nil {
		return nil
	}
	seen := map[Workload]bool{}
	var out []Workload
	for _, o := range f.Occurrences {
		w := Workload{Kind: o.Kind, Name: o.Name}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Named returns the names of the family's workloads of the given kind, sorted and distinct.
func (f *Family) Named(kind string) []string {
	var out []string
	for _, w := range f.Workloads() {
		if w.Kind == kind {
			out = append(out, w.Name)
		}
	}
	return out
}
