package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/abradner/hoist/internal/engine"
)

// CleanupLanded removes promotion st's worktree and its local branch from the operator's clone
// once — and only once — st is observed landed: merged with its remote branch deleted (PR path),
// or pushed and still intact or superseded on the base (direct path). The returned lines are one
// human-readable description per thing actually removed; nil means there was nothing to do,
// whether because the promotion has not landed or because it was already clean.
//
// It is the one function every automatic cleanup goes through (listOne, and the Drive returned by
// StartPromotion and Resume), and it re-observes the landing step itself rather than
// accept a caller's word, a recorded Phase or a stored landed sha (AGENTS.md §4.1). And because
// "landed" is itself only as good as the forge lookup behind it, it then checks the worktree
// and branch themselves hold nothing that is not on the remote (nothingUnlanded) before
// removing either.
//
// A *engine.CleanupRefusedError means the worktree or branch could not be shown to be hoist's to
// delete; everything is left in place.
func (s *Service) CleanupLanded(ctx context.Context, st *engine.PromotionState) ([]string, error) {
	return s.cleanupLanded(ctx, st, false)
}

// cleanupLanded is CleanupLanded with a dry-run switch: with dryRun it makes every
// check and observation a real run makes and returns the same lines worded "would …", having
// removed nothing — one code path, so the listing cannot drift from what a real run then does.
func (s *Service) cleanupLanded(ctx context.Context, st *engine.PromotionState, dryRun bool) ([]string, error) {
	pending, err := s.cleanupPending(ctx, st)
	if err != nil || !pending {
		return nil, err
	}
	landed, mergedHead, err := s.observeLanding(ctx, st)
	if err != nil || !landed {
		return nil, err
	}
	if err := s.nothingUnlanded(ctx, st, mergedHead); err != nil {
		return nil, err
	}
	return s.removePromotionFiles(ctx, st, dryRun)
}

// nothingUnlanded is the second, independent half of "safe to remove": the landing observation
// says the promotion's change is on the base; this says the worktree and branch about to go
// hold nothing that is not. It reads the worktree and the local branch themselves, so it does
// not depend on anything a state file records beyond which promotion this is:
//
//   - the worktree, if there is one, has nothing uncommitted (`git status` is empty);
//   - the local branch's tip is either the commit the merged PR merged (as the forge reports
//     it) or already in origin's base branch.
//
// Without it a false "landed" was destructive. The id is deterministic, so promoting the same
// digest set into the same env a second time reuses the branch name, and MergedStep finds the
// FIRST run's merged PR (#41) while the second run's commit sits unpushed in the worktree; and
// a state file that says Direct with a commit sha of the base tip reads as landed whatever its
// worktree holds. In both the branch tip is a commit that is nowhere on the remote, which is
// exactly what this refuses to remove.
func (s *Service) nothingUnlanded(ctx context.Context, st *engine.PromotionState, mergedHead string) error {
	dir, branch, clone, err := s.cleanupTarget(st)
	if err != nil {
		return err
	}
	g := s.Git()
	if _, statErr := os.Lstat(dir); statErr == nil {
		_, registered, err := g.WorktreeBranch(ctx, clone, dir)
		if err != nil {
			return err
		}
		if !registered {
			// A directory that is there but is not this clone's worktree cannot be asked what it
			// holds, and may be another repo's live worktree for the same id (a state file naming
			// the wrong repo). Automatic cleanup leaves it.
			return &engine.CleanupRefusedError{ID: st.ID, Reason: dir + " exists but is not a registered worktree of " + clone}
		}
		dirty, err := g.WorktreeDirty(ctx, dir)
		if err != nil {
			return err
		}
		if dirty {
			return &engine.CleanupRefusedError{ID: st.ID, Reason: "its worktree at " + dir + " has uncommitted changes"}
		}
	}
	tip, ok, err := g.RevParse(ctx, clone, "refs/heads/"+branch)
	if err != nil || !ok {
		return err // no local branch: nothing on it to lose
	}
	if mergedHead != "" && tip == mergedHead {
		return nil
	}
	baseTip, ok, err := g.FetchBranch(ctx, clone, "origin", st.Base)
	if err != nil {
		return err
	}
	if ok {
		onBase, err := g.IsAncestor(ctx, clone, tip, baseTip)
		if err != nil {
			return err
		}
		if onBase {
			return nil
		}
	}
	return &engine.CleanupRefusedError{ID: st.ID, Reason: fmt.Sprintf(
		"its local branch %s is at %s, which is neither the commit its merged PR merged nor in origin/%s — that commit exists nowhere else",
		branch, tip, st.Base)}
}

