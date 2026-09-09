package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/registry"
)

// The root --kube-context (#105) wins over the selected repo's kube.context when given, and
// the config value fills in when it is not — the same precedence --apps-root/--promotable
// already have. On a flags-only run the flag is all there is.
func TestSelectRepoKubeContextPrecedence(t *testing.T) {
	cfg := &config.Config{Repos: []config.RepoConfig{{Path: "/x", Dir: "/x", Kube: config.KubeConfig{Context: "cfg-ctx"}}}}
	given := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, n := range names {
			m[n] = true
		}
		return m
	}
	eff, err := selectRepo(cfg, selection{kubeContext: "flag-ctx", given: given("kube-context")})
	if err != nil || eff.kubeContext != "flag-ctx" {
		t.Fatalf("flag given: kubeContext=%q err=%v, want flag-ctx", eff.kubeContext, err)
	}
	eff, err = selectRepo(cfg, selection{given: given()})
	if err != nil || eff.kubeContext != "cfg-ctx" {
		t.Fatalf("flag not given: kubeContext=%q err=%v, want cfg-ctx", eff.kubeContext, err)
	}
	eff, err = selectRepo(&config.Config{}, selection{repo: "/y", kubeContext: "flag-ctx", given: given("repo", "kube-context")})
	if err != nil || eff.kubeContext != "flag-ctx" {
		t.Fatalf("flags only: kubeContext=%q err=%v, want flag-ctx", eff.kubeContext, err)
	}
}

// --base is never empty on either face: unset means main, given means given.
func TestSelectRepoBaseDefaultsToMain(t *testing.T) {
	eff, err := selectRepo(&config.Config{}, selection{repo: "/y", given: map[string]bool{"repo": true}})
	if err != nil || eff.base != "main" {
		t.Fatalf("base=%q err=%v, want main", eff.base, err)
	}
	eff, err = selectRepo(&config.Config{}, selection{repo: "/y", base: "develop", given: map[string]bool{"repo": true, "base": true}})
	if err != nil || eff.base != "develop" {
		t.Fatalf("base=%q err=%v, want develop", eff.base, err)
	}
}

// The in-flight pane's Resume opens its Argo/rollout adaptors against the operator's explicit
// --kube-context when one was given (#105): one TUI session, one cluster. With none given it
// uses the *resumed promotion's* repo's kube.context — which is repo B's when the TUI was
// opened on repo A, since the pane lists every state file whichever repo it belongs to
// (found by review: passing the selected repo's reconciled context here would refresh the
// wrong cluster).
func TestInFlightResumeKubeContextIsTheOverrideElseThePromotionsOwnRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{Repos: []config.RepoConfig{
		{Path: "/a", Dir: "/a", GitHub: "me/gitops-a", Kube: config.KubeConfig{Context: "ctx-a"}},
		{Path: "/b", Dir: "/b", GitHub: "me/gitops-b", Kube: config.KubeConfig{Context: "ctx-b"}},
	}}
	path, err := engine.StatePath("resume01")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(path, &engine.PromotionState{ID: "resume01", RepoFullName: "me/gitops-b", TargetEnv: "app-production"}); err != nil {
		t.Fatal(err)
	}
	prevForge, prevArgo := newForge, newArgo
	t.Cleanup(func() { newForge, newArgo = prevForge, prevArgo })
	newForge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	// What runTUI hands over when the TUI was opened on repo A: the override is the flag
	// alone, never A's reconciled context.
	sel := selection{repo: "/a", given: map[string]bool{"repo": true}}
	effA, err := selectRepo(cfg, sel)
	if err != nil || effA.kubeContext != "ctx-a" || effA.kubeOverride != "" {
		t.Fatalf("no flag: kubeContext=%q kubeOverride=%q err=%v", effA.kubeContext, effA.kubeOverride, err)
	}
	sel.kubeContext, sel.given["kube-context"] = "flag-ctx", true
	effFlag, err := selectRepo(cfg, sel)
	if err != nil || effFlag.kubeOverride != "flag-ctx" {
		t.Fatalf("flag: kubeOverride=%q err=%v", effFlag.kubeOverride, err)
	}
	for _, tc := range []struct{ override, want string }{{effFlag.kubeOverride, "flag-ctx"}, {effA.kubeOverride, "ctx-b"}} {
		var argoCtx string
		newArgo = func(c string) (argo.Argo, string, error) { argoCtx = c; return nil, c, errors.New("stop here") }
		funcs := buildInFlightFuncs(cfg, tc.override)
		_, _, err := funcs.Resume(context.Background(), "resume01")
		if err == nil || argoCtx != tc.want {
			t.Fatalf("override %q: Resume's newArgo got %q (err=%v), want %q", tc.override, argoCtx, err, tc.want)
		}
		// List re-observes against the same cluster Resume drives, or the pane and the
		// flight screen disagree about one state file (aggregate review of stack #137).
		argoCtx = ""
		if _, err := funcs.List(context.Background()); err != nil || argoCtx != tc.want {
			t.Fatalf("override %q: List's newArgo got %q (err=%v), want %q", tc.override, argoCtx, err, tc.want)
		}
		// And the CLI faces of the same two operations honour the root flag the same way.
		argoCtx = ""
		sel := selection{given: map[string]bool{}}
		if tc.override != "" {
			sel.kubeContext, sel.given["kube-context"] = tc.override, true
		}
		var out, errOut bytes.Buffer
		runPromotions(nil, cfg, sel, &out, &errOut)
		if argoCtx != tc.want {
			t.Fatalf("override %q: promotions' newArgo got %q, want %q (out=%s err=%s)", tc.override, argoCtx, tc.want, out.String(), errOut.String())
		}
		argoCtx = ""
		runResume([]string{"resume01"}, cfg, sel, &out, &errOut)
		if argoCtx != tc.want {
			t.Fatalf("override %q: resume's newArgo got %q, want %q", tc.override, argoCtx, tc.want)
		}
	}
}

