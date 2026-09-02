// Package image parses, prints and identifies container image references.
//
// A reference has up to three parts: the repo (registry path without tag or digest), an
// optional tag and an optional sha256 digest. All four shapes seen in manifests and pod
// status are accepted by Parse and printed back canonically by String. Nothing here talks
// to a registry.
package image

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// PullablePrefix is the scheme some container runtimes prepend to
// status.containerStatuses[].imageID. Parse strips it; String never emits it.
const PullablePrefix = "docker-pullable://"

var (
	// digestRE is the only thing hoist accepts as a digest: sha256 and exactly 64 lowercase
	// hex characters. Uppercase, short, or other algorithms are not digests here.
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// repoRE: a first component that may be a host with a port, then lowercase path
	// components. Loose on the host on purpose (registries vary); strict enough to reject
	// whitespace, indicators and empty components.
	repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)*$`)
)

// Ref is one image reference. Tag == "" means the reference carried no tag (implicit
// "latest"); String prints it back without a tag so that Parse/String round-trips.
type Ref struct {
	Repo   string
	Tag    string
	Digest string // "sha256:<64 hex>" or "" when unpinned
}

// Parse accepts repo:tag@sha256:…, repo:tag, repo@sha256:… (optionally prefixed with
// docker-pullable://) and repo alone. A digest that is not sha256 followed by 64 lowercase
// hex characters is an error, not a digest.
func Parse(s string) (Ref, error) {
	orig := s
	s = strings.TrimPrefix(s, PullablePrefix)
	if s == "" {
		return Ref{}, errors.New("image: empty reference")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Ref{}, fmt.Errorf("image %q: contains whitespace", orig)
	}
	var r Ref
	if i := strings.IndexByte(s, '@'); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if !digestRE.MatchString(r.Digest) {
			return Ref{}, fmt.Errorf("image %q: %q is not a digest (want sha256: followed by 64 lowercase hex characters)", orig, r.Digest)
		}
	}
	// The tag separator is the last ':' after the last '/', so a registry port
	// (localhost:5000/app) is not mistaken for a tag.
	slash := strings.LastIndexByte(s, '/')
	if colon := strings.LastIndexByte(s, ':'); colon > slash {
		r.Tag = s[colon+1:]
		s = s[:colon]
		if !tagRE.MatchString(r.Tag) {
			return Ref{}, fmt.Errorf("image %q: invalid tag %q", orig, r.Tag)
		}
	}
	if !repoRE.MatchString(s) {
		return Ref{}, fmt.Errorf("image %q: invalid repo %q", orig, s)
	}
	r.Repo = s
	return r, nil
}

// String prints the canonical form: repo[:tag][@digest].
func (r Ref) String() string {
	var b strings.Builder
	b.WriteString(r.Repo)
	if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	if r.Digest != "" {
		b.WriteByte('@')
		b.WriteString(r.Digest)
	}
	return b.String()
}

// Pinned reports whether the reference carries a digest. Only pinned references may be
// written to a manifest (AGENTS.md §4.2).
func (r Ref) Pinned() bool { return r.Digest != "" }

// Validate checks a Ref that did not come from Parse — a caller-built override, say —
// against the same rules Parse applies: a non-empty repo, a well-formed tag if present, and a
// digest that is exactly "sha256:" followed by 64 lowercase hex characters if present. It is
// the one definition of "well-formed"; Parse and Validate share the expressions.
func (r Ref) Validate() error {
	if r.Repo == "" {
		return errors.New("image: empty repo")
	}
	if !repoRE.MatchString(r.Repo) {
		return fmt.Errorf("image %s: invalid repo %q", r, r.Repo)
	}
	if r.Tag != "" && !tagRE.MatchString(r.Tag) {
		return fmt.Errorf("image %s: invalid tag %q", r, r.Tag)
	}
	if r.Digest != "" && !digestRE.MatchString(r.Digest) {
		return fmt.Errorf("image %s: %q is not a digest (want sha256: followed by 64 lowercase hex characters)", r, r.Digest)
	}
	return nil
}

// PromotionID is the deterministic identity of one promotion (AGENTS.md §4.1). Exactly:
//
//  1. lines = the distinct "<repo>@<digest>" strings of refs (Tag ignored; a repo@digest
//     pair that appears more than once contributes one line — §4.1 defines the input as a
//     *set*, so two occurrences of one image in the target env do not change the id),
//  2. sorted bytewise (sort.Strings),
//  3. preimage = "hoist/v1\n" + repoFullName + "\n" + targetEnv + "\n" + strings.Join(lines, "\n"),
//  4. id = the first 10 characters of the lowercase, unpadded standard base32 encoding of
//     sha256(preimage).
//
// The M1 brief's formula omits step 1's dedup; this is the deliberate reading, and
// TestPromotionIDFixedVectorWithDuplicate freezes it. Callers pass pinned refs; an unpinned
// ref contributes "<repo>@" and is the caller's bug, not this function's concern.
func PromotionID(repoFullName, targetEnv string, refs []Ref) string {
	lines := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		l := r.Repo + "@" + r.Digest
		if seen[l] {
			continue
		}
		seen[l] = true
		lines = append(lines, l)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte("hoist/v1\n" + repoFullName + "\n" + targetEnv + "\n" + strings.Join(lines, "\n")))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(enc)[:10]
}

// Canonical returns the repo in the registry's own spelling, so that the repo a manifest
// names and the repo a pod's imageID names can be compared: docker.io/library/nginx,
// docker.io/nginx and nginx all become index.docker.io/library/nginx, while
// ghcr.io/example/app is already canonical. It is go-containerregistry's name parsing;
// a repo it cannot parse is returned unchanged rather than failing a comparison.
func Canonical(repo string) string {
	r, err := name.NewRepository(repo)
	if err != nil {
		return repo
	}
	return r.Name()
}
