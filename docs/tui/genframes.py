# -*- coding: utf-8 -*-
import html
import sys

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

print("appended")

# ─────────────────── 13. FLIGHT (M10: built, not proposed) ───────────────────
b = Box([76])
b.top("hoist · promotion · in flight")
b.span("5pr6sd333t   app-staging → app-production      started 12m ago · deadline in 3h 48m")
b.span_rule()
b.span("✓ branch")
b.span("✓ commit")
b.span("✓ push")
b.span("✓ PR")
b.span("✓ CI")
b.span("… approval")
b.span("    no approval comment yet")
b.span("· merge")
b.span("· argo refresh")
b.span("· argo sync")
b.span("· rollout")
b.span_rule()
b.span("blocked on you — comment on PR #103 to release it:")
b.span("")
b.span("    hoist approve 5pr6sd333t")
b.bot_span()
flightframe = colour(b.render(), [
    ("5pr6sd333t", "c-bold"), ("app-staging → app-production", "c-bold"),
    ("started 12m ago · deadline in 3h 48m", "c-dim"),
    ("✓ branch", "c-good"), ("✓ commit", "c-good"), ("✓ push", "c-good"), ("✓ PR", "c-good"), ("✓ CI", "c-good"),
    ("… approval", "c-warn"),
    ("blocked on you — comment on PR #103 to release it:", "c-warn"),
    ("    hoist approve 5pr6sd333t", "c-accent"),
])
flightframe += '\n<span class="c-dim">  o open PR · R re-observe · x abort · l log · esc back</span>'

print("appended")

# ═══════════════════ v2 PROPOSAL (2026-09 audit) ═══════════════════
# Frames answering docs/audit/2026-09-ux-arch-audit.md. Unlike frames 1–13 above (which
# were pasted into mockups.html by hand), this section writes itself into mockups.html
# between the v2 markers, so `python3 docs/tui/genframes.py` is the whole regeneration.
# StrictBox refuses text wider than its cell instead of truncating it — a silently
# clipped mockup line is the same class of bug as UX-H7's clipped border — and every
# frame asserts its exact height, the way uitest.Golden does.
import os, re

class StrictBox(Box):
    def span(self, text):
        inner = self.width() - 4
        assert len(text) <= inner, f"span over {inner}: {text!r} ({len(text)})"
        super().span(text)
    def row(self, *cells):
        for w, c in zip(self.cols, cells):
            assert len(c) <= w, f"cell over {w}: {c!r} ({len(c)})"
        super().row(*cells)
    def pad_rows(self, height, blank):
        """Append blank table rows until the frame, once closed with `tail` more lines,
        is exactly `height` lines. Caller passes blank = the row tuple to repeat."""
        while len(self.lines) < height:
            self.row(*blank)
    def pad_spans(self, height):
        while len(self.lines) < height:
            self.span("")

def lr(left, right, w):
    """left and right on one line of width w, raising rather than overlapping."""
    gap = w - len(left) - len(right)
    assert gap >= 1, f"lr over {w}: {left!r} + {right!r}"
    return left + " " * gap + right

def words(h, word, cls):
    """Colour every whole-word occurrence of word (so 'pinned' never matches inside
    'unpinned'). Runs on escaped html; class names never contain these words."""
    return re.sub(r'(?<![\w-])' + re.escape(esc(word)) + r'(?![\w-])',
                  lambda m: f'<span class="{cls}">{m.group(0)}</span>', h)

def footer(text, w):
    assert len(text) <= w, f"footer over {w}: {text!r} ({len(text)})"
    return '\n<span class="c-dim">' + esc(text.rjust(w)) + '</span>'

# FRAMES holds the plain-text render of every v2 mockup frame, footer row included, keyed
# by "v2-<name>-<w>x<h>" (T3-01, --txt below). record() is called at each frame function's
# own footer() call site — the one place box.lines is complete (after check()) and the
# footer text is still a plain string, before colour() or overlay() touch it — so the plain
# and coloured renders can never drift apart the way two independently-written copies would.
FRAMES = {}
def record(name, lines, text, w):
    assert len(text) <= w, f"footer over {w}: {text!r} ({len(text)})"
    FRAMES[name] = list(lines) + [text.rjust(w)]
    return footer(text, w)

def check(box, height, w):
    ls = box.lines
    assert len(ls) == height - 1, f"frame is {len(ls)} lines, want {height - 1}"
    for l in ls:
        assert len(l) == w, f"line is {len(l)} wide, want {w}: {l!r}"

def overlay_plain(base_lines, dlg_lines, top, left):
    """Plain-text sibling of overlay() below, for FRAMES/record: no colour to dim, just the
    dialog's cells written over the base's at (top, left) — the shape a real terminal shows,
    which is what a golden eventually diffs against."""
    out = list(base_lines)
    for i, dl in enumerate(dlg_lines):
        j = top + i
        row = out[j]
        out[j] = row[:left] + dl + row[left + len(dl):]
    return out

