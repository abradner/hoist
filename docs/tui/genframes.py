# -*- coding: utf-8 -*-
import html

def esc(s): return html.escape(s, quote=False)

class Box:
    """Fixed-width box-drawing frame. Pad first, colourise second, so spans never
    disturb alignment."""
    def __init__(self, cols):
        self.cols = cols            # list of content widths
        self.lines = []
    def _rule(self, l, m, r):
        return l + m.join("─" * (c + 2) for c in self.cols) + r
    def width(self):
        return len(self._rule("╭", "┬", "╮"))
    def top(self, title=None):
        if title is None:
            self.lines.append(self._rule("╭", "┬", "╮")); return
        t = " " + title + " "
        self.lines.append("╭─" + t + "─" * (self.width() - 3 - len(t)) + "╮")
    def sep(self):     self.lines.append(self._rule("├", "┼", "┤"))
    def open_cols(self): self.lines.append(self._rule("├", "┬", "┤"))
    def close_cols(self): self.lines.append(self._rule("├", "┴", "┤"))
    def bot(self):     self.lines.append(self._rule("╰", "┴", "╯"))
    def bot_span(self):
        self.lines.append("╰" + "─" * (self.width() - 2) + "╯")
    def span_rule(self, l="├", r="┤"):
        self.lines.append(l + "─" * (self.width() - 2) + r)
    def row(self, *cells):
        out = "│"
        for w, c in zip(self.cols, cells):
            out += " " + c.ljust(w)[:w] + " │"
        self.lines.append(out)
    def span(self, text):
        inner = self.width() - 4
        self.lines.append("│ " + text.ljust(inner)[:inner] + " │")
    def render(self): return "\n".join(self.lines)

def colour(s, pairs):
    """pairs: (literal, class) or (literal, class, count). count=-1 colours every
    occurrence; the default of 1 keeps left-to-right ordering for repeated literals."""
    s = esc(s)
    for p in pairs:
        lit, cls = p[0], p[1]
        n = p[2] if len(p) > 2 else 1
        e = esc(lit)
        s = s.replace(e, f'<span class="{cls}">{e}</span>', n if n >= 0 else -1)
    return s

# ─────────────────────────── 1. MATRIX ───────────────────────────
b = Box([11, 24, 26])
b.top("hoist · matrix")
b.span("my-gitops · cluster/apps")
b.open_cols()
b.row("FAMILY", "APP-PRODUCTION", "APP-STAGING")
b.sep()
b.row("▸ orders", "v202601151010   pinned", "v202602201200      pinned")
b.row("  marketing", "sha-0000111    drifted", "sha-1a2b3c4        pinned")
b.row("  temporal", "2 images      external", "2 images         external")
b.row("  web", "v202601010101   pinned", "2 versions          split")
b.close_cols()
b.span("! marketing runs sha-77c0ffe here; manifest says sha-0000111")
b.span_rule()
b.span("⟳ 5pr6sd333t → app-production   waiting on approval   12m")
b.bot_span()
matrix = colour(b.render(), [
    ("▸ orders", "sel"),
    ("drifted", "c-warn"), ("split", "c-warn"),
    ("external", "c-dim", -1),
    ("! marketing runs sha-77c0ffe here; manifest says sha-0000111", "c-warn"),
    ("⟳ 5pr6sd333t → app-production", "c-accent"),
    ("waiting on approval", "c-warn"),
    ("12m", "c-dim"),
])
matrix += '\n<span class="c-dim">  p promote pair · d deploy image · r resume · enter details · ? help · q quit</span>'

# ─────────────────────────── 2. TAG PICKER ───────────────────────────
b = Box([16, 15, 14, 18])
b.top("hoist · deploy")
b.span("ghcr.io/example/app  →  app-production")
b.span_rule()
b.span("now running   v1 · 1111111111 · deployed 34 days ago")
b.open_cols()
b.row("TAG", "BUILT", "DIGEST", "")
b.sep()
b.row("▸ v3", "3 days ago", "3333333333", "in app-staging")
b.row("  v2", "32 days ago", "2222222222", "")
b.row("  v1", "62 days ago", "1111111111", "◂ live here")
b.close_cols()
b.span("✓ v3 has been running in app-staging for 3 days")
b.bot_span()
tags = colour(b.render(), [
    ("now running", "c-dim"),
    ("▸ v3", "sel"),
    ("in app-staging", "c-good"),
    ("◂ live here", "c-accent"),
    ("✓ v3 has been running in app-staging for 3 days", "c-good"),
])
tags += '\n<span class="c-dim">  ↑/↓ move · / filter · enter review the change · D direct commit · esc back</span>'

