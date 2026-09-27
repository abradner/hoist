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
// (zero-value, so they do nothing). That "no screen needs horizontal scroll" assumption held
// for hard-truncated content but not for a long yaml line: the plan impact pane and deploy's
// own diff viewport used to hard-wrap (bubbles' default when SoftWrap is off is actually a
// horizontal crop, not a wrap), which cut a long image reference off before its digest ever
// scrolled into view — and with Left/Right unbound there, there was no way to see the rest
// either. T3 followup, group 2: both screens now set SoftWrap on their own viewport instead of
// wiring up horizontal scroll here, so the full line — digest included — is always on screen,
// just taller. A future viewport that genuinely needs horizontal scroll over unwrapped content
// should bind Left/Right explicitly at that call site rather than here, since this keymap is
// shared by several screens that scroll only vertically.
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