def overlay(base_lines, dlg_lines, dlg_html, top, left):
    """Composite a dialog over a dimmed frame (ui.Dialog's shape): every base cell
    outside the dialog is dimmed, the dialog keeps its own colours."""
    out = []
    for i, bl in enumerate(base_lines):
        j = i - top
        if 0 <= j < len(dlg_lines):
            w = len(dlg_lines[j])
            out.append('<span class="c-dim dimmed">' + esc(bl[:left]) + '</span>' + dlg_html[j]
                       + '<span class="c-dim dimmed">' + esc(bl[left + w:]) + '</span>')
        else:
            out.append('<span class="c-dim dimmed">' + esc(bl) + '</span>')
    return "\n".join(out)

# ── v2·1a MATRIX 80x24 ──
def matrix_v2_80(prod=False):
    b = StrictBox([12, 28, 30])
    b.top("hoist · matrix · my-gitops")
    b.span("cluster/apps · base main · context my-cluster")
    b.open_cols()
    if prod:
        b.row("FAMILY", "APP-STAGING", "▸ APP-PRODUCTION ⚠")
    else:
        b.row("FAMILY", "▸ APP-STAGING", "APP-PRODUCTION ⚠")
    b.sep()
    S = lambda v, st: lr(v, st, 28)
    P = lambda v, st: lr(v, st, 30)
    b.row("▸ orders", S("v202602201200", "pinned"), P("v202601151010", "pinned"))
    b.row("  marketing", S("sha-1a2b3c4", "pinned"), P("sha-0000111", "drifted"))
    b.row("  temporal", S("2 images", "external"), P("2 images", "external"))
    b.row("  web", S("2 builds", "split"), P("v202601010101", "pinned"))
    b.row("  worker", S("v3", "unpinned"), P("v2", "unpinned"))
    b.pad_rows(15, ("", "", ""))
    b.close_cols()
    b.span("marketing in app-production runs sha-77c0ffe; manifest says sha-0000111")
    b.span_rule()
    b.span(lr("in flight · 1", "next check in 12s", 76))
    b.span(lr("5pr6sd333t  orders, marketing  app-staging → app-production", "12m", 76))
    b.span("✓ branch ✓ commit ✓ push ✓ PR #103 ✓ CI ⏸ approval · merge · argo · rollout")
    b.span("waiting for an approver to comment `hoist approve 5pr6sd333t` on PR #103")
    b.bot_span()
    check(b, 24, 80)
    h = colour(b.render(), [
        ("▸ APP-STAGING", "c-bold"),
        ("APP-PRODUCTION ⚠", "c-prod"),
        ("▸ orders", "c-bold"),
        (lr("v202602201200", "pinned", 28), "cur"),
        ("marketing in app-production runs sha-77c0ffe; manifest says sha-0000111", "c-warn"),
        ("next check in 12s", "c-dim"),
        ("5pr6sd333t", "c-bold"),
        ("✓ branch ✓ commit ✓ push ✓ PR #103 ✓ CI", "c-good"),
        ("⏸ approval", "c-warn"),
        ("· merge · argo · rollout", "c-dim"),
        ("`hoist approve 5pr6sd333t`", "c-accent"),
    ])
    for w_, c_ in (("pinned", "c-dim"), ("external", "c-dim"), ("drifted", "c-warn"),
                   ("split", "c-warn"), ("unpinned", "c-warn")):
        h = words(h, w_, c_)
    # the cursor cell's own "pinned" was dimmed inside the highlight; that is intended —
    # the highlight carries the cell, the word stays quiet.
    h += record("v2-matrix-80x24", b.lines,
                "enter actions · p promote into · t tag · w watch · r refresh · ? help · q quit", 80)
    return h, b.lines

