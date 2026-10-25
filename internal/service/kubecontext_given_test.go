package service

import (
	"testing"

	"github.com/abradner/hoist/internal/config"
)

// TestKubeContextForHonoursAGivenEmptyOverride (#295, whole-stack read of T10): an explicitly
// empty --kube-context means the kubeconfig's current context. Start already used it after
// #295, but list and resume (KubeContextFor, via ArgoRolloutFor) still read "" as "not given"
// and fell back to the repo's own context, so `hoist resume <id> --kube-context ""` drove Argo
// and the rollout watch against a different cluster from the one the promotion started on.
func TestKubeContextForHonoursAGivenEmptyOverride(t *testing.T) {
	rc := config.RepoConfig{Kube: config.KubeConfig{Context: "repo-ctx"}}
	for _, tc := range []struct {
		name string
		set  Settings
		want string
	}{
		{"not given: the repo decides", Settings{}, "repo-ctx"},
		{"given a name", Settings{KubeOverride: "flag-ctx", KubeOverrideSet: true}, "flag-ctx"},
		{"given empty: the current context", Settings{KubeOverride: "", KubeOverrideSet: true}, ""},
	} {
		if got := tc.set.KubeContextFor(rc); got != tc.want {
			t.Errorf("%s: KubeContextFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}
