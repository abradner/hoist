package app

import (
	"context"

	"github.com/abradner/hoist/internal/service"
)

// Service is the root's one seam onto internal/service — narrow, consumer-side, and named for
// what the TUI actually calls, not for internal/service.Service's own full surface (AGENTS.md
// §4.8): the root depends on this interface, and cmd/hoist's
// *service.Service satisfies it directly). A test fakes it with a plain struct rather than a
// real Service backed by git/forge/Argo — the same reason flight.Driver and plan.Func are kept
// small and local instead of importing internal/service's whole client-cache type.
//
// Every method here already exists on *service.Service with this exact signature (see
// internal/service/plan.go, start.go, promotions.go, repo.go) — this interface adds nothing,
// it only narrows what the root is allowed to assume it can call.
type Service interface {
	Plan(ctx context.Context, req service.PlanRequest) (service.PlannedChange, error)
	StartPromotion(ctx context.Context, req service.StartRequest, h service.Hooks) (service.Drive, error)
	List(ctx context.Context, o service.ListOpts) ([]service.Listed, error)
	Resume(ctx context.Context, id string, o service.ResumeOpts) (service.Drive, error)
	Abandon(ctx context.Context, id string) ([]string, error)
	RefreshRepo(ctx context.Context) (service.RepoView, error)
	Repo() service.RepoView
}