# ── v2·1b MATRIX 120x40 with detail pane ──
def matrix_v2_120():
    b = StrictBox([13, 28, 28, 38])
    b.top("hoist · matrix · my-gitops")
    b.span("cluster/apps · base main · context my-cluster · 2 envs · 5 families")
    b.open_cols()
    b.row("FAMILY", "▸ APP-STAGING", "APP-PRODUCTION ⚠", "orders · app-staging")
    b.sep()
    detail = [
        "",
        "ghcr.io/example/orders",
        "  v202602201200",
        "  sha256:4f2a9c01e7…      pinned",
        "  declared 3 days ago · a1b2c3d",
        "",
        "running   2/2 pods on this digest",
        "drift     none",
        "",
        "ahead of app-production",
        "  18 commits · 2 migrations",
        "",
        "last write  3 days ago",
        "  deploy v202602201200 · PR #101",
    ]
    C = lambda v, st: lr(v, st, 28)
    cells = [
        ("▸ orders", C("v202602201200", "pinned"), C("v202601151010", "pinned")),
        ("  marketing", C("sha-1a2b3c4", "pinned"), C("sha-0000111", "drifted")),
        ("  temporal", C("2 images", "external"), C("2 images", "external")),
        ("  web", C("2 builds", "split"), C("v202601010101", "pinned")),
        ("  worker", C("v3", "unpinned"), C("v2", "unpinned")),
    ]
    rows = 26
    for i in range(rows):
        f, s, p = cells[i] if i < len(cells) else ("", "", "")
        d = detail[i] if i < len(detail) else ""
        b.row(f, s, p, d)
    b.close_cols()
    b.span("marketing in app-production runs sha-77c0ffe; manifest says sha-0000111 · compared by digest")
    b.span_rule()
    b.span(lr("in flight · 1", "next check in 12s", 116))
    b.span(lr("5pr6sd333t   orders v2026011510 → v2026022012 · marketing sha-0000111 → sha-1a2b3c4",
              "started 12m ago", 116))
    b.span("✓ branch  ✓ commit  ✓ push  ✓ PR #103  ✓ CI 4/4  ⏸ approval  · merge  · argo refresh  · argo sync  · rollout")
    b.span("waiting for an approver to comment `hoist approve 5pr6sd333t` on PR #103 · deadline in 3h 48m")
    b.bot_span()
    check(b, 40, 120)
    h = colour(b.render(), [
        ("▸ APP-STAGING", "c-bold"),
        ("APP-PRODUCTION ⚠", "c-prod"),
        ("orders · app-staging", "c-bold"),
        ("▸ orders", "c-bold"),
        (lr("v202602201200", "pinned", 28), "cur"),
        ("sha256:4f2a9c01e7…", "c-accent"),
        ("declared 3 days ago · a1b2c3d", "c-dim"),
        ("18 commits · 2 migrations", "c-warn"),
        ("marketing in app-production runs sha-77c0ffe; manifest says sha-0000111", "c-warn"),
        ("next check in 12s", "c-dim"),
        ("5pr6sd333t", "c-bold"),
        ("✓ branch  ✓ commit  ✓ push  ✓ PR #103  ✓ CI 4/4", "c-good"),
        ("⏸ approval", "c-warn"),
        ("· merge  · argo refresh  · argo sync  · rollout", "c-dim"),
        ("`hoist approve 5pr6sd333t`", "c-accent"),
        ("deadline in 3h 48m", "c-dim"),
    ])
    for w_, c_ in (("pinned", "c-dim"), ("external", "c-dim"), ("drifted", "c-warn"),
                   ("split", "c-warn"), ("unpinned", "c-warn")):
        h = words(h, w_, c_)
    h += record("v2-matrix-120x40", b.lines,
                "enter actions · p promote into app-staging · t deploy tag · w watch · shift+r restart · "
                "tab in flight · ? help · q quit", 120)
    return h

# ── v2·2 ACTION MENU over the dimmed matrix ──
def action_menu():
    # base: the 80x24 matrix with the cursor moved to orders × app-production
    _, base = matrix_v2_80(prod=True)
    d = StrictBox([50])
    d.top("orders · app-production ⚠")
    d.span("▸ p  promote into app-production from app-staging")
    d.span("  t  deploy a tag to app-production")
    d.span("  w  watch the rollout")
    d.span("  shift+r  restart orders in app-production")
    d.span("     resume in flight 5pr6sd333t (approval)")
    d.span("     promote into app-production from…")
    d.span_rule()
    d.span("⚠ production: every write opens a PR and waits")
    d.span("  for an approver's `hoist approve` comment")
    d.bot_span()
    dh = [colour(l, [
        ("orders · app-production ⚠", "c-prod"),
        ("▸ p  promote into app-production from app-staging", "sel"),
        ("⚠ production: every write opens a PR and waits", "c-prod"),
        ("  for an approver's `hoist approve` comment", "c-prod"),
    ]) for l in d.lines]
    top, left = 6, (80 - d.width()) // 2
    h = overlay(base, d.lines, dh, top, left)
    h += record("v2-action-menu-80x24", overlay_plain(base, d.lines, top, left),
                "↑/↓ move · enter run · the letter runs it directly · esc close", 80)
    return h

# ── v2·3 HELP OVERLAY ──
def help_overlay():
    _, base = matrix_v2_80()
    d = StrictBox([29, 30])
    d.top("help · matrix")
    d.row("NAVIGATE", "ACT")
    d.row("↑↓ ←→    move", "enter  actions for this cell")
    d.row("tab      table ⇄ in flight", "p      promote into this env")
    d.row("pgup/dn  page", "t      deploy a tag")
    d.row("esc      back · close", "w      watch the rollout")
    d.row("", "shift+r restart        (asks)")
    d.row("VIEW", "shift+x abandon        (asks)")
    d.row("r F5     refresh", "")
    d.row("o        open PR in browser", "APP")
    d.row("c        config", "?      this help")
    d.row("l        activity log", "q      quit (asks if driving)")
    d.row("", "ctrl+c quit now")
    d.close_cols()
    d.span("shift+ keys always ask before they write")
    d.bot_span()
    dh = [colour(l, [("NAVIGATE", "c-bold"), ("ACT", "c-bold"), ("VIEW", "c-bold"),
                     ("APP", "c-bold"), ("(asks)", "c-dim", -1),
                     ("shift+ keys always ask before they write", "c-dim")]) for l in d.lines]
    top, left = 3, (80 - d.width()) // 2
    h = overlay(base, d.lines, dh, top, left)
    h += record("v2-help-overlay-80x24", overlay_plain(base, d.lines, top, left), "esc close", 80)
    return h

