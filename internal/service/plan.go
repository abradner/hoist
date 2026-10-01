package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/resolve"
)

// PlanRequest is everything Plan needs to build one gitops.Plan, for a promotion or a deploy —
// the one shape `hoist plan`, `hoist promote`, `hoist deploy` and both TUI paths (the plan
// screen's loadCmd, the matrix's deploy confirm) now build and hand to Plan, so a plan built for
// a confirm screen and a plan built for the CLI's own dry run can never silently diverge again
// (AGENTS.md §4's own "Divergences" section, item 10).
type PlanRequest struct {
	// Repo is the discovered GitOps repo to plan against; nil uses s.Repo().Repo (the
	// service's own current view — LoadRepo/RefreshRepo).
	Repo *gitops.Repo
	// Source is the env to read digests from; "" iff Deploy is set (a deploy has no source
	// env — image.Ref names the reference outright).
	Source, Target string
	// Deploy is set for `hoist deploy`/the TUI's tag-picker confirm: write this exact
	// reference into every occurrence of its repo in Target, rather than resolving Source's
	// running digest.
	Deploy *image.Ref
	// Overrides are the operator's own per-repo digest overrides (`--digest`, the plan
	// screen's o dialog): each wins outright over whatever Source resolves to.
	Overrides map[string]image.Ref
}

// PlannedChange is what Plan returns: the built gitops.Plan (its Warnings already carry
// resolve.Warnings and, for a deploy into a production env, WarnDeployIntoProduction — never
// re-attached by the caller), the repo it was planned against, and the resolution section when
// one ran.
type PlannedChange struct {
	Plan gitops.Plan
	Repo *gitops.Repo
	// View is the RepoView Plan actually planned against — s.Repo() at the moment Plan ran, when
	// req.Repo pointed at that same *gitops.Repo (nil, or an explicit pointer that happens to be
	// the service's own current view); a zero RepoView{Repo: r} otherwise (a caller-supplied repo
	// Plan never fetched itself, e.g. the CLI's own gitops.Discover). Request below carries this
	// into StartRequest.View so StartPromotion's freshness check runs against the view THIS plan
	// was built from, never whatever s.Repo() happens to hold when the operator later confirms —
	// fixing the fail-open gap where an F5 refresh landing between building the plan and pressing
	// Enter let a stale plan's edits pass a freshness check run against the NEW view instead of
	// the one that produced them.
	//
	// When s is in origin mode (cur.FromOrigin — LoadRepo(RepoFromOrigin) has run: TUI boot or a
	// later F5), this field is ALWAYS a real FromOrigin view with a captured SHA: Plan itself
	// fails closed above rather than ever return a zero or clone-mode View for such a caller.
	// StartPromotion enforces the other half of that guarantee at its own end (structurally, not
	// just by convention): a StartRequest.View that reaches it nil, or with FromOrigin false, or
	// with an empty SHA, while the service's current view is itself origin-mode, is refused
	// outright — a caller that lost this field on the way there (a screen that forgot to plumb
	// it) gets an error naming the fix, never a silent downgrade to the CLI's looser
	// checkCloneCurrentForBase check.
	View RepoView
	// Resolution is nil for a deploy (which never resolves a digest — the reference is
	// caller-supplied) and for a promotion planned with digest sources: none.
	Resolution *Resolution
	// fresh carries this promotion's own resolved digests/reasons, for StartPromotion's
	// checkFreshBase (fresh.go) — direct mode's own cross-check against origin's fresh tree,
	// which must reuse THIS resolution's answers rather than run a second, independent one. nil
	// for a deploy (checkFreshBase never consults it there) and cleared by Request when it is
	// built for a Mode that was never handed this exact, unfiltered Plan (see Request's own doc
	// comment) — a caller with a filtered, ticked-down plan has no single resolution behind it
	// any more, and StartRequest.Fresh being nil is exactly what tells StartPromotion to recover
	// digests from the plan's own edits instead (Divergence 8, unchanged).
	fresh *freshInputs
}

// Request builds this PlannedChange's own StartRequest for StartPromotion: the plan and repo
// exactly as Plan built them, plus this promotion's own resolved digests (fresh) so direct mode's
// fresh-base check reuses them rather than re-resolving. Only valid for the UNFILTERED plan Plan
// itself returned — a caller that ticks a subset of edits (the TUI's own confirm) builds its own
// gitops.Plan with those edits and its own StartRequest directly, passing Fresh: nil, since the
// resolution behind THIS PlannedChange no longer describes only what was kept.
func (p PlannedChange) Request(m Mode) StartRequest {
	view := p.View
	return StartRequest{Plan: p.Plan, Mode: m, Repo: p.Repo, View: &view, Fresh: p.fresh}
}

