package keys

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
)

func testStyles() ui.Styles { return ui.NewStyles(true) }

// footerHints builds a representative hint list for a screen from its own registry row: every
// bound key except esc and help, which Footer supplies itself (esc when the caller includes it
// — it does here whenever the screen has it bound — and help via Footer's own `help` argument)
// — enough to exercise shortening and dropping without hard-coding every screen's real footer.
func footerHints(s Screen) []Hint {
	entries := On(s)
	var out []Hint
	pri := 0
	for _, e := range entries {
		if e.Name == Esc.Name || e.Name == Help.Name {
			continue
		}
		out = append(out, Hint{B: e.Binding, Long: e.Show + " " + e.Name, Short: e.Show, Pri: pri})
		pri++
	}
	if Has(s, Esc) {
		out = append(out, Hint{B: Esc, Long: "esc back", Short: "esc", Pri: -1})
	}
	return out
}

// TestFooterKeepsEscAtEveryWidth uses a hint set where esc is deliberately given the LEAST
// important priority of the bunch (Pri 100, higher than every other hint here) — the shape
// that actually exercises pinning: with esc pinned, the drop loop skips straight past it no
// matter how high its own Pri number is, and Refresh/Open/Watch (all lower Pri) go first
// instead. A hint set where esc already happens to be the most important would pass whether or
// not pinning existed at all, proving nothing about the "never dropped" rule.
func TestFooterKeepsEscAtEveryWidth(t *testing.T) {
	st := testStyles()
	hints := []Hint{
		{B: Refresh, Long: "r refresh the cluster and the repo", Short: "r refresh", Pri: 0},
		{B: Open, Long: "o open the PR of the in-flight row", Short: "o open", Pri: 1},
		{B: Watch, Long: "w watch the cell's family", Short: "w watch", Pri: 2},
		{B: Esc, Long: "esc back", Short: "esc", Pri: 100},
	}
	for w := 8; w <= 120; w += 4 {
		out := Footer(st, w, "hoist · matrix", hints, true)
		if !strings.Contains(ansi.Strip(out), "esc") {
			t.Errorf("width %d: footer dropped esc: %q", w, ansi.Strip(out))
		}
	}
}

func TestFooterOneSeparator(t *testing.T) {
	st := testStyles()
	out := ansi.Strip(Footer(st, 80, "status", footerHints(ScrMatrix), true))
	if strings.Contains(out, "•") {
		t.Errorf("footer uses a bullet separator, want middle dot only: %q", out)
	}
}

func TestFooterShortBeforeDrop(t *testing.T) {
	st := testStyles()
	hints := []Hint{
		{B: Refresh, Long: "r refresh the cluster and the repo", Short: "r refresh", Pri: 5},
		{B: Open, Long: "o open in the browser", Short: "o open", Pri: 4},
		{B: Esc, Long: "esc back", Short: "esc", Pri: -1},
	}
	// Wide enough for every long form.
	full := ansi.Strip(Footer(st, 120, "", hints, true))
	if !strings.Contains(full, "r refresh the cluster and the repo") {
		t.Fatalf("expected the long form at width 120: %q", full)
	}

	// Narrow enough that the long forms cannot all fit, but the short forms still can: some
	// hint must have switched to Short while all four are still present.
	mid := ansi.Strip(Footer(st, 40, "", hints, true))
	if strings.Contains(mid, "r refresh the cluster and the repo") {
		t.Fatalf("width 40 should have shortened the refresh hint: %q", mid)
	}
	if !strings.Contains(mid, "r refresh") || !strings.Contains(mid, "esc") || !strings.Contains(mid, "help") {
		t.Fatalf("width 40 should keep every hint, shortened: %q", mid)
	}
}

func TestFooterMoreWhenDropped(t *testing.T) {
	st := testStyles()
	hints := []Hint{
		{B: Refresh, Long: "r refresh the cluster and the repo", Short: "r refresh", Pri: 5},
		{B: Open, Long: "o open in the browser", Short: "o open", Pri: 4},
		{B: Esc, Long: "esc back", Short: "esc", Pri: -1},
	}
	wide := ansi.Strip(Footer(st, 120, "", hints, true))
	if !strings.Contains(wide, "? help") {
		t.Fatalf("wide footer should show '? help': %q", wide)
	}
	narrow := ansi.Strip(Footer(st, 14, "", hints, true))
	if !strings.Contains(narrow, "? more") {
		t.Fatalf("narrow footer should show '? more' once a hint is dropped: %q", narrow)
	}
	if strings.Contains(narrow, "? help") {
		t.Fatalf("narrow footer should not still show '? help': %q", narrow)
	}
}

// TestFooterHelpFalseOmitsHint: the ? hint only appears when the caller asks for it — the help
// overlay itself, where ? closes the overlay rather than opening it, is the one screen that
// should not advertise "? help" in its own footer.
func TestFooterHelpFalseOmitsHint(t *testing.T) {
	st := testStyles()
	out := ansi.Strip(Footer(st, 80, "", []Hint{{B: Esc, Long: "esc close", Short: "esc", Pri: 0}}, false))
	if strings.Contains(out, "?") {
		t.Errorf("help=false should omit the ? hint entirely: %q", out)
	}
}

func TestFooterNeverOverWidth(t *testing.T) {
	st := testStyles()
	for _, s := range Screens() {
		hints := footerHints(s)
		for w := 10; w <= 130; w += 7 {
			out := Footer(st, w, "hoist · "+string(s), hints, true)
			if got := ansi.StringWidth(ansi.Strip(out)); got > w {
				t.Errorf("%s width %d: footer is %d cells wide: %q", s, w, got, out)
			}
		}
	}
}