# ── v2·4a FLIGHT, waiting ──
def flight_waiting():
    b = StrictBox([76])
    b.top("hoist · promotion · waiting for approval")
    b.span(lr("5pr6sd333t  app-staging → app-production", "started 12m ago", 76))
    b.span("orders v2026011510 → v2026022012 · marketing sha-0000111 → sha-1a2b3c4")
    b.span_rule()
    steps = [
        ("✓ branch", "hoist/app-production/5pr6sd333t", "12m ago"),
        ("✓ commit", "signed · 2 files", "12m ago"),
        ("✓ push", "", "12m ago"),
        ("✓ PR #103", "opened", "11m ago"),
        ("✓ CI", "4 of 4 checks passed", "6m ago"),
        ("⏸ approval", "waiting 6m", "next check in 12s"),
        ("· merge", "", ""),
        ("· argo refresh", "", ""),
        ("· argo sync", "", ""),
        ("· rollout", "2 deployments", ""),
    ]
    for g, mid, t in steps:
        b.span(lr(g.ljust(16) + mid, t, 76) if t else (g.ljust(16) + mid))
    b.span_rule()
    b.span("waiting for an approver to comment `hoist approve 5pr6sd333t`")
    b.span("on PR #103 · o opens it")
    b.span("")
    b.span("deadline in 3h 48m · leaving this screen keeps it running")
    b.span_rule()
    b.span("11m ago  PR #103 opened · github.com/me/my-gitops/pull/103")
    b.span(" 6m ago  CI green · 4 checks")
    b.bot_span()
    check(b, 24, 80)
    h = colour(b.render(), [
        ("waiting for approval", "c-warn"),
        ("5pr6sd333t", "c-bold"),
        ("app-staging → app-production", "c-bold"),
        ("started 12m ago", "c-dim"),
        ("✓ branch", "c-good"), ("✓ commit", "c-good"), ("✓ push", "c-good"),
        ("✓ PR #103", "c-good"), ("✓ CI", "c-good"),
        ("⏸ approval", "c-warn"),
        ("next check in 12s", "c-dim"),
        ("· merge", "c-dim"), ("· argo refresh", "c-dim"), ("· argo sync", "c-dim"),
        ("· rollout", "c-dim"),
        ("`hoist approve 5pr6sd333t`", "c-accent"),
        ("o opens it", "c-dim"),
        ("deadline in 3h 48m · leaving this screen keeps it running", "c-dim"),
        ("11m ago", "c-dim"), (" 6m ago", "c-dim"),
    ])
    h += record("v2-flight-waiting-80x24", b.lines,
                "esc back (keeps running) · o open PR · w watch · l log · ? help", 80)
    return h

# ── v2·4b FLIGHT, done (80x16) ──
def flight_done():
    b = StrictBox([76])
    b.top("hoist · promotion · done")
    b.span(lr("5pr6sd333t  app-staging → app-production", "finished 2m ago · took 41m", 76))
    b.span_rule()
    b.span("✓ branch  ✓ commit  ✓ push  ✓ PR #103  ✓ CI  ✓ approval  ✓ merge")
    b.span("✓ argo refresh  ✓ argo sync  ✓ rollout · 2/2 deployments available")
    b.span_rule()
    b.span("app-production now declares and runs:")
    b.span("  orders     v2026022012   sha256:4f2a9c01e7…")
    b.span("  marketing  sha-1a2b3c4   sha256:9b8c7d6e5f…")
    b.span("")
    b.span("PR #103 merged as 9f8e7d6 · the matrix has been refreshed")
    b.span_rule()
    b.span(" 2m ago  rollout complete · 2/2")
    b.span(" 4m ago  argo synced 9f8e7d6 · healthy")
    b.bot_span()
    check(b, 16, 80)
    h = colour(b.render(), [
        ("done", "c-good"),
        ("5pr6sd333t", "c-bold"),
        ("app-staging → app-production", "c-bold"),
        ("finished 2m ago · took 41m", "c-dim"),
        ("✓ branch  ✓ commit  ✓ push  ✓ PR #103  ✓ CI  ✓ approval  ✓ merge", "c-good"),
        ("✓ argo refresh  ✓ argo sync  ✓ rollout", "c-good"),
        ("sha256:4f2a9c01e7…", "c-dim"), ("sha256:9b8c7d6e5f…", "c-dim"),
        ("the matrix has been refreshed", "c-good"),
        (" 2m ago", "c-dim"), (" 4m ago", "c-dim"),
    ])
    h += record("v2-flight-done-80x16", b.lines,
                "esc back · o open PR · w watch · l log · ? help", 80)
    return h

