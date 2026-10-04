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
// It and cleanupObservedLanded (the same checks, ordered for a caller that polls: List, and the
// Drive returned by StartPromotion and Resume) are the only automatic cleanups. Both re-observe
// the landing step themselves rather than accept a caller's word, a recorded Phase or a stored
// landed sha (AGENTS.md §4.1). And because
// "landed" is itself only as good as the forge lookup behind it, it then checks the worktree
// and branch themselves hold nothing that is not on the remote (locallyRemovable,
// tipIsOnTheRemote) before removing either.
//
// A *engine.CleanupRefusedError means the worktree or branch could not be shown to be hoist's to
// delete; everything is left in place.
func (s *Service) CleanupLanded(ctx context.Context, st *engine.PromotionState) ([]string, error) {
	return s.cleanupLanded(ctx, st, false)
}

// cleanupLanded is CleanupLanded with a dry-run switch: with dryRun it makes every check and
// observation a real run makes and returns the same lines worded "would …", having removed
// nothing — one code path, so the listing cannot drift from what a real run then does.
//
// Landing is asked first, so a promotion that has not landed is a silent no-op whatever state
// its worktree is in. This is the order for a caller that does not already know and
// looks once (a sweep over every state file).
func (s *Service) cleanupLanded(ctx context.Context, st *engine.PromotionState, dryRun bool) ([]string, error) {
	pending, err := s.cleanupPending(ctx, st)
	if err != nil || !pending {
		return nil, err
	}
	landed, mergedHead, err := s.observeLanding(ctx, st)
	if err != nil || !landed {
		return nil, err
	}
	tip, err := s.locallyRemovable(ctx, st)
	if err != nil {
		return nil, err
	}
	if err := s.tipIsOnTheRemote(ctx, st, tip, mergedHead); err != nil {
		return nil, err
	}
	return s.removePromotionFiles(ctx, st, removal{dryRun: dryRun, tip: tip})
}

// cleanupObservedLanded is CleanupLanded for a caller that has just observed st landed and will
// be back: Service.List, which the TUI's in-flight pane calls every poll.approval, and a Drive's
// ticks. The checks are the same ones; the order is not. Everything that can be decided without
// origin is decided first, and an answer origin was asked for is remembered, so a promotion
// that is being left in place costs a poll nothing beyond a stat and two local git commands.
func (s *Service) cleanupObservedLanded(ctx context.Context, st *engine.PromotionState) ([]string, error) {
	pending, err := s.cleanupPending(ctx, st)
	if err != nil || !pending {
		return nil, err
	}
	tip, err := s.locallyRemovable(ctx, st)
	if err != nil {
		return nil, err
	}
	if kept := s.keptAt(st.ID, tip); kept != nil {
		return nil, kept
	}
	// From here on origin is asked — the caller's own observation is not taken on trust for a
	// deletion. An answer that leaves something in place for a reason that will still hold on
	// the next look (a refusal, a removal that failed) is remembered at this branch tip. A
	// failure to ASK is not an answer and is not remembered; nor is "not landed", which is
	// what a later look is for. A fresh process remembers nothing.
	landed, mergedHead, err := s.observeLanding(ctx, st)
	if err != nil || !landed {
		return nil, err
	}
	if err := s.tipIsOnTheRemote(ctx, st, tip, mergedHead); err != nil {
		var refused *engine.CleanupRefusedError
		if errors.As(err, &refused) {
			s.keep(st.ID, tip, err)
		}
		return nil, err
	}
	lines, err := s.removePromotionFiles(ctx, st, removal{tip: tip})
	if err != nil {
		s.keep(st.ID, tip, err)
	}
	return lines, err
}

// keptCleanup is one remembered refusal or failure: the local branch tip it was judged at, and
// what was said.
type keptCleanup struct {
	tip string
	err error
}

// keptAt is what was remembered for id at this branch tip; nil when nothing was, or when the
// branch has moved since.
func (s *Service) keptAt(id, tip string) error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	k, ok := s.cleanupKept[id]
	if !ok || k.tip != tip {
		return nil
	}
	return k.err
}

func (s *Service) keep(id, tip string, err error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if s.cleanupKept == nil {
		s.cleanupKept = map[string]keptCleanup{}
	}
	s.cleanupKept[id] = keptCleanup{tip: tip, err: err}
}

// locallyRemovable is the half of "safe to remove" that needs nothing but the clone: a directory
// at the worktree path must be this clone's registered worktree, on the promotion's own branch,
// with nothing uncommitted (`git status` empty). It returns the local branch's tip ("" when
// there is no such branch) for tipIsOnTheRemote to judge.
func (s *Service) locallyRemovable(ctx context.Context, st *engine.PromotionState) (tip string, err error) {
	dir, branch, clone, err := s.cleanupTarget(st)
	if err != nil {
		return "", err
	}
	g := s.Git()
	if _, statErr := os.Lstat(dir); statErr == nil {
		on, registered, err := g.WorktreeBranch(ctx, clone, dir)
		if err != nil {
			return "", err
		}
		if !registered {
			// A directory that is there but is not this clone's worktree cannot be asked what it
			// holds, and may be another repo's live worktree for the same id (a state file naming
			// the wrong repo). Automatic cleanup leaves it.
			return "", &engine.CleanupRefusedError{ID: st.ID, Reason: dir + " exists but is not a registered worktree of " + clone}
		}
		if on != branch {
			return "", &engine.CleanupRefusedError{ID: st.ID, Reason: fmt.Sprintf("the worktree at %s is on %q, not %q", dir, on, branch)}
		}
		dirty, err := g.WorktreeDirty(ctx, dir)
		if err != nil {
			return "", err
		}
		if dirty {
			return "", &engine.CleanupRefusedError{ID: st.ID, Reason: "its worktree at " + dir + " has uncommitted changes"}
		}
	}
	tip, _, err = g.RevParse(ctx, clone, "refs/heads/"+branch)
	return tip, err
}

