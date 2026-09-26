package service_test

import (
	"os/exec"
	"strings"
	"testing"
)

// goOrFail returns the go binary to shell out to. A skip here would be indistinguishable from a
// pass (AGENTS.md §8, "a test job that cannot distinguish 'everything passed' from 'nothing ran'
// is not a gate", and §9 entry — a CI job that silently ran zero checks is exactly the failure
// mode this guards): `go` not being on PATH means this import-boundary gate never ran at all,
// which must fail loudly rather than report green for a mutant that was never actually checked
// (t1-review.md P3 — verified this runs under `mise exec`, and a mutant importing internal/ui
// into internal/service fails it).
func goOrFail(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go not on PATH: %v — this import-boundary check cannot silently skip (AGENTS.md §8)", err)
	}
	return bin
}

func listDeps(t *testing.T, goBin, pkg string) []string {
	t.Helper()
	out, err := exec.Command(goBin, "list", "-deps", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v: %s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// TestServiceNeverImportsAppOrBubbletea enforces doc.go's own rule mechanically rather than by
// convention alone (AGENTS.md §10 meta-rule 5): internal/service is the use-case layer BOTH
// cmd/hoist and the TUI call, so it must never import the TUI back, and never a rendering
// library the CLI has no business linking in.
func TestServiceNeverImportsAppOrBubbletea(t *testing.T) {
	goBin := goOrFail(t)
	for _, dep := range listDeps(t, goBin, "github.com/abradner/hoist/internal/service") {
		if strings.Contains(dep, "hoist/internal/app") {
			t.Errorf("internal/service imports %s — the service layer must never import internal/app", dep)
		}
		if strings.Contains(dep, "charm.land") {
			t.Errorf("internal/service imports %s — the service layer must never import a Bubble Tea/Lip Gloss package", dep)
		}
	}
}

// TestPkgNeverImportsInternal enforces AGENTS.md §4.3: pkg/* packages are activity-shaped and
// never import internal/ — the boundary github.com/abradner/workflow's Temporal-activity
// wrapping would need, whether or not that library is ever adopted.
func TestPkgNeverImportsInternal(t *testing.T) {
	goBin := goOrFail(t)
	for _, dep := range listDeps(t, goBin, "github.com/abradner/hoist/pkg/...") {
		if strings.Contains(dep, "hoist/internal") {
			t.Errorf("a pkg/... package imports %s — pkg/* must never import internal/ (AGENTS.md §4.3)", dep)
		}
	}
}
