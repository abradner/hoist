package main

import (
	"testing"

	"github.com/abradner/hoist/internal/app/plan"
)

// TestDigestOverridePredicateIsShared: `--digest` (digestFlag.Set) and the plan screen's o
// dialog (plan.ValidateOverride) refuse the same inputs with the same words, because both call
// image.ParseOverride and nothing else of their own (#102; AGENTS.md §8, layered checks). The
// valid input is the positive control: it passes both, and both hand back the same Ref.
func TestDigestOverridePredicateIsShared(t *testing.T) {
	const repo = "ghcr.io/example/web"
	cases := map[string]string{
		"valid":            repo + "=" + repo + ":v9@" + digestC,
		"no equals":        repo + ":v9@" + digestC,
		"empty repo":       "=" + repo + ":v9@" + digestC,
		"malformed digest": repo + "=" + repo + ":v9@sha256:DEADBEEF",
		"repo mismatch":    repo + "=ghcr.io/example/other:v9@" + digestC,
		"unpinned":         repo + "=" + repo + ":v9",
		"tagless":          repo + "=" + repo + "@" + digestC,
		"unparseable ref":  repo + "=not an image",
	}
	for name, in := range cases {
		cliErr := digestFlag{}.Set(in)
		tuiRef, tuiErr := plan.ValidateOverride(repo, in)
		switch {
		case name == "valid":
			if cliErr != nil || tuiErr != nil {
				t.Errorf("%s: cli=%v tui=%v, want both to accept", name, cliErr, tuiErr)
			}
			if tuiRef.String() != repo+":v9@"+digestC {
				t.Errorf("%s: tui ref = %s", name, tuiRef)
			}
		case cliErr == nil || tuiErr == nil:
			t.Errorf("%s: cli=%v tui=%v, want both to refuse", name, cliErr, tuiErr)
		case cliErr.Error() != tuiErr.Error():
			t.Errorf("%s: the two faces disagree:\n cli: %v\n tui: %v", name, cliErr, tuiErr)
		}
	}
}
