package main

import (
	"flag"
	"io"
	"testing"

	"github.com/abradner/hoist/internal/config"
)

// TestPromotionsSettingsCarryAGivenEmptyKubeContext (#295): `hoist resume <id> --kube-context ""`
// and `hoist --kube-context "" promotions` must reach the service as "given, empty" (the
// kubeconfig's current context), never as "not given" (the repo's own context).
func TestPromotionsSettingsCarryAGivenEmptyKubeContext(t *testing.T) {
	rc := config.RepoConfig{Kube: config.KubeConfig{Context: "repo-ctx"}}
	for _, tc := range []struct {
		name string
		args []string
		root bool
		want string
	}{
		{"absent", nil, false, "repo-ctx"},
		{"empty on the command", []string{"--kube-context="}, false, ""},
		{"empty before the command", nil, true, ""},
		{"named", []string{"--kube-context=flag-ctx"}, false, "flag-ctx"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		kc := fs.String("kube-context", "", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		sel := selection{given: map[string]bool{"kube-context": tc.root}}
		set := promotionsSettings(&config.Config{}, *kc, kubeContextGiven(fs, sel))
		if got := set.KubeContextFor(rc); got != tc.want {
			t.Errorf("%s: KubeContextFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}
