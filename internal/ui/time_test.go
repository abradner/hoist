package ui

import (
	"testing"
	"time"
)

func TestSpanAgoUntil(t *testing.T) {
	now := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		span string
		ago  string
	}{
		{30 * time.Second, "just now", "just now"},
		{12 * time.Minute, "12m", "12m ago"},
		{3*time.Hour + 48*time.Minute, "3h 48m", "3h 48m ago"},
		{5 * time.Hour, "5h", "5h ago"},
		{24 * time.Hour, "1 day", "1 day ago"},
		{3 * 24 * time.Hour, "3 days", "3 days ago"},
		{34 * 24 * time.Hour, "4 weeks", "4 weeks ago"},
		{100 * 24 * time.Hour, "3 months", "3 months ago"},
		{3 * 365 * 24 * time.Hour, "3 years", "3 years ago"},
	}
	for _, tc := range cases {
		if got := Span(tc.d); got != tc.span {
			t.Errorf("Span(%v) = %q, want %q", tc.d, got, tc.span)
		}
		if got := Ago(now, now.Add(-tc.d)); got != tc.ago {
			t.Errorf("Ago(-%v) = %q, want %q", tc.d, got, tc.ago)
		}
	}
	if got := Ago(now, time.Time{}); got != "never" {
		t.Errorf("Ago(zero) = %q", got)
	}
	if got := Until(now, now.Add(3*time.Hour+48*time.Minute)); got != "in 3h 48m" {
		t.Errorf("Until = %q", got)
	}
	if got := Until(now, now.Add(10*time.Second)); got != "now" {
		t.Errorf("Until(now) = %q", got)
	}
	if got := Until(now, now.Add(-12*time.Minute)); got != "overdue by 12m" {
		t.Errorf("Until(past) = %q", got)
	}
	if got := Until(now, time.Time{}); got != "" {
		t.Errorf("Until(zero) = %q", got)
	}
}
