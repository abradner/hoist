package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
)

// commitTimeout is the signing timeout AGENTS.md §4.6 specifies: a commit that has not
// returned after this long is treated as a retryable failure, never a crash.
const commitTimeout = 120 * time.Second

// BranchedStep ensures a linked worktree exists for the promotion, on its branch, based on
// Base.
type BranchedStep struct{ Git git.Git }

// Name implements Step.
func (BranchedStep) Name() StepName { return StepBranched }

// Observe checks the local worktree registry, not the remote: "branched" is about this
// process's own worktree, which nothing but this promotion's own runs ever create or reuse.
// A file existing at "<WorktreeDir>/.git" is not by itself proof of anything — a stale
// pointer file from an unrelated prior state, or one that happens to resolve into the right
// clone's git dir but on the wrong branch, would satisfy that check while pointing Act's
// subsequent git add/commit at the wrong repository or the wrong branch. WorktreeBranch asks
// git's own worktree registry instead: satisfied only when WorktreeDir is registered against
// CloneDir AND checked out on exactly s.Branch (Known bug classes: recovering from a stale
// directory; trusting a filesystem shape instead of the registry that shape is supposed to
// reflect).
func (b BranchedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	branch, ok, err := b.Git.WorktreeBranch(ctx, s.CloneDir, s.WorktreeDir)
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{Satisfied: false}, nil
	}
	if branch != s.Branch {
		return Observation{Satisfied: false, Detail: fmt.Sprintf("worktree at %s is registered on %q, not %q; will be recreated", s.WorktreeDir, branch, s.Branch)}, nil
	}
	return Observation{Satisfied: true, Detail: "worktree already present at " + s.WorktreeDir + " on " + s.Branch}, nil
}

// Act implements Step.
func (b BranchedStep) Act(ctx context.Context, s *PromotionState) error {
	return b.Git.Worktree(ctx, s.CloneDir, s.WorktreeDir, s.Branch, s.Base)
}

// CommittedStep applies the plan's edits to the worktree (via gitops.Apply, which re-verifies
// before every write — AGENTS.md invariant 3, "Verify runs before git add, always" — unmodified
// from M1/M2) and commits them.
type CommittedStep struct {
	Git git.Git
	// OnWaiting is called (at most once per Act) when the commit has not returned within 5s
	// — the interactive 1Password SSH-sign prompt. May be nil.
	OnWaiting func()
}

// Name implements Step.
func (CommittedStep) Name() StepName { return StepCommitted }

// Observe implements Step.
func (c CommittedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if len(s.ExpectedBlobs) == 0 {
		blobs, err := c.expectedBlobs(ctx, s)
		if err != nil {
			return Observation{}, err
		}
		s.ExpectedBlobs = blobs
	}
	sha, ok, err := c.Git.RevParse(ctx, s.WorktreeDir, "HEAD")
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{Satisfied: false}, nil
	}
	paths := make([]string, 0, len(s.ExpectedBlobs))
	for p := range s.ExpectedBlobs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		blob, ok, err := c.Git.LsTreeBlob(ctx, s.WorktreeDir, "HEAD", path)
		if err != nil {
			return Observation{}, err
		}
		if !ok || blob != s.ExpectedBlobs[path] {
			return Observation{Satisfied: false, Detail: fmt.Sprintf("HEAD %s exists but %s does not yet match the planned content", sha, path)}, nil
		}
	}
	s.CommitSHA = sha
	return Observation{Satisfied: true, Detail: "HEAD " + sha + " already matches the planned edits"}, nil
}

