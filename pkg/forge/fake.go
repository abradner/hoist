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
	// CreateErr and FindErr, when set, are returned by every call to the matching method.
	CreateErr error
	FindErr   error
	// Calls records every method invocation, in order, for tests asserting call counts
	// ("CreatePR was called exactly once across both kill/resume attempts").
	Calls []string

	prs        []PR
	bodies     map[int]string
	nextNumber int
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

// Checks implements Forge with a stub: no checks reported, ever. M4 extends this.
func (f *Fake) Checks(_ context.Context, sha string) (CheckSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "Checks "+sha)
	return CheckSummary{}, nil
}

// Comments implements Forge with a stub: no comments reported, ever. M4 extends this.
func (f *Fake) Comments(_ context.Context, prNumber int, _ time.Time) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("Comments %d", prNumber))
	return nil, nil
}

// PRs returns a snapshot of every PR the fake has created, for a test asserting "exactly one
// PR exists".
func (f *Fake) PRs() []PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PR(nil), f.prs...)
}
