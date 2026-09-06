package uitest

import (
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// toy is the smallest screen-shaped model: it records every message it saw and, on "go",
// issues a batch whose members include a spinner tick that must be dropped.
type toy struct {
	seen []string
}

func toyUpdate(m toy, msg tea.Msg) (toy, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyPressMsg:
		m.seen = append(m.seen, "key:"+v.String())
		if v.String() == "g" {
			return m, tea.Batch(
				func() tea.Msg { return "first" },
				func() tea.Msg { return spinner.TickMsg{} },
				func() tea.Msg { return "second" },
			)
		}
	case string:
		m.seen = append(m.seen, v)
		if v == "first" {
			return m, func() tea.Msg { return "nested" }
		}
	case spinner.TickMsg:
		m.seen = append(m.seen, "TICK MUST NOT ARRIVE")
	}
	return m, nil
}

func TestKeysDrainsBatchesInOrderAndDropsSpinnerTicks(t *testing.T) {
	m := Keys(toy{}, toyUpdate, "g", "enter", "esc", "space", "y")
	want := []string{"key:g", "first", "nested", "second", "key:enter", "key:esc", "key:space", "key:y"}
	if len(m.seen) != len(want) {
		t.Fatalf("seen = %v, want %v", m.seen, want)
	}
	for i := range want {
		if m.seen[i] != want[i] {
			t.Fatalf("seen[%d] = %q, want %q (all: %v)", i, m.seen[i], want[i], m.seen)
		}
	}
}

func TestKeyCarriesTextForCharacters(t *testing.T) {
	if k := Key("y"); k.Code != 'y' || k.Text != "y" {
		t.Fatalf("Key(y) = %+v", k)
	}
	if k := Key("enter"); k.Code != tea.KeyEnter || k.Text != "" {
		t.Fatalf("Key(enter) = %+v", k)
	}
	if k := Key("ctrl+r"); k.Code != 'r' || k.Mod != tea.ModCtrl || k.String() != "ctrl+r" {
		t.Fatalf("Key(ctrl+r) = %+v", k)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown multi-character name must panic, not silently press something")
		}
	}()
	Key("bogus")
}
