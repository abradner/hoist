package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/restart"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/rollout"
)

func TestCheckRestartConfirmed(t *testing.T) {
	configured := service.Settings{Repo: &config.RepoConfig{Envs: config.EnvsConfig{Production: []string{"app-production"}}}}
	for _, tc := range []struct {
		name    string
		set     service.Settings
		env     string
		confirm string
		want    string // "" = allowed
	}{
		{"staging needs none", configured, "app-staging", "", ""},
		{"production, none given", configured, "app-production", "", "app-production is a production env; restarting it requires --confirm-production=app-production as well"},
		{"production, wrong env repeated", configured, "app-production", "app-staging", `--confirm-production="app-staging" does not match --env "app-production"; repeat the exact env to acknowledge`},
		{"production, acknowledged", configured, "app-production", "app-production", ""},
		{"no config, none given", service.Settings{}, "app-production", "", "no repos[] entry is selected, so hoist cannot tell whether app-production is production; pass --confirm-production=app-production to restart it anyway"},
		{"no config, acknowledged", service.Settings{}, "app-production", "app-production", ""},
	} {
		err := tc.set.CheckRestartConfirmed(tc.env, tc.confirm)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		var usage *service.UsageError
		if !errors.As(err, &usage) || usage.Msg != tc.want {
			t.Errorf("%s: got %v, want a UsageError saying %q", tc.name, err, tc.want)
		}
	}
}

func TestRestartIntervalDefaultsToThreeSeconds(t *testing.T) {
	if got := service.New(service.Settings{}, service.Deps{}).RestartInterval(); got != 3*time.Second {
		t.Errorf("unconfigured interval = %v, want 3s", got)
	}
	set := service.Settings{Poll: engine.PollIntervals{Rollout: 7 * time.Second}}
	if got := service.New(set, service.Deps{}).RestartInterval(); got != 7*time.Second {
		t.Errorf("configured interval = %v, want poll.rollout (7s)", got)
	}
}

// TestRestartAsksTheClusterPerCall: a rollout client that failed to build is asked for again on the
// next call (the TUI's R after a cluster that was down at boot), and its real error is returned.
func TestRestartAsksTheClusterPerCall(t *testing.T) {
	fr := &rollout.Fake{}
	calls := 0
	svc := service.New(service.Settings{}, service.Deps{
		Rollout: func(string) (rollout.Rollout, string, error) {
			calls++
			if calls == 1 {
				return nil, "", errors.New("cluster unreachable")
			}
			return fr, "ctx", nil
		},
	})
	if _, err := svc.RestartRead(context.Background(), "app-production", []string{"app"}); err == nil || err.Error() != "cluster unreachable" {
		t.Fatalf("first call: err = %v, want the cluster's own error", err)
	}
	pl, err := svc.RestartRead(context.Background(), "app-production", []string{"app"})
	if err != nil {
		t.Fatalf("second call: %v (a failed client must be retried)", err)
	}
	if len(pl.Targets) != 1 || pl.Targets[0].Name != "app" {
		t.Errorf("plan = %+v, want the one Deployment asked for", pl)
	}
}

// The restart calls use Settings.KubeContext, every one of them: a restart patches a LIVE
// Deployment, so which cluster it lands on is the one thing it must not get wrong. The fakes of
// the CLI tests ignore the context they are handed; this one records it.
func TestRestartCallsAskForTheSettingsKubeContext(t *testing.T) {
	fr := &rollout.Fake{}
	fr.SetDeployment("app-production", "app", rollout.DeploymentStatus{})
	var asked []string
	svc := service.New(service.Settings{KubeContext: "staging-ctx"}, service.Deps{
		Rollout: func(kc string) (rollout.Rollout, string, error) { asked = append(asked, kc); return fr, kc, nil },
	})
	ctx := context.Background()
	pl, err := svc.RestartRead(ctx, "app-production", []string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if _, err := svc.RestartDo(ctx, pl, at); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestartObserve(ctx, "app-production", []string{"app"}, at); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Fatal("no rollout client was asked for (positive control)")
	}
	for _, kc := range asked {
		if kc != "staging-ctx" {
			t.Errorf("a restart call asked for kube context %q, want staging-ctx (all: %v)", kc, asked)
		}
	}
}

// Do and Observe ask for the client when called too, not only Read: a cluster that was down on
// the first R is retried by whichever call needs it next.
func TestRestartDoAndObserveRetryAFailedCluster(t *testing.T) {
	fr := &rollout.Fake{}
	fr.SetDeployment("app-production", "app", rollout.DeploymentStatus{})
	attempts := 0
	svc := service.New(service.Settings{}, service.Deps{
		Rollout: func(string) (rollout.Rollout, string, error) {
			attempts++
			if attempts <= 2 {
				return nil, "", errors.New("cluster unreachable")
			}
			return fr, "ctx", nil
		},
	})
	ctx := context.Background()
	if _, err := svc.RestartDo(ctx, restart.Plan{Env: "app-production", Targets: []rollout.DeploymentStatus{{Name: "app"}}}, time.Now()); err == nil {
		t.Fatal("RestartDo against a down cluster succeeded")
	}
	if _, err := svc.RestartObserve(ctx, "app-production", []string{"app"}, time.Now()); err == nil {
		t.Fatal("RestartObserve against a down cluster succeeded")
	}
	if _, err := svc.RestartDo(ctx, restart.Plan{Env: "app-production", Targets: []rollout.DeploymentStatus{{Name: "app"}}}, time.Now()); err != nil {
		t.Errorf("RestartDo after the cluster came back: %v (the failure was remembered)", err)
	}
}
