package keys

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestHelpViewListsEveryBoundKeyExceptItself: every entry On(ScrWatch) returns except Help
// itself appears in the body, grouped under a heading.
func TestHelpViewListsEveryBoundKeyExceptItself(t *testing.T) {
	body := HelpView(ScrWatch, tea.KeyboardEnhancementsMsg{})
	for _, e := range On(ScrWatch) {
		if e.Name == Help.Name {
			if strings.Contains(body, "? "+e.Desc) {
				t.Errorf("the overlay should not list its own ? key:\n%s", body)
			}
			continue
		}
		if !strings.Contains(body, e.Show) {
			t.Errorf("HelpView(ScrWatch) is missing %q (%s):\n%s", e.Show, e.Desc, body)
		}
	}
	if !strings.Contains(body, "NAVIGATE") || !strings.Contains(body, "VIEW") {
		t.Errorf("HelpView should group under headings:\n%s", body)
	}
}

// TestHelpViewNamesShiftWritesOnlyWhenPresent: the "shift+ keys always ask" line appears only
// for a screen that actually has a Write binding (matrix does, watch does not).
func TestHelpViewNamesShiftWritesOnlyWhenPresent(t *testing.T) {
	if strings.Contains(HelpView(ScrWatch, tea.KeyboardEnhancementsMsg{}), "shift+ keys always ask") {
		t.Error("watch has no write binding; the overlay should not mention shift+ writes")
	}
	if !strings.Contains(HelpView(ScrMatrix, tea.KeyboardEnhancementsMsg{}), "shift+ keys always ask") {
		t.Error("matrix has write bindings (restart, abandon); the overlay should mention shift+ writes")
	}
}

// TestKbdLineNamesWhatWasGranted: the overlay always says whether this run can tell a caps-lock
// letter from a real shift (train3-design.md's own resolution of that question).
func TestKbdLineNamesWhatWasGranted(t *testing.T) {
	plain := HelpView(ScrWatch, tea.KeyboardEnhancementsMsg{})
	if !strings.Contains(plain, "a capital counts as shift") {
		t.Errorf("no flag 8 granted: want the legacy-path line:\n%s", plain)
	}
	granted := HelpView(ScrWatch, tea.KeyboardEnhancementsMsg{Flags: 1 << 3})
	if !strings.Contains(granted, "caps lock ignored") {
		t.Errorf("flag 8 granted: want the caps-lock line:\n%s", granted)
	}
}

// TestHelpTitleNamesTheScreen pins HelpTitle's own format, since app.go's View builds the
// dialog's title from it directly rather than formatting "help · <screen>" a second way.
func TestHelpTitleNamesTheScreen(t *testing.T) {
	if got, want := HelpTitle(ScrConfig), "help · config"; got != want {
		t.Errorf("HelpTitle(ScrConfig) = %q, want %q", got, want)
	}
}
