package service_test

import (
	"errors"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/engine"
	"github.com/abradner/hoist/internal/service"
)

// Moved from internal/app/plan/rows_test.go unchanged (service:Plan, PR B): IsProduction is
// now the service layer's own rule, shared by the plan screen's UI gating and Plan's own
// deploy-into-production warning.
func TestIsProduction(t *testing.T) {
	envs := config.EnvsConfig{Production: []string{"app-production"}}
	if !service.IsProduction("app-production", envs) {
		t.Error("app-production should be production")
	}
	if service.IsProduction("app-staging", envs) {
		t.Error("app-staging should not be production")
	}
}

func prodSettings(github string) service.Settings {
	return service.Settings{Repo: &config.RepoConfig{GitHub: github, Envs: config.EnvsConfig{Production: []string{"app-production"}}}}
}

// TestPreflightConfig: what a start needs configured, in the one set of words every face uses. The
// CLI's two halves (PreflightConfig, PreflightGate) and Service.Preflight are the same checks, so
// a case here is a case for `hoist promote`, `hoist deploy` and the TUI at once.
func TestPreflightConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  service.Settings
		m    service.Mode
		want string // "" = accepted
	}{
		{"pr, flags only", service.Settings{}, service.Mode{}, "the selected repo has no github: owner/name configured; add repos[].github to the config file"},
		{"pr, no github", prodSettings(""), service.Mode{}, "the selected repo has no github: owner/name configured; add repos[].github to the config file"},
		{"pr, configured", prodSettings("me/gitops"), service.Mode{}, ""},
		{"direct, flags only", service.Settings{}, service.Mode{Direct: true}, "--direct requires a configured repo (repos[].envs.production must be known — hoist cannot otherwise tell a production env from any other)"},
		{"direct, no github", prodSettings(""), service.Mode{Direct: true}, "the selected repo has no github: owner/name configured; add repos[].github to the config file (direct mode still needs it — promotion identity is hashed from it)"},
		{"direct, configured", prodSettings("me/gitops"), service.Mode{Direct: true}, ""},
	} {
		err := tc.set.PreflightConfig(tc.m)
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

// TestPreflightGateRefusesProductionAndUnconfirmed: the gate is the engine step's own answer —
// production is refused outright whatever was confirmed, and an unconfirmed direct commit is
// blocked. Service.Preflight composes the two halves, so it refuses what either does.
func TestPreflightGateRefusesProductionAndUnconfirmed(t *testing.T) {
	set := prodSettings("me/gitops")
	var blocked *engine.BlockedError
	if err := set.PreflightGate("app-production", service.Mode{Direct: true, Confirmed: true}); !errors.As(err, &blocked) {
		t.Errorf("a confirmed direct commit into production: got %v, want a BlockedError", err)
	}
	if err := set.PreflightGate("app-staging", service.Mode{Direct: true}); !errors.As(err, &blocked) {
		t.Errorf("an unconfirmed direct commit: got %v, want a BlockedError", err)
	}
	if err := set.PreflightGate("app-staging", service.Mode{Direct: true, Confirmed: true}); err != nil {
		t.Errorf("a confirmed direct commit into staging was refused: %v", err)
	}

	svc := service.New(set, service.Deps{})
	if err := svc.Preflight("app-production", service.Mode{Direct: true, Confirmed: true}); !errors.As(err, &blocked) {
		t.Errorf("Preflight into production: got %v, want a BlockedError", err)
	}
	var usage *service.UsageError
	if err := service.New(service.Settings{}, service.Deps{}).Preflight("app-staging", service.Mode{Direct: true, Confirmed: true}); !errors.As(err, &usage) {
		t.Errorf("Preflight with no config: got %v, want the config half's UsageError", err)
	}
	if err := svc.Preflight("app-production", service.Mode{}); err != nil {
		t.Errorf("a PR into production is not refused by the direct gate: %v", err)
	}
}
