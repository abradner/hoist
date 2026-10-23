// Package confirm builds the standalone huh.Confirm every screen raises inside a ui.Dialog. It
// is a package of its own because it needs both ui and ui/keys, and keys imports ui.
package confirm

import (
	"charm.land/huh/v2"

	"github.com/abradner/hoist/internal/ui"
	"github.com/abradner/hoist/internal/ui/keys"
)

// New returns a huh.Confirm with title, wired the way every standalone huh field in this app
// must be (AGENTS.md §9 entry 6): keys.HuhKeyMap, because huh.NewConfirm ships a zero keymap
// and ignores every key without one, the Charm theme, and ui.DialogWidth(termWidth) as its
// width. A termWidth of 0 or less leaves the width unset, for a screen that has not been sized.
//
// It deliberately does not take a Value pointer: Value(&m.field) captures a field on the copy
// of a value-typed model that built the widget, which nothing later reads. Read the answer
// with Agreed (GetValue). The caller chains Description/Affirmative/Negative on the result and
// starts it with tea.Batch(c.Init(), c.Focus()); a resize calls
// c.WithWidth(ui.DialogWidth(width)).
func New(title string, termWidth int) *huh.Confirm {
	c := huh.NewConfirm().Title(title)
	c.WithKeyMap(keys.HuhKeyMap())
	c.WithTheme(huh.ThemeFunc(huh.ThemeCharm))
	if termWidth > 0 {
		c.WithWidth(ui.DialogWidth(termWidth))
	}
	return c
}

// Agreed reads the operator's answer from the widget (GetValue), never from a bound bool.
func Agreed(c *huh.Confirm) bool {
	v, _ := c.GetValue().(bool)
	return v
}