// cleanupPending reports whether st has anything left to remove — a cheap local check (one stat,
// one show-ref) made before the landing re-observation, so a caller that polls (the TUI's
// in-flight pane) pays a forge round trip only for a promotion that actually has a worktree or
// branch left.
func (s *Service) cleanupPending(ctx context.Context, st *engine.PromotionState) (bool, error) {
	dir, branch, clone, err := s.cleanupTarget(st)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(dir); err == nil {
		return true, nil
	}
	return s.Git().LocalBranchExists(ctx, clone, branch)
}

// observeLanding asks the landing step, and only the landing step, about a copy of st — the
// caller's own state is not advanced by a cleanup. mergedHead is the commit the forge says the
// merged PR merged ("" for a direct promotion, or a forge that does not say).
func (s *Service) observeLanding(ctx context.Context, st *engine.PromotionState) (landed bool, mergedHead string, err error) {
	if !engine.LandingObservable(st) {
		return false, "", nil
	}
	cp := *st
	var step engine.Step
	if cp.Direct {
		step = engine.DirectPushedStep{Git: s.Git()}
	} else {
		f, err := s.ForgeFor(cp.RepoFullName)
		if err != nil {
			return false, "", err
		}
		step = engine.MergedStep{Forge: f, Git: s.Git()}
	}
	obs, err := step.Observe(ctx, &cp)
	if err != nil {
		return false, "", err
	}
	if !engine.Landed([]engine.StepStatus{{Step: step.Name(), Observation: obs}}) {
		return false, "", nil
	}
	if !cp.Direct && cp.PR != nil {
		mergedHead = cp.PR.HeadSHA
	}
	return true, mergedHead, nil
}

// cleanupTarget is engine.CleanupTarget plus the one check that needs configuration: the clone
// the removal runs in is the configured clone for st's repo, and st's recorded CloneDir must be
// that same directory. A state file naming some other repository is refused, not followed.
func (s *Service) cleanupTarget(st *engine.PromotionState) (dir, branch, clone string, err error) {
	dir, branch, err = engine.CleanupTarget(st)
	if err != nil {
		return "", "", "", err
	}
	clone, ok := s.cloneDirFor(st.RepoFullName)
	if !ok {
		return "", "", "", &engine.CleanupRefusedError{ID: st.ID, Reason: "repo " + st.RepoFullName + " has no configured checkout"}
	}
	if !samePath(clone, st.CloneDir) {
		return "", "", "", &engine.CleanupRefusedError{ID: st.ID, Reason: fmt.Sprintf(
			"its state file records the clone as %s, but %s is configured at %s", st.CloneDir, st.RepoFullName, clone)}
	}
	return dir, branch, clone, nil
}

// removePromotionFiles is the removal itself, with no opinion about whether st has landed: its
// two callers have each established why the worktree is no longer needed (CleanupLanded by
// observing the landing; Abandon by observing that nothing landed and holding the operator's
// confirmation). It never removes a worktree registered on any branch but the promotion's own,
// and deletes the branch only after the worktree that had it checked out is gone.
func (s *Service) removePromotionFiles(ctx context.Context, st *engine.PromotionState, dryRun bool) ([]string, error) {
	dir, branch, clone, err := s.cleanupTarget(st)
	if err != nil {
		return nil, err
	}
	on, registered, err := s.Git().WorktreeBranch(ctx, clone, dir)
	if err != nil {
		return nil, err
	}
	if registered && on != branch {
		return nil, &engine.CleanupRefusedError{ID: st.ID, Reason: fmt.Sprintf("the worktree at %s is on %q, not %q", dir, on, branch)}
	}
	return s.removeWorktreeAndBranch(ctx, clone, dir, branch, dryRun)
}

