// P2-12 (T3 review): a footer's short form must keep both the key and the verb — "d yaml", "p
// promote" — never a bare key alone ("d", "r", "esc"), which reads as "press this letter" with
// no hint what it does once a screen's real Long hint has already been dropped from view. This
// file freezes that rule mechanically over every keys.Hint literal in internal/app, the same
// AST-scan style as t3_10_test.go, rather than leaving it to review (AGENTS.md §10 meta-rule 5).
package copycheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// bareNavigation is the one class of Short exempt from the "key and a word" rule: a pure
// Spatial arrow/page/home-end combo is self-describing navigation with no verb to lose (the
// audit's own rule 3 — Spatial keys are "movement, never a write"), and every screen that uses
// one already spells the same shape out in its Long form too (e.g. "↑/↓ switch commit").
var bareNavigation = map[string]bool{
	"↑/↓": true, "←": true, "→": true,
	"pgup/pgdn": true, "home/end": true, "pgup": true, "pgdn": true,
}

// hasKeyAndWord reports whether a hint's Short text carries more than just its own key — a
// space-separated word beyond the key/arrow token itself, or membership in bareNavigation.
func hasKeyAndWord(short string) bool {
	if bareNavigation[short] {
		return true
	}
	return strings.Contains(short, " ")
}

// TestFooterShortFormsKeepTheVerb: every `Short:` field of a keys.Hint composite literal under
// internal/app either names a Spatial navigation combo or contains a space — i.e. a key plus at
// least one word — never a bare key standing alone.
//
// Proven to fail: reverting internal/app/plan/model.go's plan-confirm footer to
// `{B: keys.Diff, Long: "d yaml", Short: "d", Pri: 2}` makes this fail on that literal.
func TestFooterShortFormsKeepTheVerb(t *testing.T) {
	fset := token.NewFileSet()
	walkGoFiles(t, "../app", func(path string, src []byte) {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			// A keys.Hint literal inside a []keys.Hint{...} slice is elided (cl.Type is nil —
			// Go only spells the type on the outer literal), so matching by field shape rather
			// than by type name is what catches every element, not just an explicitly typed
			// keys.Hint{...} written on its own. "B" + "Pri" together is unique to this struct
			// in internal/app.
			var hasB, hasPri bool
			var shortKV *ast.KeyValueExpr
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "B":
					hasB = true
				case "Pri":
					hasPri = true
				case "Short":
					shortKV = kv
				}
			}
			if !hasB || !hasPri || shortKV == nil {
				return true
			}
			lit, ok := shortKV.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if value != "" && !hasKeyAndWord(value) {
				t.Errorf("%s: keys.Hint Short %q is a bare key with no verb", fset.Position(lit.Pos()).String(), value)
			}
			return true
		})
	})
}
