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

// unsplitRepo is fixtureRepo with every occurrence at the same reference: the common case,
// and the positive control for the split tests below.
func unsplitRepo() *gitops.Repo {
	r := fixtureRepo()
	occ := &r.Envs["app-production"].Families["zed"].Occurrences[0]
	occ.Ref = r.Envs["app-production"].Families["app"].Occurrences[0].Ref
	return r
}

func TestDeclaredInIsStableAndFirstByFamilyFileLine(t *testing.T) {
	d, ok := DeclaredIn(fixtureRepo(), "ghcr.io/example/app", "app-production")
	if !ok || d.Ref.Tag != "v1" || d.Occurrence.Line != 21 {
		t.Fatalf("declared = %+v ok=%v", d, ok)
	}
	// fixtureRepo's app-production carries the repo at v1 (twice, app/) and v0 (zed/): a split
	// env declares two builds, and Refs names both, distinct, in file-then-line order (#119).
	if len(d.Refs) != 2 || d.Refs[0].Tag != "v1" || d.Refs[1].Tag != "v0" || !d.Split() {
		t.Fatalf("a split env must expose every distinct declared ref, got %+v", d.Refs)
	}
	u, _ := DeclaredIn(unsplitRepo(), "ghcr.io/example/app", "app-production")
	if len(u.Refs) != 1 || u.Split() {
		t.Fatalf("an env agreeing with itself is not split, got %+v", u.Refs)
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
	return historyModelOver(t, unsplitRepo(), delta, age)
}

// historyModelOver is historyModel with the gitops repo the declared reference is read from.
func historyModelOver(t *testing.T, repo *gitops.Repo, delta history.DeltaFunc, age history.LiveAgeFunc) Model {
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
	d, _ := DeclaredIn(repo, "ghcr.io/example/app", "app-production")
	regFn, gitFn := splitListFn(true, func(context.Context) ([]string, []forge.GitTag, bool, error) { return regTags, gitTags, true, nil })
	m := New("ghcr.io/example/app", "app-production", Options{
		Mapped: true, Production: true,
		StagingEnv: "app-staging", StagingTags: []string{"v3"}, HasStagingMismatch: true,
		RegTags: regFn, GitTags: gitFn,
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

// TestGitTagsReorderRefreshesTheNewSelectionsHistoryAndMetadata is the regression test for a
// P1 an adversarial review of #PR8 found: onGitTagsLoaded moves the default selection to the
// newly-confirmed top row once BackfillGitDates lands, but never re-fired historyCmd or
// fetchVisible for it — the commit delta stayed keyed to whichever tag was selected before the
// reorder (the registry's own first-returned tag here), and in the common timing where every
// visible-window meta fetch already finishes before gitTagsCmd's own answer lands
// (onGitTagsLoaded's own doc comment), the reordered top row's registry metadata (digest,
// created) was left unloaded too. Registry order (v1, v2, v3) deliberately differs from
// git-tag date order (v3 newest) so the reorder actually moves row 0 — every other fixture in
// this package has the two orders agree, which would let this regression hide (§9 entry 9).
func TestGitTagsReorderRefreshesTheNewSelectionsHistoryAndMetadata(t *testing.T) {
	regTags := []string{"v1", "v2", "v3"}
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
	repo := unsplitRepo()
	d, ok := DeclaredIn(repo, "ghcr.io/example/app", "app-production")
	if !ok {
		t.Fatal("fixture precondition: DeclaredIn must find the declared reference")
	}
	regFn, gitFn := splitListFn(true, func(context.Context) ([]string, []forge.GitTag, bool, error) {
		return regTags, gitTags, true, nil
	})
	m := New("ghcr.io/example/app", "app-production", Options{
		Mapped: true, Production: true,
		RegTags: regFn, GitTags: gitFn,
		Meta: fixedMetas(metas),
		History: history.Funcs{
			Mapped:  func(string) bool { return true },
			Delta:   fourteenAhead,
			LiveAge: liveAge34Days,
		},
		Declared: &d,
		Now:      func() time.Time { return fixedNow },
	})
	m = m.SetSize(100, 30).SetStyles(ui.NewStyles(true))
	m = drain(m, m.Init())
	if m.state != stateReady {
		t.Fatalf("state = %v (err=%v)", m.state, m.err)
	}

	if m.selectedTag != "v3" {
		t.Fatalf("selectedTag = %q, want v3 (newest by git-tag date) once the reorder lands", m.selectedTag)
	}
	if m.currentDelta() == nil {
		t.Fatalf("no commit delta loaded for the reordered selection %q — historyCmd was never re-fired for it", m.selectedTag)
	}
	i := IndexOf(m.rows, "v3")
	if i < 0 || !m.rows[i].MetaLoaded {
		t.Fatalf("v3's own registry metadata never loaded after the reorder moved it to row 0: row=%+v", m.rows[i])
	}
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

// A split target env (one image repo at two references — fixtureRepo's v1 in app/ and v0 in
// zed/) never reads as declaring one build (#119): the header names both, says it is split
// and which reference the commit count is measured from, and both rows carry the declared
// marker. The commit pane keeps counting from the first-by-file reference (v1).
func TestSplitTargetEnvIsNamedInHeader(t *testing.T) {
	m := historyModelOver(t, fixtureRepo(), fourteenAhead, liveAge34Days)
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		m = m.SetSize(size[0], size[1])
		v := prose(m.View())
		for _, want := range []string{
			"app-production declares v1 · 111111111111 and v0 — split; commits are counted from v1 · since 4 weeks ago",
			"v3 is 14 commits ahead of v1",
		} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d view lacks %q:\n%s", size[0], size[1], want, v)
			}
		}
		if strings.Contains(v, "declares v1 · 111111111111 · since") {
			t.Errorf("%dx%d header claims a single declared build for a split env:\n%s", size[0], size[1], v)
		}
		uitest.Golden(t, "tags-split", m.View(), size[0], size[1])
	}
	// Positive control: the unsplit env's header carries neither the word nor the rule.
	if v := prose(historyModel(t, fourteenAhead, liveAge34Days).View()); strings.Contains(v, "split") {
		t.Errorf("an env agreeing with itself must not be called split:\n%s", v)
	}
}

// longBody is fourteenAhead with the first commit's body forty numbered lines and a
// migration list at the end — longer than a 24-row terminal, so only a scrolling body can
// reach the migration files (#120).
func longBody(ctx context.Context, from, to image.Ref) (migrate.Delta, error) {
	d, err := fourteenAhead(ctx, from, to)
	if len(d.Commits) > 0 {
		lines := make([]string, 40)
		for i := range lines {
			lines[i] = fmt.Sprintf("body line %02d of the rate-limiting design note", i+1)
		}
		d.Commits[0].Body = strings.Join(lines, "\n")
		d.Commits[0].Migrations = []string{"db/migrate/20260301T090000_add_rate_limits.rb"}
	}
	return d, err
}

// The commit-detail view scrolls a body longer than the terminal: PageDown shows lines the
// first page clipped, and the migration list at the end is reachable with G; ↑/↓ still
// switch commits rather than scroll, and a switched-to commit is read from its top (#120).
func TestReadingScrollsALongBody(t *testing.T) {
	m := historyModelOver(t, unsplitRepo(), longBody, liveAge34Days).SetSize(80, 24)
	m = uitest.Keys(m, updateFn, "tab", "enter")
	if !m.reading {
		t.Fatal("enter must open the commit")
	}
	first := ansi.Strip(m.View())
	if !strings.Contains(first, "body line 01") || strings.Contains(first, "body line 30") || strings.Contains(first, "add_rate_limits") {
		t.Fatalf("the first page must show the start of the body and not its end:\n%s", first)
	}
	if !strings.Contains(first, "body 0% (pgdn scrolls)") {
		t.Fatalf("a clipped body must say so in the head:\n%s", first)
	}
	uitest.Golden(t, "tags-commit-long", m.View(), 80, 24)

	m = uitest.Keys(m, updateFn, "pgdown")
	paged := ansi.Strip(m.View())
	if paged == first || strings.Contains(paged, "body line 01") || !strings.Contains(paged, "body line 30") {
		t.Fatalf("pgdown must show a different page of the body:\n%s", paged)
	}
	uitest.Golden(t, "tags-commit-long-paged", m.View(), 80, 24)

	m = uitest.Keys(m, updateFn, "G")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "add_rate_limits") || !strings.Contains(v, "body 100%") {
		t.Fatalf("G must reach the migration list at the end of the body:\n%s", v)
	}
	m = uitest.Keys(m, updateFn, "g")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "body line 01") {
		t.Fatalf("g must return to the top:\n%s", v)
	}
	m = uitest.Keys(m, updateFn, "ctrl+d")
	if v := ansi.Strip(m.View()); strings.Contains(v, "body line 01") {
		t.Fatalf("ctrl+d must scroll half a page:\n%s", v)
	}

	// ↓ is still the next commit, not a scroll, and that commit is read from its top.
	m = uitest.Keys(m, updateFn, "G", "down")
	if m.commitIdx != 1 || !m.reading {
		t.Fatalf("down must switch commits in the detail view: idx=%d reading=%v", m.commitIdx, m.reading)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "2 of 14") || !strings.Contains(v, "(no body)") || m.body.YOffset() != 0 {
		t.Fatalf("the next commit must be shown from its top: offset=%d\n%s", m.body.YOffset(), v)
	}
	m = uitest.Keys(m, updateFn, "up")
	if m.commitIdx != 0 || m.body.YOffset() != 0 {
		t.Fatalf("up must switch back, from the top: idx=%d offset=%d", m.commitIdx, m.body.YOffset())
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

// The confirm screen leads with the history it is handed and never refetches it; leaving the
// picker while the cursor tag's delta is still loading would cancel that load and show the
// no-history form for a mapped image. space waits until the answer is in.
func TestSpaceWaitsForTheCursorTagsHistory(t *testing.T) {
	m := historyModel(t, fourteenAhead, liveAge34Days)
	m.deltas["v3"] = history.State{} // asked, not answered — a fetch in flight
	m, cmd := m.Update(uitest.Key("space"))
	if cmd != nil {
		t.Fatalf("space emitted %T while the history was pending", cmd())
	}
	if !strings.Contains(m.notice, "still reading v3's commits") {
		t.Fatalf("notice = %q", m.notice)
	}
	// Positive control: once answered, the same key reviews the change.
	d, _ := fourteenAhead(context.Background(), image.Ref{}, image.Ref{Tag: "v3"})
	m.deltas["v3"] = history.State{Loaded: true, Delta: d}
	if _, cmd = m.Update(uitest.Key("space")); cmd == nil {
		t.Fatal("space emitted nothing once the history had loaded")
	} else if msg, ok := cmd().(SelectedMsg); !ok || msg.Delta == nil {
		t.Fatalf("got %+v", cmd())
	}
}

// Filtering to a different tag starts its commit cursor over: a cursor carried from the
// previous tag could point past the new tag's commits, and enter would open nothing.
func TestFilterResetsTheCommitCursor(t *testing.T) {
	m := historyModel(t, fourteenAhead, liveAge34Days)
	m = uitest.Keys(m, updateFn, "tab", "down", "down")
	if m.commitIdx != 2 {
		t.Fatalf("idx = %d", m.commitIdx)
	}
	m = uitest.Keys(m, updateFn, "esc", "/", "v", "2")
	if m.selectedTag != "v2" || m.commitIdx != 0 {
		t.Fatalf("tag=%q idx=%d; want v2 with the cursor reset", m.selectedTag, m.commitIdx)
	}
}

// The commit pane follows the cursor into the second page rather than rendering 0..k
// whatever the cursor says.
func TestCommitPaneFollowsTheCursor(t *testing.T) {
	m := historyModel(t, fourteenAhead, liveAge34Days)
	m = uitest.Keys(m, updateFn, "tab", "down", "down", "down", "down", "down", "down", "down", "down")
	v := ansi.Strip(m.View())
	if m.commitIdx != 8 || !strings.Contains(v, "earlier") {
		t.Fatalf("idx=%d, pane lacks the earlier trailer:\n%s", m.commitIdx, v)
	}
	cursorSHA := history.ShortSHA(m.currentCommits()[8].SHA)
	if !strings.Contains(v, "▸ "+cursorSHA) && !strings.Contains(v, cursorSHA) {
		t.Fatalf("pane does not show the commit under the cursor (%s):\n%s", cursorSHA, v)
	}
}

// A rollback lists the commits being removed: they are in what the env declares and not in
// the tag under the cursor, so the detail line reads the other way round.
func TestRollbackDetailReadsTheOtherWay(t *testing.T) {
	rollback := func(ctx context.Context, from, to image.Ref) (migrate.Delta, error) {
		d, err := fourteenAhead(ctx, from, to)
		if to.Tag == "v3" {
			d.Direction = migrate.DirectionRollback
		}
		return d, err
	}
	m := historyModel(t, rollback, liveAge34Days)
	m = uitest.Keys(m, updateFn, "tab", "enter")
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "1 of 14 in v1 · not in v3") {
		t.Fatalf("rollback detail must say the commit is in v1 and not in v3:\n%s", v)
	}
}
