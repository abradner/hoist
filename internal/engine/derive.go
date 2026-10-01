package engine

// derive.go holds pure functions over gitops.Repo/plan data used to build a PromotionState —
// plan-derivation helpers cmd/hoist calls once, before Drive ever runs, kept apart from the
// steps themselves (moved out of the M5-era steps file in the by-concern split — this package
// now groups files by what the code does, not by which milestone added it).

import (
	"fmt"
	"path"
	"sort"

	"github.com/abradner/hoist/pkg/gitops"
)

// ArgoAppNames returns the distinct, sorted set of Argo Application names in targetEnv whose
// family directory contains at least one edit's file. The CLI calls this once, from the same
// gitops.Repo Discover already produced, when building a PromotionState — mirroring
// RenderCommitMessage/PRTitle/RenderPRBody: a pure function of the repo's discovered structure
// and the plan, called once and then carried on PromotionState.ArgoApps (see its own doc
// comment for why carrying it does not violate "the world is the state"). An edit whose file
// matches no family in targetEnv is an internal inconsistency — BuildPlan only ever produces
// edits from occurrences it read from an env's own families — and is reported as an error
// naming the file and directory, rather than silently dropped.
func ArgoAppNames(r *gitops.Repo, targetEnv string, edits []gitops.Edit) ([]string, error) {
	byFile, err := EditApps(r, targetEnv, edits)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, app := range byFile {
		if !seen[app] {
			seen[app] = true
			names = append(names, app)
		}
	}
	sort.Strings(names)
	return names, nil
}

// EditApps maps each edit's file to the Argo Application name that owns it — the same
// Family->Application walk ArgoAppNames dedupes and sorts, kept per-file here so a caller that
// needs to scope a question to one Application's own share of a promotion (ArgoSyncedStep's
// revisionCarries, #182) doesn't have to re-derive the mapping. The CLI calls
// this once, from the same gitops.Repo Discover already produced, when building a
// PromotionState, and carries the result on PromotionState.EditApps (see its own doc comment)
// rather than recomputing it on every resume — the same "structural fact about the plan,
// computed once" treatment ArgoApps already gets. An edit whose file matches no family in
// targetEnv is an internal inconsistency — BuildPlan only ever produces edits from occurrences
// it read from an env's own families — and is reported as an error naming the file and
// directory, rather than silently dropped.
func EditApps(r *gitops.Repo, targetEnv string, edits []gitops.Edit) (map[string]string, error) {
	env, ok := r.Envs[targetEnv]
	if !ok {
		return nil, fmt.Errorf("argo apps: target env %q not found in the discovered repo", targetEnv)
	}
	byDir := make(map[string]string, len(env.Families))
	for _, f := range env.Families {
		byDir[f.Dir] = f.App
	}
	out := make(map[string]string, len(edits))
	for _, e := range edits {
		dir := path.Dir(e.File)
		app, ok := byDir[dir]
		if !ok {
			return nil, fmt.Errorf("argo apps: edit %s: no family in env %q owns directory %s", e.File, targetEnv, dir)
		}
		out[e.File] = app
	}
	return out, nil
}
