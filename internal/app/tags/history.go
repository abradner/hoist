package tags

import (
	"sort"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// The commit pane (M10, #85 screen 02/07/08): what is actually in the build under the cursor
// — how many commits ahead of what the env declares, their subjects, and which of them carry
// a migration. Derived here with no terminal dependency; model.go lays it out.

// Declared is what the target env's manifests declare for this image repo today. Refs is
// every distinct reference the env carries, in file-then-line order — one entry in the
// common case, several when the env is split (the matrix's own word for one image repo at
// two references). Ref is Refs[0]: the reference every row's commit delta is measured
// against ("v3 is 14 commits ahead of v1"), and Occurrence its first occurrence, whose line
// the live-age blame dates. A screen that renders Ref alone for a split env is claiming one
// declared build where there are several (#119): it must render Refs, or say which one the
// delta is from.
type Declared struct {
	Ref        image.Ref
	Occurrence gitops.Occurrence
	Refs       []image.Ref
}

// Split reports whether the env declares this image repo at more than one reference.
func (d Declared) Split() bool { return len(d.Refs) > 1 }

// DeclaredIn finds the target env's declared references for imageRepo: every distinct
// reference across its families, ordered by file then line, so the answer is stable when the
// env carries several and Ref is always the same one of them. ok is false when the env has
// no occurrence of the repo at all (a first deploy — there is nothing to compare with, and
// gitops.BuildDeployPlan would refuse the write anyway).
func DeclaredIn(repo *gitops.Repo, imageRepo, target string) (Declared, bool) {
	if repo == nil {
		return Declared{}, false
	}
	env, ok := repo.Envs[target]
	if !ok {
		return Declared{}, false
	}
	fams := make([]string, 0, len(env.Families))
	for name := range env.Families {
		fams = append(fams, name)
	}
	sort.Strings(fams)
	var found []gitops.Occurrence
	for _, name := range fams {
		for _, o := range env.Families[name].Occurrences {
			if o.Ref.Repo == imageRepo {
				found = append(found, o)
			}
		}
	}
	if len(found) == 0 {
		return Declared{}, false
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].File != found[j].File {
			return found[i].File < found[j].File
		}
		return found[i].Line < found[j].Line
	})
	seen := map[string]bool{}
	var refs []image.Ref
	for _, o := range found {
		if k := o.Ref.String(); !seen[k] {
			seen[k] = true
			refs = append(refs, o.Ref)
		}
	}
	return Declared{Ref: found[0].Ref, Occurrence: found[0], Refs: refs}, true
}
