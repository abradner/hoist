package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/forge"
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
