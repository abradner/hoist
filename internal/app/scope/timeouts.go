package scope

import "time"

// Timeout budgets for the calls internal/app's screens make through DoCtx or Timeout. Each is a
// conservative ceiling on how long an operator waits before a screen says a call did not answer
// — not a target latency; the underlying call is expected to finish well inside it under normal
// conditions. Constants for now, not config keys: every
// one of these calls is already re-observed on retry (R, F5, resuming the screen), so a
// conservative constant costs an extra keypress on the rare slow call rather than a wrong
// answer, and a config key would need its own validation, docs and default before it earned its
// keep.
const (
	// Resolve bounds one plan's digest resolution + BuildPlan call (internal/app/plan's own
	// loadCmd): a cluster read plus a registry HEAD per repo, chained through pkg/resolve's
	// priority order (AGENTS.md §4.10). Also reused by internal/app's own openDeploy, whose
	// call is synchronous and pure (no cluster/registry involved) but budgeted the same way for
	// consistency rather than left unbounded. Raised from 20s to 120s:
	// the registry credential chain's `op` link can be an interactive 1Password approval
	// (§4.6/§4.10 — the same reason git's own commit step gets a "waiting for signing approval"
	// notice rather than a short deadline), and 20s was tight enough to fail resolution outright
	// on a slow approval even though the underlying call was otherwise healthy.
	Resolve = 120 * time.Second
	// Drift bounds one env's cluster read in the matrix's own fan-out (F5/Init) and the watch
	// screen's own poll — one read at a time, so a single slow env or family never delays
	// another already answered.
	Drift = 10 * time.Second
	// RefreshRepo bounds the matrix's F5 re-read of the repo (fetch + discover against a cached
	// origin/<base> view, AGENTS.md §6's own `plan --dry-run` reasoning).
	RefreshRepo = 30 * time.Second
	// History bounds one repo's commit-delta/migration fetch (internal/app/plan's historyCmds,
	// internal/app/tags' own commit history) — a forge call, potentially several pages of it.
	History = 15 * time.Second
	// RestartRead bounds reading what a restart would touch (internal/restart, live pod specs).
	RestartRead = 10 * time.Second
	// RestartDo bounds actually stamping the restart annotation on every target Deployment.
	RestartDo = 15 * time.Second
	// RestartObserve bounds one rollout-progress poll.
	RestartObserve = 10 * time.Second
	// List bounds one session.Controller listing (internal/app/session) — every promotion state
	// file re-observed against the forge and cluster. Mirrored here for documentation; the
	// controller's own Config.ListTimeout already defaults to this exact value (§4.1 — nothing
	// consults this constant directly, so the two cannot silently drift without a reviewer
	// noticing the two literals disagree). Raised from 30s to 60s: every
	// in-flight promotion's own forge/cluster re-observation happens in this one call, and 30s
	// was tight for an operator with several promotions in flight against a forge under load.
	List = 60 * time.Second
	// Abandon bounds waiting for a busy Step to notice its ctx was cancelled before
	// session.Controller.Abandon gives up and calls Backend.Abandon anyway. Mirrored here the
	// same way List is; the controller's own Config.AbandonTimeout is the value actually
	// consulted. It does NOT bound the Backend.Abandon call itself — see AbandonCall.
	Abandon = time.Second
	// AbandonCall bounds one Backend.Abandon call — a real re-observation (git ls-remote plus a
	// forge PR/check lookup) followed by closing the PR and deleting the remote branch, none of
	// which is instant. Mirrored here the same way List is; the controller's own
	// Config.AbandonCallTimeout is the value actually consulted. Deliberately generous and
	// independent of Abandon above: conflating the two (a single 1s timeout used for both the
	// busy-Step wait and the call) meant a real abandon regularly failed with "context deadline
	// exceeded" (found in review of ceaccb2).
	AbandonCall = 60 * time.Second
)
