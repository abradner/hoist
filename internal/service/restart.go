package service

import (
	"context"
	"fmt"
	"time"

	"github.com/abradner/hoist/internal/restart"
)

// RestartRead reads every named Deployment's current state in env, so a caller can show what
// would roll, and every reason it would not be graceful, before anything is written
// (internal/restart.Read). Restart is the one use case that writes to the cluster rather than to
// git, so it has no plan, branch, PR or state file: these three calls are the whole of it.
//
// Each of the three asks for the rollout client itself, at call time, rather than holding one:
// Rollout memoises only a success, so a cluster that was unreachable when the TUI opened is
// retried on the next R, and the error a failed call returns is the real one from the client.
func (s *Service) RestartRead(ctx context.Context, env string, names []string) (restart.Plan, error) {
	ro, err := s.Rollout(s.settings.KubeContext)
	if err != nil {
		return restart.Plan{}, err
	}
	return restart.Read(ctx, ro, env, names)
}

// RestartDo stamps the restart annotation on every target in p at at, stopping at the first
// genuine failure and returning the names it got through, in order (internal/restart.Do).
func (s *Service) RestartDo(ctx context.Context, p restart.Plan, at time.Time) ([]string, error) {
	ro, err := s.Rollout(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	return restart.Do(ctx, ro, p, at)
}

// RestartObserve reads how far each named Deployment's rollout has got against the stamp a
// restart wrote at at (internal/restart.Observe).
func (s *Service) RestartObserve(ctx context.Context, env string, names []string, at time.Time) ([]restart.Progress, error) {
	ro, err := s.Rollout(s.settings.KubeContext)
	if err != nil {
		return nil, err
	}
	return restart.Observe(ctx, ro, env, names, at)
}

// RestartInterval is how often a running restart's rollout is re-read: poll.rollout, or three
// seconds when none is configured.
func (s *Service) RestartInterval() time.Duration {
	if d := s.settings.Poll.Rollout; d > 0 {
		return d
	}
	return 3 * time.Second
}

// CheckRestartConfirmed is production's gate for a restart. §4.5's PR-and-approval-comment gate
// cannot apply to an operation that commits nothing, so what stands in its place is a second,
// distinct acknowledgement repeating the env's exact name: a production restart must never be
// one keystroke or one line of shell history. confirm is that acknowledgement, "" when none was
// given. A refusal is a *UsageError; an env the config does not list as production needs none.
//
// With no configured repo there is no envs.production list, so every env would look
// non-production by omission. That fails closed, as the direct-commit preflight does (§8: never
// provide a fallback default for required configuration) — though not outright, since restarting
// against an unconfigured repo is a legitimate thing to want: it is "we cannot tell, so
// acknowledge it".
func (s Settings) CheckRestartConfirmed(env, confirm string) error {
	if s.Repo == nil {
		if confirm != env {
			return &UsageError{Msg: fmt.Sprintf("no configured repo, so hoist cannot tell whether %s is production; pass --confirm-production=%s to restart it anyway", env, env)}
		}
		return nil
	}
	if !s.Repo.Envs.IsProduction(env) {
		return nil
	}
	if confirm == "" {
		return &UsageError{Msg: fmt.Sprintf("%s is a production env; restarting it requires --confirm-production=%s as well", env, env)}
	}
	if confirm != env {
		return &UsageError{Msg: fmt.Sprintf("--confirm-production=%q does not match --env %q; repeat the exact env to acknowledge", confirm, env)}
	}
	return nil
}
