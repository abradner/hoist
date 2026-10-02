package main

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
)

// A command that takes no positional argument refuses any leftover one. flag.Parse stops at the
// first non-flag argument, so before this every flag typed after a stray word was dropped
// without a message — and the word itself was ignored — which let `deploy … oops --dry-run`
// perform the deploy and `restart … oops --dry-run` restart the env (#242). Each case below
// asserts the refusal AND that the thing the dropped flag was guarding did not happen.

func wantStrayRefused(t *testing.T, got int, stderr string) {
	t.Helper()
	if got != exitUsage {
		t.Errorf("exit %d, want %d (usage); stderr: %s", got, exitUsage, stderr)
	}
	if !strings.Contains(stderr, `unexpected argument "oops"`) {
		t.Errorf("stderr must name the stray argument:\n%s", stderr)
	}
}

func TestDeployRefusesAStrayArgumentBeforeDryRun(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy", "--env", "app-production", "--image", "ghcr.io/example/app:v3@" + digestThird, "oops", "--dry-run"}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	if len(f.PRs()) != 0 {
		t.Fatalf("a deploy whose --dry-run was never read opened PR(s): %+v", f.PRs())
	}
	if out.Len() != 0 {
		t.Errorf("nothing ran, so nothing belongs on stdout:\n%s", out.String())
	}
}

func TestPromoteRefusesAStrayArgument(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "promote", "--from", "app-staging", "--to", "app-production", "oops", "--override-ci-none"}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	if len(f.PRs()) != 0 {
		t.Fatalf("a promotion ran past an argument it did not understand: %+v", f.PRs())
	}
}

func TestRestartRefusesAStrayArgumentBeforeDryRun(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.RestartedAt = ""
	f := restartFake(t, st)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "restart", "--env", "app-production", "oops", "--dry-run"}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Fatalf("a restart whose --dry-run was never read restarted the env: %v", f.Calls)
		}
	}
}

func TestPlanRefusesAStrayArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	got := run([]string{"--repo", fixture, "plan", "--from", "app-staging", "--to", "app-production", "oops", "--dry-run"}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	if out.Len() != 0 {
		t.Errorf("no plan should be printed for a command line hoist did not understand:\n%s", out.String())
	}
}

func TestPromotionsRefusesAStrayArgument(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "promotions", "oops", "--archived"}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	if out.Len() != 0 {
		t.Errorf("nothing should be listed:\n%s", out.String())
	}
}

func TestWatchRefusesAStrayArgument(t *testing.T) {
	cfgPath, _, _ := newWatchFixture(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	got := runWatch([]string{"--app", "app-app-production", "oops", "--once"}, cfg, selection{given: map[string]bool{}}, &out, &errOut)
	wantStrayRefused(t, got, errOut.String())
	if out.Len() != 0 {
		t.Errorf("nothing should be watched:\n%s", out.String())
	}
}

// The control for every case above: the same command lines without the stray word still do what
// they did, so the refusal is about the word and not about the flags around it.
func TestCommandsWithoutAStrayArgumentAreUnchanged(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	var out, errOut bytes.Buffer
	if got := run([]string{"--config", cfgPath, "deploy", "--env", "app-production", "--image", "ghcr.io/example/app:v3@" + digestThird, "--dry-run"}, &out, &errOut); got != 0 {
		t.Fatalf("deploy --dry-run: exit %d; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 0 || out.Len() == 0 {
		t.Fatalf("a dry run prints its plan and opens nothing: PRs=%d stdout=%q", len(f.PRs()), out.String())
	}
	// -h still reaches flag's own help, and a flag after `--` is still a positional, refused.
	out.Reset()
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "promotions", "-h"}, &out, &errOut); got != 0 {
		t.Errorf("-h: exit %d; stderr: %s", got, errOut.String())
	}
	errOut.Reset()
	if got := run([]string{"--config", cfgPath, "promotions", "--", "--archived"}, &out, &errOut); got != exitUsage || !strings.Contains(errOut.String(), `unexpected argument "--archived"`) {
		t.Errorf("after --: exit %d; stderr: %s", got, errOut.String())
	}
}

