package tags

import "regexp"

// Class is which of the picker's three groups a registry tag lists under (#91). A registry
// lists every tag it has — releases, digest-as-tag entries, `latest`, branch names — and
// the operator is scanning for the handful of releases, so the table groups them, each
// under a divider, without dropping any: every tag stays reachable and `/` searches across
// every group. Ordering within a group is unchanged (git tag dates when mapped, else the
// registry's Created as it loads).
type Class int

const (
	// ClassRelease is a release-looking tag: a dotted number with an optional `v` prefix
	// (`v1.2.3`, `1.2.3`, `v202609060428`) or one under a `release-`/`release/` prefix
	// (`release-2026.09`), with an optional `-pre`/`+build` suffix.
	ClassRelease Class = iota
	// ClassDigest is a digest-as-tag entry (`sha-055c877f`, `sha256-<hex>`): a build named
	// by its commit or content hash — usually the same build as a release beside it, which
	// the name alone cannot prove.
	ClassDigest
	// ClassMoving is everything else: `latest`, `main`, `master`, a branch name, someone's
	// WIP — a tag that moves, or whose name says nothing about a version.
	ClassMoving
)

var (
	releaseTag = regexp.MustCompile(`^(?:v|release[-/])?\d+(?:\.\d+)*(?:[-+][0-9A-Za-z.\-+]*)?$`)
	digestTag  = regexp.MustCompile(`^(?:sha|sha256)-[0-9a-fA-F]{7,}$`)
)

// Classify sorts one tag into its Class. The rule: release-looking first — an optional `v`
// or `release-`/`release/` prefix, then digits separated by dots, then an optional
// `-`/`+` suffix (so `v1.2.3`, `1.2.3`, `v202609060428`, `release-2026.09`, `v1.2.3-rc1`);
// then digest-as-tag — `sha-` or `sha256-` followed by at least seven hex characters; and
// everything else is a moving tag. A stated convention rather than a discovered one
// (AGENTS.md §8): no registry marks a tag's kind, so the shape of its name is all there is.
func Classify(tag string) Class {
	switch {
	case releaseTag.MatchString(tag):
		return ClassRelease
	case digestTag.MatchString(tag):
		return ClassDigest
	default:
		return ClassMoving
	}
}

// Divider is the label the table draws above a group's first row — "" for the release
// group, which is what the picker is for and needs no introduction.
func (c Class) Divider() string {
	switch c {
	case ClassDigest:
		return "── digest tags (sha-…): builds named by hash ──"
	case ClassMoving:
		return "── moving tags (latest, branches): not releases ──"
	default:
		return ""
	}
}