// tipIsOnTheRemote is the second, independent half of "safe to remove": the landing observation
// says the promotion's change is on the base; this says the local branch about to be deleted
// holds no commit that is not. tip is the commit the forge says the merged PR merged, or is
// already in origin's base branch.
//
// Without it a false "landed" was destructive. The id is deterministic, so promoting the same
// digest set into the same env a second time reuses the branch name, and MergedStep finds the
// FIRST run's merged PR (#41) while the second run's commit sits unpushed in the worktree; and
// a state file that says Direct with a commit sha of the base tip reads as landed whatever its
// worktree holds. In both the branch tip is a commit that is nowhere on the remote, which is
// exactly what this refuses to remove. Together with locallyRemovable it reads only the
// worktree and the branch themselves, so it does not depend on what a state file records beyond
// which promotion this is.
func (s *Service) tipIsOnTheRemote(ctx context.Context, st *engine.PromotionState, tip, mergedHead string) error {
	if tip == "" || (mergedHead != "" && tip == mergedHead) {
		return nil // no local branch: nothing on it to lose; or exactly what was merged
	}
	_, branch, clone, err := s.cleanupTarget(st)
	if err != nil {
		return err
	}
	g := s.Git()
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

// removal says how removeWorktreeAndBranch may remove. An automatic cleanup, and the gc sweep,
// pass the branch tip their checks were made at and no force: the worktree is removed by git's
// own non-force removal (which refuses anything modified or untracked) and the branch only if
// it is still at that tip — so a file written, or a commit made, between the check and the
// removal is refused, not lost. Force is for an abandon alone: the operator confirmed retiring
// that promotion, whatever its worktree holds.
type removal struct {
	dryRun bool
	force  bool
	tip    string
}

// removePromotionFiles is the removal itself, with no opinion about whether st has landed: its
// callers have each established why the worktree is no longer needed (the landed paths by
// observing the landing and checking the worktree and tip; Abandon by observing that nothing
// landed and holding the operator's confirmation). It never removes a worktree registered on
// any branch but the promotion's own, and deletes the branch only after the worktree that had
// it checked out is gone.
func (s *Service) removePromotionFiles(ctx context.Context, st *engine.PromotionState, how removal) ([]string, error) {
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
	if _, statErr := os.Lstat(dir); statErr == nil && !registered {
		// There, but not this clone's worktree: possibly another repo's live worktree for the
		// same id, named by a state file that has the repo wrong. Not removed on any path —
		// an abandon's confirmation is for the promotion, not for whatever sits at its path.
		return nil, &engine.CleanupRefusedError{ID: st.ID, Reason: dir + " exists but is not a registered worktree of " + clone}
	}
	return s.removeWorktreeAndBranch(ctx, clone, dir, branch, how)
}

// removeWorktreeAndBranch removes dir (a worktree of clone, or a leftover directory where one
// was) and then clone's local branch, in that order — git refuses to delete a branch that is
// still checked out. It is handed a directory and branch its caller already derived and
// checked; it derives and checks nothing itself. With dryRun it removes nothing and words the
// same lines "would …".
func (s *Service) removeWorktreeAndBranch(ctx context.Context, clone, dir, branch string, how removal) ([]string, error) {
	g := s.Git()
	var lines []string

	_, registered, err := g.WorktreeBranch(ctx, clone, dir)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Lstat(dir)
	if registered || statErr == nil {
		switch {
		case how.dryRun:
			lines = append(lines, "would remove worktree "+dir)
		case how.force:
			if err := g.RemoveWorktree(ctx, clone, dir); err != nil {
				return lines, fmt.Errorf("removing worktree %s: %w", dir, err)
			}
			lines = append(lines, "removed worktree "+dir)
		default:
			if err := g.RemoveCleanWorktree(ctx, clone, dir); err != nil {
				return lines, fmt.Errorf("removing worktree %s: %w", dir, err)
			}
			lines = append(lines, "removed worktree "+dir)
		}
	}

	if how.dryRun {
		exists, err := g.LocalBranchExists(ctx, clone, branch)
		if err != nil {
			return lines, err
		}
		if exists {
			lines = append(lines, "would delete local branch "+branch)
		}
		return lines, nil
	}
	var deleted bool
	switch {
	case how.force:
		deleted, err = g.DeleteLocalBranch(ctx, clone, branch)
	case how.tip == "":
		// The caller found no local branch when it checked; one that has appeared since was
		// not part of what it decided, and is left.
		return lines, nil
	default:
		deleted, err = g.DeleteLocalBranchAt(ctx, clone, branch, how.tip)
	}
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
	_, err := c.svc.cleanupObservedLanded(ctx, st)
	var refused *engine.CleanupRefusedError
	if err == nil || errors.As(err, &refused) {
		// Done, or never going to be: either way there is nothing to retry on the next tick.
		c.cleaned = true
	}
}
