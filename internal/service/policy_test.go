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

func pairSettings(pairs map[string]string) service.Settings {
	return service.Settings{Repo: &config.RepoConfig{Envs: config.EnvsConfig{Pairs: pairs}}}
}

// TestPairEnvs: --to alone and the config pairs a source into it — filled in; ambiguous, unpaired or
// unconfigured — refused with the flag to give; --from alone — refused, the target is never derived;
// both or neither — returned as they are.
func TestPairEnvs(t *testing.T) {
	one := pairSettings(map[string]string{"stg": "prod"})
	many := pairSettings(map[string]string{"stg": "prod", "canary": "prod"})
	for _, tc := range []struct {
		name         string
		set          service.Settings
		from, to     string
		wantF, wantT string
		wantErr      string
	}{
		{"both given", one, "a", "b", "a", "b", ""},
		{"neither given", one, "", "", "", "", ""},
		{"to alone", one, "", "prod", "stg", "prod", ""},
		{"from alone is refused, even when it is paired", one, "stg", "", "", "", "--from stg alone does not say where to promote; name the target with --to (--to alone takes its source from envs.pairs)"},
		{"to with no source", one, "", "stg", "", "", "no envs.pairs entry promotes into stg; name the source with --from"},
		{"from alone, a chain end", one, "prod", "", "", "", "--from prod alone does not say where to promote; name the target with --to (--to alone takes its source from envs.pairs)"},
		{"to with several sources", many, "", "prod", "", "", "several envs.pairs entries promote into prod (canary, stg); name the source with --from"},
		{"from alone, no pair at all", pairSettings(nil), "stg", "", "", "", "--from stg alone does not say where to promote; name the target with --to (--to alone takes its source from envs.pairs)"},
		{"to, no configured repo", service.Settings{}, "", "prod", "", "", "no repos[] entry is selected, so envs.pairs is unknown; name the source with --from"},
		{"from alone, no configured repo", service.Settings{}, "stg", "", "", "", "--from stg alone does not say where to promote; name the target with --to (--to alone takes its source from envs.pairs)"},
	} {
		f, to, err := tc.set.PairEnvs(tc.from, tc.to)
		if tc.wantErr != "" {
			var usage *service.UsageError
			if !errors.As(err, &usage) || usage.Msg != tc.wantErr {
				t.Errorf("%s: err = %v, want a UsageError saying %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || f != tc.wantF || to != tc.wantT {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q)", tc.name, f, to, err, tc.wantF, tc.wantT)
		}
	}
}

// Service.Preflight hands the request's Confirmed through to the gate. A TUI direct start that has
// not been confirmed must be stopped HERE, before StartPromotion takes a claim and saves state;
// Drive's own gate would stop it later, which is the hole Preflight exists to close. It fails with
// Preflight passing Confirmed:true.
func TestPreflightPassesConfirmedThroughToTheGate(t *testing.T) {
	svc := service.New(prodSettings("me/gitops"), service.Deps{})
	var blocked *engine.BlockedError
	if err := svc.Preflight("app-staging", service.Mode{Direct: true}); !errors.As(err, &blocked) {
		t.Errorf("an unconfirmed direct start: got %v, want a BlockedError from Preflight itself", err)
	}
	if err := svc.Preflight("app-staging", service.Mode{Direct: true, Confirmed: true}); err != nil {
		t.Errorf("the same start confirmed was refused: %v", err)
	}
}