# ── v2·5a DEPLOY CONFIRM ──
def deploy_confirm():
    b = StrictBox([76])
    b.top("hoist · deploy · confirm")
    b.span(lr("ghcr.io/example/app:v3  →  app-staging", "mode: PR · shift+d direct", 76))
    b.span_rule()
    b.span("rolling out 14 commits · 2 migrations · replacing v1, declared 34 days")
    b.span_rule()
    commits = [
        "4a1c2ef  Add rate limiting to the public API",
        "e9b0d31  Fix N+1 query when resolving digests",
        lr("77c0ffe  db: add index on events.created_at", "migration", 74),
        "1b2d3e4  Bump temporal SDK to 1.31",
        "6f8a90c  Drop the legacy /v1/export endpoint",
        lr("a3e91b2  db: backfill events.tenant_id", "migration", 74),
        "9d2c4e1  Retry the registry HEAD on 429",
        "c0ffee1  Log the resolved digest at startup",
        "5e6f7a8  Move health checks to /healthz",
        "d4c3b2a  Tidy the Dockerfile layers",
    ]
    b.span("▸ " + commits[0])
    for c in commits[1:]:
        b.span("  " + c)
    b.span("↓ 4 more commits".rjust(76))
    b.span_rule()
    b.span("2 migrations run on this deploy:")
    b.span("  20260225T101500_add_events_created_at_index.rb")
    b.span("  20260301T090200_backfill_events_tenant_id.rb")
    b.span_rule()
    b.span("writes 3 image references in 1 file · verified before commit")
    b.pad_spans(22)
    b.bot_span()
    check(b, 24, 80)
    h = colour(b.render(), [
        ("ghcr.io/example/app:v3  →  app-staging", "c-bold"),
        ("mode: PR", "c-accent"), ("shift+d direct", "c-dim"),
        ("rolling out 14 commits", "c-bold"),
        ("2 migrations · replacing v1, declared 34 days", "c-warn"),
        ("▸ 4a1c2ef  Add rate limiting to the public API", "sel"),
                ("↓ 4 more commits", "c-dim"),
        ("2 migrations run on this deploy:", "c-warn"),
        ("writes 3 image references in 1 file · verified before commit", "c-dim"),
    ])
    h = words(h, "migration", "c-warn")
    h += record("v2-deploy-confirm-80x24", b.lines,
                "enter deploy · d yaml · shift+d direct · ↑/↓ commits · esc back to tags · ? help", 80)
    return h

# ── v2·5b PLAN CONFIRM ──
def plan_confirm():
    b = StrictBox([33, 40])
    b.top("hoist · promotion · confirm")
    b.span(lr("app-staging → app-production ⚠", "mode: PR", 76))
    b.span("3 repos · 41 commits · 3 migrations · 6 image references, 3 files")
    b.open_cols()
    b.row("ghcr.io/example/", "orders  v2026011510 → v2026022012")
    b.sep()
    b.row("▸ ✓ orders", "18 commits · 2 migrations")
    b.row("    v2026011510 → v2026022012", "")
    b.row("  ✓ marketing", "4a1c2ef Add rate limiting to the API")
    b.row("    sha-0000111 → sha-1a2b3c4", lr("77c0ffe db: index created_at", "migration", 40))
    b.row("  ✓ web · split in staging", "e9b0d31 Fix N+1 query on digests")
    b.row("    v2026010101 → v2026021509", lr("b71c0de db: backfill region", "migration", 40))
    b.row("  · worker · already current", "1b2d3e4 Bump temporal SDK to 1.31")
    b.row("    v2026022012, no change", "6f8a90c Drop the legacy export endpoint")
    b.row("", "…12 more")
    b.pad_rows(18, ("", ""))
    b.close_cols()
    b.span("web runs 2 builds in app-staging; promoting the newer, v202602150930")
    b.span("app-production is a production env: the PR waits for `hoist approve`")
    b.span("worker is already at v2026022012 in app-production and is left alone")
    b.bot_span()
    check(b, 24, 80)
    h = colour(b.render(), [
        ("app-staging → app-production ⚠", "c-prod"),
        ("mode: PR", "c-accent"),
        ("3 repos · 41 commits", "c-bold"),
        ("3 migrations", "c-warn"),
        ("▸ ✓ orders", "sel"),
        ("18 commits · 2 migrations", "c-warn"),
        ("· split in staging", "c-warn"),
        ("  · worker · already current", "c-dim"),
        ("    v2026022012, no change", "c-dim"),
                ("…12 more", "c-dim"),
        ("web runs 2 builds in app-staging; promoting the newer, v202602150930", "c-warn"),
        ("app-production is a production env: the PR waits for `hoist approve`", "c-prod"),
        ("worker is already at v2026022012 in app-production and is left alone", "c-dim"),
    ])
    h = words(h, "migration", "c-warn")
    h += record("v2-plan-confirm-80x24", b.lines,
                "enter promote · space tick · d yaml · e edit digest · esc back · ? help", 80)
    return h