// Act implements Step.
func (c CommittedStep) Act(ctx context.Context, s *PromotionState) error {
	if len(s.ExpectedBlobs) == 0 {
		blobs, err := c.expectedBlobs(ctx, s)
		if err != nil {
			return err
		}
		s.ExpectedBlobs = blobs
	}

	// A prior invocation of this very step may already have called gitops.Apply against this
	// worktree and then been killed before git commit returned (§4.6: signing can hang for a
	// 1Password approval that never arrives). gitops.Apply is intentionally non-idempotent —
	// Verify requires the "before" bytes it re-parses to match what the plan recorded — so
	// calling it again on a file that already holds the "after" bytes fails with "the file
	// changed after the plan was built", which is this step's own prior success, not a real
	// conflict. Partition the edits by file and only ask Apply to touch a file whose current
	// on-disk content doesn't already hash to what ExpectedBlobs says Apply would produce for
	// it; a file already there — this promotion's own earlier Apply, or an edit that was a
	// per-occurrence no-op to begin with (gitops.Edit.NoOp) — goes straight to add+commit
	// instead of through Apply a second time.
	byFile := map[string][]gitops.Edit{}
	var files []string
	for _, e := range s.Edits {
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)

	var toApply []gitops.Edit
	var alreadyDone []string
	for _, f := range files {
		p, err := gitops.ResolvePath(s.WorktreeDir, f)
		if err != nil {
			return err
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("reading %s from the worktree: %w", f, err)
		}
		blob, err := c.Git.HashObject(ctx, s.WorktreeDir, cur)
		if err != nil {
			return err
		}
		if blob == s.ExpectedBlobs[f] {
			// Already at the content Apply would produce — nothing left to write for this
			// file, whichever of the two reasons above explains it.
			alreadyDone = append(alreadyDone, f)
			continue
		}
		toApply = append(toApply, byFile[f]...)
	}

	// gitops.Apply re-verifies before it writes each file (AGENTS.md invariant 3) — this is
	// the one call in this milestone that touches the worktree's manifest bytes, and it is
	// exactly the function M1/M2's `hoist plan` already calls, unmodified. It only sees the
	// edits for files not already in their expected final state.
	changed, err := gitops.Apply(s.WorktreeDir, toApply)
	if err != nil {
		return fmt.Errorf("applying the plan's edits: %w", err)
	}
	changed = append(changed, alreadyDone...)
	if len(changed) == 0 {
		return errors.New("no files changed; nothing to commit (the caller should have detected an all-no-op plan before starting the engine)")
	}
	sort.Strings(changed)
	sha, err := c.Git.Commit(ctx, s.WorktreeDir, s.CommitMessage, changed, commitTimeout, c.OnWaiting)
	if err != nil {
		return err
	}
	s.CommitSHA = sha
	return nil
}

// expectedBlobs computes, once, what each edited file's blob hash will be once the plan's
// edits are applied — read from the user's own clone (s.CloneDir), via the same
// gitops.ApplyBytes M1/M2 already use, so ExpectedBlobs never drifts from what Apply itself
// would write. It deliberately never reads the worktree here: once this promotion has
// committed, the worktree's copy already holds the *after* content, and a second call (a
// fresh PromotionState's first Observe, in particular) would try to re-apply the edit on top
// of its own result and fail. The clone is assumed to match Base for the files this
// promotion touches — see doc.go — and is stable across any number of calls, committed or
// not, which is what lets this be computed once and trusted from then on.
func (c CommittedStep) expectedBlobs(ctx context.Context, s *PromotionState) (map[string]string, error) {
	byFile := map[string][]gitops.Edit{}
	var files []string
	for _, e := range s.Edits {
		if _, ok := byFile[e.File]; !ok {
			files = append(files, e.File)
		}
		byFile[e.File] = append(byFile[e.File], e)
	}
	sort.Strings(files)
	out := make(map[string]string, len(files))
	for _, f := range files {
		// Read "before" from the user's own clone, never from the worktree: once this
		// promotion has committed, the worktree's copy already holds the "after" content, and
		// re-reading it here (on a fresh PromotionState that has not yet rev-parsed HEAD)
		// would try to re-apply the edit on top of its own result and fail. The clone's
		// content is what BuildPlan actually planned against (see doc.go's assumption that it
		// matches Base) and is stable across any number of Observe calls, committed or not.
		p, err := gitops.ResolvePath(s.CloneDir, f)
		if err != nil {
			return nil, err
		}
		before, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s from the clone: %w", f, err)
		}
		after, err := gitops.ApplyBytes(before, byFile[f])
		if err != nil {
			return nil, err
		}
		if err := gitops.Verify(map[string][]byte{f: before}, map[string][]byte{f: after}, byFile[f]); err != nil {
			return nil, err
		}
		// hash-object is pure content hashing; any repository works as the exec context, and
		// the clone always exists by the time this runs (unlike the worktree, which the
		// Branched step may not have created yet when Committed's own Observe runs first).
		blob, err := c.Git.HashObject(ctx, s.CloneDir, after)
		if err != nil {
			return nil, err
		}
		out[f] = blob
	}
	return out, nil
}

