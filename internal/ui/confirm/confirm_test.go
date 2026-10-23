package confirm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/ui/uitest"
)

// dialog is the smallest screen-shaped holder of a Confirm, as a value type like every screen.
type dialog struct{ c *huh.Confirm }

func update(d dialog, msg tea.Msg) (dialog, tea.Cmd) {
	f, cmd := d.c.Update(msg)
	if c, ok := f.(*huh.Confirm); ok {
		d.c = c
	}
	return d, cmd
}

func open(t *testing.T, width int) dialog {
	t.Helper()
	d := dialog{c: New("Proceed?", width)}
	return uitest.Drain(d, tea.Batch(d.c.Init(), d.c.Focus()), update)
}

// Real keypresses, never the bool: a Confirm without the keys.HuhKeyMap keymap ignores every
// one of these (AGENTS.md §9 entry 6), which is what makes this test able to fail.
func TestAnswersComeFromRealKeys(t *testing.T) {
	if Agreed(open(t, 80).c) {
		t.Fatal("an untouched dialog must read as no")
	}
	for _, tc := range []struct {
		name string
		keys []string
		want bool
	}{
		{"y", []string{"y"}, true},
		{"y then n", []string{"y", "n"}, false},
		{"left then enter", []string{"left", "enter"}, true},
		{"enter alone keeps the default no", []string{"enter"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := uitest.Keys(open(t, 80), update, tc.keys...)
			if got := Agreed(d.c); got != tc.want {
				t.Fatalf("Agreed after %v = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}

func TestWidthIsTheDialogClampAndZeroLeavesItUnset(t *testing.T) {
	sized := open(t, 200).c.View()
	unsized := open(t, 0).c.View()
	if sized == "" || unsized == "" {
		t.Fatal("both dialogs must render")
	}
	narrow := open(t, 40).c.View()
	if sized == narrow {
		t.Fatalf("width must reach the widget: 200 and 40 columns rendered the same:\n%s", sized)
	}
}
