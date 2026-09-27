package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/git"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

const histSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func historyFixture(t *testing.T, appForge *forge.Fake, reg *registry.Fake) (*config.RepoConfig, *int) {
	t.Helper()
	cfg, err := config.Parse([]byte(`
repos:
  - path: /x
    apps: { ghcr.io/example/app: example/app }
    migrations: { ghcr.io/example/app: db/migrate/ }
registries:
  - prefix: ghcr.io/example/
    auth: [env]
`), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	forges := 0
	prevForge, prevRegistry := newForge, newRegistry
	newForge = func(ownerRepo string) (forge.Forge, error) {
		forges++
		if ownerRepo != "example/app" {
			t.Fatalf("newForge(%q); want the app repo", ownerRepo)
		}
		return appForge, nil
	}
	newRegistry = func(registry.AuthConfig) (registry.Registry, error) { return reg, nil }
	t.Cleanup(func() { newForge, newRegistry = prevForge, prevRegistry })
	return &cfg.Repos[0], &forges
}

// fixedHeadGit wraps whatever newGit already is (embedding lets the other, unused methods
// panic-on-nil harmlessly, since no test here calls them) and answers RevParse("HEAD") with a
// fixed sha — historyRepoFixture's own way of making h.blame's resolved HEAD deterministic
// without a real git repository on disk.
type fixedHeadGit struct {
	git.Git
	sha string
}

func (g fixedHeadGit) RevParse(context.Context, string, string) (string, bool, error) {
	return g.sha, true, nil
}

// historyRepoFixture builds a real *service.Service over a minimal on-disk gitops repo (one
// family, one Application) with its own view loaded, and pins newGit so h.blame's HEAD read
// answers headSHA deterministically — what buildHistoryFuncs' LiveAge now needs from svc.Repo()
// (PR 7) instead of the literal blameRef string the old signature took directly.
func historyRepoFixture(t *testing.T, headSHA string) *service.Service {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cluster/apps/app-production-app.yaml", ""+
		"apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: app-app-production\n  namespace: argocd\n"+
		"spec:\n  project: default\n  source:\n    repoURL: https://git.example.test/example/gitops.git\n    targetRevision: main\n    path: cluster/apps/app-production/app\n"+
		"  destination:\n    server: https://kubernetes.default.svc\n    namespace: app-production\n")
	write("cluster/apps/app-production/app/deployment.yaml", ""+
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n        - name: app\n          image: ghcr.io/example/app:v2\n")
	prevGit := newGit
	newGit = fixedHeadGit{sha: headSHA}
	t.Cleanup(func() { newGit = prevGit })
	svc := service.New(service.Settings{RepoDir: root, AppsRoot: "cluster/apps"}, serviceDeps())
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}
	return svc
}

// The image label answers without a ResolveRef call, the policy file is read at the target
// revision, and one delta call fans out to exactly the forge calls it needs.
func TestBuildHistoryFuncsLabelWinsAndPolicyIsRead(t *testing.T) {
	appForge := &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			histSHA + "..." + strings.Repeat("b", 40): {Status: "ahead", Total: 1, Commits: []forge.Commit{{SHA: "c1", Subject: "x"}}, Files: []string{"app/x.rb"}},
		},
		Files: map[string][]byte{strings.Repeat("b", 40) + " " + migrate.PolicyFile: []byte("migrations: migrations/\n")},
	}
	reg := &registry.Fake{Configs: map[string]registry.ImageMeta{
		"ghcr.io/example/app:v1": {Labels: map[string]string{migrate.RevisionLabel: histSHA}},
		"ghcr.io/example/app:v2": {Labels: map[string]string{migrate.RevisionLabel: strings.Repeat("b", 40)}},
	}}
	rc, forges := historyFixture(t, appForge, reg)
	cfg := &config.Config{Registries: []config.RegistryConfig{{Prefix: "ghcr.io/example/", Auth: []string{"env"}}}}
	h := buildHistoryFuncs(rc, &forge.Fake{}, nil, "main", svcFor(cfg))
	if !h.Mapped("ghcr.io/example/app") || h.Mapped("ghcr.io/example/other") {
		t.Fatal("Mapped must follow repos[].apps")
	}
	from := image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"}
	to := image.Ref{Repo: "ghcr.io/example/app", Tag: "v2"}
	d, err := h.Delta(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if d.From.Source != migrate.SourceLabel || d.To.Source != migrate.SourceLabel {
		t.Fatalf("sources = %s/%s; want the label", d.From.Source, d.To.Source)
	}
	if d.Prefix != "migrations/" || d.PrefixSource != migrate.PrefixFromAppRepo {
		t.Fatalf("prefix = %q from %q; want the app repo's .hoist.yaml to win over config", d.Prefix, d.PrefixSource)
	}
	for _, c := range appForge.Calls {
		if strings.HasPrefix(c, "ResolveRef") {
			t.Fatalf("a full-length label needs no ResolveRef; calls = %v", appForge.Calls)
		}
	}
	// Second ask: served from the cache, no new forge calls, one forge client for the session.
	n := len(appForge.Calls)
	if _, err := h.Delta(context.Background(), from, to); err != nil || len(appForge.Calls) != n {
		t.Fatalf("second delta: err=%v calls grew from %d to %d", err, n, len(appForge.Calls))
	}
	if *forges != 1 {
		t.Fatalf("newForge called %d times; want one memoised client", *forges)
	}
}

