package plan

import (
	"context"

	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/resolve"
)

// fakePlanFunc adapts a resolution-only fake (what this package's tests faked directly before
// service:Plan, PR B, moved BuildPlanWith itself into internal/service) into a full Func: it
// runs the fake, then BuildPlanWith over req, replicating service.Plan's own digests/reasons/
// warnings assembly exactly — so a unit test here fakes resolution alone (no cluster, no
// registry) without depending on a real service.Service. has is false for "digest sources:
// none" (the fake's zero-value shortcut): the returned PlannedChange then carries a nil
// Resolution, matching what Plan itself returns in that mode.
func fakePlanFunc(promotable []string, fn func(ctx context.Context, req service.PlanRequest) (res service.Resolution, has bool, err error)) Func {
	return func(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		res, has, err := fn(ctx, req)
		if err != nil {
			return service.PlannedChange{}, err
		}
		var resPtr *service.Resolution
		if has {
			resPtr = &res
		}
		// Mirrors service.Plan's own override patch: every override gets a Resolution entry
		// naming [override], even when the fake never ran a real resolution (has == false).
		if len(req.Overrides) > 0 {
			if resPtr == nil {
				resPtr = &service.Resolution{}
			}
			if resPtr.Res == nil {
				resPtr.Res = map[string]resolve.Resolution{}
			}
			for repo, ov := range req.Overrides {
				if cur, ok := resPtr.Res[repo]; !ok || cur.Source != resolve.SourceOverride {
					resPtr.Res[repo] = resolve.Resolution{Repo: repo, Ref: ov, Source: resolve.SourceOverride, Detail: "caller-supplied digest"}
				}
			}
		}
		digests := req.Overrides
		var reasons map[string]string
		if resPtr != nil {
			digests = resolve.Digests(resPtr.Res)
			for repo, ref := range req.Overrides {
				digests[repo] = ref
			}
			reasons = resolve.Reasons(resPtr.Res)
		}
		pl, err := gitops.BuildPlanWith(req.Repo, req.Source, req.Target, promotable, digests, reasons)
		if err != nil {
			return service.PlannedChange{}, err
		}
		if resPtr != nil {
			pl.Warnings = append(resolve.Warnings(resPtr.Res), pl.Warnings...)
		}
		return service.PlannedChange{Plan: pl, Repo: req.Repo, Resolution: resPtr}, nil
	}
}

// noneFunc is the "digest sources: none" fake: BuildPlanWith runs from the manifests alone, no
// Resolution attached — the shape a nil resolve func used to mean before plan.Func replaced it.
func noneFunc(promotable []string) Func {
	return fakePlanFunc(promotable, func(context.Context, service.PlanRequest) (service.Resolution, bool, error) {
		return service.Resolution{}, false, nil
	})
}

// errFunc is a Func that always fails outright, for the "resolution failed" screen tests.
func errFunc(err error) Func {
	return func(context.Context, service.PlanRequest) (service.PlannedChange, error) {
		return service.PlannedChange{}, err
	}
}

// viewFunc wraps noneFunc so the returned PlannedChange carries a caller-chosen, non-zero View —
// for tests pinning the loadedMsg.view -> Model.view -> StartMsg.View plumbing (t1-review.md P2-a
// follow-through, PR #202 review): noneFunc alone always leaves View at its zero value, which is
// indistinguishable from the bug under test (a dropped view assignment) without this.
func viewFunc(promotable []string, view service.RepoView) Func {
	inner := noneFunc(promotable)
	return func(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error) {
		pc, err := inner(ctx, req)
		if err != nil {
			return pc, err
		}
		pc.View = view
		return pc, nil
	}
}
