package engine

// M4 adds three steps to the promotion pipeline (AGENTS.md §1: "CI must go green ... a human
// must approve ... then the PR merges"): CIGreen, Approved, Merged, wired after PROpened by
// AllSteps. All three are, in Drive's terms, mostly Observe: there is nothing for hoist itself
// to *do* about CI running or a human commenting — the actual waiting is a poll loop in the CLI
// driver calling Observe repeatedly with a sleep, never inside a Step's own Act (AGENTS.md
// invariant 4). Act exists on all three for interface symmetry and because Merged's Act does
// have real work (the merge call itself, and branch cleanup); CIGreen's and Approved's Act are
// no-ops, confirmed against the existing Step shape rather than assumed — nothing about
// Observe/Act as a pair requires Act to do anything.
//
// CIGreenStep still anchors its grace period to forge.PR.CreatedAt (the PROpened step's own
// recorded timestamp): this repo's flow guarantees the branch is exclusively this promotion's
// own (PushedStep's Observe already Blocks if origin's tip ever disagrees with what this
// promotion pushed) and the PR is only ever opened *after* that one push completes (AllSteps'
// order: Pushed before PROpened) — so no check-run can predate PR.CreatedAt without also
// predating the one commit it would have to be about. That is a narrower claim than "any
// GitHub PR's CreatedAt is a safe anchor" (it is not, in general — a PR can be opened and its
// branch force-pushed later, which is exactly the re-anchor trap docs/pr-review-machinery.md
// describes for review threads) — it holds here specifically because of the ordering and
// exclusivity invariants above.
//
// ApprovedStep anchors on the head commit's own committer date instead (git.Git.CommitTime),
// not PR.CreatedAt: PR.CreatedAt would be correct too, for the same reason CIGreenStep's use of
// it is correct above, but it would be correct only *because of* those other steps' checks —
// not a direct fact about the commit being approved. CommitTime reads the real thing an
// approval comment has to postdate, so ApprovedStep's own doc comment can state the anchor
// without leaning on Pushed's and Merged's checks to make an earlier PR unreachable.
import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
)

// The three M4 steps, run after PROpened in that order (AllSteps): CI must go green before a
// human approval is even asked for, and merging only ever follows both.
const (
	StepCIGreen  StepName = "ci-green"
	StepApproved StepName = "approved"
	StepMerged   StepName = "merged"
)

// approvalAuto mirrors internal/config.ApprovalAuto by value, not by import:
// PromotionState.Approval is carried in as a plain string by the CLI (see state.go), and
// internal/engine deliberately does not import internal/config so that a step's Observe never
// has an ambient way to re-read policy behind the CLI's back (every consumer of policy must go
// through the value the promotion started with). Every other value (config.ApprovalComment,
// "", or anything else) requires a comment — fail closed on an unrecognised value rather than
// silently skipping approval.
const approvalAuto = "auto"

// sinceSlop is subtracted from ApprovedStep's own anchor before it is passed to
// Forge.Comments' since parameter (M4 hardening, see ApprovedStep.Observe) — a small,
// deliberately cheap safety margin against "since" turning out to be an exclusive lower bound
// server-side, never load-bearing for correctness (the local re-check enforces the exact
// anchor regardless of what this buys).
const sinceSlop = time.Second

// approveRe and rejectRe match AGENTS.md's magic comment, verbatim: an optional leading "/", the
// literal command and this promotion's id, optional surrounding whitespace, case-insensitive,
// and nothing else on the line — matched against each line of a comment's body independently
// (matchesCommand), not the whole multi-line body as one string, so a real approval sitting
// alongside other prose in the same comment still matches.
func approveRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*/?hoist\s+approve\s+` + regexp.QuoteMeta(id) + `\s*$`)
}

func rejectRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\s*/?hoist\s+reject\s+` + regexp.QuoteMeta(id) + `\s*$`)
}

