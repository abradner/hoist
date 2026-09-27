// T3-10's own guard tests: the copy sweep's gains, frozen mechanically rather than left to
// review (AGENTS.md §10 meta-rule 5). Each test below is an AST or text scan over
// internal/app, not a runtime render — consistent with the other checks in this package
// (TestNoSteeringDocsInOperatorCopy) and with the audit doc's own acceptance-check list for
// T3-10, which is entirely `git grep`-shaped.
package copycheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// walkGoFiles runs fn over every non-test .go file under root.
func walkGoFiles(t *testing.T, root string, fn func(path string, src []byte)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fn(path, src)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// walkStringLiterals runs fn over every string literal's decoded value in every non-test .go
// file under root, with the position for a useful failure message.
func walkStringLiterals(t *testing.T, root string, fn func(pos string, value string)) {
	t.Helper()
	walkGoFiles(t, root, func(path string, src []byte) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			fn(fset.Position(lit.Pos()).String(), value)
			return true
		})
	})
}

// titleLiteral matches the static portion of a screen's own frame title: "hoist", " · ", then
// a lowercase-led word (a literal noun like "config"/"restart", or a %s/%v placeholder for a
// screen whose noun is itself a runtime value, e.g. flight's own `noun`), optionally followed
// by more " · "-joined segments this check does not otherwise constrain — those can be dynamic
// (a path, an env/family pair, a state word) and are the CLI-parity, not the copy-sweep's,
// concern. AGENTS.md §4.8's own titling convention: `hoist · <noun>( · <state>)?`.
var titleLiteral = regexp.MustCompile(`^hoist · ([a-z][a-z0-9]*(-[a-z0-9]+)*|%[sv])( · .*)?$`)

// TestFrameTitlesFollowPattern: every string literal that opens a screen's own frame title
// (anything beginning "hoist") follows `hoist · <noun>…`, lowercase, the pattern every
// migrated screen in this train already converged on (T3-01 through T3-09) — never a path, a
// capitalized phrase, or the pre-M10 "confirm promotion" shape.
//
// Proven to fail: reverting any migrated screen's title literal to the old shape (e.g.
// plan's "hoist · confirm promotion") makes this fail, since that literal itself starts with
// "hoist" and does not match titleLiteral.
func TestFrameTitlesFollowPattern(t *testing.T) {
	walkStringLiterals(t, "../app", func(pos, value string) {
		// A title literal always carries the " · " separator; "hoist approve <id>" (the magic
		// comment, flight/summary.go) starts with "hoist" too but is never a frame title, so
		// requiring the separator is what tells the two apart without a per-package allowlist.
		if !strings.HasPrefix(value, "hoist") || !strings.Contains(value, " · ") {
			return
		}
		if !titleLiteral.MatchString(value) {
			t.Errorf("%s: title literal %q does not follow \"hoist · <noun>…\"", pos, value)
		}
	})
}

// TestNoParenPlural: no "(s)" pluralization survives in internal/app's own string literals —
// ui.Plural (T3-02) is the one place a count decides "1 X"/"N Xs" (UX-L2).
func TestNoParenPlural(t *testing.T) {
	walkStringLiterals(t, "../app", func(pos, value string) {
		if strings.Contains(value, "(s)") {
			t.Errorf("%s: literal %q still has a \"(s)\" pluralization — use ui.Plural", pos, value)
		}
	})
}

// walkCallExprs runs fn over every real call expression's "pkg.Func" spelling (as written —
// import alias included) in every non-test .go file under root — a comment mentioning the same
// text (e.g. a doc comment explaining why a call was replaced) is not source, so it never
// reaches this, unlike a plain substring search over the file's raw bytes would.
func walkCallExprs(t *testing.T, root string, fn func(pos string, call string)) {
	t.Helper()
	walkGoFiles(t, root, func(path string, src []byte) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			fn(fset.Position(call.Pos()).String(), pkg.Name+"."+sel.Sel.Name)
			return true
		})
	})
}