# ── v2·6a TAG PICKER, filling the body ──
def tags_v2():
    b = StrictBox([13, 13, 13, 28])
    b.top("hoist · tags · app-staging")
    b.span("ghcr.io/example/app · app-staging declares v1, set 34 days ago")
    b.open_cols()
    b.row("TAG", "BUILT", "DIGEST", "SINCE v1")
    b.sep()
    b.row("▸ v3", "3 days ago", "3333333333", "14 commits · 2 migrations")
    b.row("  v2", "4 weeks ago", "2222222222", "3 commits")
    b.row("  v1", "2 months ago", "1111111111", "◂ declared here")
    b.row("── digest", "", "", "")
    b.row("  sha-3333333", "3 days ago", "3333333333", "same build as v3")
    b.row("── moving", "", "", "")
    b.row("  latest", "3 days ago", "3333333333", "same build as v3")
    b.close_cols()
    b.span("v3 · 14 commits ahead of v1 · 2 migrations")
    b.span("  4a1c2ef  Add rate limiting to the public API")
    b.span("  e9b0d31  Fix N+1 query when resolving digests")
    b.span(lr("  77c0ffe  db: add index on events.created_at", "migration", 76))
    b.span("  1b2d3e4  Bump temporal SDK to 1.31")
    b.span("  6f8a90c  Drop the legacy /v1/export endpoint")
    b.span(lr("  a3e91b2  db: backfill events.tenant_id", "migration", 76))
    b.span("  9d2c4e1  Retry the registry HEAD on 429")
    b.span("↓ 7 more commits".rjust(76))
    b.pad_spans(22)
    b.bot_span()
    check(b, 24, 80)
    h = colour(b.render(), [
        ("▸ v3", "sel"),
        ("14 commits · 2 migrations", "c-warn"),
        ("◂ declared here", "c-accent"),
        ("── digest", "c-dim"), ("── moving", "c-dim"),
        ("same build as v3", "c-dim", -1),
        ("v3 · 14 commits ahead of v1", "c-bold"),
                ("↓ 7 more commits", "c-dim"),
    ])
    h = words(h, "migration", "c-warn")
    h += record("v2-tags-80x24", b.lines,
                "enter review v3 · → read commit · / filter · r reload · esc back · ? help", 80)
    return h

# ── v2·6b EMPTY / ERROR states (80x12 and 80x10) ──
def empty_matrix():
    b = StrictBox([76])
    b.top("hoist · matrix · my-gitops")
    b.span("")
    b.span("no environments discovered under cluster/apps")
    b.span("")
    b.span("hoist reads Argo CD Application wrappers under the apps root and names")
    b.span("each env by its spec.destination.namespace. Check repos[].apps_root in")
    b.span("the config, or pass --apps-root / --repo.")
    b.span("")
    b.span("c shows the config hoist loaded")
    b.pad_spans(10)
    b.bot_span()
    check(b, 12, 80)
    h = colour(b.render(), [
        ("no environments discovered under cluster/apps", "c-warn"),
        ("repos[].apps_root", "c-accent"), ("--apps-root", "c-accent"), ("--repo", "c-accent"),
    ])
    h += record("v2-empty-matrix-80x12", b.lines, "r refresh · c config · ? help · q quit", 80)
    return h

def tags_error():
    b = StrictBox([76])
    b.top("hoist · tags · app-staging")
    b.span("ghcr.io/example/app · app-staging declares v1")
    b.span_rule()
    b.span("could not list tags: ghcr.io answered 403 (denied) for every credential")
    b.span("source tried: env, keychain, cluster")
    b.span("")
    b.span("gh's own token cannot read packages. Add a token with read:packages")
    b.span("to GHCR_TOKEN, or point registries[].cluster at the pull secret.")
    b.bot_span()
    check(b, 10, 80)
    h = colour(b.render(), [
        ("could not list tags: ghcr.io answered 403 (denied) for every credential", "c-bad"),
        ("read:packages", "c-accent"), ("GHCR_TOKEN", "c-accent"),
        ("registries[].cluster", "c-accent"),
    ])
    h += record("v2-tags-error-80x10", b.lines, "r retry · esc back · ? help", 80)
    return h

# ── HTML ──
DOTS = ('<span class="dot" style="background:#e0706a"></span><span class="dot" '
        'style="background:#e0a854"></span><span class="dot" style="background:#6ec49a"></span>')

def term(title, pre):
    return (f'    <div class="term">\n      <div class="term-bar">\n        {DOTS}\n'
            f'        <span class="term-title">{esc(title)}</span>\n      </div>\n'
            f'      <div class="term-body"><pre>{pre}</pre></div>\n    </div>\n')

def notes(items):
    lis = "".join(f'      <li><span class="marker m-good">→</span><span>{i}</span></li>\n'
                  for i in items)
    return f'    <ul class="notes">\n{lis}    </ul>\n'

