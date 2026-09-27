// Package keys is the one registry of the whole approved screen × key table
// (docs/audit/2026-09-ux-arch-audit.md, "Proposed keymap"), plus the two things every screen
// needs to act on it: a write-binding matcher that tells legacy-shift from caps lock (§9 entry
// 13), and the footer helper that lays out hints under §9 entry 10's "the frame owns every
// row" rule.
//
// This package imports internal/ui (for ui.Styles, in Footer) and bubbles/key, huh and
// bubbletea; ui must never import keys back — keys is one layer further from the terminal,
// built on top of the palette rather than under it.
//
// T3-01 through T3-10 (the audit doc) migrated every screen in internal/app to this package's
// registry, matcher and footer helper (AGENTS.md §4.8); a raw bubbles keymap or `key.NewBinding`
// call outside this package is a regression now, frozen by internal/copycheck rather than left
// to review. A binding's Name is the semantic id a keypress resolves to ("refresh", "abandon") — one
// letter has exactly one Name across every screen it is bound on (audit rule 1), enforced by
// TestLetterOneMeaning below, not by review alone (AGENTS.md §10 meta-rule 5).
package keys
