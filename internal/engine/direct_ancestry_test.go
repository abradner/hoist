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

// failingAncestryGit is a git.Git whose IsAncestor fails the first `fail` times it is asked.
type failingAncestryGit struct {
	git.Git
	fail int
}

func (g *failingAncestryGit) IsAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	if g.fail > 0 {
		g.fail--
		return false, errors.New("git merge-base: exit status 128")
	}
	return g.Git.IsAncestor(ctx, dir, a, b)
}

// TestDirectModeAncestryFailureIsNotAVerdict (#256 item 1): a superseded direct promotion that has
// been cleaned up is judged landed by ancestry. A failing local `merge-base --is-ancestor` is
// "could not be asked", not "never pushed" — folded into the latter it let BranchedStep rebuild the
// worktree of a landed promotion. It must surface as a retryable LandingUnknownError, leave the
// worktree gone, and the next ask (which works) must read done.
func TestDirectModeAncestryFailureIsNotAVerdict(t *testing.T) {
	fx := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g := &failingAncestryGit{Git: git.Exec{}}

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
		t.Fatalf("a failed ancestry lookup recreated the worktree (stat err = %v)", serr)
	}
	done, last, err := ObserveAll(ctx(), ObserveSteps(s, g, nil, nil, nil, nil), s)
	if err != nil || !done {
		t.Fatalf("the next ask must read done: done=%v err=%v last=%s %+v", done, err, last.Step, last.Observation)
	}
}