def screen(num, title, verdict, intro, frames, answers):
    """frames: [(label, term title, pre html, caption)] — caption is one line of
    reasoning, answers the finding IDs it closes."""
    out = [f'  <section class="screen v2">\n    <div class="screen-head">\n'
           f'      <span class="screen-num">{num}</span>\n      <h2>{title}</h2>\n'
           f'      <span class="verdict new">{verdict}</span>\n    </div>\n'
           f'    <p class="intro">{intro}</p>\n']
    for label, ttl, pre, caption, ids in frames:
        out.append(f'    <p class="frame-label">{esc(label)}</p>\n')
        out.append(term(ttl, pre))
        out.append(f'    <p class="answers"><b>Answers</b> {esc(ids)} — {caption}</p>\n')
    out.append(notes(answers))
    out.append('  </section>\n')
    return "".join(out)

m80, _ = matrix_v2_80()
V2 = [
    '<!-- v2:begin — generated by docs/tui/genframes.py; edit there, not here -->\n',
    '  <style>\n'
    '    .v2-head { margin-top: 96px; border-top: 2px solid var(--ink); padding-top: 22px; }\n'
    '    .cur    { background: #2d3d5c; color: #ffffff; }\n'
    '    .c-prod { color: #d9a0f0; }\n'
    '    .dimmed { opacity: 0.55; }\n'
    '    .answers { font-size: 14.5px; color: var(--ink-soft); margin: 10px 0 0; max-width: 72ch; }\n'
    '    .answers b { font-family: var(--mono); font-size: 12px; letter-spacing: 0.06em;'
    ' text-transform: uppercase; color: var(--accent); margin-right: 6px; }\n'
    '  </style>\n',
    '  <header class="v2-head">\n    <p class="eyebrow">v2 proposal · 2026-09 audit</p>\n'
    '    <h1>One keymap, and screens that answer back</h1>\n'
    '    <p class="standfirst">Frames for <code>docs/audit/2026-09-ux-arch-audit.md</code>. Each '
    'caption names the finding IDs it answers. Keys follow that doc\'s proposed keymap: '
    '<code>enter</code> primary, <code>esc</code> back and never a cancel, <code>shift+</code> only for '
    'writes. Colour: normal states are muted so only exceptions carry colour; production has '
    'its own hue, separate from warnings (UX-M15). Step glyphs, everywhere: '
    '<code>✓</code> done · <code>◐</code> active · <code>·</code> pending · <code>✗</code> '
    'failed · <code>⏸</code> waiting (UX-M1).</p>\n  </header>\n',
    screen("v2·01", "Matrix", "pipeline order · cell cursor",
           "Envs in pipeline order from <code>envs.pairs</code>, source before target, so "
           "promotion reads left to right and <code>p</code> promotes <i>into</i> the cursor "
           "column. The cursor starts on the first non-production column.",
           [("80×24", "hoist — matrix", m80,
             "the column and row meet in one highlighted cell; the in-flight pane lives inside "
             "the frame with its live step and when hoist next looks.",
             "UX-H1 UX-H2 UX-M9 UX-M11 UX-M14 UX-M16 FB-M6"),
            ("120×40", "hoist — matrix", matrix_v2_120(),
             "the spare height becomes a detail pane for the selected cell (images, digest, "
             "drift, what is ahead, last write) instead of 25 blank rows.",
             "UX-M10 UX-H1")],
           ["<b>Only exceptions are coloured.</b> <i>pinned</i> and <i>external</i> are muted; "
            "<i>drifted</i>, <i>split</i> and <i>unpinned</i> keep the amber.",
            "<b>The footer is priority-ordered</b> through one helper, so <code>? help</code> and "
            "<code>q quit</code> are the last to go, never the first (UX-H12).",
            "<b>Approval copy says who acts.</b> An approver comments; hoist cannot approve as "
            "you (UX-H10). Approving from the TUI is #90, out of scope."]),
    screen("v2·02", "Action menu", "enter does something",
           "<code>enter</code> on a cell opens what can be done <i>there</i>. Each item shows "
           "its shortcut, so the menu teaches the keys.",
           [("80×24 · dialog over the dimmed matrix", "hoist — matrix · actions", action_menu(),
             "the dead key becomes the discoverable one, and production says what a write "
             "there will cost before you pick one.",
             "UX-H3 UX-H1 FB-L8")],
           ["<b><code>shift+p</code> is gone.</b> \"Promote into this env from another source\" is "
            "the menu's last item, so no shift+ key is spent on a non-write.",
            "<b>Resume lives here and on the in-flight pane.</b> <code>r</code> is refresh "
            "only, on every screen."]),
    screen("v2·03", "Help overlay", "every screen",
           "<code>?</code> on any screen draws that screen's keys from the keymap registry, "
           "grouped by purpose: navigate, act, view, app.",
           [("80×24 · matrix", "hoist — matrix · help", help_overlay(),
             "one overlay generated from the registry replaces a single help line that was cut "
             "off at 80 and 120 columns.",
             "UX-H11 UX-H4")],
           ["<b><code>shift+</code> keys are marked \"asks\".</b> Every shift+ key is a write and "
            "every write asks first, so the overlay states it once."]),
    screen("v2·04", "Flight", "one glyph set · reason once",
           "The screen you watch for hours. It says what is moving, what it waits on, who "
           "acts, and when hoist next looks. Leaving it keeps the promotion running.",
           [("80×24 · waiting for approval", "hoist — promotion", flight_waiting(),
             "the waiting reason is stated once and names the TUI key; relative times "
             "throughout; esc says it keeps running.",
             "UX-H6 UX-H10 UX-H12 UX-M1 UX-M2 UX-M4 FB-H2 FB-M6 UX-L1"),
            ("80×16 · done", "hoist — promotion", flight_done(),
             "done says what the env now declares and runs, and that the matrix has already "
             "been refreshed. No F5 needed.",
             "FB-H1 UX-L1 UX-H12")],
           ["<b><code>x abort</code> is gone.</b> <code>esc</code> leaves without stopping "
            "anything, so there is nothing left for abort to mean (FB-L2).",
            "<b><code>w</code> jumps to watch</b> for this promotion's family and target, "
            "which the audit found took 5–8 keys (UX keypress table).",
            "<b>The footer drops what does not apply</b>, for example abandon on a finished "
            "promotion."]),
    screen("v2·05", "Confirm screens", "totals · worded warnings",
           "<code>enter</code> writes; <code>d</code> shows the yaml; <code>shift+d</code> toggles "
           "direct mode, and is never offered for a production target.",
           [("80×24 · deploy confirm", "hoist — deploy · confirm", deploy_confirm(),
             "the list counts what it hides, the mode sits in the header with its one key, and "
             "esc returns to the picker the tag came from.",
             "UX-M13 UX-M6 UX-M7 UX-M12 FB-L7"),
            ("80×24 · plan confirm", "hoist — promotion · confirm", plan_confirm(),
             "a totals line, warnings as sentences instead of a bare <code>!</code>, a no-op "
             "row greyed and explained; no <code>shift+d</code> because the target is production.",
             "UX-M17 UX-M12 UX-M6 UX-H4")],
           ["<b>\"Image references\", not \"occurrences\".</b> The same count in words an "
            "operator uses (terminology sweep, UX-L1).",
            "<b>\"declared 34 days\", never \"live\".</b> The confirm screens have no cluster "
            "read, so they say what the manifest declares (principle 1)."]),
    screen("v2·06", "Tag picker and empty states", "fills the body · says what to check",
           "The picker's commit pane takes the rest of the body. Empty and error states name "
           "the setting to check instead of stopping at a sentence.",
           [("80×24 · tag picker", "hoist — tags", tags_v2(),
             "enter reviews the tag, as it is the primary action everywhere; reading a commit "
             "moves to →.",
             "UX-H8 UX-M5 UX-H4"),
            ("80×12 · matrix, nothing discovered", "hoist — matrix", empty_matrix(),
             "a dead end becomes a pointer at repos[].apps_root, --apps-root and the config "
             "view.",
             "UX-M18"),
            ("80×10 · tag picker, registry refused", "hoist — tags", tags_error(),
             "the gap message wraps instead of truncating, and names the fix AGENTS.md §6.1 "
             "already knows.",
             "UX-M18")],
           ["<b>Groups keep their dividers</b> (#91): releases lead, then digest tags, then "
            "moving tags, and <code>/</code> filters across all three."]),
    '<!-- v2:end -->\n',
]

