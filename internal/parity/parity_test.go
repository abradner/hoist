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
// CLI is the subcommand and the flags that do it, as tokens: a subcommand token (or
// "(launch)" for the root flag set — the TUI is `hoist` with no subcommand, so those flags
// belong to both faces by construction) followed by the `--flag` tokens that command
// registers; a row may name several subcommands, each owning the flags after it. Free words
// are ignored.
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
		CLI:  "(launch) --version --config --repo --apps-root --promotable --base --kube-context --digest-sources --registry-auth --cluster-secret --op-ref",
		TUI:  "(launch) the same flags: the matrix is hoist with no subcommand, and --version exits before either face starts; --base and --kube-context (#105) reach the confirm path and every cluster adaptor, and the title names them when they are not the defaults",
	},
	{
		Name: "plan a promotion, read-only",
		CLI:  "plan --from --to --dry-run --repo --apps-root --promotable --digest-sources",
		TUI:  "matrix.OpenPlanMsg p (the paired target) P (any target, what --to gives the CLI); the plan screen writes nothing until enter",
	},
	{
		Name: "promote: drive the pipeline to rollout",
		CLI:  "promote --from --to --repo --apps-root --promotable",
		TUI:  "plan.StartMsg enter on the plan screen; the flight screen is the CLI's progress output, R re-observes",
	},
	{
		Name: "direct mode: commit to the base branch, no PR (non-production only)",
		CLI:  "promote --direct --confirm-direct deploy --direct --confirm-direct",
		TUI:  "m on the plan and deploy screens; D in the picker emits tags.DirectRequestedMsg — each behind a huh.Confirm dialog",
	},
	{
		Name: "deploy one named image into an env",
		CLI:  "deploy --env --image --dry-run --repo --apps-root --promotable",
		TUI:  "matrix.OpenTagsMsg d, tags.SelectedMsg space, deploy.StartMsg enter; d on the deploy screen is the dry run's diff",
	},
	{
		Name: "restart an env's Deployments without changing what they declare",
		CLI:  "restart --env --family --confirm-production --dry-run --repo --apps-root",
		TUI:  "matrix.OpenRestartMsg R on the family under the cursor; the target list is the dry run, production takes a huh.Confirm",
	},
	{
		Name: "what an env is running right now, from its pods",
		CLI:  "plan --from --dry-run (the Resolution section names each repo's running digest and its source)",
		TUI:  "matrix.DriftMsg at boot and on F5/ctrl+r: the drifted word on the cell and the sentence under the table",
	},
	{
		Name: "list what is in flight, re-observed",
		CLI:  "promotions --kube-context --repo --archived",
		TUI:  "the in-flight pane under the matrix, re-observed at boot and every poll.approval, in the launch's --kube-context when given",
		Gap:  "--repo/--archived scope and retire terminal state files by age (state.retain) — no TUI equivalent yet, #171",
	},
	{
		Name: "resume a promotion from wherever Observe finds it",
		CLI:  "resume --env --kube-context",
		TUI:  "matrix.ResumeMsg r (or enter on the pane), driven in the launch's --kube-context when given",
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
		Name: "abandon a promotion that never landed: retire its state, close its PR and delete its branch if it opened either — refused outright if the promotion has already landed",
		CLI:  "abandon --confirm-abandon",
		TUI:  "flight.AbandonMsg X on the flight screen, behind a huh.Confirm",
	},
	{
		Name: "watch one Application converge, outside any promotion",
		CLI:  "watch --app --once --repo --apps-root",
		TUI:  "matrix.OpenWatchMsg w on the cursor cell; the first paint is --once, r polls now, esc pops (#101)",
	},
	{
		Name: "override one repo's digest before planning",
		CLI:  "plan --digest promote --digest",
		TUI:  "o on the plan screen: a huh.Input dialog, validated like --digest, rebuilds the plan with the override named as its source",
	},
	{
		Name: "treat a PR with no checks as green under ci.none: prompt",
		CLI:  "resume --override-ci-none promote --override-ci-none deploy --override-ci-none",
		TUI:  "flight.OverrideCINoneMsg c on the flight screen, behind a huh.Confirm",
	},
	{
		Name: "show the effective config and where it came from",
		CLI:  "config show path",
		TUI:  "matrix.OpenConfigMsg C — the same redacted, defaults-filled text, titled with the path (or the CLI's no-file sentence)",
	},
	{
		Name: "per-run overrides of what the config file says: base branch and kube context",
		CLI:  "promote --base --kube-context plan --kube-context deploy --base --kube-context restart --kube-context watch --kube-context",
		TUI:  "(launch) --base --kube-context — the root flags every subcommand's own flag defaults to (#105)",
	},
	{
		Name: "per-run overrides of what the config file says: digest sources and the registry credential chain",
		CLI:  "promote --digest-sources --registry-auth --cluster-secret --op-ref plan --digest-sources --registry-auth --cluster-secret --op-ref",
		TUI:  "(launch) --digest-sources --registry-auth --cluster-secret --op-ref — the root flags every subcommand's own flag defaults to (#132)",
	},
}

const launch = "(launch)"

var issueRef = regexp.MustCompile(`#\d+`)

