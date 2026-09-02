package gitops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/image"
)

const fixtureRoot = "../../testdata/repo"

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	webNew    = "ghcr.io/example/web:v202602150930@sha256:1f7e5c3a9b2d4e6f8a0c1b3d5e7f9a2b4c6d8e0f1a3b5c7d9e1f2a4b6c8d0e2f"
	countaNew = "ghcr.io/example/counta:v202602201200@sha256:c0c0a123c0c0a123c0c0a123c0c0a123c0c0a123c0c0a123c0c0a123c0c0a123"
	mktNew    = "ghcr.io/example/marketing:sha-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d@sha256:77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee77c0ffee"
)

var promotable = []string{"ghcr.io/example/"}

func mustRef(t *testing.T, s string) image.Ref {
	t.Helper()
	r, err := image.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func discoverFixture(t *testing.T) *Repo {
	t.Helper()
	r, err := Discover(fixtureRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func planFixture(t *testing.T) Plan {
	t.Helper()
	p, err := BuildPlan(discoverFixture(t), "app-staging", "app-production", promotable, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeRepo materialises files (slash paths → contents) under a fresh temp dir.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// copyDir copies the fixture into a temp dir so Apply can write without touching testdata.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func wrapper(name, sourcePath, namespace string) string {
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
spec:
  source:
    path: %s
  destination:
    namespace: %s
`, name, sourcePath, namespace)
}

// deployment renders a Deployment whose containers are name→image pairs, in order.
func deployment(name string, containers ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\nspec:\n  template:\n    spec:\n      containers:\n", name)
	for i := 0; i+1 < len(containers); i += 2 {
		fmt.Fprintf(&b, "        - name: %s\n          image: %s\n", containers[i], containers[i+1])
	}
	return b.String()
}

// occurrencesOf scans a single-file manifest string the way Discover would.
func occurrencesOf(t *testing.T, file, content string) []Occurrence {
	t.Helper()
	docs, err := parseDocs([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	var out []Occurrence
	for i, d := range docs {
		occ, err := scanDoc(file, i, d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, occ...)
	}
	return out
}

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func editsFor(p Plan, file string) []Edit {
	var out []Edit
	for _, e := range p.Edits {
		if e.File == file {
			out = append(out, e)
		}
	}
	return out
}
