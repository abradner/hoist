package keys

import "charm.land/huh/v2"

// HuhKeyMap is the one huh.KeyMap every standalone huh field in this app should use, built
// from huh.NewDefaultKeyMap() rather than a zero one (AGENTS.md §9 entry 6: a bare
// &huh.KeyMap{} ignores every key, which shipped broken twice before that rule existed).
//
// Two changes from the default:
//   - g/G (goto top/bottom) are stripped everywhere they exist, since g/G retire under the
//     keymap's own rule 7 (home/end replace them; h/l already retired as arrow aliases).
//   - MultiSelect's Toggle is space only — the default also binds "x" for toggle, which the
//     keymap's rule 5 reserves for abandon (a Write binding on the screens that use it) and
//     the migration checklist's own plan-screen row (x toggle repo → space toggle repo).
func HuhKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()

	km.MultiSelect.Toggle.SetKeys("space")

	km.Select.GotoTop.SetKeys("home")
	km.Select.GotoBottom.SetKeys("end")
	km.MultiSelect.GotoTop.SetKeys("home")
	km.MultiSelect.GotoBottom.SetKeys("end")
	km.FilePicker.GotoTop.SetKeys()
	km.FilePicker.GotoBottom.SetKeys()

	return km
}
