package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
)

// landedVerdict is what a revision of the base branch says about a promotion that already
// pushed or merged: not whether the promotion's own commit is reachable, but whether the
// change it made is still the thing that env declares.
//
// It exists because ancestry and blob equality each answer only half the question, and the
// two halves used to be conflated:
//
//   - Ancestry alone cannot see a revert. A revert commit never removes the reverted commit
//     from history, so "our commit is an ancestor of the tip" stays true forever, including
//     after someone has undone every byte of it.
//   - Blob equality alone cannot see a supersede. A later, entirely legitimate deploy into the
//     same env rewrites the same image scalars, so the planned blob is no longer at the tip —
//     which is indistinguishable, by hash, from having been reverted.
//
// Those two need opposite answers. A reverted promotion is genuinely not landed; a superseded
// one is landed and finished, and re-running it would UNDO the newer deploy. Treating
// supersede as "not landed" is what wedged spritz-staging: DirectPushedStep never went
// satisfied, so findInFlight's one-in-flight-per-env rule refused every later promotion into
// that env, permanently, with no recovery but deleting the state file by hand (#166).
type landedVerdict int

const (
	// landedIntact: every planned path is byte-identical to what this promotion planned.
	landedIntact landedVerdict = iota
	// landedSuperseded: the planned refs are gone, but every occurrence still names the same
	// IMAGE REPO at some other reference — an ordinary later deploy into the same env. This
	// promotion landed and was then legitimately replaced.
	landedSuperseded
	// landedReverted: at least one occurrence declares the exact reference this promotion
	// edited AWAY from. Nothing this promotion wrote is in effect.
	landedReverted
	// landedGone: a planned file is absent at this revision, or an occurrence no longer names
	// the promotion's image repo at all — the subject of the promotion is not there to judge.
	landedGone
)

// observeLanded reports what rev says about s's own change, reading dir's object database
// (which must already have rev — callers FetchBranch first, exactly as every other
// content-checking Observe in this package does).
//
// The returned detail is a human sentence for the Observation, naming what is actually
// declared at rev when that differs from the plan; it is never the only signal — the verdict
// is.
//
// The order of the per-occurrence checks matters and is not arbitrary: the ORIGINAL reference
// is looked for before the planned one. A file that declares both (a promotion applied to one
// occurrence but not another, or a split image repo — AGENTS.md gotcha 9) is then reported
// reverted rather than intact, which is the conservative direction: a promotion re-run against
// a partially-applied file writes the remaining occurrences, while calling it landed leaves
// them wrong forever. A no-op edit is exempt, since for those the original and the planned
// reference are the same string.
func observeLanded(ctx context.Context, g git.Git, dir, rev string, s *PromotionState) (landedVerdict, string, error) {
	intact, err := blobsIntact(ctx, g, dir, rev, s)
	if err != nil {
		return landedGone, "", err
	}
	if intact {
		return landedIntact, "", nil
	}
	byFile := map[string][]gitops.Edit{}
	for _, e := range s.Edits {
		byFile[e.File] = append(byFile[e.File], e)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var superseded []string
	for _, f := range files {
		content, ok, err := g.CatFile(ctx, dir, rev, f)
		if err != nil {
			return landedGone, "", err
		}
		if !ok {
			return landedGone, fmt.Sprintf("%s is not present at %s", f, rev), nil
		}
		for _, e := range byFile[f] {
			switch {
			case !e.NoOp() && declaresRef(content, e.Ref):
				return landedReverted, fmt.Sprintf(
					"%s still declares %s — the reference this promotion edited away from",
					f, e.Ref.String(),
				), nil
			case declaresRef(content, e.New):
				// This occurrence carries exactly what was planned. Some other file or
				// occurrence is what made the blob check fail; keep looking.
			case declaresRepo(content, e.New.Repo):
				superseded = append(superseded, fmt.Sprintf("%s declares %s", f, strings.Join(refsOf(content, e.New.Repo), ", ")))
			default:
				return landedGone, fmt.Sprintf(
					"%s no longer declares %s at all", f, e.New.Repo,
				), nil
			}
		}
	}
	if len(superseded) == 0 {
		// Every occurrence carried its planned reference even though some blob differed:
		// something else in the file changed. That is landed as far as this promotion is
		// concerned.
		return landedIntact, "", nil
	}
	sort.Strings(superseded)
	superseded = dedupe(superseded)
	return landedSuperseded, fmt.Sprintf(
		"superseded by a later change to the same image repo: %s (this promotion planned %s)",
		strings.Join(superseded, "; "), plannedRefs(s),
	), nil
}

// blobsIntact reports whether every path in s.ExpectedBlobs is byte-identical at rev to what
// this promotion planned to write. This is the fast, exact path — no file content is read at
// all when it holds.
func blobsIntact(ctx context.Context, g git.Git, dir, rev string, s *PromotionState) (bool, error) {
	paths := make([]string, 0, len(s.ExpectedBlobs))
	for p := range s.ExpectedBlobs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		blob, ok, err := g.LsTreeBlob(ctx, dir, rev, p)
		if err != nil {
			return false, err
		}
		if !ok || blob != s.ExpectedBlobs[p] {
			return false, nil
		}
	}
	return true, nil
}

// declaresRef reports whether content declares exactly ref. A full reference carries the repo,
// the tag and the digest, so a plain substring search cannot collide across repos: neither
// "ghcr.io/o/app:v1@sha256:…" nor any prefix of it appears inside "ghcr.io/o/app-web:v1@…".
func declaresRef(content []byte, ref image.Ref) bool {
	return bytes.Contains(content, []byte(ref.String()))
}

// declaresRepo reports whether content names repo as an image repo — followed by ":" (a tag)
// or "@" (a bare digest). The delimiter is what makes this safe: "ghcr.io/o/spritz" is a
// prefix of "ghcr.io/o/spritz-marketing", so a bare Contains would report the wrong repo as
// still declared and turn a repo that had been swapped out entirely into a "supersede".
func declaresRepo(content []byte, repo string) bool {
	return bytes.Contains(content, []byte(repo+":")) || bytes.Contains(content, []byte(repo+"@"))
}

// refsOf lists every distinct reference to repo in content, for the Detail line. Best effort
// and presentational only: nothing decides anything on it.
func refsOf(content []byte, repo string) []string {
	var out []string
	rest := content
	for {
		i := bytes.Index(rest, []byte(repo))
		if i < 0 {
			break
		}
		tail := rest[i:]
		end := bytes.IndexFunc(tail, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\''
		})
		if end < 0 {
			end = len(tail)
		}
		if end > len(repo) && (tail[len(repo)] == ':' || tail[len(repo)] == '@') {
			out = append(out, string(tail[:end]))
		}
		rest = tail[end:]
	}
	sort.Strings(out)
	return dedupe(out)
}

// plannedRefs names every distinct reference this promotion planned to write, for the Detail.
func plannedRefs(s *PromotionState) string {
	var out []string
	for _, e := range s.Edits {
		out = append(out, e.New.String())
	}
	sort.Strings(out)
	return strings.Join(dedupe(out), ", ")
}

// dedupe drops adjacent duplicates from an already-sorted slice.
func dedupe(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}