// The root --digest-sources (#132) reaches the TUI's resolve function exactly as `plan
// --digest-sources` reaches plan's: none means no resolution and no cluster adaptor is
// even constructed. The default (pods first) is the positive control: it opens the cluster.
func TestRootDigestSourcesReachTheTUIResolveFunc(t *testing.T) {
	var got effective
	var gotCfg *config.Config
	orig := tuiRunner
	t.Cleanup(func() { tuiRunner = orig })
	tuiRunner = func(eff effective, cfg *config.Config, _, _ io.Writer) int { got, gotCfg = eff, cfg; return 42 }
	r, err := gitops.Discover(fixture, gitops.DefaultAppsRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args     []string
		clusters int
	}{
		{[]string{"--repo", fixture, "--digest-sources", "none"}, 0},
		{[]string{"--repo", fixture}, 1},
	} {
		if code := run(tc.args, io.Discard, io.Discard); code != 42 {
			t.Fatalf("%v: exit %d, want the runner's 42", tc.args, code)
		}
		contexts, _ := installFakes(t, &k8s.Fake{}, &registry.Fake{})
		out, err := buildResolveFuncWith(gotCfg, got.cfg, got.promotable, got.resolveFlags())(context.Background(), r, "app-staging", nil)
		if err != nil {
			t.Fatalf("%v: resolve: %v", tc.args, err)
		}
		if len(*contexts) != tc.clusters || (tc.clusters == 0) != (len(out.Resolutions) == 0) {
			t.Errorf("%v: clusters opened %v, resolutions %d; want %d clusters", tc.args, *contexts, len(out.Resolutions), tc.clusters)
		}
	}
}

