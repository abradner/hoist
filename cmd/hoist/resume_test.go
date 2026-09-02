package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
)

func TestPromotionsEmptyStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var out, errOut bytes.Buffer
	if got := runPromotions(nil, cfg, &out, &errOut); got != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", got, errOut.String())
	}
	if !strings.Contains(out.String(), "no promotions found") {
		t.Fatalf("stdout = %q, want a no-promotions message", out.String())
	}
}

func TestResumeRequiresExactlyOneOfIDOrEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume(nil, cfg, &bytes.Buffer{}, &errOut); got != exitUsage {
		t.Fatalf("neither id nor --env: exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
	errOut.Reset()
	if got := runResume([]string{"--env", "app-production", "some-id"}, cfg, &bytes.Buffer{}, &errOut); got != exitUsage {
		t.Fatalf("both id and --env: exit %d, want %d; stderr: %s", got, exitUsage, errOut.String())
	}
}

func TestResumeUnknownIDFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume([]string{"no-such-id"}, cfg, &bytes.Buffer{}, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if !strings.Contains(errOut.String(), "no-such-id") {
		t.Fatalf("stderr should name the missing id: %s", errOut.String())
	}
}

func TestResumeUnknownEnvFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "xdg-state"))
	cfg := &config.Config{}
	var errOut bytes.Buffer
	if got := runResume([]string{"--env", "app-production"}, cfg, &bytes.Buffer{}, &errOut); got != exitFailure {
		t.Fatalf("exit %d, want %d; stderr: %s", got, exitFailure, errOut.String())
	}
	if !strings.Contains(errOut.String(), "app-production") {
		t.Fatalf("stderr should name the env: %s", errOut.String())
	}
}
