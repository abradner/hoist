package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abradner/hoist/internal/config"
	"github.com/abradner/hoist/internal/service"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/registry"
)

func tagsRegistry() *registry.Fake {
	return &registry.Fake{
		TagLists: map[string][]string{"ghcr.io/example/app": {"v1", "v2"}},
		Configs:  map[string]registry.ImageMeta{"ghcr.io/example/app:v2": {Labels: map[string]string{"k": "v"}}},
	}
}

func TestTagSourceMappedReadsRegistryAndForge(t *testing.T) {
	af := &forge.Fake{GitTags: []forge.GitTag{{Name: "v2"}}}
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(ownerRepo string) (forge.Forge, error) {
		if ownerRepo != "example/app" {
			t.Fatalf("Forge(%q); want the app repo", ownerRepo)
		}
		return af, nil
	}, tagsRegistry()))
	src := svc.OpenTagSource("ghcr.io/example/app")
	if src.Err() != nil || !src.Mapped || !src.HasGitTags() {
		t.Fatalf("Err=%v Mapped=%v HasGitTags=%v; want a mapped source", src.Err(), src.Mapped, src.HasGitTags())
	}
	if got, err := src.RegistryTags(context.Background()); err != nil || strings.Join(got, ",") != "v1,v2" {
		t.Fatalf("RegistryTags = %v, %v", got, err)
	}
	if got, mapped, err := src.GitTags(context.Background()); err != nil || !mapped || len(got) != 1 {
		t.Fatalf("GitTags = %v, %v, %v", got, mapped, err)
	}
	if meta, err := src.Meta(context.Background(), "v2"); err != nil || meta.Labels["k"] != "v" {
		t.Fatalf("Meta = %+v, %v", meta, err)
	}
}

// A forge that cannot be built leaves the source unmapped (the picker orders by registry dates,
// principle 5), and the next picker open asks again, so a `gh` login fixed mid-session is used.
func TestTagSourceRetriesAForgeThatCouldNotBeBuilt(t *testing.T) {
	attempts := 0
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(string) (forge.Forge, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("gh not logged in")
		}
		return &forge.Fake{}, nil
	}, tagsRegistry()))
	if src := svc.OpenTagSource("ghcr.io/example/app"); src.Mapped || src.HasGitTags() || src.Err() != nil {
		t.Fatalf("first open: Mapped=%v HasGitTags=%v Err=%v; want unmapped and no registry error", src.Mapped, src.HasGitTags(), src.Err())
	}
	if src := svc.OpenTagSource("ghcr.io/example/app"); !src.Mapped || !src.HasGitTags() {
		t.Fatal("second open: the forge had recovered but the source is still unmapped")
	}
}

// A repo repos[].apps does not name is simply unmapped: no forge is asked for.
func TestTagSourceUnmappedRepoBuildsNoForge(t *testing.T) {
	svc := service.New(service.Settings{Repo: &config.RepoConfig{}}, histDeps(func(string) (forge.Forge, error) {
		t.Fatal("a forge was built for an unmapped image repo")
		return nil, nil
	}, tagsRegistry()))
	src := svc.OpenTagSource("ghcr.io/example/app")
	if src.Mapped || src.HasGitTags() {
		t.Fatal("an unmapped repo reported git tags")
	}
	if got, mapped, err := src.GitTags(context.Background()); got != nil || mapped || err != nil {
		t.Fatalf("GitTags on an unmapped source = %v, %v, %v; want nothing", got, mapped, err)
	}
}

// A registry that cannot be built is reported by every call that needs it, not hidden.
func TestTagSourceRegistryFailureIsReported(t *testing.T) {
	deps := histDeps(func(string) (forge.Forge, error) { return &forge.Fake{}, nil }, nil)
	deps.Registry = func(registry.AuthConfig) (registry.Registry, error) { return nil, errors.New("no credentials") }
	src := service.New(service.Settings{Repo: histRepoConfig("")}, deps).OpenTagSource("ghcr.io/example/app")
	if src.Err() == nil || src.Mapped {
		t.Fatalf("Err=%v Mapped=%v; want the registry error and unmapped", src.Err(), src.Mapped)
	}
	if _, err := src.RegistryTags(context.Background()); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("RegistryTags err = %v", err)
	}
	if _, err := src.Meta(context.Background(), "v1"); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("Meta err = %v", err)
	}
}

// CS-3: a forge failure at run time reaches the caller. The adaptor this replaced turned it into
// "unmapped, no error", which is not the same thing as an unmapped repo; the picker still degrades
// to registry order on either (it treats an error as unmapped and does not yet show it), but the
// reason now reaches it.
func TestTagSourceGitTagsReportsTheForgeError(t *testing.T) {
	svc := service.New(service.Settings{Repo: histRepoConfig("")}, histDeps(func(string) (forge.Forge, error) {
		return &forge.Fake{TagsErr: errors.New("rate limited")}, nil
	}, tagsRegistry()))
	got, mapped, err := svc.OpenTagSource("ghcr.io/example/app").GitTags(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rate limited") || mapped || got != nil {
		t.Fatalf("GitTags = %v, %v, %v; want the forge's error, unmapped, no tags", got, mapped, err)
	}
}