func matchesCommand(re *regexp.Regexp, body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if re.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// isNewerComment reports whether candidate is strictly newer than current (nil current always
// loses). Primarily by CreatedAt; on an exact tie — GitHub's comment timestamp precision can
// collide, so an approve and a later reject can genuinely share one recorded CreatedAt — by
// forge.Comment.ID instead, preferring the numerically larger one. GitHub assigns comment IDs
// from a single, strictly increasing sequence at creation time (never reused, never assigned
// out of post order — this is the same ordering FindPR's own body-marker fallback and the
// PR-number sequence itself already rely on being monotonic), so on a timestamp tie a larger ID
// reliably means "posted later" even when CreatedAt cannot tell the two apart. Used both while
// scanning for the newest approve/reject of each kind and for the final approve-vs-reject
// comparison, so a tie is broken exactly the same way in both places.
func isNewerComment(candidate, current *forge.Comment) bool {
	if current == nil {
		return true
	}
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.ID > current.ID
	}
	return false
}

// CIGreenStep is satisfied once the pushed head sha's checks are all green, under the
// configured ci.none policy when nothing has been reported at all (R-003).
type CIGreenStep struct {
	Forge forge.Forge
	// Now is injectable so tests can assert the grace-period boundary precisely; nil means
	// time.Now.
	Now func() time.Time
}

// Name implements Step.
func (CIGreenStep) Name() StepName { return StepCIGreen }

func (c CIGreenStep) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Observe implements Step. total>0 && pending==0 && failure==0 && skipped==0 is satisfied
// (invariant 1's condition, extended: see below); failure>0 is always Blocked, named by
// check-run name when the forge can give one; skipped>0 is likewise always Blocked — a `skipped`
// conclusion means a check-run never actually ran at all (a path filter, a conditional job), and
// forge.CheckSummary carries no required-vs-optional distinction that would let this step tell
// "safely skipped" from "a required gate that silently never ran", so a skipped run is treated
// as a hard gate, not silently folded into green (AGENTS.md §2 principle 5's own stated
// exception: "warn, don't block, except where the runbook blocks" — a required check skipped out
// from under a promotion is exactly that case). total==0 is a grace-period Waiting, then the
// ci.none policy: green satisfies, prompt Blocks with an override path (CINoneOverride, set by
// `hoist resume --override-ci-none`), block Blocks with none at all — an operator who chose
// block gets no in-band bypass, only "wait for real checks" or "change ci.none and resume",
// which is the entire point of choosing the stricter of the two non-green policies over the
// milder one.
func (c CIGreenStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.PR == nil {
		return Observation{Satisfied: false}, nil
	}
	sha := s.PushedSHA
	if sha == "" {
		sha = s.CommitSHA
	}
	sum, err := c.Forge.Checks(ctx, sha)
	if err != nil {
		// Known bug classes: a 404 or permissions hiccup must be retried, never read as "zero
		// checks reported" — returning the error here (rather than a zero CheckSummary with a
		// nil error) is what keeps that distinction all the way to the CLI's poll loop, which
		// retries transient Observe errors rather than treating them as authoritative absence.
		return Observation{}, fmt.Errorf("checking CI status for %s: %w", sha, err)
	}
	if sum.Failure > 0 || sum.Skipped > 0 {
		var parts []string
		if sum.Failure > 0 {
			detail := fmt.Sprintf("%d of %d checks failed", sum.Failure, sum.Total)
			if len(sum.FailedNames) > 0 {
				names := append([]string(nil), sum.FailedNames...)
				sort.Strings(names)
				detail += ": " + strings.Join(names, ", ")
			}
			parts = append(parts, detail)
		}
		if sum.Skipped > 0 {
			detail := fmt.Sprintf("%d of %d checks were skipped (never ran)", sum.Skipped, sum.Total)
			if len(sum.SkippedNames) > 0 {
				names := append([]string(nil), sum.SkippedNames...)
				sort.Strings(names)
				detail += ": " + strings.Join(names, ", ")
			}
			parts = append(parts, detail)
		}
		return Observation{Blocked: strings.Join(parts, "; ")}, nil
	}
	if sum.Total > 0 {
		if sum.Pending > 0 {
			return Observation{Waiting: true, Detail: fmt.Sprintf("CI: %d/%d checks complete", sum.Total-sum.Pending, sum.Total)}, nil
		}
		return Observation{Satisfied: true, Detail: fmt.Sprintf("CI green (%d checks)", sum.Total)}, nil
	}
	elapsed := c.now().Sub(s.PR.CreatedAt)
	if elapsed < s.CIGrace {
		return Observation{Waiting: true, Detail: fmt.Sprintf("no checks reported yet (%s of %s grace elapsed)", elapsed.Round(time.Second), s.CIGrace)}, nil
	}
	switch s.CINone {
	case "block":
		return Observation{Blocked: "no checks reported after the grace period and ci.none=block; hoist has no override for block — wait for real checks, or change ci.none to prompt/green in config and re-run"}, nil
	case "green":
		return Observation{Satisfied: true, Detail: "no checks reported after the grace period; ci.none=green"}, nil
	default: // "prompt", and any empty value a caller forgot to fill from Normalize's default
		if s.CINoneOverride {
			return Observation{Satisfied: true, Detail: "no checks reported after the grace period; overridden via --override-ci-none"}, nil
		}
		return Observation{Blocked: "no checks reported after the grace period; ci.none=prompt requires an explicit override — re-run `hoist resume " + s.ID + " --override-ci-none`"}, nil
	}
}

