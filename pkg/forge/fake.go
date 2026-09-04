package forge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Forge for tests in other packages (internal/engine's resume tests in
// particular): no test anywhere points at a real GitHub repo (AGENTS.md hard constraints).
// HeadSHAs, keyed by branch name, lets a test tell the Fake what a real forge would have
// discovered on its own (the pushed tip) — Fake has no access to git, so it cannot compute
// this itself; a test that cares about PR.HeadSHA sets it before calling CreatePR.
type Fake struct {
	mu sync.Mutex

	HeadSHAs map[string]string
	// GitTags is what Tags returns; TagsErr, when set, is returned instead.
	GitTags []GitTag
	TagsErr error
	// CreateErr and FindErr, when set, are returned by every call to the matching method.
	CreateErr error
	FindErr   error
	// ChecksBySHA and ChecksErr configure Checks: a sha not present in the map reports the
	// zero CheckSummary (no checks reported at all), exactly like a real repo with no CI
	// configured; ChecksErr, when set, is returned instead for every call (simulating a 404 or
	// permissions hiccup a caller must retry rather than treat as authoritative absence).
	ChecksBySHA map[string]CheckSummary
	ChecksErr   error
	// CommentsByPR and CommentsErr configure Comments. AddComment is the intended way tests
	// populate CommentsByPR (thread-safe); the field itself may also be set directly before any
	// concurrent use begins.
	CommentsByPR map[int][]Comment
	CommentsErr  error
	// Allowed configures IsAllowedAuthor: logins mapped to true are collaborators with write
	// permission; anything absent is not. AllowedErr, when set, is returned instead — simulating
	// a token missing the scope IsAllowedAuthor needs.
	Allowed    map[string]bool
	AllowedErr error
	// MergeErr, when set, is returned by every call to MergePR regardless of head sha.
	MergeErr error

	// Calls records every method invocation, in order, for tests asserting call counts
	// ("CreatePR was called exactly once across both kill/resume attempts").
	Calls []string

	prs        []PR
	bodies     map[int]string
	nextNumber int
}

// AddComment appends c to prNumber's comments, thread-safely — the way a test simulates a
// human commenting on the PR mid-poll.
func (f *Fake) AddComment(prNumber int, c Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommentsByPR == nil {
		f.CommentsByPR = map[int][]Comment{}
	}
	f.CommentsByPR[prNumber] = append(f.CommentsByPR[prNumber], c)
}

// SetChecks records sha's CheckSummary, thread-safely — the way a test simulates CI going from
// pending to green (or to a named failure) between polls.
func (f *Fake) SetChecks(sha string, sum CheckSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ChecksBySHA == nil {
		f.ChecksBySHA = map[string]CheckSummary{}
	}
	f.ChecksBySHA[sha] = sum
}

// CreatePR implements Forge. As a safety net for tests exercising the resume property, it
// refuses to open a second open PR for a head branch that already has one — the real GitHub
// API enforces the same thing (422 "A pull request already exists"), so this is not a
// behavior unique to the fake, just enforced here too rather than only against production.
func (f *Fake) CreatePR(_ context.Context, spec PRSpec) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "CreatePR "+spec.Head)
	if f.CreateErr != nil {
		return PR{}, f.CreateErr
	}
	for _, p := range f.prs {
		if p.HeadBranch == spec.Head && !p.Merged {
			return PR{}, fmt.Errorf("forge: an open PR #%d already exists for head %s", p.Number, spec.Head)
		}
	}
	f.nextNumber++
	pr := PR{
		Number:     f.nextNumber,
		URL:        fmt.Sprintf("https://forge.example.invalid/pr/%d", f.nextNumber),
		HeadBranch: spec.Head,
		HeadSHA:    f.HeadSHAs[spec.Head],
		Base:       spec.Base,
		CreatedAt:  time.Now(),
	}
	if f.bodies == nil {
		f.bodies = map[int]string{}
	}
	f.bodies[pr.Number] = spec.Body
	f.prs = append(f.prs, pr)
	return pr, nil
}

// FindPR implements Forge: first by head branch, then by searching every stored PR's body for
// bodyMarker (mirroring the real adaptor's fallback for a renamed or recreated branch).
func (f *Fake) FindPR(_ context.Context, headBranch, bodyMarker string) (PR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "FindPR "+headBranch)
	if f.FindErr != nil {
		return PR{}, false, f.FindErr
	}
	for _, p := range f.prs {
		if p.HeadBranch == headBranch {
			return p, true, nil
		}
	}
	if bodyMarker != "" {
		// Search newest-first, as the real adaptor does, so a duplicate created in error
		// during a test resolves to the most recent one.
		ordered := append([]PR(nil), f.prs...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Number > ordered[j].Number })
		for _, p := range ordered {
			if strings.Contains(f.bodies[p.Number], bodyMarker) {
				return p, true, nil
			}
		}
	}
	return PR{}, false, nil
}

// Checks implements Forge: sha's configured CheckSummary (ChecksBySHA), or the zero value
// (no checks reported) when nothing was configured for it.
func (f *Fake) Checks(_ context.Context, sha string) (CheckSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Checks "+sha)
	if f.ChecksErr != nil {
		return CheckSummary{}, f.ChecksErr
	}
	return f.ChecksBySHA[sha], nil
}

