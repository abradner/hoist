package main

import (
	"runtime/debug"
	"testing"
)

// --version names what is running: the ldflag goreleaser set wins; a `go install …@vX.Y.Z`
// build carries the module version instead; a checkout build ("(devel)", or no build info at
// all) is "dev". Each branch, with the input that reaches it.
func TestResolveVersion(t *testing.T) {
	tagged := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	devel := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	empty := &debug.BuildInfo{}
	cases := []struct {
		name   string
		ldflag string
		bi     *debug.BuildInfo
		ok     bool
		want   string
	}{
		{"ldflag wins over module version", "v0.2.0", tagged, true, "v0.2.0"},
		{"ldflag wins with no build info", "0.0.0-SNAPSHOT-abc", nil, false, "0.0.0-SNAPSHOT-abc"},
		{"go install at a tag", "dev", tagged, true, "v0.1.0"},
		{"checkout build is (devel)", "dev", devel, true, "dev"},
		{"build info with no version", "dev", empty, true, "dev"},
		{"no build info at all", "dev", nil, false, "dev"},
	}
	for _, c := range cases {
		if got := resolveVersion(c.ldflag, c.bi, c.ok); got != c.want {
			t.Errorf("%s: resolveVersion(%q, …) = %q, want %q", c.name, c.ldflag, got, c.want)
		}
	}
}