# ─────────────────────────── 3. DEPLOY CONFIRM ───────────────────────────
b = Box([76])
b.top("hoist · confirm deploy")
b.span("ghcr.io/example/app:v3   →   app-production                      mode: PR")
b.span_rule()
b.span("3 occurrences · 1 file · from v1 · digest 3333333333")
b.sep()
b.span("--- a/cluster/apps/app-production/app/deployment.yaml")
b.span("+++ b/cluster/apps/app-production/app/deployment.yaml")
b.span("@@ -21,7 +21,7 @@")
b.span("           containers:")
b.span("             - name: app")
b.span("-              image: ghcr.io/example/app:v1@sha256:1111111111…")
b.span("+              image: ghcr.io/example/app:v3@sha256:3333333333…")
b.span("               imagePullPolicy: IfNotPresent")
b.span("@@ -95,7 +95,7 @@")
b.span("             - name: worker")
b.span("-              image: ghcr.io/example/app:v1@sha256:1111111111…")
b.span("+              image: ghcr.io/example/app:v3@sha256:3333333333…")
b.span_rule()
b.span("✓ v3 has been running in app-staging for 3 days")
b.bot()
confirm = colour(b.render(), [
    ("ghcr.io/example/app:v3   →   app-production", "c-bold"),
    ("mode: PR", "c-accent"),
    ("--- a/cluster/apps/app-production/app/deployment.yaml", "c-dim"),
    ("+++ b/cluster/apps/app-production/app/deployment.yaml", "c-dim"),
    ("@@ -21,7 +21,7 @@", "c-dim"),
    ("-              image: ghcr.io/example/app:v1@sha256:1111111111…", "del"),
    ("+              image: ghcr.io/example/app:v3@sha256:3333333333…", "add"),
    ("@@ -95,7 +95,7 @@", "c-dim"),
    ("-              image: ghcr.io/example/app:v1@sha256:1111111111…", "del"),
    ("+              image: ghcr.io/example/app:v3@sha256:3333333333…", "add"),
    ("✓ v3 has been running in app-staging for 3 days", "c-good"),
])
confirm += '\n<span class="c-dim">  enter deploy · m mode · d full digests · esc back</span>'

# ─────────────────────────── 4. PLAN CONFIRM ───────────────────────────
b = Box([36, 39])
b.top("hoist · confirm promotion")
b.span("app-staging  →  app-production                            mode: PR")
b.span_rule()
b.span("3 repos · 6 occurrences · 3 files")
b.open_cols()
b.row("REPO                    CHANGE", "DIFF")
b.sep()
b.row("▸ ✓ orders", "--- orders/app.yaml")
b.row("      v2026011510 → v2026022012", "@@ -19,7 +19,7 @@")
b.row("      2 files", "       containers:")
b.row("", "         - name: orders")
b.row("  ✓ marketing", "-        image: …:v2026011510@…")
b.row("      sha-0000111 → sha-1a2b3c4", "+        image: …:v2026022012@…")
b.row("      1 file", "")
b.row("", "--- orders/purge-cronjob.yaml")
b.row("  ✓ web  !", "@@ -13,5 +13,5 @@")
b.row("      v2026010101 → v2026021509", "         - name: purge")
b.row("      3 files", "-        image: …:v2026011510@…")
b.sep()
b.row("! web runs 2 versions in staging", "+        image: …:v2026022012@…")
b.row("  v202601010101, v202602150930", "")
b.bot()
plan = colour(b.render(), [
    ("app-staging  →  app-production", "c-bold"),
    ("mode: PR", "c-accent"),
    ("▸ ✓ orders", "sel"),
    ("--- orders/app.yaml", "c-dim"),
    ("@@ -19,7 +19,7 @@", "c-dim"),
    ("-        image: …:v2026011510@…", "del"),
    ("+        image: …:v2026022012@…", "add"),
    ("--- orders/purge-cronjob.yaml", "c-dim"),
    ("@@ -13,5 +13,5 @@", "c-dim"),
    ("-        image: …:v2026011510@…", "del"),
    ("+        image: …:v2026022012@…", "add"),
    ("! web runs 2 versions in staging", "c-warn"),
    ("  v202601010101, v202602150930", "c-dim"),
])
plan += '\n<span class="c-dim">  tab pane · x toggle · m mode · d full digests · enter confirm · esc back</span>'

