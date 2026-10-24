package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/git"
)

// TestDirectModeMissingCommitIsNotLandedNotAnError (foreman's read of #307; #311): when the image
// repo has been superseded and this clone does not have the promotion's own commit (a re-clone,
// an unpushed commit gc'd), the ancestry query fails with "not a valid commit" on every walk.
// That must read "not landed", as before #307, not a retryable error that never clears and
// keeps every later promotion out of the env.
func TestDirectModeMissingCommitIsNotLandedNotAnError(t *testing.T) {
	w := newWorld(t)
	s := w.directDeploy()
	w.supersede(supersededRef)
	s.CommitSHA = "0123456789abcdef0123456789abcdef01234567" // a commit this clone has never had
	for walk := 1; walk <= 3; walk++ {
		obs, err := (DirectPushedStep{Git: w.g}).Observe(context.Background(), s)
		if err != nil {
			t.Fatalf("walk %d: a commit the clone lacks is no landing, not an error: %v", walk, err)
		}
		if obs.Satisfied {
			t.Fatalf("walk %d: a commit the clone lacks is no evidence of landing: %+v", walk, obs)
		}
	}
}

// failingObjectExistsGit is a git.Git whose ObjectExists fails the first `fail` times it is asked.
type failingObjectExistsGit struct {
	git.Git
	fail int
}

func (g *failingObjectExistsGit) ObjectExists(ctx context.Context, dir, sha string) (bool, error) {
	if g.fail > 0 {
		g.fail--
		return false, errors.New("git cat-file: exit status 128")
	}
	return g.Git.ObjectExists(ctx, dir, sha)
}

// TestDirectModeObjectExistsFailureIsNotMissing (foreman's read of #338): when the existence
// check itself fails, nothing is known about the commit. That is "could not ask", a retryable
// LandingUnknownError, never "missing" (not landed): read as missing, it would let
// BranchedStep rebuild the worktree of a landed promotion (#256). The worktree must stay gone,
// and the next ask, which works, must read done.
func TestDirectModeObjectExistsFailureIsNotMissing(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := &failingObjectExistsGit{Git: git.Exec{}}

	s := newState(fx, wt)
	s.Direct = true
	if err := Drive(ctx(), DirectSteps(g, nil, true, nil), s, nil); err != nil {
		t.Fatalf("landing: %v", err)
	}
	supersedeBase(t, fx, "ghcr.io/example/app:v3@sha256:"+strings.Repeat("2", 64))
	removePromotionWorktree(t, s)

	g.fail = 1
	_, _, err := ObserveAll(ctx(), ObserveSteps(s, g, nil, nil, nil, nil), s)
	var unknown *LandingUnknownError
	if !errors.As(err, &unknown) || !Retryable(err) {
		t.Fatalf("err = %v (%T), want a retryable *LandingUnknownError", err, err)
	}
	if _, serr := os.Stat(wt); !os.IsNotExist(serr) {
		t.Fatalf("a failed existence check recreated the worktree (stat err = %v)", serr)
	}
	done, last, err := ObserveAll(ctx(), ObserveSteps(s, g, nil, nil, nil, nil), s)
	if err != nil || !done {
		t.Fatalf("the next ask must read done: done=%v err=%v last=%s %+v", done, err, last.Step, last.Observation)
	}
}
