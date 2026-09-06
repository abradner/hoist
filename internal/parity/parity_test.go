// Package parity holds one rule about hoist's two faces, enforced rather than remembered:
// everything an operator can do with a subcommand and its flags they can do from the TUI, and
// the reverse — or the gap is named, with the issue that closes it.
//
// The table below is the registry. The test reads the code on both sides — the subcommand
// dispatch and every flag in cmd/hoist, every navigation message the TUI root switches on in
// internal/app — and fails when either side has something the table does not mention, when the
// table mentions something neither side has any more, or when a row with one side empty does
// not cite an issue. A new subcommand, flag or screen therefore lands with its parity stated,
// in the same PR, or does not land.
//
// Shape borrowed from internal/copycheck: a rule about the operator's surface, checked by
// parsing the source rather than by reviewers remembering it.
package parity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// op is one thing an operator can do.
//
// CLI is the subcommand and the flags that do it, as tokens: the first token is the
// subcommand (or "(launch)" for the root flag set — the TUI is `hoist` with no subcommand, so
// those flags belong to both faces by construction), then `--flag` tokens, then free words.
// TUI names the root navigation messages (`pkg.TypeMsg`, as written in internal/app/app.go)
// and the keys that emit them. Gap is required when either side is empty and must cite the
// issue (`#NN`) that closes it; on a two-sided row it is an optional note.
type op struct {
	Name string
	CLI  string
	TUI  string
	Gap  string
}

var registry = []op{
	{
		Name: "launch: choose the config file and the repo, or print the version",
		CLI:  "(launch) --version --config --repo --apps-root --promotable",
		TUI:  "(launch) the same flags: the matrix is hoist with no subcommand, and --version exits before either face starts",
	},
	{
		Name: "plan a promotion, read-only",
		CLI:  "plan --from --to --dry-run",
		TUI:  "matrix.OpenPlanMsg p (the paired target) P (any target, what --to gives the CLI); the plan screen writes nothing until enter",
	},
	{
		Name: "promote: drive the pipeline to rollout",
		CLI:  "promote --from --to",
		TUI:  "plan.StartMsg enter on the plan screen; the flight screen is the CLI's progress output, R re-observes",
	},
	{
		Name: "direct mode: commit to the base branch, no PR (non-production only)",
		CLI:  "promote --direct --confirm-direct deploy --direct --confirm-direct",
		TUI:  "m on the plan and deploy screens; D in the picker emits tags.DirectRequestedMsg — each behind a huh.Confirm dialog",
	},
	{
		Name: "deploy one named image into an env",
		CLI:  "deploy --env --image --dry-run",
		TUI:  "matrix.OpenTagsMsg d, tags.SelectedMsg space, deploy.StartMsg enter; d on the deploy screen is the dry run's diff",
	},
	{
		Name: "restart an env's Deployments without changing what they declare",
		CLI:  "restart --env --family --confirm-production --dry-run",
		TUI:  "matrix.OpenRestartMsg R on the family under the cursor; the target list is the dry run, production takes a huh.Confirm",
	},
	{
		Name: "list what is in flight, re-observed",
		CLI:  "promotions",
		TUI:  "the in-flight pane under the matrix, re-observed at boot and every poll.approval",
	},
	{
		Name: "resume a promotion from wherever Observe finds it",
		CLI:  "resume --env",
		TUI:  "matrix.ResumeMsg r (or enter on the pane)",
	},
	{
		Name: "open the promotion's PR",
		CLI:  "promote (prints the PR URL) resume (the same)",
		TUI:  "flight.OpenPRMsg o on the flight screen, o on the in-flight pane",
	},
	{
		Name: "stop watching a promotion; the branch, PR and state file stay",
		CLI:  "promote ctrl-c (resume picks it up again)",
		TUI:  "flight.AbortMsg x on the flight screen",
	},
	{
		Name: "watch one Application converge, outside any promotion",
		CLI:  "watch --app --once",
		Gap:  "#101 — no watch screen yet",
	},
	{
		Name: "override one repo's digest before planning",
		CLI:  "plan --digest",
		Gap:  "#102 — the plan screen plans from the resolver alone",
	},
	{
		Name: "treat a PR with no checks as green under ci.none: prompt",
		CLI:  "resume --override-ci-none",
		Gap:  "#103 — the TUI's confirm path passes a fixed false; the flight screen shows the block but offers no override",
	},
	{
		Name: "show the effective config and where it came from",
		CLI:  "config show path",
		Gap:  "#104 — no config screen; the TUI reads the same file but never shows it",
	},
	{
		Name: "per-run overrides of what the config file says: base branch, kube context, digest sources, registry credential chain",
		CLI:  "promote --base --kube-context --digest-sources --registry-auth --cluster-secret --op-ref",
		Gap:  "#105 — the TUI hard-codes main and reads kube.context, digest_sources and registries[] from config only",
	},
}

