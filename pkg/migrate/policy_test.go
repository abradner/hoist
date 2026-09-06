package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abradner/hoist/pkg/forge"
)

func TestMigrationsPathOrder(t *testing.T) {
	cases := []struct {
		name       string
		file       string // "" = absent
		configured string
		prefix     string
		source     string
	}{
		{"app repo beats config", "migrations: src/main/resources/db/migration\n", "db/migrate/", "src/main/resources/db/migration/", PrefixFromAppRepo},
		{"app repo can disable", "migrations: none\n", "db/migrate/", "", PrefixFromAppRepo},
		{"file present but silent falls to config", "{}\n", "custom/", "custom/", PrefixFromConfig},
		{"config when no file", "", "db/migrate/", "db/migrate/", PrefixFromConfig},
		{"config can disable", "", "none", "", PrefixFromConfig},
		{"default when nothing", "", "", "db/migrate/", PrefixFromDefault},
		{"trailing slash normalised", "", "db/migrate", "db/migrate/", PrefixFromConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &forge.Fake{Files: map[string][]byte{}}
			if tc.file != "" {
				f.Files["abc "+PolicyFile] = []byte(tc.file)
			}
			prefix, source, err := MigrationsPath(context.Background(), f, "abc", tc.configured)
			if err != nil {
				t.Fatal(err)
			}
			if prefix != tc.prefix || source != tc.source {
				t.Fatalf("got %q from %q; want %q from %q", prefix, source, tc.prefix, tc.source)
			}
		})
	}
}

func TestMigrationsPathRejectsUnknownKeysAndBadPaths(t *testing.T) {
	cases := map[string]string{
		"unknown key":   "migration: db/migrate/\n",
		"absolute path": "migrations: /db/migrate\n",
		"escapes root":  "migrations: ../other\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := &forge.Fake{Files: map[string][]byte{"abc " + PolicyFile: []byte(body)}}
			_, _, err := MigrationsPath(context.Background(), f, "abc", "")
			if err == nil || !strings.Contains(err.Error(), PolicyFile) {
				t.Fatalf("err = %v; want an error naming %s", err, PolicyFile)
			}
		})
	}
}

// A forge error reading the policy file is not "use the default": the same scope gap would
// break Compare next, and a default printed over an app-repo override is a wrong claim.
func TestMigrationsPathForgeErrorPropagates(t *testing.T) {
	f := &forge.Fake{ReadErr: errors.New("HTTP 403")}
	_, _, err := MigrationsPath(context.Background(), f, "abc", "db/migrate/")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizePrefix(t *testing.T) {
	good := map[string]string{"db/migrate/": "db/migrate/", "db/migrate": "db/migrate/", "db/./migrate": "db/migrate/", "none": ""}
	good["\tdb/migrate \n"] = "db/migrate/" // surrounding whitespace is trimmed
	for in, want := range good {
		got, err := NormalizePrefix(in)
		if err != nil || got != want {
			t.Errorf("NormalizePrefix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/abs", "..", "../x", "a/../../b"} {
		if _, err := NormalizePrefix(bad); err == nil {
			t.Errorf("NormalizePrefix(%q) accepted", bad)
		}
	}
}
