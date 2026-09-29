package keys

import (
	"regexp"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/abradner/hoist/internal/ui/uitest"
)

// TestOneClassPerKey asserts audit rule 1: every non-write key string in the registry maps to
// exactly one Class, across every screen it appears on.
func TestOneClassPerKey(t *testing.T) {
	classOf := map[string]Class{}
	for _, r := range table {
		if r.B.Class == Write {
			continue // matched by Matches, not a fixed key string; see TestShiftOnlyOnWrites
		}
		for _, k := range r.B.Keys {
			if c, ok := classOf[k]; ok {
				if c != r.B.Class {
					t.Errorf("key %q is Class %v on %q (%s) but %v elsewhere", k, r.B.Class, r.Screen, r.B.Name, c)
				}
				continue
			}
			classOf[k] = r.B.Class
		}
	}
}

// TestLetterOneMeaning asserts audit rule 1's other half: a single-character key resolves to
// exactly one Name everywhere it is bound (unmodified keys only — a write binding's shift+
// form is a different key from its own bare letter, by design: r refreshes, shift+r restarts).
func TestLetterOneMeaning(t *testing.T) {
	nameOf := map[string]string{}
	for _, r := range table {
		if r.B.Class == Write {
			continue
		}
		for _, k := range r.B.Keys {
			if len([]rune(k)) != 1 {
				continue
			}
			if n, ok := nameOf[k]; ok {
				if n != r.B.Name {
					t.Errorf("key %q means %q on %q but %q elsewhere", k, r.B.Name, r.Screen, n)
				}
				continue
			}
			nameOf[k] = r.B.Name
		}
	}
}

func TestQOnlyOnMatrix(t *testing.T) {
	for _, r := range table {
		if r.B.Name == Quit.Name && r.Screen != ScrMatrix {
			t.Errorf("q is bound on %q, want matrix only", r.Screen)
		}
	}
	if !Has(ScrMatrix, Quit) {
		t.Error("q is not bound on the matrix")
	}
}

func TestEnterUnboundOnWatchingScreens(t *testing.T) {
	for _, s := range []Screen{ScrFlight, ScrWatch, ScrConfig} {
		if Has(s, Enter) {
			t.Errorf("enter is bound on %q, a watching screen with no primary action", s)
		}
	}
}

// TestShiftOnlyOnWrites asserts audit rule 5: only a Write-class binding is ever shown with a
// shift+ prefix.
func TestShiftOnlyOnWrites(t *testing.T) {
	for _, r := range table {
		hasShift := len(r.B.Show) >= 6 && r.B.Show[:6] == "shift+"
		if hasShift != (r.B.Class == Write) {
			t.Errorf("%q on %q: Show=%q, Class=%v — shift+ and Write class must agree", r.B.Name, r.Screen, r.B.Show, r.B.Class)
		}
	}
}

// bareCapital matches a standalone single capital letter: the shape "press R" (or a footer's
// "R restart") that rule 5 says a write key must never be displayed as, because a legacy
// terminal cannot tell a deliberate shift+r from an accidental caps-lock r.
var bareCapital = regexp.MustCompile(`(^|[\s·])[A-Z]([\s·]|$)`)

func TestNoBareCapital(t *testing.T) {
	for _, r := range table {
		if bareCapital.MatchString(r.B.Show) {
			t.Errorf("%q on %q: Show %q has a bare capital", r.B.Name, r.Screen, r.B.Show)
		}
		if bareCapital.MatchString(r.Desc) {
			t.Errorf("%q on %q: Desc %q has a bare capital", r.B.Name, r.Screen, r.Desc)
		}
	}
	st := testStyles()
	for w := 40; w <= 200; w += 10 {
		for _, s := range Screens() {
			hints := footerHints(s)
			out := Footer(st, w, "status", hints, true)
			if bareCapital.MatchString(out) {
				t.Errorf("Footer(%q, width=%d) has a bare capital: %q", s, w, out)
			}
		}
	}
}

// TestWriteMatches drives the four-rule Matches test from the audit doc directly, one
// tea.KeyPressMsg at a time, rather than through key.Matches (which cannot see a write binding
// at all — Binding.Bubbles only exists for components that need a fixed key.Binding).
func TestWriteMatches(t *testing.T) {
	r := Restart // shift+r
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"legacy shift", "shift+r", true},
		{"legacy capital", "capslock-unaware-R", true}, // see below: same byte as legacy shift
		{"kitty shift", "shift+r", true},
		{"kitty caps lock, no shift", "capslock+r", false},
		{"kitty shift+caps lock", "shift+capslock+r", true},
		{"lower case, no modifier", "r", false},
		{"ctrl+shift", "ctrl+shift+r", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := writeKey(c.key)
			if got := r.Matches(msg); got != c.want {
				t.Errorf("Matches(%q) = %v, want %v (msg=%+v)", c.key, got, c.want, msg)
			}
		})
	}
}

// writeKey builds the tea.KeyPressMsg each TestWriteMatches case needs. Legacy terminals (no
// keyboard enhancement granted) send a bare uppercase byte for both shift+letter and
// caps-lock-then-letter — "legacy capital" reproduces exactly that ambiguous byte, the case
// rule 4 accepts since it is the only signal a legacy terminal ever sends.
func writeKey(name string) tea.KeyPressMsg {
	switch name {
	case "shift+r":
		return uitest.Key("shift+r")
	case "capslock-unaware-R":
		return tea.KeyPressMsg{Code: 'r', Text: "R", Mod: tea.ModShift}
	case "capslock+r":
		return uitest.Key("capslock+r")
	case "shift+capslock+r":
		return tea.KeyPressMsg{Code: 'r', Text: "R", Mod: tea.ModShift | tea.ModCapsLock}
	case "r":
		return tea.KeyPressMsg{Code: 'r', Text: "r"}
	case "ctrl+shift+r":
		return tea.KeyPressMsg{Code: 'r', Text: "R", Mod: tea.ModShift | tea.ModCtrl}
	}
	panic("writeKey: unknown case " + name)
}

func TestHuhKeyMapNotZero(t *testing.T) {
	km := HuhKeyMap()
	if len(km.Confirm.Accept.Keys()) == 0 {
		t.Error("Confirm.Accept has no keys — a zero keymap regression (AGENTS.md §9 entry 6)")
	}
	if len(km.Confirm.Reject.Keys()) == 0 {
		t.Error("Confirm.Reject has no keys")
	}
	if len(km.Confirm.Submit.Keys()) == 0 {
		t.Error("Confirm.Submit has no keys")
	}
	got := km.MultiSelect.Toggle.Keys()
	if len(got) != 1 || got[0] != "space" {
		t.Errorf("MultiSelect.Toggle.Keys() = %v, want exactly [\"space\"]", got)
	}
}
