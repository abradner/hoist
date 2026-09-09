package watch

import (
	"testing"
	"time"
)

func TestEveryReadsAsACadence(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Second: "5s", time.Minute: "1m", 10 * time.Minute: "10m",
		90 * time.Second: "1m30s", time.Hour: "1h", time.Hour + 10*time.Minute: "1h10m", 0: "0s",
	} {
		if got := every(d); got != want {
			t.Errorf("every(%v) = %q, want %q", d, got, want)
		}
	}
}