const launch = "(launch)"

var issueRef = regexp.MustCompile(`#\d+`)

// TestEveryOperationIsOnBothFacesOrNamesItsGap is the parity gate.
func TestEveryOperationIsOnBothFacesOrNamesItsGap(t *testing.T) {
	subcommands := parseSubcommands(t, "../../cmd/hoist/main.go")
	flags := parseFlags(t, "../../cmd/hoist")
	msgs := parseNavigationMessages(t, "../../internal/app/app.go")

	// Positive control on the parsers: an empty side would let the checks below pass for
	// the wrong reason (AGENTS.md §8: when asserting absence, include a positive control).
	if len(subcommands) == 0 || len(flags) == 0 || len(msgs) == 0 {
		t.Fatalf("parsers found nothing: subcommands=%v flags=%v msgs=%v", subcommands, flags, msgs)
	}

	seenSub := map[string]bool{}
	seenFlag := map[string]bool{}
	seenMsg := map[string]bool{}

	for _, o := range registry {
		if o.CLI == "" && o.TUI == "" {
			t.Errorf("%q: neither side — a row must do something", o.Name)
			continue
		}
		if (o.CLI == "" || o.TUI == "") && !issueRef.MatchString(o.Gap) {
			t.Errorf("%q: one side only, and Gap %q cites no issue (#NN)", o.Name, o.Gap)
		}
		for i, tok := range strings.Fields(o.CLI) {
			switch {
			case i == 0 && tok == launch:
			case i == 0 && !subcommands[tok]:
				t.Errorf("%q: CLI starts with %q, which is not a subcommand in cmd/hoist/main.go", o.Name, tok)
			case strings.HasPrefix(tok, "--"):
				name := strings.TrimPrefix(tok, "--")
				if !flags[name] {
					t.Errorf("%q: CLI names %s, which no cmd/hoist flag set registers", o.Name, tok)
				}
				seenFlag[name] = true
			case subcommands[tok]:
				seenSub[tok] = true
			}
		}
		for _, tok := range strings.Fields(o.TUI) {
			if strings.HasSuffix(tok, "Msg") && strings.Contains(tok, ".") {
				if !msgs[tok] {
					t.Errorf("%q: TUI names %s, which internal/app/app.go does not switch on", o.Name, tok)
				}
				seenMsg[tok] = true
			}
		}
	}

	for _, s := range sorted(subcommands) {
		if !seenSub[s] {
			t.Errorf("subcommand %q has no row in the parity registry", s)
		}
	}
	for _, f := range sorted(flags) {
		if !seenFlag[f] {
			t.Errorf("flag --%s has no row in the parity registry", f)
		}
	}
	for _, m := range sorted(msgs) {
		if !seenMsg[m] {
			t.Errorf("navigation message %s has no row in the parity registry", m)
		}
	}
}

// parseSubcommands reads the `switch cmd := fs.Arg(0); cmd { case "plan": …}` dispatch: every
// string-literal case in a switch whose init calls fs.Arg.
func parseSubcommands(t *testing.T, path string) map[string]bool {
	t.Helper()
	f := parse(t, path)
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Init == nil || !callsArg(sw.Init) {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					out[strings.Trim(lit.Value, `"`)] = true
				}
			}
		}
		return true
	})
	return out
}

func callsArg(s ast.Stmt) bool {
	as, ok := s.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Arg"
}

// parseFlags collects every flag name registered on a `fs` flag set in the package's
// non-test files: fs.String/Bool/Duration/Int("name", …) and fs.Var/StringVar/…Var(&x, "name", …).
func parseFlags(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f := parse(t, filepath.Join(dir, name))
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "fs" {
				return true
			}
			idx := -1
			switch m := sel.Sel.Name; {
			case m == "Var" || strings.HasSuffix(m, "Var"):
				idx = 1
			case m == "String" || m == "Bool" || m == "Duration" || m == "Int":
				idx = 0
			}
			if idx < 0 || len(call.Args) <= idx {
				return true
			}
			if lit, ok := call.Args[idx].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				out[strings.Trim(lit.Value, `"`)] = true
			}
			return true
		})
	}
	return out
}

// parseNavigationMessages collects every `case pkg.SomethingMsg:` the root Update switches
// on, as written (import alias included). *BackMsg types are pure navigation — esc — and
// have no operation to pair with, so they are left out.
func parseNavigationMessages(t *testing.T, path string) map[string]bool {
	t.Helper()
	f := parse(t, path)
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name == "tea" || !strings.HasSuffix(sel.Sel.Name, "Msg") || strings.HasSuffix(sel.Sel.Name, "BackMsg") {
				continue
			}
			out[pkg.Name+"."+sel.Sel.Name] = true
		}
		return true
	})
	return out
}

func parse(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
