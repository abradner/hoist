package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/abradner/hoist/internal/ui/keys"
)

// decodeKey feeds raw bytes through ultraviolet's own event decoder (the same decoder
// bubbletea's input reader uses — see charm.land/bubbletea/v2's input.go, which converts a
// uv.KeyPressEvent to a tea.KeyPressMsg by a plain type conversion) and returns the resulting
// key press. It fails the test if the bytes don't decode to a KeyPressEvent, so a typo in a
// probe sequence is caught immediately rather than silently comparing zero values.
func decodeKey(t *testing.T, seq string) tea.KeyPressMsg {
	t.Helper()
	var dec uv.EventDecoder
	n, ev := dec.Decode([]byte(seq))
	if n != len(seq) {
		t.Fatalf("decodeKey(%q): consumed %d of %d bytes", seq, n, len(seq))
	}
	kp, ok := ev.(uv.KeyPressEvent)
	if !ok {
		t.Fatalf("decodeKey(%q): got %T, want uv.KeyPressEvent", seq, ev)
	}
	return tea.KeyPressMsg(kp)
}

// TestKittyShiftedPunctuationNeedsAlternateKeys is the byte-level evidence behind the
// ReportAlternateKeys fix in app.go's View: internal/app/app.go:1236 (now +2 lines). Review
// found that requesting only ReportAllKeysAsEscapeCodes (flag 8) makes a kitty-protocol
// terminal send shift+/, shift+; and shift+2 as their BASE codepoint plus the Shift modifier —
// with no shifted-key component in the sequence — because the terminal only includes that
// component when ReportAlternateKeys (flag 4) was also requested. The decoder then upper-cases
// the base rune, which is a no-op for punctuation, so it lands on "/", ";" and "2" instead of
// "?", ":" and "@". This is exactly the audit doc's named risk (P1-1 in the T3 review): "?"
// never opens help, and ":" can't be typed into the digest-override input.
func TestKittyShiftedPunctuationNeedsAlternateKeys(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		// Probed directly against ultraviolet's decoder (T3 review, P1-1): a terminal granted
		// only flag 8 sends the bare codepoint with the Shift modifier and nothing else.
		{"shift+slash, no alternate-key component", "\x1b[47;2u", "/"},
		{"shift+semicolon, no alternate-key component", "\x1b[59;2u", ";"},
		{"shift+2, no alternate-key component", "\x1b[50;2u", "2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := decodeKey(t, c.seq).Key()
			if k.Text != c.want {
				t.Fatalf("Text = %q, want %q (the pre-ReportAlternateKeys, still-broken shape)", k.Text, c.want)
			}
		})
	}
}

// TestKittyShiftedPunctuationDecodesCorrectlyWithAlternateKeys is the fix side of the same
// evidence: once a terminal actually includes the shifted-key sub-parameter — which is what a
// terminal does once ReportAlternateKeys is granted, the flag internal/app/app.go now requests
// alongside ReportAllKeysAsEscapeCodes — the decoder resolves the correct shifted character
// directly from that component rather than upper-casing the base rune. This is what makes "?"
// open help and lets ":" reach the digest-override input on a kitty-protocol terminal.
func TestKittyShiftedPunctuationDecodesCorrectlyWithAlternateKeys(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want string
	}{
		// unicode-key-code:shifted-key-code ; modifiers u — the extended CSI-u form a terminal
		// sends once ReportAlternateKeys is granted.
		{"shift+slash with shifted key '?' (63)", "\x1b[47:63;2u", "?"},
		{"shift+semicolon with shifted key ':' (58)", "\x1b[59:58;2u", ":"},
		{"shift+2 with shifted key '@' (64)", "\x1b[50:64;2u", "@"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := decodeKey(t, c.seq).Key()
			if k.Text != c.want {
				t.Fatalf("Text = %q, want %q", k.Text, c.want)
			}
		})
	}
}

// TestCapsLockOnlyStillRejectedForWrites: the fix must not accidentally widen what counts as
// shift. A caps-lock letter with no Shift modifier held — decoded here from the same extended
// CSI-u form the ReportAlternateKeys fix relies on — still must not satisfy a Write binding's
// Matches (internal/ui/keys/keys.go, point 3 of Binding.Matches: "if the terminal reported
// ModCapsLock, ModShift must also be set"). This is §9.13/keys.CINone's own guarantee; the
// point of this test is that requesting the extra kitty flags didn't quietly change it.
func TestCapsLockOnlyStillRejectedForWrites(t *testing.T) {
	// 'c' = 99, kitty modifier byte 65 = kittyCapsLock(64)+1, i.e. caps lock alone, no shift.
	msg := decodeKey(t, "\x1b[99;65u")
	k := msg.Key()
	if !k.Mod.Contains(tea.ModCapsLock) {
		t.Fatalf("test setup: decoded Mod = %v, want ModCapsLock set", k.Mod)
	}
	if k.Mod.Contains(tea.ModShift) {
		t.Fatalf("test setup: decoded Mod = %v, want ModShift NOT set", k.Mod)
	}
	if keys.CINone.Matches(msg) {
		t.Error("Binding.Matches accepted a caps-lock-only letter as a write — shift is required")
	}
}

// TestShiftLetterStillMatchesForWrites is TestCapsLockOnlyStillRejectedForWrites' positive
// control: without it, a Matches that always returns false would pass the negative test above
// for the wrong reason.
func TestShiftLetterStillMatchesForWrites(t *testing.T) {
	// 'c' = 99, kitty modifier byte 2 = kittyShift(1)+1.
	msg := decodeKey(t, "\x1b[99;2u")
	k := msg.Key()
	if !k.Mod.Contains(tea.ModShift) {
		t.Fatalf("test setup: decoded Mod = %v, want ModShift set", k.Mod)
	}
	if !keys.CINone.Matches(msg) {
		t.Error("Binding.Matches rejected a real shift+c — the positive control is broken")
	}
}
