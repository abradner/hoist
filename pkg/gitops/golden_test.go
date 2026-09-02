package gitops

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

const goldenDir = "../../testdata/golden"

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join(goldenDir, name)
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./pkg/gitops -update)", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("%s differs from golden (-want +got):\n%s", name, diff)
	}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestGoldenDiscover(t *testing.T) {
	r := discoverFixture(t)
	r.Root = "<fixture>"
	golden(t, "discover.json", marshal(t, r))
}

func TestGoldenPlan(t *testing.T) {
	p := planFixture(t)
	p.GeneratedAt = time.Time{}
	golden(t, "plan.json", marshal(t, p))
}

func TestGoldenApplied(t *testing.T) {
	p := planFixture(t)
	tmp := copyDir(t, fixtureRoot)
	changed, err := Apply(tmp, p.Edits)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, f := range changed {
		b, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString("=== " + f + "\n")
		out.Write(b)
	}
	golden(t, "applied.txt", out.Bytes())
}
