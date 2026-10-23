package matrix

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/app/scope"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// DriftFunc reports what one env's cluster is running — the raw pod observations, keyed by
// canonical image repo, every distinct running reference kept (see Running) — asked for
// per env at boot so the matrix can say "drifted" where the manifest and the cluster
// disagree. It is not the planning resolver, which picks one digest per repo and would
// collapse a partial rollout (#122). nil means no cluster is configured and no cell is
// ever claimed drifted (nor not drifted). cmd/hoist builds it over pkg/k8s (AGENTS.md
// §4.8: this package takes a function value, never an adaptor).
type DriftFunc func(ctx context.Context, env string) (map[string][]image.Ref, error)

// DriftMsg is one env's answer. from is the scope ID of the refresh it belongs to (scope.New
// at construction and again on each refresh), so a slow answer from before a refresh (F5) or
// a previous instance of this screen is dropped rather than painting stale drift over a newer
// answer.
type DriftMsg struct {
	from    scope.ID
	env     string
	running map[string][]image.Ref
	err     error
}

// RefreshRepoFunc re-reads the repo this matrix shows — cmd/hoist's own fetch-then-discover
// against a cached origin/<base> view (#PR7), never the operator's own working tree
// (AGENTS.md §4.6). Mirrors DriftFunc's own shape: a plain function type this screen holds
// and calls (§4.8 — a screen may call a function type it's handed, just never import the
// git/network machinery behind it itself), built once by cmd/hoist and installed with
// WithRefreshRepo. The returned rev is origin/<base>'s tip at the moment this view was built
// (svc.RefreshRepo's own RepoView.SHA, #PR8) — "" when the fetch fell back to the clone, since
// there is then nothing freshly read to name.
type RefreshRepoFunc func(ctx context.Context) (*gitops.Repo, string, error)

// StampedBy implements scope.Stamped.
func (d DriftMsg) StampedBy() scope.ID { return d.from }

// RepoRefreshedMsg carries RefreshRepoFunc's answer back to Update. Gen is the scope ID of
// the askRepoRefresh call that issued it, the same way DriftMsg's own from does for the
// cluster fan-out. A Model that has not yet asked holds the zero ID, which is what an
// unstamped Gen of 0 matches.
type RepoRefreshedMsg struct {
	Gen  scope.ID
	Repo *gitops.Repo
	Rev  string
	Err  error
}

// StampedBy implements scope.Stamped.
func (r RepoRefreshedMsg) StampedBy() scope.ID { return r.Gen }

// Init asks the cluster what every env is running, one command per env, when a DriftFunc
// was supplied.
func (m Model) Init() tea.Cmd { return m.askCluster() }

// refresh starts a new drift generation (F5): every env pending again, every earlier answer
// kept on screen until its replacement lands, so a refresh never blanks the table.
func (m Model) refresh() (Model, tea.Cmd) {
	if m.drift == nil || len(m.matrix.Envs) == 0 {
		return m, nil
	}
	m.driftID = scope.New()
	m.pending = map[string]bool{}
	for _, env := range m.matrix.Envs {
		m.pending[env] = true
	}
	return m.layout(), m.askCluster()
}

// askCluster issues one command per env for the current generation.
func (m Model) askCluster() tea.Cmd {
	if m.drift == nil || len(m.matrix.Envs) == 0 {
		return nil
	}
	id, drift := m.driftID, m.drift
	timeout := m.driftTimeout
	if timeout <= 0 {
		timeout = scope.Drift
	}
	cmds := make([]tea.Cmd, 0, len(m.matrix.Envs))
	for _, env := range m.matrix.Envs {
		env := env
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := scope.Timeout(timeout)
			defer cancel()
			running, err := drift(ctx, env)
			if err != nil && errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("did not answer in %s", timeout)
			}
			return DriftMsg{from: id, env: env, running: running, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// askRepoRefresh re-reads the repo (#PR7's F5, alongside askCluster's own cluster fan-out).
func (m Model) askRepoRefresh() (Model, tea.Cmd) {
	if m.refreshRepo == nil {
		return m, nil
	}
	if m.refreshingRepo {
		m.refreshAgain = true
		return m, nil
	}
	m.refreshingRepo = true
	m.repoID = scope.New()
	id, refresh := m.repoID, m.refreshRepo
	return m, func() tea.Msg {
		ctx, cancel := scope.Timeout(scope.RefreshRepo)
		defer cancel()
		repo, rev, err := refresh(ctx)
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("did not answer in %s", scope.RefreshRepo)
		}
		return RepoRefreshedMsg{Gen: id, Repo: repo, Rev: rev, Err: err}
	}
}

// RequestRefresh is the completion-triggered refresh: exactly what F5 already
// does, exported so the root can call it when a drive lands or finishes.
func (m Model) RequestRefresh() (Model, tea.Cmd) {
	nm, driftCmd := m.refresh()
	nm, repoCmd := nm.askRepoRefresh()
	return nm, tea.Batch(driftCmd, repoCmd)
}

// WithDrift replaces the cluster-asking function.
func (m Model) WithDrift(drift DriftFunc) Model {
	m.drift = drift
	m.pending = map[string]bool{}
	if drift == nil {
		return m
	}
	for _, env := range m.matrix.Envs {
		if _, answered := m.running[env]; answered {
			continue
		}
		if _, failed := m.driftErr[env]; failed {
			continue
		}
		m.pending[env] = true
	}
	return m
}

// WithRepo replaces the *gitops.Repo the table is computed from.
func (m Model) WithRepo(repo *gitops.Repo) Model {
	if repo == nil {
		return m
	}
	m.repo = repo
	m.matrix = Order(Compute(m.repo, m.promotable, m.running), m.envs)
	m.row = clamp(m.row, max(len(m.matrix.Rows)-1, 0))
	m.col = clamp(m.col, max(len(m.matrix.Envs)-1, 0))
	return m.layout()
}

// WithRefreshRepo installs the function F5 calls to re-read the repo.
func (m Model) WithRefreshRepo(refresh RefreshRepoFunc) Model {
	m.refreshRepo = refresh
	return m
}
