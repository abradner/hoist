package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// promotionIDPattern is the shape image.PromotionID produces: the first ten characters of a
// lowercase, unpadded base32 encoding. Anything else is not an id hoist ever generated, and is
// never joined onto a path that is about to be deleted.
var promotionIDPattern = regexp.MustCompile(`^[a-z2-7]{10}$`)

// CleanupRefusedError is CleanupTarget's (and internal/service's own clone check's) answer when
// a promotion's worktree or branch is not provably hoist's to delete. It is a refusal, not a
// failure: the caller leaves everything in place and reports Reason, and the promotion's own
// outcome is unaffected.
type CleanupRefusedError struct {
	ID     string
	Reason string
}

func (e *CleanupRefusedError) Error() string {
	return fmt.Sprintf("%s: leaving its worktree and local branch in place: %s", e.ID, e.Reason)
}

// PromotionWorktree is the one worktree directory hoist may remove for promotion id:
// <CacheDir>/worktrees/<id>, derived here from the id alone. It is the single place the
// "only ever <cache>/worktrees/<id>" rule is enforced (docs/repo-map.md R-011) — every
// caller that removes a promotion's worktree gets its path from this function or from
// CleanupTarget, which calls it, and never from a state file or a directory listing.
//
// The id check is the enforcement: an id that is not the shape hoist generates is refused, so no
// separator, ".." or absolute path is ever joined onto the cache directory. Refusing a path that
// exists as a symbolic link or a plain file is a second, narrower rule — hoist only ever creates
// a real directory there, and `git worktree remove` resolves the path it is given, so a link is
// not followed to find out what it points at. The rule is about that last path element only:
// where the operator's cache directory itself lives, symlinked or not, is theirs to choose.
//
// git.RemoveWorktree's own guard (the path is not the clone, does not contain it and is not
// inside it) is a different property and stays where it is.
func PromotionWorktree(id string) (string, error) {
	if !promotionIDPattern.MatchString(id) {
		return "", &CleanupRefusedError{ID: id, Reason: fmt.Sprintf("%q is not a promotion id", id)}
	}
	dir, err := WorktreeDir(id)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		// $XDG_CACHE_HOME must be absolute (the XDG spec says a relative value is invalid). A
		// relative one would make this path mean something different in every working
		// directory, and git.RemoveWorktree's own "not the clone" guard compares paths it can
		// only relate when both are absolute.
		return "", &CleanupRefusedError{ID: id, Reason: "the hoist cache directory " + filepath.Dir(filepath.Dir(dir)) + " is not an absolute path"}
	}
	fi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		return dir, nil
	case err != nil:
		return "", err
	case fi.Mode()&os.ModeSymlink != 0:
		return "", &CleanupRefusedError{ID: id, Reason: dir + " is a symbolic link, not a worktree hoist created"}
	case !fi.IsDir():
		return "", &CleanupRefusedError{ID: id, Reason: dir + " is not a directory"}
	}
	return dir, nil
}

// CleanupTarget returns the worktree directory and local branch hoist may delete for s, both
// derived from s.ID and s.TargetEnv rather than read from s.WorktreeDir and s.Branch. Those two
// recorded fields are checked, not trusted: a state file is an index of where to look (§4.1),
// and one whose recorded worktree or branch is not the one its own id names — edited by hand,
// written under a different cache directory, or simply hostile — is refused rather than
// followed. So the worst a state file can do is name a promotion whose own worktree gets
// removed; it cannot aim the removal anywhere else.
func CleanupTarget(s *PromotionState) (dir, branch string, err error) {
	dir, err = PromotionWorktree(s.ID)
	if err != nil {
		return "", "", err
	}
	if filepath.Clean(s.WorktreeDir) != dir {
		return "", "", &CleanupRefusedError{ID: s.ID, Reason: fmt.Sprintf(
			"its state file records the worktree as %s, but this promotion's worktree is %s", s.WorktreeDir, dir)}
	}
	if s.TargetEnv == "" || strings.ContainsAny(s.TargetEnv, `/\`) {
		return "", "", &CleanupRefusedError{ID: s.ID, Reason: fmt.Sprintf("%q is not an env name", s.TargetEnv)}
	}
	if s.Base == "" || strings.HasPrefix(s.Base, "hoist/") {
		// Landing is judged against origin/<Base>. A base that is itself a promotion branch —
		// this promotion's own, in particular — makes "pushed to its branch" read as "landed on
		// its base".
		return "", "", &CleanupRefusedError{ID: s.ID, Reason: fmt.Sprintf("its state file records the base branch as %q, which is not a base a promotion lands on", s.Base)}
	}
	branch = BranchName(s.TargetEnv, s.ID)
	if s.Branch != branch {
		return "", "", &CleanupRefusedError{ID: s.ID, Reason: fmt.Sprintf(
			"its state file records the branch as %q, but this promotion's branch is %q", s.Branch, branch)}
	}
	return dir, branch, nil
}

// Landed reports whether statuses — one walk's own per-step observations, from Status or
// DriveStatus — include the landing step cleanly satisfied: MergedStep for the PR path (merged,
// not reverted, remote branch deleted), DirectPushedStep for the direct path (intact or
// superseded on the base). Nothing after that step reads the promotion's worktree or its local
// branch (§4.1), so this is the point from which both can go.
func Landed(statuses []StepStatus) bool {
	for _, st := range statuses {
		if (st.Step == StepMerged || st.Step == StepDirectPushed) && cleanlySatisfied(st.Observation) {
			return true
		}
	}
	return false
}