// Plan builds one gitops.Plan: BuildDeployPlan for req.Deploy, else digest resolution (when
// s.Settings().Resolve.Order is non-empty) followed by BuildPlanWith — the exact sequence
// `hoist plan`/`hoist promote`/the TUI plan screen each used to run for themselves. Resolution
// errors are a whole-operation failure (AGENTS.md §4.10): a per-repo registry miss is a
// resolve.Resolution that failed to resolve, never an error return, so there is nothing left
// here to plan from without a resolution attempt that could not even run at all.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (PlannedChange, error) {
	cur := s.Repo()
	r := req.Repo
	if r == nil {
		r = cur.Repo
	}
	if r == nil {
		return PlannedChange{}, fmt.Errorf("service: Plan: no repo loaded (LoadRepo was never called, and PlanRequest.Repo was nil)")
	}
	// view is s's own current RepoView when r came from it (nil req.Repo, or an explicit pointer
	// that happens to match it) — the case that needs a frozen snapshot, since s.view can move
	// under a later F5 — else a bare RepoView wrapping the caller-supplied r (Dir/FromOrigin left
	// zero, exactly the CLI's own never-called-LoadRepo shape), never s's live view, which may
	// describe a completely different repo than the one this plan actually used.
	view := RepoView{Repo: r}
	if r == cur.Repo {
		view = cur
	} else if cur.FromOrigin {
		// s is in origin mode (LoadRepo(RepoFromOrigin) has run — TUI boot or a later F5) and
		// req.Repo names a DIFFERENT *gitops.Repo than the one s.view currently holds: an F5
		// refresh landed between whatever built this request (the TUI's plan.Func closure, the
		// deploy confirm's own PlanRequest) and this call actually running. Building
		// RepoView{Repo: r} here — as the branch above does for req.Repo == cur.Repo — would
		// silently downgrade StartPromotion's later freshness check from CheckRepoViewCurrent
		// (the tight origin-tip comparison) to checkCloneCurrentForBase (the
		// looser local-disk check), since FromOrigin would read false. Fail closed instead: the
		// CLI path below never reaches this branch, because it never calls LoadRepo at all, so
		// cur.FromOrigin is always false there regardless of what Repo it passes.
		return PlannedChange{}, fmt.Errorf("service: Plan: the repo view changed while this plan was being built — go back and reopen the plan")
	}

	if req.Deploy != nil {
		pl, err := gitops.BuildDeployPlan(r, req.Target, *req.Deploy, s.settings.Promotable)
		if err != nil {
			return PlannedChange{}, err
		}
		WarnDeployIntoProduction(&pl, s.envsConfig())
		return PlannedChange{Plan: pl, Repo: r, View: view}, nil
	}

	opts := s.settings.Resolve
	var res *Resolution
	if len(opts.Order) > 0 {
		var err error
		res, err = s.resolveImages(ctx, r, req.Source, req.Overrides)
		if err != nil {
			return PlannedChange{}, err
		}
	}
	// Every override still gets a Resolution entry naming [override] as its source, even in
	// "digest sources: none" mode where no resolver ran at all — the override is a fact about
	// the plan, not about the resolver, and both faces show it that way (AGENTS.md §4.10).
	if len(req.Overrides) > 0 {
		if res == nil {
			res = &Resolution{}
		}
		if res.Res == nil {
			res.Res = map[string]resolve.Resolution{}
		}
		for repo, ov := range req.Overrides {
			if cur, ok := res.Res[repo]; !ok || cur.Source != resolve.SourceOverride {
				res.Res[repo] = resolve.Resolution{Repo: repo, Ref: ov, Source: resolve.SourceOverride, Detail: "caller-supplied digest"}
			}
		}
	}
	planDigests := req.Overrides
	var reasons map[string]string
	if res != nil {
		planDigests, reasons = res.digests(req.Overrides), res.reasons()
	}
	pl, err := gitops.BuildPlanWith(r, req.Source, req.Target, s.settings.Promotable, planDigests, reasons)
	if err != nil {
		return PlannedChange{}, err
	}
	if res != nil {
		// runPlan/runPromote (cmd/hoist) and the TUI plan screen all used to prepend this
		// themselves; Plan is now the one place that does it, so it is fixed by construction
		// rather than by every caller remembering to (AGENTS.md §4's Divergences, item 10).
		pl.Warnings = append(resolve.Warnings(res.Res), pl.Warnings...)
	}
	return PlannedChange{Plan: pl, Repo: r, View: view, Resolution: res, fresh: &freshInputs{digests: planDigests, reasons: reasons}}, nil
}

func (s *Service) envsConfig() config.EnvsConfig {
	if s.settings.Repo != nil {
		return s.settings.Repo.Envs
	}
	return config.EnvsConfig{}
}

// CheckOverrides refuses a --digest override BuildPlan would never consult: one for a repo
// outside the promotable prefixes (BuildPlan iterates promotable repos only, so an override for
// a third-party image would be accepted and change nothing), or one for a repo that has no
// occurrence in the source env (a typo in the repo name would plan the source env's ref instead
// of the caller's). Either way -h promises the override is planned, so silence is wrong. An
// unknown source env and an empty prefix list are left for BuildPlan to report. Moved from
// cmd/hoist's own checkOverrides unchanged, reading s.settings.Promotable rather than taking it
// as a parameter.
func (s *Service) CheckOverrides(r *gitops.Repo, from string, overrides map[string]image.Ref) error {
	if len(overrides) == 0 {
		return nil
	}
	prefixes := s.settings.Promotable
	if len(prefixes) > 0 {
		var outside []string
		for repo := range overrides {
			if !gitops.IsPromotable(repo, prefixes) {
				outside = append(outside, repo)
			}
		}
		if len(outside) > 0 {
			sort.Strings(outside)
			return fmt.Errorf("override for %s is not a promotable repo; prefixes: %s", strings.Join(outside, ", "), strings.Join(prefixes, ", "))
		}
	}
	env, ok := r.Envs[from]
	if !ok {
		return nil
	}
	present := map[string]bool{}
	for _, f := range env.Families {
		for _, o := range f.Occurrences {
			present[o.Ref.Repo] = true
		}
	}
	var missing, repos []string
	for repo := range overrides {
		if !present[repo] {
			missing = append(missing, repo)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	for repo := range present {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return fmt.Errorf("override for %s matches no image in %s; images there: %s", strings.Join(missing, ", "), from, strings.Join(repos, ", "))
}
