package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/argo"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/rollout"
)

func TestSettingsArgoNamespace(t *testing.T) {
	if got := (service.Settings{}).ArgoNamespace(); got != config.DefaultArgoNamespace {
		t.Errorf("flags-only ArgoNamespace = %q, want the default %q", got, config.DefaultArgoNamespace)
	}
	rc := &config.RepoConfig{}
	rc.Kube.ArgoNamespace = "gitops-system"
	if got := (service.Settings{Repo: rc}).ArgoNamespace(); got != "gitops-system" {
		t.Errorf("configured ArgoNamespace = %q, want gitops-system", got)
	}
}

// TestOpenWatchAppNamesTheContextAndRetriesAFailedCluster: the handle carries the kubeconfig
// context the client resolved to (the CLI prints it), and a
// client that failed to build is asked for again on the next open rather than remembered.
func TestOpenWatchAppNamesTheContextAndRetriesAFailedCluster(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", gitops.DefaultAppsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Apps) == 0 {
		t.Fatal("fixture repo has no Applications")
	}
	name := r.Apps[0].Name

	fa, fr := &argo.Fake{}, &rollout.Fake{}
	attempts := 0
	svc := service.New(service.Settings{AppsRoot: gitops.DefaultAppsRoot}, service.Deps{
		Argo: func(string) (argo.Argo, string, error) {
			attempts++
			if attempts == 1 {
				return nil, "", errors.New("cluster unreachable")
			}
			return fa, "resolved-context", nil
		},
		Rollout: func(string) (rollout.Rollout, string, error) { return fr, "resolved-context", nil },
	})
	if _, err := svc.OpenWatchApp(r, name); err == nil || !strings.Contains(err.Error(), "cluster unreachable") {
		t.Fatalf("first open: err = %v, want the cluster failure", err)
	}
	w, err := svc.OpenWatchApp(r, name)
	if err != nil {
		t.Fatalf("second open: %v (a failed client must be retried, not remembered)", err)
	}
	if w.KubeContext != "resolved-context" {
		t.Errorf("KubeContext = %q, want the context the client resolved to", w.KubeContext)
	}
	if _, err := svc.OpenWatchApp(r, "no-such-app"); err == nil || !strings.Contains(err.Error(), `no Application "no-such-app" found under cluster/apps (known: `) {
		t.Errorf("unknown app: err = %v", err)
	}
}
