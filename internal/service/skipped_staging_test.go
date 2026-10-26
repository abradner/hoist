package service

import (
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/gitops"
)

// TestSkippedStagingWarnsForAnUnpairedSource: a source with no pair of its own (a qa or preview
// env) promoting straight into a production env the pipeline reaches from somewhere else carries
// the skipped-staging warning, and its message says what the pipeline does say, not that it
// "sends qa to staging first" (Alex, 2026-10-26: "warn"). The configured predecessor, a
// non-production target and a target nothing promotes into stay silent.
func TestSkippedStagingWarnsForAnUnpairedSource(t *testing.T) {
	envs := config.EnvsConfig{
		Production: []string{"prod"},
		Pairs:      map[string]string{"dev": "staging", "staging": "prod"},
	}
	pl := &gitops.Plan{SourceEnv: "qa", TargetEnv: "prod"}
	WarnSkippedStagingProduction(pl, envs)
	if len(pl.Warnings) != 1 || pl.Warnings[0].Code != gitops.WarnSkippedStaging {
		t.Fatalf("qa -> prod: want one skipped-staging warning, got %+v", pl.Warnings)
	}
	msg := pl.Warnings[0].Message
	for _, want := range []string{"qa", "prod", "staging"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "sends qa to") {
		t.Errorf("qa has no pair; the message must not claim one: %q", msg)
	}
	for _, c := range []struct {
		source, target string
		envs           config.EnvsConfig
	}{
		{"staging", "prod", envs},
		{"qa", "staging", envs},
		{"qa", "prod", config.EnvsConfig{Production: []string{"prod"}}},
	} {
		pl := &gitops.Plan{SourceEnv: c.source, TargetEnv: c.target}
		WarnSkippedStagingProduction(pl, c.envs)
		if len(pl.Warnings) != 0 {
			t.Errorf("%s -> %s: unexpected warning %+v", c.source, c.target, pl.Warnings)
		}
	}
}