// Act implements Step: nothing to do. CI runs itself; hoist only observes it.
func (CIGreenStep) Act(context.Context, *PromotionState) error { return nil }

// ApprovedStep enforces R-001: the author of an approval comment is checked by GitHub login via
// the API, never the comment body, and bots are excluded (invariant 2).
type ApprovedStep struct {
	Forge forge.Forge
	Git   git.Git
}

// Name implements Step.
func (ApprovedStep) Name() StepName { return StepApproved }

// Observe implements Step. An env whose approval mode is "auto" is satisfied immediately (no
// comment required) — ApprovedStep trusts RepoConfig.Approval's own guarantee that a production
// env can never resolve to auto by a config default (§4.5) rather than re-deriving it, since
// duplicating that rule here would be the same enforcement in two places with no way to keep
// them in sync (AGENTS.md §8, "layered checks"). Otherwise: the newest allowed-author comment
// matching approve or reject, posted at or after the head commit's own committer date (this
// type's own doc comment above explains the anchor), decides it — reject wins when it is the
// newer of the two (Known bug classes: a correctly-typed reject after a correctly-typed approve
// must win; a typo'd reject must never match at all, since the pattern is exact).
func (a ApprovedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	if s.Approval == approvalAuto {
		return Observation{Satisfied: true, Detail: "approval mode is auto for " + s.TargetEnv + "; no comment required"}, nil
	}
	if s.PR == nil {
		return Observation{Satisfied: false}, nil
	}
	since, err := a.Git.CommitTime(ctx, s.CloneDir, s.CommitSHA)
	if err != nil {
		return Observation{}, fmt.Errorf("reading commit time for %s: %w", s.CommitSHA, err)
	}
	// Bounding Comments' own fetch to since-sinceSlop (rather than time.Time{}, "everything")
	// cuts API/pagination load on a long-lived, noisy PR (M4 hardening) — but it is purely an
	// efficiency narrowing, never the actual enforcement: every comment returned is still
	// re-checked against the exact anchor (since, unslopped) in the loop below. sinceSlop exists
	// because GitHub's own docs describe "since" as "last updated after the given time" — read
	// literally that is exclusive (>), which would let the API itself silently drop a comment
	// posted in the same instant as since, before this step ever saw it at all; the local
	// re-check can't recover a comment the API never returned. Padding the query backward by a
	// second costs a little extra fetch on an old PR and loses nothing, since the local filter
	// still enforces the true boundary exactly.
	comments, err := a.Forge.Comments(ctx, s.PR.Number, since.Add(-sinceSlop))
	if err != nil {
		return Observation{}, fmt.Errorf("listing PR #%d comments: %w", s.PR.Number, err)
	}
	aRe, rRe := approveRe(s.ID), rejectRe(s.ID)
	// allowedCache memoizes isAllowed per author login for this Observe call only: a PR with
	// many comments from the same handful of authors would otherwise repeat the same
	// collaborator-permission API lookup (Forge.IsAllowedAuthor) once per comment, risking
	// avoidable rate-limit pressure. Scoped to this call's stack frame, never a field on
	// ApprovedStep or PromotionState — permissions can change between polls, so nothing here
	// persists across separate Observe invocations (a config-side login is already covered by
	// s.Approvers, which needs no lookup at all).
	allowedCache := map[string]bool{}
	var lastApprove, lastReject *forge.Comment
	for i := range comments {
		c := &comments[i]
		if c.CreatedAt.Before(since) {
			// Known bug classes: posted before the head commit existed — it approved a
			// different, superseded diff. Never satisfies, however it reads.
			continue
		}
		if c.AuthorType == "Bot" {
			continue
		}
		allowed, ok := allowedCache[c.Author]
		if !ok {
			var aerr error
			allowed, aerr = a.isAllowed(ctx, c.Author, s)
			if aerr != nil {
				return Observation{}, fmt.Errorf("checking whether %s may approve #%d: %w", c.Author, s.PR.Number, aerr)
			}
			allowedCache[c.Author] = allowed
		}
		if !allowed {
			continue
		}
		switch {
		case matchesCommand(aRe, c.Body):
			if isNewerComment(c, lastApprove) {
				lastApprove = c
			}
		case matchesCommand(rRe, c.Body):
			if isNewerComment(c, lastReject) {
				lastReject = c
			}
		}
	}
	switch {
	case lastReject != nil && isNewerComment(lastReject, lastApprove):
		return Observation{Blocked: fmt.Sprintf("rejected by %s at %s", lastReject.Author, lastReject.CreatedAt.Format(time.RFC3339))}, nil
	case lastApprove != nil:
		return Observation{Satisfied: true, Detail: "approved by " + lastApprove.Author}, nil
	default:
		return Observation{Waiting: true, Detail: "waiting for `hoist approve " + s.ID + "` from an approver"}, nil
	}
}