// Comments implements Forge: prNumber's configured comments (CommentsByPR) whose CreatedAt is
// at or after since, oldest first — exactly the ordering and filter the real adaptor's own
// "since" query param gives.
func (f *Fake) Comments(_ context.Context, prNumber int, since time.Time) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("Comments %d", prNumber))
	if f.CommentsErr != nil {
		return nil, f.CommentsErr
	}
	var out []Comment
	for _, c := range f.CommentsByPR[prNumber] {
		if !c.CreatedAt.Before(since) {
			out = append(out, c)
		}
	}
	// Sorted here rather than trusting caller insertion order: the doc comment above promises
	// "oldest first", matching the real adaptor's own ordering, but a test populating
	// CommentsByPR out of chronological order (or a future concurrent AddComment) would
	// otherwise silently return them in whatever order they happened to land — this can mask a
	// real "last valid comment wins" bug at the ApprovedStep layer, since a fake in the wrong
	// order proves nothing about that logic's actual correctness. Tie-broken by ID, mirroring
	// isNewerComment's own tie-break rule in internal/engine/steps_m4.go.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// IsAllowedAuthor implements Forge against the Allowed map.
func (f *Fake) IsAllowedAuthor(_ context.Context, login string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "IsAllowedAuthor "+login)
	if f.AllowedErr != nil {
		return false, f.AllowedErr
	}
	return f.Allowed[login], nil
}

// MergePR implements Forge: squash-merges prNumber only if its recorded HeadSHA still equals
// expectedHeadSHA (simulating the real API's atomic "merge iff head is X" — Known bug classes:
// a stale head must be refused, not raced). Merging an already-merged PR returns an error, the
// same way a second real merge call against an already-merged PR would 405 — exercising the
// caller's "re-check FindPR before concluding failure" path (the named adversary: a process
// killed mid-merge-call).
func (f *Fake) MergePR(_ context.Context, prNumber int, expectedHeadSHA string) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("MergePR %d", prNumber))
	if f.MergeErr != nil {
		return PR{}, f.MergeErr
	}
	for i := range f.prs {
		if f.prs[i].Number != prNumber {
			continue
		}
		if f.prs[i].Merged {
			return PR{}, fmt.Errorf("forge: PR #%d is already merged", prNumber)
		}
		// A test that never configured HeadSHAs (most of them — HeadSHA defaults to "") hasn't
		// opted into exercising the stale-head scenario at all; only compare when the fake
		// actually has a recorded head to compare against, mirroring MergedStep's own Observe
		// pre-check (steps_m4.go), which is equally permissive about an unknown head.
		if expectedHeadSHA != "" && f.prs[i].HeadSHA != "" && f.prs[i].HeadSHA != expectedHeadSHA {
			return PR{}, fmt.Errorf("forge: PR #%d head is %s, not the expected %s: %w", prNumber, f.prs[i].HeadSHA, expectedHeadSHA, ErrStaleHead)
		}
		f.prs[i].Merged = true
		if f.prs[i].MergeSHA == "" {
			switch {
			case expectedHeadSHA != "":
				// expectedHeadSHA is the one real, resolvable git sha this call was actually
				// given — in every test that drives a genuine merge (mergeToBase pushes exactly
				// this sha onto the base, standing in for what a real GitHub squash-merge would
				// have produced), it is what really lands in the base's history. Recording it as
				// MergeSHA is what lets MergedStep.Observe's ancestry check
				// (git.Git.IsAncestor, M4 hardening finding #2) resolve to something real,
				// instead of the placeholder below, which no git object ever backs.
				f.prs[i].MergeSHA = expectedHeadSHA
			default:
				// No real sha was given (a test pre-seeding a merge without ever intending to
				// call Observe afterward) — fall back to a clearly-synthetic placeholder, same
				// as before this fix.
				f.prs[i].MergeSHA = "merged-" + f.prs[i].HeadSHA
			}
		}
		return f.prs[i], nil
	}
	return PR{}, fmt.Errorf("forge: no PR #%d", prNumber)
}

// Tags implements Forge.
func (f *Fake) Tags(_ context.Context) ([]GitTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Tags")
	if f.TagsErr != nil {
		return nil, f.TagsErr
	}
	return append([]GitTag(nil), f.GitTags...), nil
}

// PRs returns a snapshot of every PR the fake has created, for a test asserting "exactly one
// PR exists".
func (f *Fake) PRs() []PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PR(nil), f.prs...)
}

// SetHeadSHA overwrites prNumber's recorded HeadSHA — how a test simulates "something else
// moved the branch after this promotion last observed it pushed" (the stale-head-at-merge
// adversary) without needing a second real git push.
func (f *Fake) SetHeadSHA(prNumber int, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs {
		if f.prs[i].Number == prNumber {
			f.prs[i].HeadSHA = sha
			return
		}
	}
}

// SetBase is the test-only hook standing in for another actor retargeting a PR's base after
// this promotion last observed it — the same rare, real race MergedStep's own success-path base
// check (round-6 hardening) exists to catch (mirrors SetHeadSHA's own stale-head test hook).
func (f *Fake) SetBase(prNumber int, base string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs {
		if f.prs[i].Number == prNumber {
			f.prs[i].Base = base
			return
		}
	}
}

// SetClosed is the test-only hook standing in for an operator closing a PR on GitHub without
// merging it (round-9 finding: PROpenedStep must refuse to adopt one of these as satisfied).
func (f *Fake) SetClosed(prNumber int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs {
		if f.prs[i].Number == prNumber {
			f.prs[i].Closed = closed
			return
		}
	}
}