// TestEveryOperationIsOnBothFacesOrNamesItsGap is the parity gate.
func TestEveryOperationIsOnBothFacesOrNamesItsGap(t *testing.T) {
	subcommands := parseSubcommands(t, "../../cmd/hoist/main.go")
	flags := parseFlags(t, "../../cmd/hoist")
	msgs, aliases := parseNavigationMessages(t, "../../internal/app/app.go")
	producers := parseProducers(t, "../../internal/app", aliases)

	// Positive control on the parsers: an empty side would let the checks below pass for
	// the wrong reason (AGENTS.md §8: when asserting absence, include a positive control).
	if len(subcommands) == 0 || len(flags) == 0 || len(msgs) == 0 || len(producers) == 0 {
		t.Fatalf("parsers found nothing: subcommands=%v flags=%v msgs=%v producers=%v", subcommands, flags, msgs, producers)
	}

	seenSub := map[string]bool{}
	seenFlag := map[string]bool{} // "subcommand --flag"
	seenMsg := map[string]bool{}

	for _, o := range registry {
		if o.CLI == "" && o.TUI == "" {
			t.Errorf("%q: neither side — a row must do something", o.Name)
			continue
		}
		if (o.CLI == "" || o.TUI == "") && !issueRef.MatchString(o.Gap) {
			t.Errorf("%q: one side only, and Gap %q cites no issue (#NN)", o.Name, o.Gap)
		}
		owner := ""
		for i, tok := range strings.Fields(o.CLI) {
			switch {
			case tok == launch:
				owner = launch
			case i == 0 && !subcommands[tok]:
				t.Errorf("%q: CLI starts with %q, which is not a subcommand in cmd/hoist/main.go", o.Name, tok)
			case strings.HasPrefix(tok, "--"):
				// A flag belongs to the subcommand before it: --override-ci-none on resume
				// and --override-ci-none on promote are two registrations, and a row that
				// names one must not cover the other.
				name := strings.TrimPrefix(tok, "--")
				if owner == "" {
					t.Errorf("%q: %s appears before any subcommand", o.Name, tok)
					continue
				}
				if !flags[owner][name] {
					t.Errorf("%q: CLI names %s, which %s does not register", o.Name, tok, owner)
				}
				seenFlag[owner+" "+tok] = true
			case subcommands[tok]:
				owner = tok
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
	for _, owner := range sortedKeys(flags) {
		for _, f := range sorted(flags[owner]) {
			if !seenFlag[owner+" --"+f] {
				t.Errorf("%s --%s has no row in the parity registry", owner, f)
			}
		}
	}
	for _, m := range sorted(msgs) {
		if !seenMsg[m] {
			t.Errorf("navigation message %s has no row in the parity registry", m)
		}
		// The root consuming a message proves nothing if no screen can emit it: a deleted
		// `p` producer would leave `case matrix.OpenPlanMsg` in place and the row green.
		if !producers[m] {
			t.Errorf("navigation message %s is switched on by the root but no screen constructs it", m)
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

// parseFlags collects every flag registered on a `fs` flag set in the package's non-test
// files — fs.String/Bool/Duration/Int("name", …) and fs.Var/StringVar/…Var(&x, "name", …) —
// keyed by the subcommand that owns the flag set: flag.NewFlagSet("hoist deploy", …) names
// it, and the bare "hoist" set is "(launch)". Each function body is walked in order, so a
// registration is attributed to the NewFlagSet call above it in the same function.
func parseFlags(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f := parse(t, filepath.Join(dir, name))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			owner := ""
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "flag" && sel.Sel.Name == "NewFlagSet" && len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						words := strings.Fields(strings.Trim(lit.Value, `"`))
						owner = launch
						if len(words) > 1 {
							owner = words[1]
						}
					}
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
					if owner == "" {
						t.Fatalf("%s: %s registers a flag before any flag.NewFlagSet", name, fn.Name.Name)
					}
					if out[owner] == nil {
						out[owner] = map[string]bool{}
					}
					out[owner][strings.Trim(lit.Value, `"`)] = true
				}
				return true
			})
		}
	}
	return out
}

// parseNavigationMessages collects every `case pkg.SomethingMsg:` the root Update switches
// on, as written (import alias included), plus the alias → import path map so the
// producers can be found. *BackMsg types are pure navigation — esc — and have no operation
// to pair with, so they are left out.
func parseNavigationMessages(t *testing.T, path string) (map[string]bool, map[string]string) {
	t.Helper()
	f := parse(t, path)
	aliases := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		alias := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		aliases[alias] = p
	}
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
	return out, aliases
}

// parseProducers finds, for every message package the root imports under internal/app, the
// message types some non-test file in that package constructs (`TypeMsg{`), keyed the way
// the root names them (alias.Type).
func parseProducers(t *testing.T, appDir string, aliases map[string]string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for alias, p := range aliases {
		const prefix = "github.com/abradner/hoist/internal/app/"
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		dir := filepath.Join(appDir, strings.TrimPrefix(p, prefix))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f := parse(t, filepath.Join(dir, name))
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); ok && strings.HasSuffix(id.Name, "Msg") {
					out[alias+"."+id.Name] = true
				}
				return true
			})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
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