open("frames.txt", "w", encoding="utf-8").write(
    "===MATRIX===\n" + matrix +
    "\n===TAGS===\n" + tags +
    "\n===CONFIRM===\n" + confirm +
    "\n===PLAN===\n" + plan + "\n")
print("generated")

# ─────────────────────────── 5. IN FLIGHT (expanded) ───────────────────────────
b = Box([72])
b.top("in flight")
b.span("5pr6sd333t    app-staging → app-production           started 12m ago")
b.span("● branch  ● commit  ● push  ● PR #103  ● CI 4/4  ◍ approval  ○ merge")
b.span_rule()
b.span("blocked on you — comment on PR #103 to release it:")
b.span("")
b.span("    hoist approve 5pr6sd333t")
b.span("")
b.span("approvers  me          waiting 11m          deadline in 3h 48m")
b.span_rule()
b.span("○ argo refresh   ○ argo sync   ○ rollout          2 deployments")
b.bot_span()
inflight = colour(b.render(), [
    ("5pr6sd333t", "c-bold"),
    ("app-staging → app-production", "c-bold"),
    ("started 12m ago", "c-dim"),
    ("● branch  ● commit  ● push  ● PR #103  ● CI 4/4", "c-good"),
    ("◍ approval", "c-warn"),
    ("○ merge", "c-dim"),
    ("blocked on you — comment on PR #103 to release it:", "c-warn"),
    ("    hoist approve 5pr6sd333t", "c-accent"),
    ("approvers  me          waiting 11m          deadline in 3h 48m", "c-dim"),
    ("○ argo refresh   ○ argo sync   ○ rollout          2 deployments", "c-dim"),
])
inflight += '\n<span class="c-dim">  enter open flight screen · o open PR · r resume · esc back</span>'

# ─────────────────────────── 6. IN FLIGHT (compact) ───────────────────────────
b = Box([44])
b.top("in flight")
b.span("⟳ 5pr6sd333t → app-production")
b.span("  blocked on approval · 12m")
b.bot_span()
compact = colour(b.render(), [
    ("⟳ 5pr6sd333t → app-production", "c-accent"),
    ("blocked on approval", "c-warn"),
    ("· 12m", "c-dim"),
])
compact += '\n<span class="c-dim">  enter details</span>'

open("frames.txt", "a", encoding="utf-8").write(
    "===INFLIGHT===\n" + inflight + "\n===COMPACT===\n" + compact + "\n")
print("appended")

# ─────────────────── 7. TAG PICKER WITH COMMIT PANE ───────────────────
b = Box([14, 15, 14, 20])
b.top("hoist · deploy")
b.span("ghcr.io/example/app  →  app-production")
b.span_rule()
b.span("now running   v1 · 1111111111 · deployed 34 days ago")
b.open_cols()
b.row("TAG", "BUILT", "DIGEST", "")
b.sep()
b.row("▸ v3", "3 days ago", "3333333333", "in app-staging")
b.row("  v2", "32 days ago", "2222222222", "")
b.row("  v1", "62 days ago", "1111111111", "◂ live here")
b.close_cols()
b.span("v3 is 14 commits ahead of v1              2 migrations")
b.span("")
b.span("  4a1c2ef  Add rate limiting to the public API")
b.span("  e9b0d31  Fix N+1 query when resolving digests")
b.span("▸ 77c0ffe  db: add index on events.created_at            migration")
b.span("  1b2d3e4  Bump temporal SDK to 1.31")
b.span("  …10 more")
b.bot_span()
picker2 = colour(b.render(), [
    ("▸ v3", "sel"),
    ("in app-staging", "c-good"),
    ("◂ live here", "c-accent"),
    ("v3 is 14 commits ahead of v1", "c-bold"),
    ("2 migrations", "c-warn"),
    ("▸ 77c0ffe  db: add index on events.created_at            migration", "sel"),
    ("  …10 more", "c-dim"),
])
picker2 += '\n<span class="c-dim">  ↑/↓ tag · tab commits · enter read commit · space review the change · esc back</span>'

