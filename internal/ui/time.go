package ui

import (
	"fmt"
	"time"
)

// Relative time, the way an operator judges it: "3 days ago" is the decision, "2026-03-01
// 00:00" is arithmetic homework (docs/tui/mockups.html). Every screen that renders one takes
// a `now func() time.Time` so its golden files are stable.

// Span words a duration: "just now" under a minute, then "12m", "3h 48m", "3 days",
// "5 weeks", "3 months", "2 years". Coarse on purpose — the reader is comparing builds, not
// timing them.
func Span(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	case d < 14*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24/7), "week")
	case d < 2*365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	default:
		return plural(int(d.Hours()/24/365), "year")
	}
}

// Ago is Span for a past instant: "34 days ago", "just now", or "never" for a zero time.
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "just now"
	}
	return Span(d) + " ago"
}

// Until is Span for a future instant: "in 3h 48m", "now" when it has arrived, "overdue by
// 12m" when it has passed, or "" for a zero time (no deadline).
func Until(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := t.Sub(now)
	switch {
	case d < -time.Minute:
		return "overdue by " + Span(-d)
	case d < time.Minute:
		return "now"
	default:
		return "in " + Span(d)
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
