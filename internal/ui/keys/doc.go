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
// T3-01 (train3-design.md) lands the registry, the matcher and the footer helper with no
// screen wired to any of them yet: AGENTS.md §4.8 records this as transitional, and every
// screen still carries its own bubbles keymap until its own PR in the T3 train migrates it.
// A binding's Name is the semantic id a keypress resolves to ("refresh", "abandon") — one
// letter has exactly one Name across every screen it is bound on (audit rule 1), enforced by
// TestLetterOneMeaning below, not by review alone (AGENTS.md §10 meta-rule 5).
package keys