// A flag handed to another flag as its value is the same hole by another door: what an unquoted
// empty shell variable produces (`--confirm-direct $ENV --dry-run`). --confirm-direct without
// --direct is never looked at, so the deploy simply ran.
func TestDeployRefusesDryRunSwallowedAsAValue(t *testing.T) {
	cfgPath, _, f := newPromoteFixture(t)
	rolloutFor(t, "ghcr.io/example/app:v3@"+digestThird)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "deploy", "--env", "app-production", "--image", "ghcr.io/example/app:v3@" + digestThird, "--confirm-direct", "--dry-run"}, &out, &errOut)
	if got != exitUsage || !strings.Contains(errOut.String(), "--confirm-direct takes a value, but what follows it is --dry-run") {
		t.Errorf("exit %d; stderr: %s", got, errOut.String())
	}
	if len(f.PRs()) != 0 {
		t.Fatalf("a deploy whose --dry-run was taken as another flag's value opened PR(s): %+v", f.PRs())
	}
}

func TestRestartRefusesDryRunSwallowedAsAValue(t *testing.T) {
	cfgPath, _, _ := newPromoteFixture(t)
	st := rolled("app-production")
	st.RestartedAt = ""
	f := restartFake(t, st)
	var out, errOut bytes.Buffer
	got := run([]string{"--config", cfgPath, "restart", "--env", "app-production", "--confirm-production", "--dry-run"}, &out, &errOut)
	if got != exitUsage || !strings.Contains(errOut.String(), "--confirm-production takes a value, but what follows it is --dry-run") {
		t.Errorf("exit %d; stderr: %s", got, errOut.String())
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "Restart ") {
			t.Fatalf("a restart whose --dry-run was taken as another flag's value restarted the env: %v", f.Calls)
		}
	}
}

func TestFlagTakenAsValue(t *testing.T) {
	fs := flag.NewFlagSet("hoist test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("env", "", "")
	fs.String("confirm", "", "")
	fs.Bool("dry-run", false, "")
	for _, tc := range []struct {
		args      []string
		name, val string
	}{
		{[]string{"--env", "prod", "--dry-run"}, "", ""},
		{[]string{"--confirm", "--dry-run"}, "confirm", "--dry-run"},
		{[]string{"-confirm", "-dry-run"}, "confirm", "-dry-run"},
		{[]string{"--env", "prod", "--confirm", "--dry-run"}, "confirm", "--dry-run"},
		{[]string{"--confirm=--dry-run"}, "", ""},             // said outright with "=": the operator's value
		{[]string{"--confirm", "-"}, "", ""},                  // a lone dash is an ordinary value
		{[]string{"--dry-run", "--env", "prod"}, "", ""},      // a boolean takes no separate value
		{[]string{"--confirm"}, "", ""},                       // flag.Parse reports the missing value
		{[]string{"--bogus", "--dry-run"}, "", ""},            // and the unknown flag
		{[]string{"stray", "--confirm", "--dry-run"}, "", ""}, // flags end at the first positional
		{[]string{"--", "--confirm", "--dry-run"}, "", ""},
	} {
		name, val, got := flagTakenAsValue(fs, tc.args)
		if name != tc.name || val != tc.val || got != (tc.name != "") {
			t.Errorf("%v: got (%q, %q, %v), want (%q, %q)", tc.args, name, val, got, tc.name, tc.val)
		}
	}
}

// TestOnlyTheParsersCallFlagParse keeps the rule mechanical: a subcommand's command line goes
// through parseFlagsOnly, or through a parser that says it takes an id — never straight to
// flag.Parse, which is how every hole above got in. The allowlist names each function that may
// call it and why.
func TestOnlyTheParsersCallFlagParse(t *testing.T) {
	allowed := map[string]string{
		"run":            "the root flag set: what is left over is the subcommand",
		"parseFlagsOnly": "the checked parser itself",
		"parseWithID":    "the id-taking parser resume and abandon share",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Parse" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fs" {
					callers = append(callers, fn.Name.Name)
				}
				return true
			})
		}
	}
	sort.Strings(callers)
	// Positive control: the search finds the calls it is meant to police.
	if len(callers) < 3 {
		t.Fatalf("found only %v calling fs.Parse; the search is broken", callers)
	}
	for _, c := range callers {
		if _, ok := allowed[c]; !ok {
			t.Errorf("%s calls fs.Parse directly; parse its command line through parseFlagsOnly (or a parser that takes an id)", c)
		}
	}
}
