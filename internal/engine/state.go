package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
)

// PromotionState is everything one promotion's steps read and write. It is JSON-serialised
// to a state file that AGENTS.md §4.1 calls "an index of what to look at, never evidence of
// what happened": every field an Observe method reads to decide truth also exists
// independently on the remote (the branch, the commit, the PR) or is deterministically
// re-derivable from the CLI's own inputs (CloneDir, Base, Edits, the rendered messages) — so
// deleting this file and rebuilding PromotionState the same way (same --repo/--from/--to and
// the same resolved digests) reproduces the same id, branch and marker, and Observe finds the
// same already-satisfied steps on the remote. See DeriveID's doc comment and the CLI wiring
// in cmd/hoist for exactly what is recomputed versus loaded.
//
// Beyond the brief's listed shape, this adds the fields Act needs to do its work — CloneDir,
// WorktreeDir, Base, Edits, CommitMessage, PRTitle, PRBody — since a step cannot act on a
// promotion it cannot locate or a Plan it does not carry. None of them are secret or
// unbounded (AGENTS.md §4.3): they are local paths, a branch name, and rendered text already
// destined for a public commit/PR.
type PromotionState struct {
	ID, RepoFullName, SourceEnv, TargetEnv string
	Branch                                 string
	ExpectedBlobs                          map[string]string // repo-relative path -> expected git blob hash after Apply
	CommitSHA, PushedSHA                   string
	PR                                     *forge.PR
	// Direct records that this promotion was started with --direct (M6): committed straight to
	// Base with no branch pushed to origin and no PR. Every other field is identical between
	// the two modes and nothing else can tell them apart after the fact — DirectPushedStep
	// pushes to Base, so Branch is set but was never pushed, which is indistinguishable from a
	// PR promotion that died before PushedStep ran. Without this field, resuming a direct
	// promotion re-observes it through AllSteps, finds Branch missing on origin, and pushes it
	// and opens a PR — the exact thing the operator asked not to happen (Codex review, PR #43).
	Direct      bool
	Phase       StepName // an index/hint only — Observe never trusts it, see AGENTS.md §4.1
	History     []HistoryEntry
	GeneratedAt time.Time

	// CloneDir is the user's own clone (--repo). WorktreeDir is the linked worktree this
	// promotion's own steps operate in, under $XDG_CACHE_HOME/hoist/worktrees/<id>. Base is
	// the branch the worktree and the PR are based on (the clone's default branch).
	CloneDir, WorktreeDir, Base string

	// Edits are the gitops.Plan's edits this promotion writes, unmodified from what M1/M2's
	// BuildPlan produced (AGENTS.md invariant 3 — this milestone never re-implements or
	// bypasses gitops.Apply/Verify).
	Edits []gitops.Edit

	// CommitMessage, PRTitle and PRBody are rendered once (identity.go/template.go) from the
	// plan and id, then carried here so a resumed run acts on exactly the same text it would
	// have rendered fresh — RenderPRBody and CommitMessage are pure functions of the plan and
	// id, so recomputing them is also always safe; storing them here just avoids requiring
	// the caller to keep the whole gitops.Plan around only to re-render text.
	CommitMessage   string
	PRTitle, PRBody string

	// The M4 fields below are policy, read once from internal/config by the CLI when this
	// PromotionState is built (a step's Observe must not import internal/config — see
	// steps_m4.go) and carried here exactly like CommitMessage/PRTitle/PRBody above: none of
	// them is secret or unbounded (AGENTS.md §4.3), and a resumed run re-reading the config
	// file could in principle see a changed policy — carrying the value used when the
	// promotion started is deliberate, not an oversight, so a promotion never straddles two
	// different policies mid-flight. `cmd/hoist/resume.go`'s runResume enforces this by loading
	// these fields from the persisted state file only, never re-assigning them from the current
	// RepoConfig — a fix landed after that invariant was found violated in an earlier draft;
	// don't reintroduce a re-read here without updating this comment and the invariant it states.

	// CINone and CIGrace are RepoConfig.CI as of when this promotion started: green|prompt|block
	// and the grace duration CIGreenStep waits before applying that policy to a PR reporting
	// zero checks.
	CINone  string
	CIGrace time.Duration
	// CINoneOverride, once set (via `hoist resume --override-ci-none`), lets CIGreenStep treat
	// a still-empty check-run set as satisfied under ci.none: prompt after the grace period —
	// the explicit override invariant 1 requires. ci.none: block never consults this field: it
	// has no override path through this flag at all (see CIGreenStep's doc comment for why).
	CINoneOverride bool

	// Approval is RepoConfig.Approval(TargetEnv) as of when this promotion started: "auto" or
	// "comment". Approvers and Collaborators are RepoConfig.Approvers and .Collaborators.
	Approval      string
	Approvers     []string
	Collaborators bool

	// MergeSHA is the squash-merge commit sha, once MergedStep's Act (or a re-observed
	// already-merged PR) reports one.
	MergeSHA string

	// The M5 fields below are the same two categories steps_m5.go's three new steps need,
	// alongside the M4 fields above: a config-sourced fact re-read on resume (ArgoNamespace),
	// and a structural fact about the plan already committed to, computed once and carried
	// rather than recomputed (ArgoApps, like Edits/CommitMessage/PRTitle/PRBody).

	// ArgoNamespace is where this promotion's target env's Argo Application custom resources
	// live on the cluster (RepoConfig.Kube.ArgoNamespace) — never spec.destination.namespace,
	// which TargetEnv already names (see pkg/argo's package doc). Read once when the
	// promotion is built and re-read on `hoist resume` from the current config, UNLIKE
	// CINone/CIGrace/Approval/Approvers/Collaborators just above: those gate decisions against
	// historical events (an already-recorded approval/CI comment) and must never straddle two
	// policies mid-flight, but ArgoNamespace only names where a live Get lands — re-reading it
	// lets `hoist resume` follow the Applications if an operator moves them to a different
	// namespace mid-flight, and a stale value would fail loudly (Application not found) rather
	// than silently misjudge anything (see cmd/hoist/resume.go's runResume for the same
	// reasoning at the call site).
	ArgoNamespace string
	// ArgoApps is the distinct, sorted set of Argo Application names (in TargetEnv) whose
	// family directory contains at least one of Edits' files — computed once, from
	// gitops.Discover's own Family->Application mapping, by ArgoAppNames when the promotion is
	// first built (see its doc comment), then carried unchanged across every resume. AGENTS.md
	// §4.1's "the world is the state" governs the Argo *status* ArgoRefreshedStep/
	// ArgoSyncedStep re-derive from a fresh Get on every Observe; it does not require
	// re-discovering which Application owns which family on every call, any more than it
	// requires BuildPlan to re-run on every resume — Edits is exactly this same kind of
	// carried, not re-derived, plan-time fact.
	//
	// A state file written before M5 added this field decodes it as empty, which is not the same
	// thing as a promotion computed to touch no Application (round-1 review finding): a non-empty
	// Edits with an empty ArgoApps is only possible for a state that predates ArgoAppNames ever
	// running against it, since a real call against a non-empty edit set always yields at least
	// one name or an error (see ArgoAppNames' own doc comment). cmd/hoist/resume.go's
	// ensureArgoApps repairs exactly that case, once, the first time such a state is resumed after
	// upgrading past M5 — everywhere else in this package, ArgoApps is read as carried, never
	// recomputed.
	ArgoApps []string
}

