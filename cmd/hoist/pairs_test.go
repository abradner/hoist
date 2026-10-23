package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// pairsConfig is a config for the fixture repo whose envs.pairs says app-staging promotes into
// app-production, plus whatever extra pairs a case adds (written as "src: tgt" YAML entries).
func pairsConfig(t *testing.T, extraPairs ...string) string {
	t.Helper()
	pairs := append([]string{"app-staging: app-production"}, extraPairs...)
	return writeConfig(t, "repos:\n  - path: "+absFixture(t)+"\n    promotable: [ghcr.io/example/]\n    envs:\n      pairs: { "+strings.Join(pairs, ", ")+" }\n")
}

// #173: envs.pairs states which env promotes into which, and until now only the TUI read it.
// `hoist plan` and `hoist promote` demanded both --from and --to even when the config already
// named the other one. With one given, the other is the configured pair; the dry run's output is
// byte for byte the explicit command's, and stderr says which pair it used.
func TestPlanTakesTheMissingEnvFromEnvsPairs(t *testing.T) {
	cfg := pairsConfig(t)
	args := func(extra ...string) []string {
		return append([]string{"--config", cfg, "--digest-sources", "none", "plan", "--dry-run"}, extra...)
	}
	code, want, errOut := run3(t, args("--from", "app-staging", "--to", "app-production")...)
	if code != 0 {
		t.Fatalf("explicit plan: exit %d: %s", code, errOut)
	}
	for _, tc := range []struct {
		name  string
		extra []string
		note  string
	}{
		{"--to alone", []string{"--to", "app-production"}, "--from not given; envs.pairs promotes app-staging into app-production"},
		{"--from alone", []string{"--from", "app-staging"}, "--to not given; envs.pairs promotes app-staging into app-production"},
	} {
		code, got, errOut := run3(t, args(tc.extra...)...)
		if code != 0 {
			t.Errorf("%s: exit %d: %s", tc.name, code, errOut)
			continue
		}
		if got != want {
			t.Errorf("%s: output differs from the explicit command's:\n%s\nwant:\n%s", tc.name, got, want)
		}
		if !strings.Contains(errOut, tc.note) {
			t.Errorf("%s: stderr %q does not say which pair was used (%q)", tc.name, errOut, tc.note)
		}
	}
}

// The refusals say what is missing and why the config could not fill it in, and none of them
// writes anything: they are usage errors, as a missing flag always was.
func TestPlanRefusesWhenEnvsPairsCannotFillTheOtherEnvIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   string
		extra []string
		want  string
	}{
		{"nothing promotes into the target", pairsConfig(t), []string{"--to", "app-staging"}, "no envs.pairs entry promotes into app-staging; name the source with --from"},
		{"the source has no target", pairsConfig(t), []string{"--from", "app-production"}, "no envs.pairs entry says where app-production promotes to; name the target with --to"},
		{"several sources", pairsConfig(t, "app-canary: app-production"), []string{"--to", "app-production"}, "several envs.pairs entries promote into app-production (app-canary, app-staging); name the source with --from"},
		{"no configured repo", "", []string{"--to", "app-production"}, "no repos[] entry is selected, so envs.pairs is unknown; name the source with --from"},
	} {
		args := []string{"--digest-sources", "none", "plan", "--dry-run"}
		if tc.cfg != "" {
			args = append([]string{"--config", tc.cfg}, args...)
		} else {
			args = append([]string{"--repo", absFixture(t)}, args...)
		}
		code, out, errOut := run3(t, append(args, tc.extra...)...)
		if code != exitUsage || !strings.Contains(errOut, "hoist plan: "+tc.want) {
			t.Errorf("%s: exit %d, stderr %q; want %d and %q", tc.name, code, errOut, exitUsage, tc.want)
		}
		if out != "" {
			t.Errorf("%s: a refused plan printed on stdout: %s", tc.name, out)
		}
	}
}

// A flag given EMPTY is not a flag left out: `--to "$TARGET"` with TARGET unset is refused as it
// always was, not quietly filled in from the config (§9 entry 16: an argument hoist did not act on
// must not look accepted, least of all when the difference is a write). The explicit empty value
// is the case the "given" check exists for; the omitted flag beside it is the positive control.
func TestAnEmptyEnvFlagIsRefusedNotFilledInFromEnvsPairs(t *testing.T) {
	cfg := pairsConfig(t)
	for _, cmd := range []string{"plan", "promote"} {
		for _, args := range [][]string{
			{"--from", "app-staging", "--to", ""},
			{"--from", "", "--to", "app-production"},
			{"--to", ""},
		} {
			full := append([]string{"--config", cfg, "--digest-sources", "none", cmd}, args...)
			if cmd == "plan" {
				full = append(full, "--dry-run")
			}
			code, out, errOut := run3(t, full...)
			if code != exitUsage || !strings.Contains(errOut, "hoist "+cmd+": --repo, --from and --to are required") || strings.Contains(errOut, "envs.pairs promotes") || out != "" {
				t.Errorf("%s %q: exit %d, stdout %q, stderr %.200q; want the plain refusal and nothing planned or started", cmd, args, code, out, errOut)
			}
		}
	}
	if code, _, errOut := run3(t, "--config", cfg, "--digest-sources", "none", "plan", "--dry-run", "--to", "app-production"); code != 0 {
		t.Errorf("positive control: the same plan with the flag omitted rather than empty failed: exit %d: %.200s", code, errOut)
	}
}

// Neither env given is still the old refusal: a config with pairs does not say which of them the
// operator means.
func TestPlanWithNeitherEnvIsStillRefused(t *testing.T) {
	code, _, errOut := run3(t, "--config", pairsConfig(t), "plan", "--dry-run")
	if code != exitUsage || !strings.Contains(errOut, "hoist plan: --repo, --from and --to are required") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// promote resolves the pair the same way, before anything is planned or written: with the pair
// filled in from the config it drives the promotion `promote --from … --to …` drives, and with
// nothing to fill it in it refuses without touching the repo or the forge.
func TestPromoteTakesTheMissingEnvFromEnvsPairs(t *testing.T) {
	cfgPath, clone, _ := newPromoteFixture(t)
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	paired := strings.Replace(string(body), "      grace: 5ms\n", "      grace: 5ms\n    envs:\n      pairs: { app-staging: app-production }\n", 1)
	if paired == string(body) {
		t.Fatal("could not add envs.pairs to the fixture config")
	}
	if err := os.WriteFile(cfgPath, []byte(paired), 0o644); err != nil {
		t.Fatal(err)
	}

	before := refsOf(t, clone)
	code, _, errOut := run3(t, "--config", cfgPath, "promote", "--to", "app-staging")
	if code != exitUsage || !strings.Contains(errOut, "hoist promote: no envs.pairs entry promotes into app-staging; name the source with --from") {
		t.Fatalf("refusal: exit %d, stderr %q", code, errOut)
	}
	if refsOf(t, clone) != before {
		t.Fatal("a refused promote changed the clone")
	}

	code, out, errOut := run3(t, "--config", cfgPath, "promote", "--to", "app-production")
	if code != 0 {
		t.Fatalf("promote --to app-production: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "hoist promote: app-staging -> app-production") {
		t.Errorf("stdout does not report the paired promotion:\n%s", out)
	}
	if !strings.Contains(errOut, "--from not given; envs.pairs promotes app-staging into app-production") {
		t.Errorf("stderr does not say which pair was used:\n%s", errOut)
	}
}

// refsOf is every ref in the clone: a refused promote creates no branch.
func refsOf(t *testing.T, clone string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", clone, "for-each-ref").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
