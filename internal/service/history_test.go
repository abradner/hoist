package service_test

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

var (
	histFrom = image.Ref{Repo: "ghcr.io/example/app", Tag: "v1"}
	histTo   = image.Ref{Repo: "ghcr.io/example/app", Tag: "v2"}
)

// histRepoConfig is a repo that maps ghcr.io/example/app to example/app and names a migrations
// directory for it; gitops is the gitops repo's own owner/name, for the live age.
func histRepoConfig(gitopsRepo string) *config.RepoConfig {
	return &config.RepoConfig{
		GitHub:     gitopsRepo,
		Apps:       map[string]string{"ghcr.io/example/app": "example/app"},
		Migrations: map[string]string{"ghcr.io/example/app": "db/migrate/"},
	}
}

// histDeps serves forges by owner/name through forgeFor, and one registry.
func histDeps(forgeFor func(ownerRepo string) (forge.Forge, error), reg registry.Registry) service.Deps {
	return service.Deps{
		Forge:    forgeFor,
		Registry: func(registry.AuthConfig) (registry.Registry, error) { return reg, nil },
		Git:      func() git.Git { return git.Exec{} },
	}
}

func labelled() *registry.Fake {
	return &registry.Fake{Configs: map[string]registry.ImageMeta{
		"ghcr.io/example/app:v1": {Labels: map[string]string{migrate.RevisionLabel: histSHA}},
		"ghcr.io/example/app:v2": {Labels: map[string]string{migrate.RevisionLabel: strings.Repeat("b", 40)}},
	}}
}

func appForge() *forge.Fake {
	return &forge.Fake{
		Comparisons: map[string]forge.Comparison{
			histSHA + "..." + strings.Repeat("b", 40): {Status: "ahead", Total: 1, Commits: []forge.Commit{{SHA: "c1", Subject: "x"}}, Files: []string{"app/x.rb"}},
		},
		Files: map[string][]byte{strings.Repeat("b", 40) + " " + migrate.PolicyFile: []byte("migrations: migrations/\n")},
	}
}

// The image label answers without a ResolveRef call, the policy file is read at the target
// revision, and one delta call fans out to exactly the forge calls it needs.
func TestHistoryDeltaLabelWinsAndPolicyIsRead(t *testing.T) {
	af := appForge()
	forges := 0
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(ownerRepo string) (forge.Forge, error) {
		forges++
		if ownerRepo != "example/app" {
			t.Fatalf("Forge(%q); want the app repo", ownerRepo)
		}
		return af, nil
	}, labelled()))
	if !svc.HistoryMapped("ghcr.io/example/app") || svc.HistoryMapped("ghcr.io/example/other") {
		t.Fatal("HistoryMapped must follow repos[].apps")
	}
	d, err := svc.HistoryDelta(context.Background(), histFrom, histTo)
	if err != nil {
		t.Fatal(err)
	}
	if d.From.Source != migrate.SourceLabel || d.To.Source != migrate.SourceLabel {
		t.Fatalf("sources = %s/%s; want the label", d.From.Source, d.To.Source)
	}
	if d.Prefix != "migrations/" || d.PrefixSource != migrate.PrefixFromAppRepo {
		t.Fatalf("prefix = %q from %q; want the app repo's .hoist.yaml to win over config", d.Prefix, d.PrefixSource)
	}
	for _, c := range af.Calls {
		if strings.HasPrefix(c, "ResolveRef") {
			t.Fatalf("a full-length label needs no ResolveRef; calls = %v", af.Calls)
		}
	}
	// Second ask: served from the cache, no new forge calls, one forge client for the session.
	n := len(af.Calls)
	if _, err := svc.HistoryDelta(context.Background(), histFrom, histTo); err != nil || len(af.Calls) != n {
		t.Fatalf("second delta: err=%v calls grew from %d to %d", err, n, len(af.Calls))
	}
	if forges != 1 {
		t.Fatalf("the forge was built %d times; want one remembered client", forges)
	}
}

// An unmapped image repo is a named gap, not a forge construction.
func TestHistoryDeltaUnmappedRepoIsUnresolved(t *testing.T) {
	forges := 0
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(string) (forge.Forge, error) { forges++; return &forge.Fake{}, nil }, &registry.Fake{}))
	ref := image.Ref{Repo: "ghcr.io/example/other", Tag: "v1"}
	_, err := svc.HistoryDelta(context.Background(), ref, ref)
	if !errors.Is(err, migrate.ErrUnresolved) || !strings.Contains(err.Error(), "repos[].apps") {
		t.Fatalf("err = %v; want ErrUnresolved naming repos[].apps", err)
	}
	if forges != 0 {
		t.Fatalf("a forge was built %d times for an unmapped repo", forges)
	}
}

// A revision the label names but the app repo does not hold is said so, not "unknown".
func TestHistoryDeltaLabelNotInRepoIsNamed(t *testing.T) {
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(string) (forge.Forge, error) { return &forge.Fake{Files: map[string][]byte{}}, nil }, labelled()))
	_, err := svc.HistoryDelta(context.Background(), histFrom, histTo)
	if !errors.Is(err, migrate.ErrUnresolved) || !strings.Contains(err.Error(), "is not in example/app") {
		t.Fatalf("err = %v", err)
	}
}

