package service_test

import (
	"testing"

	"github.com/abradner/hoist/internal/config"
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