here = os.path.dirname(os.path.abspath(__file__))

# T3-01: `genframes.py --txt` writes the plain-text render of every v2 frame (FRAMES, above,
# populated as a side effect of building the mockup HTML above) to docs/tui/frames/, one file
# per frame, so a later redesign PR can `diff -u` its own golden against the approved mockup
# without eyeballing HTML (train3-design.md, "How goldens get compared to mockups"). It writes
# only those files — never mockups.html — so a normal run (no flag) is unaffected and a --txt
# run leaves no stray files beyond docs/tui/frames/*.txt.
if "--txt" in sys.argv:
    frames_dir = os.path.join(here, "frames")
    os.makedirs(frames_dir, exist_ok=True)
    for name, lines in sorted(FRAMES.items()):
        with open(os.path.join(frames_dir, f"{name}.txt"), "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
    print(f"{len(FRAMES)} frames written to docs/tui/frames/")
else:
    path = os.path.join(here, "mockups.html")
    doc = open(path, encoding="utf-8").read()
    block = "".join(V2)
    if "<!-- v2:begin" in doc:
        doc = re.sub(r"<!-- v2:begin.*?<!-- v2:end -->\n", lambda _: block, doc, flags=re.S)
    else:
        anchor = "  <footer>"
        assert anchor in doc, "mockups.html lost its <footer> anchor"
        doc = doc.replace(anchor, block + "\n" + anchor, 1)
    open(path, "w", encoding="utf-8").write(doc)
    print("v2 written to mockups.html")