// StateDir is $XDG_STATE_HOME/hoist, else ~/.local/state/hoist — the XDG rule on every
// platform, never ~/Library (mirrors internal/config.DefaultPath's rule for the config file).
func StateDir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "hoist"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "hoist"), nil
}

// LandedSHA is the commit this promotion put on the branch Argo actually tracks — the one the
// M5 steps compare an Application's status.sync.revision against. The two modes reach it
// differently and neither field alone is the answer:
//
//   - A PR promotion lands via MergeSHA, the squash commit MergedStep records on Base. Its
//     PushedSHA names a commit on the promotion branch, which Argo never tracks.
//   - A direct promotion (M6) never merges anything, so MergeSHA stays empty forever. Its
//     PushedSHA IS on Base, because DirectPushedStep pushes there rather than to a branch —
//     and is the base tip carrying this promotion's content, re-derived on each observation,
//     not the original commit object (see DirectPushedStep.Observe for why the distinction is
//     the difference between converging and waiting out the deadline).
//
// Deriving it rather than persisting a third field keeps one source of truth per mode and
// needs no migration for state files written before direct mode existed: Direct is false for
// every one of them, so they resolve to MergeSHA exactly as they always did.
func (s *PromotionState) LandedSHA() string {
	if s.Direct {
		return s.PushedSHA
	}
	return s.MergeSHA
}

// landedStep is the step whose History entry timestamps the landing, for the same reason
// LandedSHA exists: ArgoRefreshedStep anchors "has Argo reconciled since this promotion
// landed?" on it, and a direct promotion has no Merged entry to anchor on.
func (s *PromotionState) landedStep() StepName {
	if s.Direct {
		return StepDirectPushed
	}
	return StepMerged
}

// StatePath is where SaveState/LoadState keep one promotion's state, keyed by its id.
func StatePath(id string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "promotions", id+".json"), nil
}

// CacheDir is $XDG_CACHE_HOME/hoist, else ~/.cache/hoist — the XDG rule on every platform,
// never ~/Library. WorktreeDir(id) is CacheDir/worktrees/<id>, the location AGENTS.md §4.6
// and the M3 brief's invariant 1 name explicitly.
func CacheDir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "hoist"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating cache dir: %w", err)
	}
	return filepath.Join(home, ".cache", "hoist"), nil
}

// WorktreeDir is the worktree path for promotion id, under CacheDir.
func WorktreeDir(id string) (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worktrees", id), nil
}

// SaveState writes s to path atomically: a temp file in the same directory, then rename, so a
// process killed mid-write never leaves partial JSON where the next run will look (Known bug
// classes: "A state file write that isn't atomic"). The file states its own permissions
// explicitly (AGENTS.md §8) rather than inheriting the umask — 0600, since it names local
// paths and, once M4 lands, will sit next to CI/approval state a stranger on a shared machine
// has no business reading.
func SaveState(path string, s *PromotionState) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	success = true
	return nil
}

// LoadState reads the state file at path. A missing file is not an error: it returns
// (nil, nil), since a state file is only ever a cache of History — Observe never needs it to
// determine truth (AGENTS.md invariant 4; see PromotionState's doc comment).
func LoadState(path string) (*PromotionState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s PromotionState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// ListStates reads every promotion state file under $XDG_STATE_HOME/hoist/promotions/, sorted
// by ID for a stable listing. A missing promotions directory is not an error: it returns (nil,
// nil), the same as "no promotions started yet". Reading it is purely informational — the
// caller (hoist resume, and the one-in-flight-per-env check at the start of hoist promote)
// still re-observes each promotion's steps against the remote before trusting anything about
// its current phase; this only recovers the set of IDs and the CloneDir/TargetEnv/Branch
// needed to rebuild each one's Steps and re-observe it (AGENTS.md §4.1 — Phase itself is never
// trusted).
func ListStates() ([]*PromotionState, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "promotions"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*PromotionState
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, "promotions", e.Name())
		s, err := LoadState(p)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
