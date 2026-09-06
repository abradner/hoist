package tags

import (
	"sort"

	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// The commit pane (M10, #85 screen 02/07/08): what is actually in the build under the cursor
// — how many commits ahead of what the env declares, their subjects, and which of them carry
// a migration. Derived here with no terminal dependency; model.go lays it out.

// Declared is what the target env's manifests declare for this image repo today: the
// reference every row is compared against ("v3 is 14 commits ahead of v1"), and the
// occurrence whose line the live-age blame dates.
type Declared struct {
	Ref        image.Ref
	Occurrence gitops.Occurrence
}

// DeclaredIn finds the target env's declared reference for imageRepo: the first occurrence,
// by family then file then line, so the answer is stable when the env carries several. ok is
// false when the env has no occurrence of the repo at all (a first deploy — there is nothing
// to compare with, and gitops.BuildDeployPlan would refuse the write anyway).
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
	return Declared{Ref: found[0].Ref, Occurrence: found[0]}, true
}