// isAllowed is R-001's author check: named in RepoConfig.Approvers (case-insensitive — GitHub
// logins are), or, when the repo opts in (RepoConfig.Collaborators), a write-or-higher
// collaborator (write, maintain, or admin) per the forge's own permission API. A permission-scope
// error from IsAllowedAuthor is returned, never folded into false (Known bug classes: "don't
// silently deny on 403").
func (a ApprovedStep) isAllowed(ctx context.Context, login string, s *PromotionState) (bool, error) {
	for _, ap := range s.Approvers {
		if strings.EqualFold(ap, login) {
			return true, nil
		}
	}
	if !s.Collaborators {
		return false, nil
	}
	return a.Forge.IsAllowedAuthor(ctx, login)
}

// Act implements Step: nothing to do. An approval is a human commenting, not this step's job.
func (ApprovedStep) Act(context.Context, *PromotionState) error { return nil }

// MergedStep enforces R-003's neighbor: merge only once Approved is satisfied (guaranteed by
// AllSteps' ordering, never re-checked here — AGENTS.md §8, layered checks) and refuse a stale
// head using the forge's own atomic "merge iff head is X" (Known bug classes: no client-side
// check-then-merge race). Observe reports done only once the PR is both merged *and* its branch
// is gone, so a process killed between a successful merge and the branch delete resumes into
// Act again rather than reporting done prematurely; Act itself re-checks FindPR before treating
// a failed MergePR call as a real failure (the named adversary: "did the merge actually happen
// server-side even though the client never saw the response").
type MergedStep struct {
	Forge forge.Forge
	Git   git.Git
}

