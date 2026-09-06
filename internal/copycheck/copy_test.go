// Package copycheck holds one rule about what hoist says to the people using it, enforced
// rather than remembered.
package copycheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSteeringDocsInOperatorCopy: nothing hoist prints may cite AGENTS.md.
//
// AGENTS.md is the contributors' steering document. An operator reading "direct mode is not
// offered: it is a production env" is being pointed at a file they do not have,
// to explain a rule the sentence had already explained — and a rule a user needs is a rule that
// belongs in the README. (Raised by the operator, who got "cannot select without a digest
// " while trying to deploy.)
//
// Comments are exempt: this checks string literals only, so the doc references that help someone
// reading the code stay exactly where they are useful.
func TestNoSteeringDocsInOperatorCopy(t *testing.T) {
	roots := []string{"../../cmd", "../../internal", "../../pkg"}
	const forbidden = "AGENTS"

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0) // no comments: literals only
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if strings.Contains(lit.Value, forbidden) {
					t.Errorf("%s: string literal cites %s — say the rule instead, and put the long form in the README:\n  %s",
						fset.Position(lit.Pos()), forbidden, lit.Value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
