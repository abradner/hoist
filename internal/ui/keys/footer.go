package keys

import (
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abradner/hoist/internal/ui"
)

// Hint is one key shown in a screen's footer: the binding it names, its long and short display
// text, and a priority Footer uses to decide what gives way first when the terminal is narrow.
// A lower Pri is more important — Footer shortens and drops from the HIGHEST Pri down, so the
// hints a screen lists first (usually its own primary actions) survive longest by construction
// whenever the caller also gives them the lower numbers; nothing here assumes an order beyond
// what Pri states.
type Hint struct {
	B           Binding
	Long, Short string
	Pri         int
}

// Footer lays out one status-bar line: status on the left, hints on the right, joined by
// " · " in the order given (the audit doc, "Footer rules"). help, when true, appends the ?
// hint itself — the caller never builds one — so "? help" becomes "? more" once anything else
// is dropped is Footer's own bookkeeping rather than something every call site has to get
// right (spec wins over any mockup still showing "? help" once a screen is that narrow, this
// PR's binding decision). esc, if present in hints, is named by Binding and is never dropped
// alongside it. status is cut before any hint is touched, and removed entirely rather than
// shown as a near-unreadable fragment once its own available room drops under 8 cells. The
// result is never wider than width, because ui.StatusBar — the last step — guarantees it.
func Footer(st ui.Styles, width int, status string, hints []Hint, help bool) string {
	if width <= 0 {
		return ""
	}
	all := hints
	if help {
		all = make([]Hint, 0, len(hints)+1)
		all = append(all, hints...)
		all = append(all, Hint{B: Help, Pri: -1})
	}
	hintText := shrinkHints(all, width)
	avail := width - ansi.StringWidth(hintText) - 1
	left := status
	if avail < 8 {
		left = ""
	}
	return ui.StatusBar(width, styled(st.Status, left), styled(st.Hint, hintText))
}

func styled(s lipgloss.Style, text string) string {
	if text == "" {
		return ""
	}
	return s.Render(text)
}

// hintItem is shrinkHints' own working copy of a Hint: whether it is still included, and
// whether it has already been switched to its Short form.
type hintItem struct {
	h       Hint
	short   bool
	include bool
}

// pinned reports whether a hint's binding is exempt from both shortening and dropping — esc
// and help, by Name, per the audit doc's "esc and ? are never dropped".
func pinned(b Binding) bool {
	return b.Name == Esc.Name || b.Name == Help.Name
}

// shrinkHints joins hints into one string that fits width, escalating through the rules in
// order: try every hint at full length; if that overflows, switch hints to their short form,
// starting with the least important (highest Pri) and excluding the pinned pair; if hints are
// still too wide even all-short, start dropping them, again least important first. help's own
// text ("? help"/"? more") is recomputed on every join, since whether anything has been
// dropped can change from one escalation step to the next.
func shrinkHints(hints []Hint, width int) string {
	items := make([]hintItem, len(hints))
	for i, h := range hints {
		items[i] = hintItem{h: h, include: true}
	}

	join := func() string {
		dropped := false
		for _, it := range items {
			if !it.include {
				dropped = true
				break
			}
		}
		parts := make([]string, 0, len(items))
		for _, it := range items {
			if !it.include {
				continue
			}
			if it.h.B.Name == Help.Name {
				if dropped {
					parts = append(parts, "? more")
				} else {
					parts = append(parts, "? help")
				}
				continue
			}
			if it.short && it.h.Short != "" {
				parts = append(parts, it.h.Short)
			} else {
				parts = append(parts, it.h.Long)
			}
		}
		return strings.Join(parts, " · ")
	}
	fits := func() bool { return ansi.StringWidth(join()) <= width }

	if fits() {
		return join()
	}

	byPri := func(order []int) {
		sort.SliceStable(order, func(a, b int) bool {
			return items[order[a]].h.Pri > items[order[b]].h.Pri
		})
	}

	// Phase 1: switch to short form, lowest priority first. Esc and help may shorten too —
	// "never dropped" (below) is not "never shortened": at the narrowest widths "esc"/"? help"
	// still have to fit beside each other.
	shortenOrder := make([]int, len(items))
	for i := range items {
		shortenOrder[i] = i
	}
	byPri(shortenOrder)
	for _, i := range shortenOrder {
		if fits() {
			break
		}
		if items[i].h.Short != "" {
			items[i].short = true
		}
	}

	// Phase 2: drop, lowest priority first — pinned (esc, help) excluded.
	dropOrder := make([]int, 0, len(items))
	for i, it := range items {
		if !pinned(it.h.B) {
			dropOrder = append(dropOrder, i)
		}
	}
	byPri(dropOrder)
	for _, i := range dropOrder {
		if fits() {
			break
		}
		items[i].include = false
	}
	return join()
}
