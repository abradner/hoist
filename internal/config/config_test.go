package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// write puts body in a temp config.yaml and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const full = `
repos:
  - name: my-gitops
    path: ~/src/my-gitops
    github: me/my-gitops
    apps_root: k8s/apps
    promotable: [ghcr.io/me/]
    envs:
      production: [app-production]
      pairs: { app-staging: app-production }
      approval: { app-staging: comment }
    approvers: [me]
    collaborators: true
    ci: { none: prompt, grace: 90s }
    kube: { context: my-cluster }
    digest_sources: [pods, registry]
    apps: { ghcr.io/me/app: me/app }
registries:
  - prefix: ghcr.io/me/
    auth: [env, cluster]
    cluster: { namespace: app-staging, secret: ghcr-pull }
    op: op://vault/item/field
poll: { ci: 1s, approval: 2s, argo: 3s, rollout: 4s, deadline: 5h }
`

func TestLoadFullFile(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	p := write(t, full)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Found || c.File != p {
		t.Errorf("Found=%v File=%q", c.Found, c.File)
	}
	want := Config{
		File:  p,
		Found: true,
		Repos: []RepoConfig{{
			Name: "my-gitops", Path: "~/src/my-gitops", Dir: "/home/me/src/my-gitops", Key: "repos[0]",
			GitHub: "me/my-gitops", AppsRoot: "k8s/apps", Promotable: []string{"ghcr.io/me/"},
			Envs: EnvsConfig{
				Production: []string{"app-production"},
				Pairs:      map[string]string{"app-staging": "app-production"},
				Approval:   map[string]string{"app-staging": "comment", "app-production": "comment"},
			},
			Approvers:     []string{"me"},
			Collaborators: true,
			CI:            CIConfig{None: "prompt", Grace: Duration(90 * time.Second)},
			Kube:          KubeConfig{Context: "my-cluster", ArgoNamespace: "argocd"},
			DigestSources: []string{"pods", "registry"},
			Apps:          map[string]string{"ghcr.io/me/app": "me/app"},
		}},
		Registries: []RegistryConfig{{
			Prefix: "ghcr.io/me/", Auth: []string{"env", "cluster"},
			Cluster: ClusterSecret{Namespace: "app-staging", Secret: "ghcr-pull"},
			Op:      "op://vault/item/field",
		}},
		Poll: PollConfig{CI: Duration(time.Second), Approval: Duration(2 * time.Second), Argo: Duration(3 * time.Second), Rollout: Duration(4 * time.Second), Deadline: Duration(5 * time.Hour)},
	}
	if diff := cmp.Diff(want, *c); diff != "" {
		t.Errorf("Load mismatch (-want +got):\n%s", diff)
	}
}

// Every optional knob gets its documented default, applied by Normalize, so no consumer
// ever sees the zero value; required values (path) get nothing.
func TestDefaults(t *testing.T) {
	c, err := Load(write(t, "repos:\n  - path: /src/gitops\n    envs: { production: [prod] }\nregistries:\n  - prefix: ghcr.io/me/\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Repos[0]
	for name, got := range map[string]any{
		"name":             r.Name,
		"apps_root":        r.AppsRoot,
		"promotable":       r.Promotable,
		"ci.none":          r.CI.None,
		"ci.grace":         r.CI.Grace,
		"digest_sources":   strings.Join(r.DigestSources, ","),
		"approval[prod]":   r.Envs.Approval["prod"],
		"registries.auth":  strings.Join(c.Registries[0].Auth, ","),
		"poll.ci":          c.Poll.CI,
		"poll.approval":    c.Poll.Approval,
		"poll.argo":        c.Poll.Argo,
		"poll.rollout":     c.Poll.Rollout,
		"poll.deadline":    c.Poll.Deadline,
		"Approval(prod)":   r.Approval("prod"),
		"Approval(other)":  r.Approval("staging"),
		"kube.context":     r.Kube.Context,
		"kube.argo_ns":     r.Kube.ArgoNamespace,
		"github":           r.GitHub,
		"registries.op":    c.Registries[0].Op,
		"registries.clust": c.Registries[0].Cluster,
	} {
		want := map[string]any{
			"name":             "gitops",
			"apps_root":        "cluster/apps",
			"promotable":       []string(nil),
			"ci.none":          "green",
			"ci.grace":         Duration(3 * time.Minute),
			"digest_sources":   "pods,manifest,registry",
			"approval[prod]":   "comment",
			"registries.auth":  "env,keychain,cluster,op",
			"poll.ci":          Duration(20 * time.Second),
			"poll.approval":    Duration(30 * time.Second),
			"poll.argo":        Duration(5 * time.Second),
			"poll.rollout":     Duration(3 * time.Second),
			"poll.deadline":    Duration(4 * time.Hour),
			"Approval(prod)":   "comment",
			"Approval(other)":  "auto",
			"kube.context":     "",
			"kube.argo_ns":     "argocd",
			"github":           "",
			"registries.op":    "",
			"registries.clust": ClusterSecret{},
		}[name]
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", name, diff)
		}
	}
	// An explicit per-env setting is the only thing that changes a production default (§4.5).
	c, err = Load(write(t, "repos:\n  - path: /src/gitops\n    envs: { production: [prod], approval: { prod: auto } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Repos[0].Approval("prod"); got != "auto" {
		t.Errorf("explicit approval ignored: %q", got)
	}
}

func TestMissingFileIsEmptyConfigNotError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope", "config.yaml")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if c.Found || c.File != p || len(c.Repos) != 0 {
		t.Errorf("Found=%v File=%q repos=%d", c.Found, c.File, len(c.Repos))
	}
	if c.Poll.CI != Duration(20*time.Second) {
		t.Errorf("defaults not applied to the empty config: poll.ci=%s", c.Poll.CI)
	}
	// An empty file is a present, empty config.
	c, err = Load(write(t, ""))
	if err != nil || !c.Found {
		t.Errorf("empty file: err=%v Found=%v", err, c != nil && c.Found)
	}
}

// A file that exists and does not parse is always an error naming the file — never a
// silent fall-back to defaults.
// The doc comment on Load says a missing file yields normalized, validated defaults; F1
// regression: the missing-file branch used to return right after Normalize, skipping
// Validate. Prove Validate actually runs on that path by making a default fail it, and
// prove the ordinary missing-file path is still untouched (Found=false, no error).
func TestMissingFileStillValidatesDefaults(t *testing.T) {
	saved := defaultPoll.CI
	defaultPoll.CI = 0 // an invalid default: poll durations must be positive
	defer func() { defaultPoll.CI = saved }()

	p := filepath.Join(t.TempDir(), "nope", "config.yaml")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "poll.ci") {
		t.Errorf("missing file with an invalid default: err = %v, want a poll.ci validation error", err)
	}
}

func TestBadFileIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"not yaml":        "repos: [\n",
		"wrong shape":     "repos: notalist\n",
		"bare number dur": "poll: { ci: 20 }\n",
		"bad duration":    "poll: { ci: soon }\n",
	} {
		p := write(t, body)
		c, err := Load(p)
		if err == nil {
			t.Errorf("%s: accepted: %+v", name, c)
			continue
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("%s: error does not name the file: %v", name, err)
		}
	}
	// Positive control: a valid duration string parses via time.ParseDuration.
	c, err := Load(write(t, "poll: { ci: 1m30s, deadline: '2h' }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll.CI != Duration(90*time.Second) || c.Poll.Deadline != Duration(2*time.Hour) {
		t.Errorf("durations: ci=%s deadline=%s", c.Poll.CI, c.Poll.Deadline)
	}
}

// KnownFields(true): a typo is an error that names the line, not a silently ignored key.
func TestUnknownFieldIsRejected(t *testing.T) {
	for _, body := range []string{
		"repos:\n  - path: /x\n    apps_roots: y\n",
		"repos:\n  - path: /x\n    envs: { prodution: [p] }\n",
		"registries:\n  - prefix: ghcr.io/\n    token: abc\n",
		"pol: { ci: 1s }\n",
		"repos:\n  - path: /x\n    dir: /y\n", // derived fields are not file keys either
	} {
		p := write(t, body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("accepted unknown field in:\n%s", body)
			continue
		}
		if s := err.Error(); !strings.Contains(s, "not found in type") || !strings.Contains(s, p) {
			t.Errorf("error should name the file and the unknown field: %v", err)
		}
	}
	// Positive control: the same shape with the right key loads.
	if _, err := Load(write(t, "repos:\n  - path: /x\n    apps_root: y\n")); err != nil {
		t.Fatal(err)
	}
}

// Validation errors carry the file and the YAML path of the bad value, and all of them are
// reported at once.
func TestValidationErrorsNamePath(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"registry prefix required", "registries:\n  - auth: [env]\n", "registries[0].prefix: is required"},
		{"empty promotable", "repos:\n  - path: /x\n    promotable: []\n", "repos[0].promotable: must not be empty"},
		{"empty promotable entry", "repos:\n  - path: /x\n    promotable: ['']\n", "repos[0].promotable[0]: empty prefix"},
		{"pair to itself", "repos:\n  - path: /x\n    envs: { pairs: { app-staging: app-staging } }\n", "repos[0].envs.pairs.app-staging: an env cannot promote to itself"},
		{"pair empty target", "repos:\n  - path: /x\n    envs: { pairs: { app-staging: '' } }\n", "repos[0].envs.pairs.app-staging: target env is empty"},
		{"approval enum", "repos:\n  - path: /x\n    envs: { approval: { app-production: maybe } }\n", "repos[0].envs.approval.app-production: want one of comment|auto, got \"maybe\""},
		{"production twice", "repos:\n  - path: /x\n    envs: { production: [p, p] }\n", "repos[0].envs.production[1]: \"p\" listed twice"},
		{"ci.none enum", "repos:\n  - path: /x\n    ci: { none: yellow }\n", "repos[0].ci.none: want one of green|prompt|block"},
		{"ci.grace negative", "repos:\n  - path: /x\n    ci: { grace: -1s }\n", "repos[0].ci.grace: must be a positive duration"},
		{"digest_sources enum", "repos:\n  - path: /x\n    digest_sources: [pods, argo]\n", "repos[0].digest_sources[1]: want one of pods|manifest|registry"},
		{"digest_sources empty", "repos:\n  - path: /x\n    digest_sources: []\n", "repos[0].digest_sources: must not be empty"},
		{"digest_sources dup", "repos:\n  - path: /x\n    digest_sources: [pods, pods]\n", "repos[0].digest_sources[1]: \"pods\" listed twice"},
		{"github shape", "repos:\n  - path: /x\n    github: justme\n", "repos[0].github: want owner/name"},
		{"apps value shape", "repos:\n  - path: /x\n    apps: { ghcr.io/me/app: app }\n", "repos[0].apps.ghcr.io/me/app: want owner/name"},
		{"apps key with tag", "repos:\n  - path: /x\n    apps: { ghcr.io/me/app:v1: me/app }\n", "repos[0].apps.ghcr.io/me/app:v1: key must be an image repo"},
		{"auth enum", "registries:\n  - prefix: ghcr.io/\n    auth: [gh]\n", "registries[0].auth[0]: want one of env|keychain|cluster|op"},
		{"auth empty", "registries:\n  - prefix: ghcr.io/\n    auth: []\n", "registries[0].auth: must not be empty"},
		{"cluster half", "registries:\n  - prefix: ghcr.io/\n    cluster: { namespace: ns }\n", "registries[0].cluster: needs both namespace and secret"},
		{"op shape", "registries:\n  - prefix: ghcr.io/\n    op: vault/item\n", "registries[0].op: want an op://"},
		{"poll negative", "poll: { argo: -5s }\n", "poll.argo: must be a positive duration"},
		{"duplicate name", "repos:\n  - path: /x\n    name: a\n  - path: /y\n    name: a\n", "repos[1].name: \"a\" is also repos[0]"},
		{"duplicate path", "repos:\n  - path: /x\n  - path: /x/\n", "repos[1].path: \"/x/\" is also repos[0]"},
		{"nameless pathless", "repos:\n  - github: me/x\n", "repos[0]: needs a name or a path"},
		{"approver empty", "repos:\n  - path: /x\n    approvers: ['']\n", "repos[0].approvers[0]: empty login"},
	}
	for _, tc := range cases {
		p := write(t, tc.body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if s := err.Error(); !strings.Contains(s, p+": "+tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, s, p+": "+tc.want)
		}
	}
	// All problems are reported together, not one per round trip.
	_, err := Load(write(t, "repos:\n  - path: /x\n    promotable: []\n    ci: { none: yellow }\nregistries:\n  - auth: [env]\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"repos[0].promotable", "repos[0].ci.none", "registries[0].prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("combined error lacks %s: %v", want, err)
		}
	}
	// Env names are not checked against a repo here: a name no wrapper declares loads.
	if _, err := Load(write(t, "repos:\n  - path: /x\n    envs: { production: [does-not-exist], pairs: { nor: this } }\n")); err != nil {
		t.Errorf("unknown env names must not be validated at load: %v", err)
	}
}

// F2 regression: a decoder that only calls Decode once silently ignores every document
// after the first `---`, so a second document (accidental or a leftover template) never
// takes effect and never errors either. Parse must reject it.
func TestMultipleYAMLDocumentsRejected(t *testing.T) {
	p := write(t, "repos:\n  - path: /x\n---\nrepos:\n  - path: /y\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("accepted a file with two YAML documents")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("error = %v, want it to name the multiple-documents problem", err)
	}
	// Positive control: a single document, with trailing blank lines and a comment, still
	// loads — the rejection is specific to an actual second document.
	if _, err := Load(write(t, "repos:\n  - path: /x\n\n# trailing comment\n\n")); err != nil {
		t.Errorf("a single document with trailing noise: %v", err)
	}
}

// F3 regression: yaml.v3 never trims a scalar, so a stray leading/trailing space in an
// env name, a pairs key/value, an approval key, or a registry prefix loads without error
// and then silently never matches anything compared against it by ==. Reject instead of
// accepting a value that would sabotage its own comparison.
func TestSurroundingWhitespaceRejected(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"production padded", "repos:\n  - path: /x\n    envs: { production: [' prod'] }\n", "repos[0].envs.production[0]: \" prod\" has surrounding whitespace"},
		{"pairs key padded", "repos:\n  - path: /x\n    envs: { pairs: { 'staging ': production } }\n", "repos[0].envs.pairs[staging ] (key): \"staging \" has surrounding whitespace"},
		{"pairs value padded", "repos:\n  - path: /x\n    envs: { pairs: { staging: ' production' } }\n", "repos[0].envs.pairs.staging: \" production\" has surrounding whitespace"},
		{"approval key padded", "repos:\n  - path: /x\n    envs: { approval: { ' prod': comment } }\n", "repos[0].envs.approval[ prod] (key): \" prod\" has surrounding whitespace"},
		{"registry prefix padded", "registries:\n  - prefix: ' ghcr.io/'\n", "registries[0].prefix: \" ghcr.io/\" has surrounding whitespace"},
		{"argo_namespace padded", "repos:\n  - path: /x\n    kube: { argo_namespace: ' argocd' }\n", "repos[0].kube.argo_namespace: \" argocd\" has surrounding whitespace"},
	}
	for _, tc := range cases {
		p := write(t, tc.body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if s := err.Error(); !strings.Contains(s, tc.want) {
			t.Errorf("%s: error %q lacks %q", tc.name, s, tc.want)
		}
	}
	// Positive control: the same shapes with no surrounding whitespace load cleanly.
	if _, err := Load(write(t, "repos:\n  - path: /x\n    envs: { production: [prod], pairs: { staging: prod }, approval: { prod: comment } }\nregistries:\n  - prefix: ghcr.io/\n")); err != nil {
		t.Errorf("clean values rejected: %v", err)
	}
}

func TestTildeExpansion(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	c, err := Load(write(t, "repos:\n  - path: ~/src/a\n  - path: '~'\n  - path: /abs/b\n  - path: rel/c\n  - path: ~user/d\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ path, dir, name string }{
		{"~/src/a", "/home/me/src/a", "a"},
		{"~", "/home/me", "me"}, // a bare ~ is YAML null; it has to be quoted to mean home
		{"/abs/b", "/abs/b", "b"},
		{"rel/c", "rel/c", "c"},
		{"~user/d", "~user/d", "d"}, // ~user is not supported; left as written
	}
	for i, w := range want {
		r := c.Repos[i]
		if r.Path != w.path || r.Dir != w.dir || r.Name != w.name {
			t.Errorf("repos[%d]: Path=%q Dir=%q Name=%q, want %+v", i, r.Path, r.Dir, r.Name, w)
		}
	}
	// Path is kept as written, so config show never prints the home directory.
	out, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "/home/me") || !strings.Contains(string(out), "path: ~/src/a") {
		t.Errorf("Marshal resolved the home directory:\n%s", out)
	}
}

func TestRepoSelection(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	two, err := Load(write(t, "repos:\n  - path: ~/src/a\n  - name: bee\n    path: /src/b\n"))
	if err != nil {
		t.Fatal(err)
	}
	one, err := Load(write(t, "repos:\n  - path: /src/only\n"))
	if err != nil {
		t.Fatal(err)
	}
	none, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	cases := []struct {
		name string
		cfg  *Config
		sel  string
		want string // Key of the expected repo, or "" for an error
		err  string
	}{
		{"only entry, no selector", one, "", "repos[0]", ""},
		{"two entries, no selector", two, "", "", "2 repos configured; choose one with --repo <name|path>: a, bee"},
		{"no entries, no selector", none, "", "", "no repos configured"},
		{"by default name", two, "a", "repos[0]", ""},
		{"by explicit name", two, "bee", "repos[1]", ""},
		{"by path as written", two, "~/src/a", "repos[0]", ""},
		{"by expanded path", two, "/home/me/src/a", "repos[0]", ""},
		{"by unclean path", two, "/src/b/", "repos[1]", ""},
		{"by relative path", &Config{File: "f", Repos: []RepoConfig{{Name: "here", Path: cwd, Dir: cwd, Key: "repos[0]"}}}, ".", "repos[0]", ""},
		{"unknown selector", two, "zed", "", "\"zed\" is not in"},
		{"selector on empty config", none, "zed", "", "\"zed\" is not in"},
	}
	for _, tc := range cases {
		got, err := tc.cfg.Repo(tc.sel)
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.err)
			}
			if strings.Contains(tc.err, "is not in") && !errors.Is(err, ErrUnknownRepo) {
				t.Errorf("%s: not-found error must wrap ErrUnknownRepo: %v", tc.name, err)
			}
			continue
		}
		if err != nil || got.Key != tc.want {
			t.Errorf("%s: got %q err %v, want %q", tc.name, got.Key, err, tc.want)
		}
	}
	if _, err := two.Repo(""); errors.Is(err, ErrUnknownRepo) {
		t.Error("ambiguity must not look like not-found")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	t.Setenv("XDG_CONFIG_HOME", "")
	p, err := DefaultPath()
	if err != nil || p != filepath.Join("/home/me", ".config", "hoist", "config.yaml") {
		t.Errorf("DefaultPath() = %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	p, err = DefaultPath()
	if err != nil || p != filepath.Join("/xdg", "hoist", "config.yaml") {
		t.Errorf("DefaultPath() with XDG_CONFIG_HOME = %q, %v", p, err)
	}
}

func TestRedactedAndMarshal(t *testing.T) {
	c, err := Load(write(t, full))
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Redacted().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "op://") || !strings.Contains(s, "op: <redacted>") {
		t.Errorf("op reference not redacted:\n%s", s)
	}
	if c.Registries[0].Op != "op://vault/item/field" {
		t.Error("Redacted mutated the original")
	}
	for _, want := range []string{"grace: 1m30s", "deadline: 5h0m0s", "apps_root: k8s/apps", "path: ~/src/my-gitops"} {
		if !strings.Contains(s, want) {
			t.Errorf("effective config lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "dir:") || strings.Contains(s, "key:") || strings.Contains(s, "file:") {
		t.Errorf("derived fields leaked into the file shape:\n%s", s)
	}
	// The effective config is itself a valid config (checked unredacted: <redacted> is not an op ref).
	raw, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw, "show"); err != nil {
		t.Errorf("Marshal output does not round-trip: %v", err)
	}
}

// The documented example must load: it is the schema readers copy from.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "docs", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Found || len(c.Repos) != 1 || len(c.Registries) != 1 || c.Repos[0].Name != "my-gitops" {
		t.Errorf("example: Found=%v repos=%d registries=%d", c.Found, len(c.Repos), len(c.Registries))
	}
}