// CS-3: a forge that could not be built when history first asked (no `gh` login yet) is asked for
// again on the next call, not remembered as broken for the rest of the session. It fails on the
// adaptor this replaced, which kept the first error in a per-session map.
func TestHistoryRecoversWhenTheForgeComesBack(t *testing.T) {
	af := appForge()
	attempts := 0
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(string) (forge.Forge, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("gh not logged in")
		}
		return af, nil
	}, labelled()))
	if _, err := svc.HistoryDelta(context.Background(), histFrom, histTo); err == nil || !strings.Contains(err.Error(), "gh not logged in") || !errors.Is(err, migrate.ErrUnresolved) {
		t.Fatalf("first delta: err = %v, want ErrUnresolved carrying the forge failure", err)
	}
	if _, err := svc.HistoryDelta(context.Background(), histFrom, histTo); err != nil {
		t.Fatalf("second delta after the forge recovered: %v (the failure was remembered for the session)", err)
	}
}

// histRepoFixture loads a minimal gitops view into a service whose git client answers HEAD with
// head — what LiveAge needs from the current view, without a real git repository.
type fixedHeadGit struct {
	git.Git
	sha *string
}

func (g fixedHeadGit) RevParse(context.Context, string, string) (string, bool, error) {
	return *g.sha, true, nil
}

func histViewService(t *testing.T, head *string, gitopsForge func(string) (forge.Forge, error)) *service.Service {
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
	deps := histDeps(gitopsForge, &registry.Fake{})
	deps.Git = func() git.Git { return fixedHeadGit{sha: head} }
	svc := service.New(service.Settings{RepoDir: root, AppsRoot: "cluster/apps", Base: "main", Repo: histRepoConfig("example/gitops")}, deps)
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}
	return svc
}

func blames(m map[string]map[int]forge.LineOrigin) func(string) (forge.Forge, error) {
	f := &forge.Fake{Blames: m}
	return func(ownerRepo string) (forge.Forge, error) {
		if ownerRepo != "example/gitops" {
			return nil, errors.New("asked for " + ownerRepo + "; want the gitops repo")
		}
		return f, nil
	}
}

// No forge for the gitops repo means no live age — the reason, not a panic — and the age comes
// back as soon as the forge does (CS-3: the old adaptor was handed the boot-time forge, or its
// error, and could never recover).
func TestLiveAgeNeedsTheGitopsForgeAndRecovers(t *testing.T) {
	head := "head"
	attempts := 0
	inner := blames(map[string]map[int]forge.LineOrigin{"head f.yaml": {3: {SHA: "1111"}}})
	svc := histViewService(t, &head, func(o string) (forge.Forge, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("gh not logged in")
		}
		return inner(o)
	})
	occ := gitops.Occurrence{File: "f.yaml", Line: 3}
	if _, err := svc.LiveAge(context.Background(), occ); err == nil || !strings.Contains(err.Error(), "gh not logged in") {
		t.Fatalf("err = %v, want the forge's reason", err)
	}
	age, err := svc.LiveAge(context.Background(), occ)
	if err != nil || age.SHA != "1111" {
		t.Fatalf("after the forge came back: age=%+v err=%v", age, err)
	}
}

// A repo with no github: at all has no gitops forge to blame, said as such.
func TestLiveAgeWithoutGitopsRepoSaysSo(t *testing.T) {
	svc := service.New(service.Settings{Repo: &config.RepoConfig{}}, histDeps(func(ownerRepo string) (forge.Forge, error) {
		return nil, errors.New("repo \"" + ownerRepo + "\" is not owner/name")
	}, &registry.Fake{}))
	if _, err := svc.LiveAge(context.Background(), gitops.Occurrence{File: "f.yaml", Line: 3}); err == nil || !strings.Contains(err.Error(), "live age needs the gitops repo's forge") {
		t.Fatalf("err = %v", err)
	}
}

// LiveAge reads the service's CURRENT view on every call: a HEAD that moved with a later
// LoadRepo (an F5, or the post-completion refresh) is blamed on the next call, not the boot-time
// one. It fails if HEAD were resolved once, or cached by root path alone: the second call would
// see "aaaa000", not the moved "bbbb111".
func TestLiveAgeUsesCurrentViewHEAD(t *testing.T) {
	head := "aaaa000"
	svc := histViewService(t, &head, blames(map[string]map[int]forge.LineOrigin{
		"aaaa000 f.yaml": {3: {SHA: "1111"}},
		"bbbb111 f.yaml": {3: {SHA: "2222"}},
	}))
	occ := gitops.Occurrence{File: "f.yaml", Line: 3}
	age, err := svc.LiveAge(context.Background(), occ)
	if err != nil || age.SHA != "1111" {
		t.Fatalf("before HEAD moves: age=%+v err=%v, want the sha at aaaa000's blame", age, err)
	}
	head = "bbbb111"
	if _, err := svc.LoadRepo(context.Background(), service.RepoFromClone); err != nil {
		t.Fatal(err)
	}
	age, err = svc.LiveAge(context.Background(), occ)
	if err != nil || age.SHA != "2222" {
		t.Fatalf("after HEAD moves: age=%+v err=%v, want the sha at bbbb111's blame", age, err)
	}
}
