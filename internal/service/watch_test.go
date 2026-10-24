package service_test

import (
	"context"
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

// The watch clients are asked for Settings.KubeContext, and the Application is read in the
// configured Argo namespace (repos[].kube.argo_namespace), not the default: both are one-token
// moves from the old explicit arguments, and the CLI tests' fakes ignore them.
func TestOpenWatchUsesTheSettingsKubeContextAndArgoNamespace(t *testing.T) {
	r, err := gitops.Discover("../../testdata/repo", gitops.DefaultAppsRoot)
	if err != nil {
		t.Fatal(err)
	}
	name := r.Apps[0].Name
	rc := &config.RepoConfig{}
	rc.Kube.ArgoNamespace = "gitops-system"

	fa, fr := &argo.Fake{}, &rollout.Fake{}
	fa.SetStatus(argo.Application{Namespace: "gitops-system", Name: name}, argo.Status{SyncStatus: "Synced"})
	var asked []string
	svc := service.New(service.Settings{AppsRoot: gitops.DefaultAppsRoot, KubeContext: "staging-ctx", Repo: rc}, service.Deps{
		Argo: func(kc string) (argo.Argo, string, error) { asked = append(asked, "argo:"+kc); return fa, kc, nil },
		Rollout: func(kc string) (rollout.Rollout, string, error) {
			asked = append(asked, "rollout:"+kc)
			return fr, kc, nil
		},
	})
	w, err := svc.OpenWatchApp(r, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 || asked[0] != "argo:staging-ctx" || asked[1] != "rollout:staging-ctx" {
		t.Errorf("clients asked for %v, want argo and rollout in staging-ctx", asked)
	}
	if w.KubeContext != "staging-ctx" {
		t.Errorf("KubeContext = %q", w.KubeContext)
	}
	// The Fake answers only for the Application in the configured namespace: a Read that used the
	// default "argocd" would be ErrNotFound at the Get, before any workload is read.
	_, err = w.Read(context.Background())
	if err != nil && strings.Contains(err.Error(), "reading Argo Application") {
		t.Errorf("Read looked for the Application outside the configured Argo namespace: %v", err)
	}
	if len(fa.Calls) == 0 || !strings.Contains(fa.Calls[0], "gitops-system") {
		t.Errorf("Argo was asked %v, want the gitops-system namespace", fa.Calls)
	}
}