# ─────────────────── 8. COMMIT DETAIL ───────────────────
b = Box([74])
b.top("hoist · deploy · commit")
b.span("77c0ffe   db: add index on events.created_at")
b.span("in v3 · not in v1 · 11 days ago")
b.span_rule()
b.span("db: add index on events.created_at")
b.span("")
b.span("The purge cronjob scans events by created_at every hour and was doing a")
b.span("sequential scan over roughly 40M rows. Adds a btree index, created")
b.span("concurrently so the migration does not lock writes.")
b.span("")
b.span("Expect around 4 minutes on production-sized data.")
b.span_rule()
b.span("db/migrate/20260225T101500_add_events_created_at_index.rb")
b.bot_span()
commit = colour(b.render(), [
    ("77c0ffe   db: add index on events.created_at", "c-bold"),
    ("in v3 · not in v1 · 11 days ago", "c-dim"),
    ("Expect around 4 minutes on production-sized data.", "c-warn"),
    ("db/migrate/20260225T101500_add_events_created_at_index.rb", "c-warn"),
])
commit += '\n<span class="c-dim">  ↑/↓ next commit · esc back to the list</span>'

# ─────────────────── 9. DEGRADED (no app repo mapped) ───────────────────
b = Box([14, 15, 14, 20])
b.top("hoist · deploy")
b.span("ghcr.io/example/app  →  app-production")
b.open_cols()
b.row("TAG", "BUILT", "DIGEST", "")
b.sep()
b.row("▸ v3", "3 days ago", "3333333333", "in app-staging")
b.row("  v1", "62 days ago", "1111111111", "◂ live here")
b.close_cols()
b.span("no commit history — ghcr.io/example/app has no app repo in repos[].apps")
b.bot_span()
degraded = colour(b.render(), [
    ("▸ v3", "sel"),
    ("in app-staging", "c-good"),
    ("◂ live here", "c-accent"),
    ("no commit history — ghcr.io/example/app has no app repo in repos[].apps", "c-dim"),
])
degraded += '\n<span class="c-dim">  ↑/↓ tag · space review the change · esc back</span>'

open("frames.txt", "a", encoding="utf-8").write(
    "===PICKER2===\n" + picker2 + "\n===COMMIT===\n" + commit +
    "\n===DEGRADED===\n" + degraded + "\n")
print("appended")

# ─────────────────── 10. DEPLOY CONFIRM, COMMITS-LED ───────────────────
b = Box([74])
b.top("hoist · confirm deploy")
b.span("ghcr.io/example/app:v3   →   app-production                  mode: PR")
b.span_rule()
b.span("rolling out 14 commits · 2 migrations · replacing v1, live 34 days")
b.span_rule()
b.span("  4a1c2ef  Add rate limiting to the public API")
b.span("  e9b0d31  Fix N+1 query when resolving digests")
b.span("  77c0ffe  db: add index on events.created_at                 migration")
b.span("  1b2d3e4  Bump temporal SDK to 1.31")
b.span("  6f8a90c  Drop the legacy /v1/export endpoint")
b.span("  a3e91b2  db: backfill events.tenant_id                      migration")
b.span("                                                      ↓ 8 more commits")
b.span_rule()
b.span("2 migrations run on this deploy:")
b.span("  20260225T101500_add_events_created_at_index.rb")
b.span("  20260301T090200_backfill_events_tenant_id.rb")
b.span_rule()
b.span("writes 3 occurrences in 1 file                        d  see the yaml")
b.bot_span()
confirm2 = colour(b.render(), [
    ("ghcr.io/example/app:v3   →   app-production", "c-bold"),
    ("mode: PR", "c-accent"),
    ("rolling out 14 commits", "c-bold"),
    ("2 migrations · replacing v1, live 34 days", "c-warn"),
    ("migration", "c-warn"), ("migration", "c-warn"),
    ("↓ 8 more commits", "c-dim"),
    ("2 migrations run on this deploy:", "c-warn"),
    ("  20260225T101500_add_events_created_at_index.rb", "c-dim"),
    ("  20260301T090200_backfill_events_tenant_id.rb", "c-dim"),
    ("writes 3 occurrences in 1 file", "c-dim"),
    ("d  see the yaml", "c-dim"),
])
confirm2 += '\n<span class="c-dim">  enter deploy · ↑/↓ read a commit · d yaml diff · m mode · esc back</span>'

