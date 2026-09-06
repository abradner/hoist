package tags

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/app/history"
	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/uitest"
	"github.com/abradner/hoist/pkg/forge"
	"github.com/abradner/hoist/pkg/gitops"
	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/migrate"
	"github.com/abradner/hoist/pkg/registry"
)

func fixtureRepo() *gitops.Repo {
	return &gitops.Repo{Root: "repo", Envs: map[string]*gitops.Env{
		"app-production": {Name: "app-production", Families: map[string]*gitops.Family{
			"app": {Name: "app", Occurrences: []gitops.Occurrence{
				{File: "cluster/apps/app-production/app/deployment.yaml", Line: 21, Ref: image.Ref{Repo: "ghcr.io/example/app", Tag: "v1", Digest: "sha256:" + strings.Repeat("1", 64)}},
				{File: "cluster/apps/app-production/app/deployment.yaml", Line: 95, Ref: image.Ref{Repo: "ghcr.io/example/app", Tag: "v1", Digest: "sha256:" + strings.Repeat("1", 64)}},
			}},
			"zed": {Name: "zed", Occurrences: []gitops.Occurrence{
				{File: "cluster/apps/app-production/zed/a.yaml", Line: 1, Ref: image.Ref{Repo: "ghcr.io/example/app", Tag: "v0"}},
			}},
		}},
	}}
}

func TestDeclaredInIsStableAndFirstByFamilyFileLine(t *testing.T) {
	d, ok := DeclaredIn(fixtureRepo(), "ghcr.io/example/app", "app-production")
	if !ok || d.Ref.Tag != "v1" || d.Occurrence.Line != 21 {
		t.Fatalf("declared = %+v ok=%v", d, ok)
	}
	if _, ok := DeclaredIn(fixtureRepo(), "ghcr.io/example/other", "app-production"); ok {
		t.Fatal("an image the env does not declare must not be found")
	}
	if _, ok := DeclaredIn(nil, "x", "y"); ok {
		t.Fatal("nil repo")
	}
}

// A picker with history wired: the declared line, the provenance column, the commit pane
// under the cursor tag, tab into it, enter to read a commit, space to review with the delta
// carried on the message.
func historyModel(t *testing.T, delta history.DeltaFunc, age history.LiveAgeFunc) Model {
	t.Helper()
	regTags := []string{"v3", "v2", "v1"}
	gitTags := []forge.GitTag{
		{Name: "v1", Date: fixedNow.Add(-62 * 24 * time.Hour)},
		{Name: "v2", Date: fixedNow.Add(-32 * 24 * time.Hour)},
		{Name: "v3", Date: fixedNow.Add(-3 * 24 * time.Hour)},
	}
	metas := map[string]registry.ImageMeta{
		"v1": {Digest: "sha256:" + strings.Repeat("1", 64)},
		"v2": {Digest: "sha256:" + strings.Repeat("2", 64)},
		"v3": {Digest: "sha256:" + strings.Repeat("3", 64)},
	}
	d, _ := DeclaredIn(fixtureRepo(), "ghcr.io/example/app", "app-production")
	m := New("ghcr.io/example/app", "app-production", Options{
		Mapped: true, Production: true,
		StagingEnv: "app-staging", StagingTags: []string{"v3"}, HasStagingMismatch: true,
		List: func(context.Context) ([]string, []forge.GitTag, bool, error) { return regTags, gitTags, true, nil },
		Meta: fixedMetas(metas),
		History: history.Funcs{
			Mapped:  func(string) bool { return true },
			Delta:   delta,
			LiveAge: age,
		},
		Declared: &d,
		Now:      func() time.Time { return fixedNow },
	})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v (err=%v)", m.state, m.err)
	}
	return m
}

func fourteenAhead(_ context.Context, from, to image.Ref) (migrate.Delta, error) {
	commits := []migrate.Commit{
		{SHA: "4a1c2ef0000", Subject: "Add rate limiting to the public API", Date: fixedNow.Add(-2 * 24 * time.Hour), Author: "dev"},
		{SHA: "e9b0d310000", Subject: "Fix N+1 query when resolving digests", Date: fixedNow.Add(-4 * 24 * time.Hour), Author: "dev"},
		{SHA: "77c0ffe0000", Subject: "db: add index on events.created_at", Body: "The purge cronjob scans events by created_at every hour.\n\nExpect around 4 minutes on production-sized data.", Date: fixedNow.Add(-11 * 24 * time.Hour), Author: "dev", Migrations: []string{"db/migrate/20260225T101500_add_events_created_at_index.rb"}},
	}
	for i := 0; i < 11; i++ {
		commits = append(commits, migrate.Commit{SHA: strings.Repeat(string(rune('a'+i)), 10), Subject: "older change", Author: "dev", Date: fixedNow.Add(-20 * 24 * time.Hour)})
	}
	if to.Tag == "v1" {
		return migrate.Delta{From: migrate.Revision{Ref: from, SHA: "x", Source: migrate.SourceGitTag}, To: migrate.Revision{Ref: to, SHA: "x", Source: migrate.SourceGitTag}, Direction: migrate.DirectionSame, Prefix: "db/migrate/"}, nil
	}
	return migrate.Delta{
		From: migrate.Revision{Ref: from, SHA: "1111111", Source: migrate.SourceGitTag}, To: migrate.Revision{Ref: to, SHA: "3333333", Source: migrate.SourceLabel},
		Direction: migrate.DirectionForward, Commits: commits, Total: len(commits),
		Migrations: []string{"db/migrate/20260225T101500_add_events_created_at_index.rb"}, MigrationCommits: 1,
		Prefix: "db/migrate/", PrefixSource: migrate.PrefixFromDefault,
	}, nil
}