// An explicit empty --registry-auth is refused at the root with the message `hoist plan`
// gives it — one refusal, one wording, whichever face it is given on.
func TestRootEmptyRegistryAuthRefusedLikePlan(t *testing.T) {
	const want = "--registry-auth: empty; list at least one of env, keychain, cluster, op\n"
	code, _, rootErr := run3(t, "--repo", fixture, "--registry-auth", "")
	if code != exitUsage || rootErr != "hoist: "+want {
		t.Errorf("root: exit %d stderr %q; want %d and %q", code, rootErr, exitUsage, "hoist: "+want)
	}
	code, _, planErr := run3(t, planArgs("--dry-run", "--registry-auth", "")...)
	if code != exitUsage || planErr != "hoist plan: "+want {
		t.Errorf("plan: exit %d stderr %q; want %d and %q", code, planErr, exitUsage, "hoist plan: "+want)
	}
	if strings.TrimPrefix(rootErr, "hoist: ") != strings.TrimPrefix(planErr, "hoist plan: ") {
		t.Errorf("messages differ:\n root %q\n plan %q", rootErr, planErr)
	}
	// promote refuses the same way — it used to fall back to the config's chain silently.
	code, _, promoteErr := run3(t, "--repo", fixture, "promote", "--from", "app-staging", "--to", "app-production", "--registry-auth", "")
	if code != exitUsage || promoteErr != "hoist promote: "+want {
		t.Errorf("promote: exit %d stderr %q; want %d and %q", code, promoteErr, exitUsage, "hoist promote: "+want)
	}
}

// A subcommand's own --digest-sources defaults to the root's and still wins when given:
// `hoist --digest-sources none plan` builds no cluster, `hoist --digest-sources none plan
// --digest-sources pods` builds one.
func TestSubcommandResolveFlagDefaultsToRootAndStillWins(t *testing.T) {
	for _, tc := range []struct {
		extra    []string
		clusters int
	}{
		{nil, 0},
		{[]string{"--digest-sources", "pods"}, 1},
	} {
		contexts, _ := installFakes(t, &k8s.Fake{}, &registry.Fake{})
		args := append([]string{"--digest-sources", "none"}, planArgs(append([]string{"--dry-run"}, tc.extra...)...)...)
		if code, _, errOut := run3(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		if len(*contexts) != tc.clusters {
			t.Errorf("%v: clusters opened %v, want %d", args, *contexts, tc.clusters)
		}
	}
}

// The root --registry-auth and --op-ref (#132) reach the tag picker's and the history's
// registry clients: the order they build with is the flag's, not the registries[] entry's,
// the same override runResolution applies. The entry's own order is the positive control.
func TestRootRegistryAuthReachesTagPickerAndHistory(t *testing.T) {
	cfgPath := writeConfig(t, `
repos:
  - path: `+absFixture(t)+`
    promotable: [ghcr.io/example/]
    apps: { ghcr.io/example/app: example/app }
registries:
  - prefix: ghcr.io/example/
    auth: [keychain]
`)
	var got effective
	var gotCfg *config.Config
	orig := tuiRunner
	t.Cleanup(func() { tuiRunner = orig })
	tuiRunner = func(eff effective, cfg *config.Config, _, _ io.Writer) int { got, gotCfg = eff, cfg; return 42 }
	prevForge := newForge
	t.Cleanup(func() { newForge = prevForge })
	newForge = func(string) (forge.Forge, error) { return &forge.Fake{}, nil }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", cfgPath, "--registry-auth", "env,op", "--op-ref", "op://vault/item/field"}, "env,op op://vault/item/field"},
		{[]string{"--config", cfgPath}, "keychain "},
	} {
		if code := run(tc.args, io.Discard, io.Discard); code != 42 {
			t.Fatalf("%v: exit %d, want 42", tc.args, code)
		}
		regOpts, err := resolutionOptions(gotCfg, got.cfg, got.resolveFlags())
		if err != nil {
			t.Fatal(err)
		}
		_, authCfgs := installFakes(t, &k8s.Fake{}, &registry.Fake{})
		buildTagsFunc(gotCfg, got.cfg, got.kubeContext, regOpts)("ghcr.io/example/app")
		h := buildHistoryFuncs(gotCfg, got.cfg, nil, &forge.Fake{}, nil, "head", got.base, got.kubeContext, regOpts)
		_, _ = h.Revision(context.Background(), image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"})
		if len(*authCfgs) != 2 {
			t.Fatalf("%v: registries built %d, want the picker's and the history's", tc.args, len(*authCfgs))
		}
		for i, c := range *authCfgs {
			order := make([]string, 0, len(c.Order))
			for _, a := range c.Order {
				order = append(order, string(a))
			}
			if g := strings.Join(order, ",") + " " + c.OpRef; g != tc.want {
				t.Errorf("%v: registry %d built with %q, want %q", tc.args, i, g, tc.want)
			}
		}
	}
}
