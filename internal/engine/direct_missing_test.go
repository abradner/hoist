package engine

import (
	"context"
	"testing"
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