// An unmapped image repo is a named gap, not a forge construction.
func TestBuildHistoryFuncsUnmappedRepoIsUnresolved(t *testing.T) {
	rc, forges := historyFixture(t, &forge.Fake{}, &registry.Fake{})
	h := buildHistoryFuncs(rc, nil, nil, "main", svcFor(nil))
	ref := image.Ref{Repo: "ghcr.io/example/other", Tag: "v1"}
	_, err := h.Delta(context.Background(), ref, ref)
	if !errors.Is(err, migrate.ErrUnresolved) || !strings.Contains(err.Error(), "repos[].apps") {
		t.Fatalf("err = %v; want ErrUnresolved naming repos[].apps", err)
	}
	if *forges != 0 {
		t.Fatalf("newForge called %d times for an unmapped repo", *forges)
	}
}

// A revision the label names but the app repo does not hold is said so, not "unknown".
func TestBuildHistoryFuncsLabelNotInRepoIsNamed(t *testing.T) {
	appForge := &forge.Fake{Files: map[string][]byte{}}
	reg := &registry.Fake{Configs: map[string]registry.ImageMeta{
		"ghcr.io/example/app:v1": {Labels: map[string]string{migrate.RevisionLabel: histSHA}},
		"ghcr.io/example/app:v2": {Labels: map[string]string{migrate.RevisionLabel: strings.Repeat("b", 40)}},
	}}
	rc, _ := historyFixture(t, appForge, reg)
	h := buildHistoryFuncs(rc, nil, nil, "main", svcFor(nil))
	_, err := h.Delta(context.Background(), image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"}, image.Ref{Repo: "ghcr.io/example/app", Tag: "v2"})
	if !errors.Is(err, migrate.ErrUnresolved) || !strings.Contains(err.Error(), "is not in example/app") {
		t.Fatalf("err = %v", err)
	}
}

// No forge for the gitops repo means no live age — the reason, not a panic.
func TestBuildHistoryFuncsLiveAgeWithoutForge(t *testing.T) {
	rc, _ := historyFixture(t, &forge.Fake{}, &registry.Fake{})
	h := buildHistoryFuncs(rc, nil, errors.New("gh not logged in"), "main", svcFor(nil))
	_, err := h.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3})
	if err == nil || !strings.Contains(err.Error(), "gh not logged in") {
		t.Fatalf("err = %v", err)
	}
	gitopsForge := &forge.Fake{Blames: map[string]map[int]forge.LineOrigin{"head f.yaml": {3: {SHA: "1111"}}}}
	h = buildHistoryFuncs(rc, gitopsForge, nil, "main", historyRepoFixture(t, "head"))
	age, err := h.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3})
	if err != nil || age.SHA != "1111" {
		t.Fatalf("age=%+v err=%v", age, err)
	}
}

// No apps mapping means no delta — but the live age blames the gitops repo, which needs no
// mapping, so it is still wired (an empty repos[].apps used to lose "since 4 weeks ago" too).
func TestBuildHistoryFuncsWithoutAppsKeepsLiveAge(t *testing.T) {
	gitopsForge := &forge.Fake{Blames: map[string]map[int]forge.LineOrigin{"head f.yaml": {3: {SHA: "1111"}}}}
	h := buildHistoryFuncs(&config.RepoConfig{}, gitopsForge, nil, "main", historyRepoFixture(t, "head"))
	if h.Delta != nil || h.Revision != nil {
		t.Fatal("no apps mapping: no delta or revision func, so screens degrade without a call")
	}
	if h.Mapped == nil || h.Mapped("ghcr.io/example/x") {
		t.Fatal("Mapped must answer false for every repo")
	}
	if h.LiveAge == nil {
		t.Fatal("LiveAge must be wired without an apps mapping")
	}
	age, err := h.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3})
	if err != nil || age.SHA != "1111" {
		t.Fatalf("age=%+v err=%v", age, err)
	}
}

// TestLiveAgeUsesCurrentViewHEAD is PR 7's own regression test for buildHistoryFuncs' half of
// the boot-frozen-closure bug: the old signature took blameRef as a plain string, resolved once
// in runTUI at boot, so a HEAD that moved later in the session (a later LoadRepo/RefreshRepo —
// an F5, or the post-completion refresh PR 4 added) was never seen — every LiveAge kept blaming
// the boot-time sha. buildHistoryFuncs now reads svc.Repo() through historyAdaptor.blame on
// every call, and blame's own cache is keyed by the *gitops.Repo pointer the view holds, which
// changes identity on every LoadRepo (gitops.Discover always returns a fresh value) — so a HEAD
// that moves between two LiveAge calls is picked up on the second one. This test fails if
// blameRef were still resolved once (or cached only by root path): the second LiveAge would see
// "aaaa000...", not the moved "bbbb111...".
func TestLiveAgeUsesCurrentViewHEAD(t *testing.T) {
	svc := historyRepoFixture(t, "aaaa000")
	gitopsForge := &forge.Fake{Blames: map[string]map[int]forge.LineOrigin{
		"aaaa000 f.yaml": {3: {SHA: "1111"}},
		"bbbb111 f.yaml": {3: {SHA: "2222"}},
	}}
	h := buildHistoryFuncs(&config.RepoConfig{}, gitopsForge, nil, "main", svc)

	age, err := h.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3})
	if err != nil || age.SHA != "1111" {
		t.Fatalf("before HEAD moves: age=%+v err=%v, want the sha at aaaa000's blame", age, err)
	}

	// Move HEAD and reload the view, the way an F5 refresh (svc.RefreshRepo) or a landed
	// promotion's relist does.
	newGit = fixedHeadGit{sha: "bbbb111"}
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}

	age, err = h.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3})
	if err != nil || age.SHA != "2222" {
		t.Fatalf("after HEAD moves: age=%+v err=%v, want the sha at bbbb111's blame", age, err)
	}
}