func liveAge34Days(_ context.Context, occ gitops.Occurrence) (migrate.LineAge, error) {
	if occ.Line != 21 {
		return migrate.LineAge{}, errors.New("blamed the wrong line")
	}
	return migrate.LineAge{SHA: "abc", Since: fixedNow.Add(-34 * 24 * time.Hour)}, nil
}

func TestHistoryPaneShowsTheDeltaUnderTheCursor(t *testing.T) {
	m := historyModel(t, fourteenAhead, liveAge34Days)
	v := ansi.Strip(m.View())
	for _, want := range []string{
		"app-production declares  v1 · 111111111111 · since 4 weeks ago",
		"▸ v3", "3 days ago", "in app-staging", "◂ declared here",
		"v3 is 14 commits ahead of v1 · 1 migration",
		"4a1c2ef  Add rate limiting to the public API",
		"77c0ffe  db: add index on events.created_at", "migration",
		"…", "more",
		"tab commits · enter read commit · space review the change",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "D direct") {
		t.Errorf("production must not offer D:\n%s", v)
	}
	uitest.Golden(t, "tags-history", m.View(), 100, 30)

	// Moving the cursor asks for that tag's delta; v1 is what the env declares.
	m = uitest.Keys(m, updateFn, "down", "down")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "v1 is what app-production declares") {
		t.Fatalf("same-revision wording missing:\n%s", v)
	}
	m = uitest.Keys(m, updateFn, "up", "up")

	// tab into the commits, down to the migration commit, enter to read it.
	m = uitest.Keys(m, updateFn, "tab", "down", "down")
	if m.focus != focusCommits || m.commitIdx != 2 {
		t.Fatalf("focus=%v idx=%d", m.focus, m.commitIdx)
	}
	m = uitest.Keys(m, updateFn, "enter")
	if !m.reading {
		t.Fatal("enter on a commit must open the detail view")
	}
	v = ansi.Strip(m.View())
	for _, want := range []string{"hoist · deploy · commit", "77c0ffe   db: add index on events.created_at", "3 of 14 in v3 · not in v1", "Expect around 4 minutes on production-sized data.", "migrations in this commit:", "20260225T101500_add_events_created_at_index.rb"} {
		if !strings.Contains(v, want) {
			t.Errorf("detail lacks %q:\n%s", want, v)
		}
	}
	uitest.Golden(t, "tags-commit", m.View(), 100, 30)
	m = uitest.Keys(m, updateFn, "esc")
	if m.reading {
		t.Fatal("esc must return to the list, not leave the picker")
	}

	// space reviews the change, carrying the loaded delta and the declared reference.
	_, cmd := m.Update(uitest.Key("space"))
	if cmd == nil {
		t.Fatal("space emitted nothing")
	}
	msg, ok := cmd().(SelectedMsg)
	if !ok || msg.Tag != "v3" || msg.Delta == nil || len(msg.Delta.Commits) != 14 || msg.Declared == nil || msg.Declared.Ref.Tag != "v1" {
		t.Fatalf("SelectedMsg = %+v", msg)
	}
}

// Every way history can be missing is a sentence naming why, never a blank pane.
func TestHistoryPaneNamesEveryGap(t *testing.T) {
	m := historyModel(t, func(_ context.Context, _, to image.Ref) (migrate.Delta, error) {
		return migrate.Delta{}, fmt.Errorf("%w: %s has no revision label and example/app has no tag named %s", migrate.ErrUnresolved, to.Tag, to.Tag)
	}, nil)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "no commit history — v3 has no revision label and example/app has no tag named v3") {
		t.Errorf("unresolved gap:\n%s", v)
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "since") {
		t.Errorf("no LiveAge wired: no age claim:\n%s", v)
	}
	m = historyModel(t, func(context.Context, image.Ref, image.Ref) (migrate.Delta, error) {
		return migrate.Delta{}, errors.New("HTTP 403 rate limit")
	}, nil)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "commit history unavailable — HTTP 403 rate limit") {
		t.Errorf("forge error:\n%s", v)
	}
	// enter with nothing to read says so instead of doing nothing.
	m, _ = m.Update(uitest.Key("enter"))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "no commit to read here — space reviews the change") {
		t.Errorf("enter notice:\n%s", v)
	}
	// No declared reference at all (a first deploy).
	n := readyModel(t, "app-staging", true, false)
	if v := ansi.Strip(n.View()); !strings.Contains(v, "no commit history — app-staging does not declare ghcr.io/example/app yet") {
		t.Errorf("undeclared gap:\n%s", v)
	}
}

// A stale history answer (an earlier picker instance) never lands.
func TestStaleHistoryResultIsDiscarded(t *testing.T) {
	m := historyModel(t, fourteenAhead, nil)
	stale := historyMsg{gen: m.generation - 1, tag: "v3", delta: migrate.Delta{Direction: migrate.DirectionSame}}
	m2, _ := m.Update(stale)
	if len(m2.currentCommits()) != 14 {
		t.Fatal("a stale generation's delta replaced the current one")
	}
}

func updateFn(m Model, msg tea.Msg) (Model, tea.Cmd) { return m.Update(msg) }
