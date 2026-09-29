// P2-12 (T3 review): a footer's short form must keep both the key and the verb — "d yaml", "p
// promote" — never a bare key alone ("d", "r", "esc"), which reads as "press this letter" with
// no hint what it does once a screen's real Long hint has already been dropped from view. This
// file freezes that rule mechanically over every keys.Hint literal in internal/app, the same
// AST-scan style as t3_10_test.go, rather than leaving it to review (AGENTS.md §10 meta-rule 5).
//
// Verification-review followup (T3 followup, group 1): the original rule ("has a space, or is a
// pure arrow/page/home-end combo") passed two real bugs — the matrix's "p promote into" short
// form (a key, a verb, and a dangling preposition with no object once the target name was
// dropped for space) and the tags reader's "↑/↓" short form (a bare arrow with no verb at all,
// because "↑/↓" was blanket-exempted as if it were always self-describing navigation like
// pgup/pgdn or home/end — but unlike those, a plain ↑/↓ means something different on every
// screen it appears on: choose, switch commit, scroll). The rule now also rejects a Short whose
// last word is a bare preposition, and no longer exempts ↑/←/→ from the key-and-word check —
// only pgup/pgdn and home/end are self-describing regardless of screen, since they only ever
// mean "scroll".
package copycheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// bareNavigation is the narrow class of Short exempt from the "key and a word" rule: pgup/pgdn
// and home/end mean the same thing — scroll — on every screen that binds them, so the key alone
// is self-describing. ↑/↓ (and the plain ←/→ arrows) are deliberately NOT in this set: a bare
// arrow means something different per screen (choose, switch commit, scroll, move a cursor
// between panes), so dropping its verb in the short form leaves a reader guessing. Two real
// bugs shipped from an earlier, broader version of this exemption that included "↑/↓", "←" and
// "→" (T3 followup, group 1) — a hint needs its own word, or a Long form callers can fall back
// to, not a blanket pass for anything shaped like an arrow.
var bareNavigation = map[string]bool{
	"pgup/pgdn": true, "home/end": true, "pgup": true, "pgdn": true,
}

// danglingPreposition is a Short's last word that leaves it reading as a command with the
// object cut off — "p promote into" once its target name was dropped for space. Passing the
// "has a space" check isn't enough on its own; the trailing word has to actually land the hint.
var danglingPreposition = map[string]bool{
	"into": true, "to": true, "for": true, "with": true, "from": true,
	"by": true, "at": true, "of": true, "on": true, "as": true,
}

func lastWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[len(fields)-1])
}

// hasKeyAndWord reports whether a hint's Short text carries more than just its own key — a
// space-separated word beyond the key/arrow token itself, or membership in bareNavigation — and
// that the trailing word isn't a dangling preposition with its object cut off.
func hasKeyAndWord(short string) bool {
	if bareNavigation[short] {
		return true
	}
	if !strings.Contains(short, " ") {
		return false
	}
	return !danglingPreposition[lastWord(short)]
}

// TestFooterShortFormsKeepTheVerb: every `Short:` field of a keys.Hint composite literal under
// internal/app either names a Spatial navigation combo or contains a space — i.e. a key plus at
// least one word — never a bare key standing alone.
//
// Proven to fail: reverting internal/app/plan/model.go's plan-confirm footer to
// `{B: keys.Diff, Long: "d yaml", Short: "d", Pri: 2}` makes this fail on that literal.
// Also proven to fail on each real bug this followup fixed: reverting
// internal/app/matrix/model.go's Promote hint's Short back to "p promote into" fails on the
// dangling preposition, and reverting internal/app/tags/model.go's reader Up hint's Short back
// to "↑/↓" fails on the bare-arrow check now that ↑/↓ is no longer in bareNavigation.
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
