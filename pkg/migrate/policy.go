package migrate

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/abradner/hoist/pkg/forge"
)

// PolicyFile is the file an app repo may carry to describe itself to hoist. Today it holds
// one key. It lives in the app repo because the app repo is what knows its own migration
// convention; the gitops repo's config can override per image repo, and the Rails default
// covers the repos this tool was written for.
const PolicyFile = ".hoist.yaml"

// MigrationsDisabled is the value, in either the policy file or config, that says "this app
// has no migrations to look for".
const MigrationsDisabled = "none"

// DefaultMigrationsPath is the Rails convention, the default when neither the app repo nor
// config says otherwise.
const DefaultMigrationsPath = "db/migrate/"

// Where a migrations prefix came from, for the screen header.
const (
	PrefixFromAppRepo = "app repo"
	PrefixFromConfig  = "config"
	PrefixFromDefault = "default"
)

// policy is PolicyFile's schema, decoded with KnownFields so a typo is an error naming the
// file, the same rule internal/config applies (AGENTS.md §4.9).
type policy struct {
	Migrations string `yaml:"migrations"`
}

// MigrationsPath decides the migrations prefix for one app repo at ref: the repo's own
// PolicyFile if it has one and names migrations, else configured (what config.yaml holds for
// this image repo — already defaulted by config's Normalize, so it is never empty for a
// mapped repo; "" here means the caller had no config at all), else DefaultMigrationsPath.
// The returned prefix is cleaned and ends in "/", or is "" when disabled. source is one of
// the PrefixFrom* constants.
//
// A forge error reading the policy file is returned: it is the same scope gap Compare would
// hit next, and answering "default" over it would print a default the app repo may have
// overridden.
func MigrationsPath(ctx context.Context, f forge.Forge, ref, configured string) (prefix, source string, err error) {
	content, ok, err := f.ReadFile(ctx, ref, PolicyFile)
	if err != nil {
		return "", "", fmt.Errorf("migrate: reading %s: %w", PolicyFile, err)
	}
	if ok {
		var p policy
		dec := yaml.NewDecoder(bytes.NewReader(content))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			return "", "", fmt.Errorf("migrate: %s in the app repo: %w", PolicyFile, err)
		}
		if v := strings.TrimSpace(p.Migrations); v != "" {
			prefix, err := NormalizePrefix(v)
			if err != nil {
				return "", "", fmt.Errorf("migrate: %s in the app repo: migrations: %w", PolicyFile, err)
			}
			return prefix, PrefixFromAppRepo, nil
		}
	}
	if v := strings.TrimSpace(configured); v != "" {
		prefix, err := NormalizePrefix(v)
		if err != nil {
			return "", "", fmt.Errorf("migrate: configured migrations path: %w", err)
		}
		return prefix, PrefixFromConfig, nil
	}
	return DefaultMigrationsPath, PrefixFromDefault, nil
}

// NormalizePrefix cleans a migrations path to the form Delta matches on: relative, no "..",
// trailing "/"; or "" for MigrationsDisabled. internal/config's Validate calls it too, so the
// two agree on what is acceptable.
func NormalizePrefix(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == MigrationsDisabled {
		return "", nil
	}
	if v == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(v, "/") {
		return "", fmt.Errorf("%q is absolute; want a path relative to the app repo root", v)
	}
	clean := path.Clean(v)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q escapes the app repo root", v)
	}
	return clean + "/", nil
}
