package service

import (
	"context"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/registry"
)

// F4 regression: registryEntryFor and entryAuthConfig, together, are what used to be a
// single registryFor call applied to every repo — the bug. registryEntryFor picks by
// longest matching prefix, never the first entry that merely overlaps; entryAuthConfig
// must never leak one entry's cluster secret or op ref onto a repo a different entry (or
// no entry) covers. Moved from cmd/hoist/resolution_test.go (service:Plan, PR B).
func TestRegistryEntryForPicksLongestMatchNeverFirstOverlap(t *testing.T) {
	registries := []config.RegistryConfig{
		{Prefix: "ghcr.io/", Auth: []string{"env"}, Op: "op://vault/broad/field"},
		{Prefix: "ghcr.io/example/web", Auth: []string{"cluster"}, Cluster: config.ClusterSecret{Namespace: "app-staging", Secret: "web-pull"}},
	}
	cases := []struct {
		repo, wantPrefix string
	}{
		{"ghcr.io/example/web", "ghcr.io/example/web"}, // matches both; the longer, more specific entry wins
		{"ghcr.io/example/webhooks", "ghcr.io/"},       // falls through to the broad entry, not the web-scoped one
		{"ghcr.io/example/marketing", "ghcr.io/"},      // only the broad entry covers it
		{"quay.io/example/other", ""},                  // no entry covers it at all
	}
	for _, tc := range cases {
		e := registryEntryFor(registries, tc.repo)
		got := ""
		if e != nil {
			got = e.Prefix
		}
		if got != tc.wantPrefix {
			t.Errorf("registryEntryFor(%q) = %q, want %q", tc.repo, got, tc.wantPrefix)
		}
	}

	// entryAuthConfig: the entry decides its own values only. The broad ghcr.io/ entry's
	// op ref must never appear for a repo the specific web entry covers, and the web
	// entry's cluster secret must never appear for a repo only the broad entry covers.
	webEntry := registryEntryFor(registries, "ghcr.io/example/web")
	auth, clusterSecret, opRef := entryAuthConfig(webEntry, ResolveOptions{})
	if len(auth) != 1 || auth[0] != registry.AuthCluster || clusterSecret != "app-staging/web-pull" || opRef != "" {
		t.Errorf("web entry: auth=%v clusterSecret=%q opRef=%q, want cluster/app-staging/web-pull/\"\"", auth, clusterSecret, opRef)
	}
	broadEntry := registryEntryFor(registries, "ghcr.io/example/marketing")
	auth, clusterSecret, opRef = entryAuthConfig(broadEntry, ResolveOptions{})
	if len(auth) != 1 || auth[0] != registry.AuthEnv || clusterSecret != "" || opRef != "op://vault/broad/field" {
		t.Errorf("broad entry: auth=%v clusterSecret=%q opRef=%q, want env/\"\"/op://vault/broad/field", auth, clusterSecret, opRef)
	}
	// A repo no entry covers gets the default chain, and neither entry's cluster secret
	// or op ref.
	auth, clusterSecret, opRef = entryAuthConfig(nil, ResolveOptions{})
	if len(auth) != len(registry.DefaultAuthOrder) || clusterSecret != "" || opRef != "" {
		t.Errorf("unmatched repo: auth=%v clusterSecret=%q opRef=%q, want the default chain and nothing else", auth, clusterSecret, opRef)
	}
	// An explicit flag overrides every entry outright, for any repo.
	auth, clusterSecret, opRef = entryAuthConfig(webEntry, ResolveOptions{Auth: []registry.AuthSource{registry.AuthEnv}, ClusterSecret: "x/y", OpRef: "op://flag/a/b"})
	if len(auth) != 1 || auth[0] != registry.AuthEnv || clusterSecret != "x/y" || opRef != "op://flag/a/b" {
		t.Errorf("flag override: auth=%v clusterSecret=%q opRef=%q", auth, clusterSecret, opRef)
	}
}

// Codex P1 (draft #29 pass): a bare-host prefix like "ghcr.io" must never match a
// different host that merely shares its leading bytes. Before the fix, registryEntryFor
// used a raw strings.HasPrefix, so an entry configured for "ghcr.io" (no trailing slash)
// — with an op ref or a cluster secret attached — matched "ghcr.io.attacker.example/…"
// too, and entryAuthConfig would hand that host the entry's credentials. Moved from
// cmd/hoist/resolution_test.go (service:Plan, PR B).
func TestRegistryEntryForRejectsHostConfusion(t *testing.T) {
	registries := []config.RegistryConfig{
		{Prefix: "ghcr.io", Op: "op://vault/ghcr/field"}, // no trailing slash, deliberately
	}
	if e := registryEntryFor(registries, "ghcr.io.attacker.example/org/app"); e != nil {
		t.Fatalf("registryEntryFor matched an attacker-controlled host via prefix confusion: %+v", e)
	}
	if e := registryEntryFor(registries, "ghcr.io/abradner/app"); e == nil || e.Op != "op://vault/ghcr/field" {
		t.Fatalf("registryEntryFor(real repo) = %+v, want the ghcr.io entry", e)
	}
}

// A direct Head/Tags call on the per-repo registry (M6's tag picker will make them) must be
// routed to the client scoped to that repo and must error for a repo with no registry —
// never fall through to whichever client happened to be built first (F4). Moved from
// cmd/hoist/resolution_test.go (service:Plan, PR B).
func TestMultiRegistryRoutesDirectCallsByRepo(t *testing.T) {
	a := &registry.Fake{Digests: map[string]string{"ghcr.io/acme/app:v1": "sha256:" + strings.Repeat("a", 64)}}
	b := &registry.Fake{Digests: map[string]string{"quay.io/other/app:v1": "sha256:" + strings.Repeat("b", 64)}}
	m := &multiRegistry{byRepo: map[string]registry.Registry{"ghcr.io/acme/app": a, "quay.io/other/app": b}, primary: a}
	ctx := context.Background()
	if d, err := m.Head(ctx, image.Ref{Repo: "quay.io/other/app", Tag: "v1"}); err != nil || !strings.HasPrefix(d, "sha256:bbbb") {
		t.Fatalf("Head routed wrong: %q, %v", d, err)
	}
	if _, err := m.Head(ctx, image.Ref{Repo: "ghcr.io/nobody/app", Tag: "v1"}); err == nil || !strings.Contains(err.Error(), "no registry configured") {
		t.Fatalf("unmatched repo: got %v, want no-registry error", err)
	}
	if _, err := m.Tags(ctx, "ghcr.io/nobody/app"); err == nil {
		t.Fatal("Tags for an unmatched repo must error, not use the primary client")
	}
}