// removeWorktreeAndBranch removes dir (a worktree of clone, or a leftover directory where one
// was) and then clone's local branch, in that order — git refuses to delete a branch that is
// still checked out. It is handed a directory and branch its caller already derived and
// checked; it derives and checks nothing itself. With dryRun it removes nothing and words the
// same lines "would …".
func (s *Service) removeWorktreeAndBranch(ctx context.Context, clone, dir, branch string, dryRun bool) ([]string, error) {
	g := s.Git()
	var lines []string

	_, registered, err := g.WorktreeBranch(ctx, clone, dir)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Lstat(dir)
	if registered || statErr == nil {
		if dryRun {
			lines = append(lines, "would remove worktree "+dir)
		} else {
			if err := g.RemoveWorktree(ctx, clone, dir); err != nil {
				return lines, fmt.Errorf("removing worktree %s: %w", dir, err)
			}
			lines = append(lines, "removed worktree "+dir)
		}
	}

	if dryRun {
		exists, err := g.LocalBranchExists(ctx, clone, branch)
		if err != nil {
			return lines, err
		}
		if exists {
			lines = append(lines, "would delete local branch "+branch)
		}
		return lines, nil
	}
	deleted, err := g.DeleteLocalBranch(ctx, clone, branch)
	if err != nil {
		return lines, fmt.Errorf("deleting local branch %s: %w", branch, err)
	}
	if deleted {
		lines = append(lines, "deleted local branch "+branch)
	}
	return lines, nil
}

// cloneDirFor is the configured checkout for repoFullName: this run's own selected repo when it
// is the one asked about, else that repo's config entry.
func (s *Service) cloneDirFor(repoFullName string) (string, bool) {
	if r := s.settings.Repo; r != nil && r.GitHub == repoFullName && s.settings.RepoDir != "" {
		return s.settings.RepoDir, true
	}
	if rc, ok := RepoConfigFor(s.settings.Config, repoFullName); ok && rc.Dir != "" {
		return rc.Dir, true
	}
	return "", false
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return resolvedPath(a) == resolvedPath(b)
}

func resolvedPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// cleaningDrive is the Drive StartPromotion and Resume hand back: the Driver they built, plus
// the cleanup of the promotion's worktree and local branch at the first tick that shows it
// landed. It wraps rather than edits the Driver so that driving stays one concern and tidying up
// another; the promotion's outcome never depends on the cleanup, whose failure is deliberately
// not surfaced here — the next listing re-observes and retries it.
type cleaningDrive struct {
	Drive
	svc *Service

	mu      sync.Mutex
	cleaned bool
}

func (c *cleaningDrive) Step(ctx context.Context) (Tick, error) {
	t, err := c.Drive.Step(ctx)
	c.afterTick(ctx, t)
	return t, err
}

func (c *cleaningDrive) Run(ctx context.Context, h RunHooks) error {
	onTick := h.OnTick
	h.OnTick = func(t Tick) {
		c.afterTick(ctx, t)
		if onTick != nil {
			onTick(t)
		}
	}
	err := c.Drive.Run(ctx, h)
	if err == nil {
		// Run returns nil only when the promotion is fully done, which a last tick that was
		// never Waiting does not report through OnTick.
		st := c.State()
		c.clean(ctx, &st)
	}
	return err
}

func (c *cleaningDrive) afterTick(ctx context.Context, t Tick) {
	if engine.Landed(t.Statuses) {
		st := t.State
		c.clean(ctx, &st)
	}
}

func (c *cleaningDrive) clean(ctx context.Context, st *engine.PromotionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cleaned {
		return
	}
	_, err := c.svc.CleanupLanded(ctx, st)
	var refused *engine.CleanupRefusedError
	if err == nil || errors.As(err, &refused) {
		// Done, or never going to be: either way there is nothing to retry on the next tick.
		c.cleaned = true
	}
}