# ─────────────────── 11. THE YAML, ONE KEY AWAY ───────────────────
b = Box([74])
b.top("hoist · confirm deploy · yaml")
b.span("ghcr.io/example/app:v3   →   app-production                  mode: PR")
b.span_rule()
b.span("--- a/cluster/apps/app-production/app/deployment.yaml")
b.span("+++ b/cluster/apps/app-production/app/deployment.yaml")
b.span("@@ -21,7 +21,7 @@")
b.span("           containers:")
b.span("             - name: app")
b.span("-              image: ghcr.io/example/app:v1@sha256:1111111111…")
b.span("+              image: ghcr.io/example/app:v3@sha256:3333333333…")
b.span("@@ -95,7 +95,7 @@")
b.span("             - name: worker")
b.span("-              image: ghcr.io/example/app:v1@sha256:1111111111…")
b.span("+              image: ghcr.io/example/app:v3@sha256:3333333333…")
b.span_rule()
b.span("3 occurrences · 1 file · verified before commit       d  back to commits")
b.bot_span()
yamlpane = colour(b.render(), [
    ("ghcr.io/example/app:v3   →   app-production", "c-bold"),
    ("mode: PR", "c-accent"),
    ("--- a/cluster/apps/app-production/app/deployment.yaml", "c-dim"),
    ("+++ b/cluster/apps/app-production/app/deployment.yaml", "c-dim"),
    ("@@ -21,7 +21,7 @@", "c-dim"),
    ("-              image: ghcr.io/example/app:v1@sha256:1111111111…", "del"),
    ("+              image: ghcr.io/example/app:v3@sha256:3333333333…", "add"),
    ("@@ -95,7 +95,7 @@", "c-dim"),
    ("-              image: ghcr.io/example/app:v1@sha256:1111111111…", "del"),
    ("+              image: ghcr.io/example/app:v3@sha256:3333333333…", "add"),
    ("3 occurrences · 1 file · verified before commit", "c-dim"),
    ("d  back to commits", "c-dim"),
])
yamlpane += '\n<span class="c-dim">  enter deploy · d back to commits · m mode · esc back</span>'

open("frames.txt", "a", encoding="utf-8").write(
    "===CONFIRM2===\n" + confirm2 + "\n===YAMLPANE===\n" + yamlpane + "\n")
print("appended")

# ─────────────────── 12. PLAN CONFIRM, IMPACT-LED ───────────────────
b = Box([76])
b.top("hoist · confirm promotion")
b.span("app-staging  →  app-production                              mode: PR")
b.span_rule()
b.span("3 repos · 41 commits · 3 migrations · replacing images live 12-38 days")
b.span_rule()
b.span("▸ ✓ orders      v2026011510 → v2026022012    18 commits · 2 migrations")
b.span("     4a1c2ef  Add rate limiting to the public API")
b.span("     77c0ffe  db: add index on events.created_at            migration")
b.span("     …16 more")
b.span("")
b.span("  ✓ marketing   sha-0000111 → sha-1a2b3c4     6 commits")
b.span("     2f1e0a9  Update the pricing page copy")
b.span("     …5 more")
b.span("")
b.span("  ✓ web  !      v2026010101 → v2026021509    17 commits · 1 migration")
b.span("     9c3b7d1  Switch the queue driver to temporal")
b.span("     …16 more")
b.span_rule()
b.span("! web runs 2 versions in app-staging — promoting v202602150930")
b.span_rule()
b.span("writes 6 occurrences in 3 files                      d  see the yaml")
b.bot_span()
plan2 = colour(b.render(), [
    ("app-staging  →  app-production", "c-bold"),
    ("mode: PR", "c-accent"),
    ("3 repos · 41 commits", "c-bold"),
    ("3 migrations · replacing images live 12-38 days", "c-warn"),
    ("▸ ✓ orders      v2026011510 → v2026022012    18 commits · 2 migrations", "sel"),
    ("migration", "c-warn"),
    ("     …16 more", "c-dim"),
    ("     …5 more", "c-dim"),
    ("     …16 more", "c-dim"),
    ("! web runs 2 versions in app-staging — promoting v202602150930", "c-warn"),
    ("writes 6 occurrences in 3 files", "c-dim"),
    ("d  see the yaml", "c-dim"),
])
plan2 += '\n<span class="c-dim">  enter confirm · x toggle repo · tab expand commits · d yaml · m mode · esc back</span>'

open("frames.txt", "a", encoding="utf-8").write("===PLAN2===\n" + plan2 + "\n")
print("appended")
