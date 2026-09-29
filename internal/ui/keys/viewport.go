package keys

import "charm.land/bubbles/v2/viewport"

// ViewportKeyMap is the one viewport.KeyMap every read-only scrolling body in this app should
// use — config, activity and the tag picker's commit reader already built this same literal by
// hand (three copies drifting independently); P2-6 in the T3 review found that plan, deploy,
// flight, restart and watch's own viewports were left on viewport.New()'s bubbles-library
// default instead, which binds space/f/b to PageDown/PageUp, "d"/"u" (bare, no ctrl) to
// half-page, and left/right/h/l to horizontal scroll — none of it ever shown on a footer or
// help overlay, all of it live. That is why space still scrolled the deploy confirm screen
// after the audit retired the gesture, and why "d" (yaml diff on plan/deploy, unbound
// everywhere else) paged flight/watch/restart's own log/body out from under whatever the
// screen's own switch thought "d" meant.
//
// Only paging and line movement are bound, through the same bindings the footer/help overlay
// already show (PgUp/PgDn/HalfPageUp/HalfPageDown/Up/Down) — Left/Right are left unbound
// (zero-value, so they do nothing) since no screen using this needs horizontal scroll.
func ViewportKeyMap() viewport.KeyMap {
	return viewport.KeyMap{
		PageDown:     PgDn.Bubbles(),
		PageUp:       PgUp.Bubbles(),
		HalfPageDown: HalfPageDown.Bubbles(),
		HalfPageUp:   HalfPageUp.Bubbles(),
		Down:         Down.Bubbles(),
		Up:           Up.Bubbles(),
	}
}