// PushedStep pushes the worktree's branch to origin.
type PushedStep struct{ Git git.Git }

// Name implements Step.
func (PushedStep) Name() StepName { return StepPushed }

// Observe implements Step.
func (p PushedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	remoteSHA, ok, err := p.Git.LsRemoteBranch(ctx, s.CloneDir, "origin", s.Branch)
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{Satisfied: false}, nil
	}
	if s.CommitSHA != "" && remoteSHA != s.CommitSHA {
		return Observation{Blocked: fmt.Sprintf(
			"origin/%s is already at %s, but this promotion's commit is %s — something else moved this branch; refusing to force-push. Delete or fast-forward it manually if that was intentional.",
			s.Branch, remoteSHA, s.CommitSHA,
		)}, nil
	}
	s.PushedSHA = remoteSHA
	return Observation{Satisfied: true, Detail: "origin/" + s.Branch + " is already at " + remoteSHA}, nil
}

// Act implements Step.
func (p PushedStep) Act(ctx context.Context, s *PromotionState) error {
	if err := p.Git.Push(ctx, s.WorktreeDir, "origin", s.Branch); err != nil {
		remoteSHA, ok, lerr := p.Git.LsRemoteBranch(ctx, s.CloneDir, "origin", s.Branch)
		if lerr == nil && ok && s.CommitSHA != "" && remoteSHA != s.CommitSHA {
			return fmt.Errorf("push rejected: origin/%s is at %s, this promotion's commit is %s — treating this as a real conflict, not retrying with --force: %w", s.Branch, remoteSHA, s.CommitSHA, err)
		}
		return fmt.Errorf("git push (retryable — check network and try again): %w", err)
	}
	s.PushedSHA = s.CommitSHA
	return nil
}

// PROpenedStep finds or opens the promotion's pull request.
type PROpenedStep struct{ Forge forge.Forge }

// Name implements Step.
func (PROpenedStep) Name() StepName { return StepPROpened }

// Observe implements Step.
func (p PROpenedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	pr, ok, err := p.Forge.FindPR(ctx, s.Branch, Marker(s.ID))
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{Satisfied: false}, nil
	}
	s.PR = &pr
	return Observation{Satisfied: true, Detail: fmt.Sprintf("PR #%d already exists (%s)", pr.Number, pr.URL)}, nil
}

// Act implements Step.
func (p PROpenedStep) Act(ctx context.Context, s *PromotionState) error {
	pr, err := p.Forge.CreatePR(ctx, forge.PRSpec{Title: s.PRTitle, Body: s.PRBody, Head: s.Branch, Base: s.Base})
	if err != nil {
		return err
	}
	s.PR = &pr
	return nil
}

// Steps returns the four steps, in order, wired to g and f.
func Steps(g git.Git, f forge.Forge, onWaiting func()) []Step {
	return []Step{
		BranchedStep{Git: g},
		CommittedStep{Git: g, OnWaiting: onWaiting},
		PushedStep{Git: g},
		PROpenedStep{Forge: f},
	}
}