// TestNoKeyNewBindingOutsideKeys: every key.Binding in internal/app is built through
// internal/ui/keys now (Binding.Bubbles() for viewport/table interop), never a raw
// key.NewBinding call of its own — the guard against a screen quietly reintroducing a second,
// unregistered key vocabulary the audit doc's own acceptance check for T3-09 already
// checked one screen at a time; this freezes it over the whole package.
func TestNoKeyNewBindingOutsideKeys(t *testing.T) {
	walkCallExprs(t, "../app", func(pos, call string) {
		if call == "key.NewBinding" {
			t.Errorf("%s: calls key.NewBinding directly — build the binding in internal/ui/keys and use its .Bubbles()", pos)
		}
	})
}

// TestNoHuhDefaultKeyMapOutsideKeys: every standalone huh field in internal/app is wired
// through keys.HuhKeyMap() (AGENTS.md §9 entry 6), never huh.NewDefaultKeyMap() directly — the
// one place that's allowed to call the latter is internal/ui/keys/huh.go itself, which builds
// keys.HuhKeyMap() from it (a different package, never walked by this test).
func TestNoHuhDefaultKeyMapOutsideKeys(t *testing.T) {
	walkCallExprs(t, "../app", func(pos, call string) {
		if call == "huh.NewDefaultKeyMap" {
			t.Errorf("%s: calls huh.NewDefaultKeyMap directly — use keys.HuhKeyMap()", pos)
		}
	})
}

// TestNoBubblesHelpInApp: bubbles' own help.Model retired with the matrix's bubbles table
// (T3-04, UX-M16 — "•" separator on the matrix, "·" elsewhere): internal/ui/keys.HelpView is
// the one help overlay every screen shares now.
func TestNoBubblesHelpInApp(t *testing.T) {
	walkGoFiles(t, "../app", func(path string, src []byte) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "bubbles/v2/help") {
				t.Errorf("%s: imports bubbles/v2/help — internal/ui/keys.HelpView replaced it (T3-04)", path)
			}
		}
	})
}

// bareCapital mirrors internal/ui/keys's own unexported regexp of the same name (keys_test.go):
// a standalone single capital letter, the "press R" shape rule 5 forbids for a write key
// (indistinguishable from a caps-lock accident on a legacy terminal). Not imported directly —
// it is a test-only value in another package's _test.go, not part of the keys package's own
// API — so it is redeclared here against the same rule, over every literal in internal/app
// rather than only the keys registry's own Show/Desc fields.
var bareCapital = regexp.MustCompile(`(^|[\s·])[A-Z]([\s·]|$)`)

// TestNoBareCapitalHintLiteral: no string literal anywhere in internal/app displays a bare
// capital letter as a key hint — every write binding is shown as "shift+<letter>"
// (internal/ui/keys.write, rule 5) and every footer/help string goes through keys.Footer/
// HelpView, which already only ever emit "shift+x" forms; this is the belt-and-braces check
// that no screen's own hand-built string (a notice, a dialog title, a static footer for a
// state keys.Footer doesn't cover) slips a bare "R"/"X" back in.
//
// Proven to fail: a literal like "R restart" (the pre-T3 flight footer's own shape) matches
// bareCapital and fails this test.
func TestNoBareCapitalHintLiteral(t *testing.T) {
	walkStringLiterals(t, "../app", func(pos, value string) {
		if bareCapital.MatchString(value) {
			t.Errorf("%s: literal %q has a bare capital letter — writes are shown as shift+<letter>, never a capital alone", pos, value)
		}
	})
}

// TestGuideHasNoBareCapitalKeys: docs/guide.md never tells the operator to press a bare
// capital letter (no backticked single capital, “ `R` “) — every write key is spelled
// `shift+<letter>` in prose too, matching what the footer and help overlay actually show
// (AGENTS.md §4.8's transitional note, made a flat rule by this PR).
//
// Proven to fail: reintroducing a line like "press `X` to abandon" makes this fail.
func TestGuideHasNoBareCapitalKeys(t *testing.T) {
	src, err := os.ReadFile("../../docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	backtickedCapital := regexp.MustCompile("`[A-Z]`")
	for i, line := range strings.Split(string(src), "\n") {
		if backtickedCapital.MatchString(line) {
			t.Errorf("docs/guide.md:%d: backticked bare capital key: %q", i+1, line)
		}
	}
}
