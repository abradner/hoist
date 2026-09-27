package service

import (
	"time"

	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
	"github.com/abradner/hoist/pkg/rollout"
)

// Deps is the set of constructors a Service is built from — never the clients themselves.
// cmd/hoist's serviceDeps() wraps its own package-level test seams (newGit, newForge, ...) in
// closures that read the seam AT CALL TIME, so a test that reassigns one mid-test (about 108
// do) still sees its own fake through any Service built before or after the reassignment;
// nothing here is a snapshot taken once.
//
// None of these constructors ever see a secret value directly — that is still resolved inside
// pkg/k8s and pkg/registry exactly as it is today (AGENTS.md §4.10, R-002); Deps only decides
// which constructor runs, never what it reads.
type Deps struct {
	Git      func() git.Git
	Forge    func(ownerRepo string) (forge.Forge, error)
	Argo     func(kubeContext string) (argo.Argo, string, error)
	Rollout  func(kubeContext string) (rollout.Rollout, string, error)
	Cluster  func(kubeContext string) (k8s.Cluster, string, error)
	Registry func(registry.AuthConfig) (registry.Registry, error)
	Store    StateStore
	Now      func() time.Time

	// NoCache disables Service's own success-memoization of Forge/Argo/Rollout (service.go).
	// Set only by a test that swaps a seam (newForge, newArgo, ...) BETWEEN calls against one
	// long-lived Service instance — the TUI's own session-lifetime Service is exactly that
	// shape, and a memoized client built from the seam's first value would otherwise survive
	// the swap. Production code never sets this; see service.go's own doc comment for why
	// memoizing at all is still correct there.
	NoCache bool
}

// StateStore is what a Service reads and writes promotion state through — the interface
// FileStore below implements over engine's own state.go/claim.go functions, and the one a
// test's fake can implement instead without touching $XDG_STATE_HOME.
type StateStore interface {
	List() ([]*engine.PromotionState, error)
	ListArchived() ([]*engine.PromotionState, error)
	Load(id string) (*engine.PromotionState, error)
	Save(*engine.PromotionState) error
	Delete(id string) error
	Archive(id string) error
	Claim(repoFullName, targetEnv, id string) (release func(), err error)
	WorktreeDir(id string) (string, error)
}

// FileStore is StateStore over the real $XDG_STATE_HOME/hoist files engine/state.go and
// engine/claim.go already read and write — a thin wrapper, not a reimplementation: every
// method is exactly the existing engine call, so this changes no on-disk shape and needs no
// migration.
type FileStore struct{}

// List returns every live promotion state file, per engine.ListStates.
func (FileStore) List() ([]*engine.PromotionState, error) { return engine.ListStates() }

// ListArchived returns every archived promotion state file, per engine.ListArchivedStates.
func (FileStore) ListArchived() ([]*engine.PromotionState, error) {
	return engine.ListArchivedStates()
}

// Load reads one promotion's state file by id, per engine.LoadState. A missing file is not an
// error: it returns (nil, nil), same as engine.LoadState.
func (FileStore) Load(id string) (*engine.PromotionState, error) {
	path, err := engine.StatePath(id)
	if err != nil {
		return nil, err
	}
	return engine.LoadState(path)
}

// Save writes s's state file, per engine.SaveState.
func (FileStore) Save(s *engine.PromotionState) error {
	path, err := engine.StatePath(s.ID)
	if err != nil {
		return err
	}
	return engine.SaveState(path, s)
}

// Delete removes one promotion's state file by id, per engine.DeleteState. Idempotent: an
// already-absent file is not an error.
func (FileStore) Delete(id string) error {
	path, err := engine.StatePath(id)
	if err != nil {
		return err
	}
	return engine.DeleteState(path)
}

// Archive moves id's live state file into the archive subdirectory, per engine.ArchiveState.
func (FileStore) Archive(id string) error { return engine.ArchiveState(id) }

// Claim atomically claims repoFullName/targetEnv for promotion id, per engine.ClaimInFlight.
func (FileStore) Claim(repoFullName, targetEnv, id string) (func(), error) {
	return engine.ClaimInFlight(repoFullName, targetEnv, id)
}

// WorktreeDir is the worktree path for promotion id, per engine.WorktreeDir.
func (FileStore) WorktreeDir(id string) (string, error) { return engine.WorktreeDir(id) }