// Name implements Step.
func (MergedStep) Name() StepName { return StepMerged }

// Observe implements Step.
func (m MergedStep) Observe(ctx context.Context, s *PromotionState) (Observation, error) {
	pr, ok, err := m.Forge.FindPR(ctx, s.Branch, Marker(s.ID))
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return Observation{Satisfied: false}, nil
	}
	// A PR found by head branch name alone that targets a different base than this promotion is
	// not this promotion's own (M4 hardening, belt-and-suspenders: PROpenedStep's own Observe
	// already refuses to adopt one — steps.go — so reaching this with a mismatched s.PR would
	// mean that check was bypassed, or an older PromotionState predating it was carried
	// forward). Refuse to merge rather than assume a name match is enough.
	if pr.Base != s.Base {
		return Observation{Blocked: fmt.Sprintf(
			"PR #%d for branch %s targets base %q, not %q — refusing to merge a PR aimed at a different base",
			pr.Number, s.Branch, pr.Base, s.Base,
		)}, nil
	}
	s.PR = &pr
	if !pr.Merged {
		expected := s.PushedSHA
		if expected == "" {
			expected = s.CommitSHA
		}
		if expected != "" && pr.HeadSHA != "" && pr.HeadSHA != expected {
			return Observation{Blocked: fmt.Sprintf(
				"PR #%d's head is now %s, but this promotion last observed %s pushed — something else moved the branch; refusing to merge",
				pr.Number, pr.HeadSHA, expected,
			)}, nil
		}
		return Observation{Satisfied: false}, nil
	}
	s.MergeSHA = pr.MergeSHA
	_, branchStillThere, err := m.Git.LsRemoteBranch(ctx, s.CloneDir, "origin", s.Branch)
	if err != nil {
		return Observation{}, err
	}
	if branchStillThere {
		return Observation{Satisfied: false, Detail: "merged as " + pr.MergeSHA + "; branch not yet deleted"}, nil
	}
	return Observation{Satisfied: true, Detail: "merged as " + pr.MergeSHA + "; branch deleted"}, nil
}

// Act implements Step.
func (m MergedStep) Act(ctx context.Context, s *PromotionState) error {
	if s.PR == nil {
		return fmt.Errorf("merging: no PR recorded on this promotion")
	}
	if !s.PR.Merged {
		expected := s.PushedSHA
		if expected == "" {
			expected = s.CommitSHA
		}
		pr, err := m.Forge.MergePR(ctx, s.PR.Number, expected)
		if err != nil {
			fresh, ok, ferr := m.Forge.FindPR(ctx, s.Branch, Marker(s.ID))
			if ferr == nil && ok && fresh.Merged {
				s.PR = &fresh
			} else {
				return fmt.Errorf("merging PR #%d: %w", s.PR.Number, err)
			}
		} else {
			s.PR = &pr
		}
		s.MergeSHA = s.PR.MergeSHA
	}
	if err := m.Git.DeleteRemoteBranch(ctx, s.CloneDir, "origin", s.Branch); err != nil {
		return fmt.Errorf("merge succeeded (%s) but deleting origin/%s failed, will retry: %w", s.MergeSHA, s.Branch, err)
	}
	return nil
}

// AllSteps returns every step a promotion drives through, in order: Steps' four (branch,
// commit, push, PR) plus CIGreen, Approved and Merged. Kept separate from Steps itself so M3's
// own resume tests (which exercise exactly the four-step property) keep working unchanged —
// `hoist promote` and `hoist resume` always drive AllSteps now (the issue's own "done when":
// promote runs the full pipeline, not just to PROpened).
func AllSteps(g git.Git, f forge.Forge, onWaiting func()) []Step {
	return append(Steps(g, f, onWaiting), CIGreenStep{Forge: f}, ApprovedStep{Forge: f, Git: g}, MergedStep{Forge: f, Git: g})
}
